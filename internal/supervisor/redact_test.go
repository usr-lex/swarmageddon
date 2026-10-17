package supervisor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/usr-lex/swarmageddon/internal/redact"
	"github.com/usr-lex/swarmageddon/internal/supervisor/roomcfg"
)

// Secret env values are redacted from the supervisor's logs once read.
func TestRoomEnvRedacts(t *testing.T) {
	secrets := t.TempDir()
	os.WriteFile(filepath.Join(secrets, "env_NPM_TOKEN"), []byte("npm_supervisor_redact\n"), 0o600)
	RoomEnv(&roomcfg.Room{Room: "athena", SecretEnv: []string{"NPM_TOKEN"}}, secrets)
	if got := redact.Default.String("token npm_supervisor_redact"); got != "token [redacted]" {
		t.Fatalf("got %q", got)
	}
}
