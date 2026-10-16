package claude

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// PrepareDialogs pre-answers the dialogs that would block an unattended
// start (ADR 0006): onboarding, folder trust, resume-from-summary and, in
// bypass mode, the dangerous-mode confirmation. Files are owned by uid/gid.
func PrepareDialogs(home, trustDir string, bypass bool, uid, gid int) error {
	err := updateJSON(filepath.Join(home, ".claude.json"), uid, gid, func(d map[string]any) {
		d["hasCompletedOnboarding"] = true
		d["resumeReturnDismissed"] = true
		projects, _ := d["projects"].(map[string]any)
		if projects == nil {
			projects = map[string]any{}
		}
		p, _ := projects[trustDir].(map[string]any)
		if p == nil {
			p = map[string]any{}
		}
		p["hasTrustDialogAccepted"] = true
		projects[trustDir] = p
		d["projects"] = projects
	})
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".claude")
	if err := mkdirOwned(dir, uid, gid); err != nil {
		return err
	}
	return updateJSON(filepath.Join(dir, "settings.json"), uid, gid, func(d map[string]any) {
		if bypass {
			d["skipDangerousModePermissionPrompt"] = true
		} else {
			delete(d, "skipDangerousModePermissionPrompt")
		}
	})
}

// updateJSON applies change to a JSON object file, writing atomically.
func updateJSON(path string, uid, gid int, change func(map[string]any)) error {
	d := map[string]any{}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(strings.TrimSpace(string(b))) > 0 {
			if err := json.Unmarshal(b, &d); err != nil {
				return err
			}
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	change(d)
	out, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".swarm.tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	if os.Getuid() == 0 {
		if err := os.Chown(tmp, uid, gid); err != nil {
			return err
		}
	}
	return os.Rename(tmp, path)
}

func mkdirOwned(dir string, uid, gid int) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if os.Getuid() == 0 {
		return os.Chown(dir, uid, gid)
	}
	return nil
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// ProjectDir is where Claude Code keeps transcripts for a working folder.
func ProjectDir(home, cwd string) string {
	return filepath.Join(home, ".claude", "projects", nonAlnum.ReplaceAllString(cwd, "-"))
}

// ResumeArgs picks how to resume: --resume <id> if that transcript exists,
// else --continue if any transcript exists for cwd, else nothing.
func ResumeArgs(home, cwd, sessionID string) []string {
	dir := ProjectDir(home, cwd)
	if sessionID != "" {
		if _, err := os.Stat(filepath.Join(dir, sessionID+".jsonl")); err == nil {
			return []string{"--resume", sessionID}
		}
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.jsonl")); len(m) > 0 {
		return []string{"--continue"}
	}
	return nil
}
