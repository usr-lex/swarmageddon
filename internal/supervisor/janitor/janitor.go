// Package janitor keeps the room's container engine store from filling the
// engine volume (design §12; remote-labs' docker-janitor), using the docker
// CLI against Podman's Docker-compatible socket.
package janitor

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Config holds the thresholds; zero values take the defaults.
type Config struct {
	Interval   time.Duration // default 1 h
	PruneAfter time.Duration // default 24 h
	// ImagesAbovePercent of the engine volume also removes unused tagged images (default 70).
	ImagesAbovePercent int
	// WarnPercent marks the volume low (default 80).
	WarnPercent int
	// AggressivePercent prunes everything unused, volumes included (default 90).
	AggressivePercent int
	// EngineDir is the engine volume mount (default /var/lib/containers).
	EngineDir string
	// Docker is the CLI (default "docker"); Env is added to its environment
	// (e.g. DOCKER_HOST=unix:///var/run/docker.sock).
	Docker string
	Env    []string
}

func (c *Config) defaults() {
	if c.Interval == 0 {
		c.Interval = time.Hour
	}
	if c.PruneAfter == 0 {
		c.PruneAfter = 24 * time.Hour
	}
	if c.ImagesAbovePercent == 0 {
		c.ImagesAbovePercent = 70
	}
	if c.WarnPercent == 0 {
		c.WarnPercent = 80
	}
	if c.AggressivePercent == 0 {
		c.AggressivePercent = 90
	}
	if c.EngineDir == "" {
		c.EngineDir = "/var/lib/containers"
	}
	if c.Docker == "" {
		c.Docker = "docker"
	}
}

// Plan is the docker commands for one run at a given volume use.
func (c Config) Plan(usedPercent int) [][]string {
	c.defaults()
	until := "until=" + c.PruneAfter.String()
	cmds := [][]string{
		{"container", "prune", "-f", "--filter", until},
		// No builder prune: Podman's Docker API has no build cache endpoint
		// (404); its intermediate layers are images, pruned above.
		{"image", "prune", "-f", "--filter", until},
		{"volume", "prune", "-f"},
	}
	if usedPercent >= c.ImagesAbovePercent {
		cmds = append(cmds, []string{"image", "prune", "-af"})
	}
	if usedPercent >= c.AggressivePercent {
		cmds = append(cmds, []string{"system", "prune", "-af", "--volumes"})
	}
	return cmds
}

// Usage is a volume's use in bytes.
type Usage struct {
	Used, Capacity int64
}

// Percent is used/capacity in whole percent.
func (u Usage) Percent() int {
	if u.Capacity <= 0 {
		return 0
	}
	return int(u.Used * 100 / u.Capacity)
}

// Stat returns the filesystem use of the volume holding path.
func Stat(path string) (Usage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Usage{}, err
	}
	capacity := int64(st.Blocks) * int64(st.Bsize)
	free := int64(st.Bfree) * int64(st.Bsize)
	return Usage{Used: capacity - free, Capacity: capacity}, nil
}

// Janitor runs the plan periodically.
type Janitor struct {
	Config Config
	Log    *slog.Logger
	// OnUsage is called after each run with the engine volume's use and
	// whether it is above WarnPercent.
	OnUsage func(u Usage, low bool)
	// run executes one docker command (replaced in tests).
	run func(ctx context.Context, args ...string) error
}

func (j *Janitor) docker(ctx context.Context, args ...string) error {
	if j.run != nil {
		return j.run(ctx, args...)
	}
	cmd := exec.CommandContext(ctx, j.Config.Docker, args...)
	cmd.Env = append(cmd.Environ(), j.Config.Env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RunOnce prunes according to the current use.
func (j *Janitor) RunOnce(ctx context.Context) {
	j.Config.defaults()
	u, err := Stat(j.Config.EngineDir)
	if err != nil {
		j.Log.Warn("janitor: statfs", "err", err)
	}
	for _, args := range j.Config.Plan(u.Percent()) {
		if err := j.docker(ctx, args...); err != nil {
			j.Log.Warn("janitor", "err", err)
		}
	}
	after, err := Stat(j.Config.EngineDir)
	if err == nil {
		j.Log.Info("janitor: engine volume", "usedPercent", after.Percent(), "before", u.Percent())
		if j.OnUsage != nil {
			j.OnUsage(after, after.Percent() >= j.Config.WarnPercent)
		}
	}
}

// Run waits for the engine to answer, then runs every Interval.
func (j *Janitor) Run(ctx context.Context) {
	j.Config.defaults()
	for ctx.Err() == nil && j.docker(ctx, "info") != nil {
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
	for {
		j.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(j.Config.Interval):
		}
	}
}
