package api

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/k8s"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/terminal"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/transcript"
	"github.com/usr-lex/swarmageddon/internal/supervisor/roomcfg"
)

// roomContainer is the room pod's main container.
const roomContainer = "room"

// runner returns the exec runner for non-TTY commands. terminal.Kube
// implements both Execer and Runner.
func (s *Server) runner() (terminal.Runner, error) {
	if r, ok := s.Exec.(terminal.Runner); ok {
		return r, nil
	}
	return nil, connect.NewError(connect.CodeUnavailable, errors.New("exec into rooms is not available"))
}

func transcriptItemProto(it transcript.Item) *ccv1.TranscriptItem {
	return &ccv1.TranscriptItem{Id: it.ID, At: ts(it.At), Role: it.Role, Text: it.Text, ToolName: it.ToolName,
		ToolInput: it.ToolInput, ToolResult: it.ToolResult}
}

// WatchTranscript follows the room's newest session transcript with
// `tail -F`, run read-only as the agent user, and streams parsed items.
func (s roomSvc) WatchTranscript(ctx context.Context, req *connect.Request[ccv1.WatchTranscriptRequest], stream *connect.ServerStream[ccv1.WatchTranscriptResponse]) error {
	ref := req.Msg.GetRoom()
	if _, err := s.canView(ctx, ref.GetWorkspace()); err != nil {
		return err
	}
	r, err := s.K.Room(ctx, ref.GetWorkspace(), ref.GetName())
	if err != nil {
		return kerr(err)
	}
	run, err := s.runner()
	if err != nil {
		return err
	}
	tail := int(req.Msg.GetTailLines())
	if tail <= 0 {
		tail = 200
	}
	tail = min(tail, 2000)
	cli := string(r.Spec.CLI)
	if cli != string(v1alpha1.CLIOre) {
		cli = string(v1alpha1.CLIClaude)
	}
	cwd := (&roomcfg.Room{WorkDir: r.Spec.WorkDir, Repos: r.Spec.Repos}).WorkPath("/home/" + r.Name)
	cmd := transcript.Command(r.Name, cli, cwd, tail)

	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := run.Run(execCtx, v1alpha1.WorkspaceNamespace(ref.GetWorkspace()), k8s.PodName(r.Name), roomContainer, cmd, pw, io.Discard)
		pw.CloseWithError(err)
		done <- err
	}()
	stop := func() error {
		cancel()
		pr.Close()
		return <-done
	}
	p := &transcript.Parser{CLI: cli}
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 64*1024), 32<<20)
	for sc.Scan() {
		for _, it := range p.Parse(sc.Bytes()) {
			if err := stream.Send(&ccv1.WatchTranscriptResponse{Item: transcriptItemProto(it)}); err != nil {
				stop()
				return err
			}
		}
	}
	err = stop()
	if ctx.Err() != nil {
		return nil // the viewer left
	}
	if err == nil {
		err = sc.Err() // e.g. a line over the limit
	}
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, errors.New("reading the transcript: "+err.Error()))
	}
	return nil
}

// pruneArgs are the docker arguments for a prune kind.
func pruneArgs(kind string, volumes bool) ([]string, error) {
	switch kind {
	case "containers":
		return []string{"container", "prune", "-f"}, nil
	case "images":
		return []string{"image", "prune", "-af"}, nil
	case "volumes":
		return []string{"volume", "prune", "-f"}, nil
	case "buildCache":
		return []string{"builder", "prune", "-af"}, nil
	case "all":
		if volumes {
			return []string{"system", "prune", "-f", "--volumes"}, nil
		}
		return []string{"system", "prune", "-f"}, nil
	}
	return nil, errInvalid("kind must be containers, images, volumes, buildCache or all")
}

var reclaimedRE = regexp.MustCompile(`(?m)^Total(?: reclaimed space)?:\s*([0-9.]+)\s*([kKMGTP]?i?B)\s*$`)

// reclaimed sums the "Total reclaimed space" lines of docker's output
// (decimal units, as docker prints them).
func reclaimed(out string) int64 {
	mult := map[string]float64{"B": 1, "kB": 1e3, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12, "PB": 1e15,
		"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40}
	var total float64
	for _, m := range reclaimedRE.FindAllStringSubmatch(out, -1) {
		n, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			continue
		}
		total += n * mult[m[2]]
	}
	return int64(total)
}

const maxPruneOutput = 16 << 10

// PruneEngine runs `docker ... prune` as the agent user in the room.
func (s roomSvc) PruneEngine(ctx context.Context, req *connect.Request[ccv1.PruneEngineRequest]) (*connect.Response[ccv1.PruneEngineResponse], error) {
	ref := req.Msg.GetRoom()
	if _, err := s.canOperate(ctx, ref.GetWorkspace()); err != nil {
		return nil, err
	}
	args, err := pruneArgs(req.Msg.GetKind(), req.Msg.GetVolumes())
	if err != nil {
		return nil, err
	}
	r, err := s.K.Room(ctx, ref.GetWorkspace(), ref.GetName())
	if err != nil {
		return nil, kerr(err)
	}
	run, err := s.runner()
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	var out bytes.Buffer
	cmd := append([]string{"runuser", "-u", r.Name, "--", "docker"}, args...)
	err = run.Run(cctx, v1alpha1.WorkspaceNamespace(ref.GetWorkspace()), k8s.PodName(r.Name), roomContainer, cmd, &out, &out)
	text := transcript.Clip(out.String(), maxPruneOutput)
	n := reclaimed(out.String())
	s.audit(ctx, req, "room.prune", "room/"+r.Name, ref.GetWorkspace(), nil,
		map[string]any{"command": strings.Join(cmd[4:], " "), "reclaimedBytes": n}, err)
	if err != nil {
		msg := strings.TrimSpace(text)
		if msg == "" {
			msg = err.Error()
		}
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("prune failed: "+msg))
	}
	return connect.NewResponse(&ccv1.PruneEngineResponse{Output: text, ReclaimedBytes: n}), nil
}
