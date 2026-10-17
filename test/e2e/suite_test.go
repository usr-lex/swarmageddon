//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// reserve is kept back from every wait for the steps that always run last
// (the log scan), so a slow step can't use up the whole test timeout.
const reserve = 4 * time.Minute

// suite carries what the ordered steps share.
type suite struct {
	t       *testing.T
	kube    client.Client
	kctx    string // kubeconfig context
	kubectl string
	start   time.Time
	nonce   string // marks this run's messages, branches and files
	passed  map[string]bool

	cc       *cc    // the control center; signed in as the local admin once claimed
	lbErr    error  // why the k3d load balancer didn't answer, if it didn't
	token    string // the setup token, read the way NOTES.txt says
	reset    bool   // the token was reissued because an earlier run claimed admin
	password string // the local admin's password

	branch string // the forge steps' branch, head commit and pull request
	sha    string
	pr     int
}

func newSuite(t *testing.T) *suite {
	kctx := os.Getenv("E2E_CONTEXT")
	if kctx == "" {
		kctx = "k3d-swarm-dev"
	}
	kubectl := os.Getenv("KUBECTL")
	if kubectl == "" {
		kubectl = "../../bin/tools/kubectl"
		if _, err := os.Stat(kubectl); err != nil {
			kubectl = "kubectl"
		}
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: kctx}).ClientConfig()
	if err != nil {
		t.Fatalf("kubeconfig context %s: %v", kctx, err)
	}
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1.AddToScheme(s)
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	return &suite{t: t, kube: c, kctx: kctx, kubectl: kubectl, start: now,
		nonce: fmt.Sprintf("%x", now.Unix()&0xffffff), passed: map[string]bool{}}
}

// step runs f as the subtest name, after the steps before it. It is skipped
// when a step it needs did not pass, so one failure doesn't cascade into a
// list of misleading errors.
func (s *suite) step(name string, needs []string, f func(t *testing.T)) {
	s.t.Run(name, func(t *testing.T) {
		for _, n := range needs {
			if !s.passed[n] {
				t.Skipf("needs %q, which did not pass", n)
			}
		}
		f(t)
		s.passed[name] = !t.Failed()
	})
}

// marker is a word unique to this run, for finding what a step sent.
func (s *suite) marker(what string) string { return "e2e-" + s.nonce + "-" + what }

// until is when a wait of d must end: d from now, but never past the test
// binary's deadline minus reserve.
func (s *suite) until(d time.Duration) time.Time {
	end := time.Now().Add(d)
	if dl, ok := s.t.Deadline(); ok && dl.Add(-reserve).Before(end) {
		end = dl.Add(-reserve)
	}
	return end
}

// wait retries f every few seconds until it returns nil or d passes, and
// returns f's last error.
func (s *suite) wait(d time.Duration, f func() error) error {
	end := s.until(d)
	for {
		err := f()
		if err == nil || time.Now().After(end) {
			return err
		}
		time.Sleep(3 * time.Second)
	}
}

// eventually is wait that fails t.
func (s *suite) eventually(t *testing.T, d time.Duration, what string, f func() error) {
	t.Helper()
	if err := s.wait(d, f); err != nil {
		t.Fatalf("%s: not done after %s: %v", what, d, err)
	}
}

// kc runs kubectl against the e2e cluster and returns its combined output.
func (s *suite) kc(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, s.kubectl, append([]string{"--context", s.kctx}, args...)...).CombinedOutput()
	return string(out), err
}

// inRoom runs a bash script in a room as its agent user, in a login shell,
// the way a person does in the room's Shell tab (which is the same exec).
func (s *suite) inRoom(room, script string) (string, error) {
	return s.kc("-n", wsNS, "exec", podOf(room), "-c", "room", "--", "runuser", "-l", room, "-c", script)
}

func podOf(room string) string { return "room-" + room + "-0" }

// portForward keeps `kubectl port-forward svc/control-center` running on a
// free local port until the test ends, restarting it when the pod it picked
// goes away, and returns the URL. It's the fallback NOTES.txt prints.
func (s *suite) portForward() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	s.t.Cleanup(cancel)
	go func() {
		for ctx.Err() == nil {
			_ = exec.CommandContext(ctx, s.kubectl, "--context", s.kctx, "-n", v1.SystemNamespace,
				"port-forward", "svc/control-center", fmt.Sprintf("%d:443", port)).Run()
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}()
	return fmt.Sprintf("https://localhost:%d/", port), nil
}

// clip shortens command output for an error message.
func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 2000 {
		return s[:1000] + "\n...\n" + s[len(s)-1000:]
	}
	return s
}

// field returns the value after "key=" on a line of out.
func field(out, key string) string {
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), key+"="); ok {
			return v
		}
	}
	return ""
}
