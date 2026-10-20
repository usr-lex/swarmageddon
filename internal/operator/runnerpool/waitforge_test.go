package runnerpool

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A new runner pod's first connection to Forgejo can be refused while the
// cluster's network policy doesn't know its address yet (CI run 32: every
// pod failed Declare with "connection refused" and was replaced, for 15
// minutes). The pod waits for Forgejo to answer, then runs forgejo-runner
// with its arguments unchanged.
func TestRunnerWaitsForForgejo(t *testing.T) {
	bin := t.TempDir()
	tries := filepath.Join(bin, "tries")
	out := filepath.Join(bin, "out")
	stub := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Refused twice, then up.
	stub("curl", `echo x >> `+tries+`; [ "$(wc -l < `+tries+`)" -ge 3 ]`)
	stub("sleep", "true")
	stub("forgejo-runner", `echo "$@" > `+out)
	stub("wc", `/usr/bin/wc "$@"`)

	cmd := waitCommand()
	c := exec.Command(cmd[0], append(cmd[1:], "one-job", "--url", "x y")...)
	c.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "FORGE_URL=http://forgejo:3000"}
	if b, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	got, _ := os.ReadFile(out)
	if strings.TrimSpace(string(got)) != "one-job --url x y" {
		t.Errorf("forgejo-runner got %q", got)
	}
	n, _ := os.ReadFile(tries)
	if strings.Count(string(n), "x") != 3 {
		t.Errorf("curl ran %d times, want 3", strings.Count(string(n), "x"))
	}
}
