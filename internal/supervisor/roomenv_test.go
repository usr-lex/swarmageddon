package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/usr-lex/swarmageddon/internal/supervisor/proc"
	"github.com/usr-lex/swarmageddon/internal/supervisor/roomcfg"
)

func TestRoomEnvAndProxy(t *testing.T) {
	secrets := t.TempDir()
	os.WriteFile(filepath.Join(secrets, "env_API_TOKEN"), []byte("s3cr'et\n"), 0o600)
	os.WriteFile(filepath.Join(secrets, "env_HTTPS_PROXY"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(secrets, "gateway_key"), []byte("k"), 0o600)
	room := &roomcfg.Room{
		Room: "athena", Workspace: "acme", Env: map[string]string{"FOO": "bar", "bad-name": "x"}, SecretEnv: []string{"API_TOKEN", "HTTPS_PROXY", "MISSING"},
		ForgeURL: "http://forgejo.swarmageddon-platform.svc:3000", ForgeSSHHost: "forgejo-ssh.example:2222",
		GatewayURL: "http://gateway.swarmageddon-platform.svc:4000",
		Network:    roomcfg.NetworkSettings{Mode: "limited", ProxyURL: "http://egress.swarmageddon-platform.svc:3128", NoProxy: []string{"registry.local"}},
	}
	env, skipped := RoomEnv(room, secrets)
	if strings.Join(env, ";") != "API_TOKEN=s3cr'et;FOO=bar" || len(skipped) != 3 {
		t.Errorf("env %q skipped %q", env, skipped)
	}
	if p, _ := ProxyEnv(room, "tok\n"); p[0] != "HTTPS_PROXY=http://athena:tok@egress.swarmageddon-platform.svc:3128" {
		t.Errorf("proxy %q", p[0])
	}
	proxy, err := ProxyEnv(room, "athena:p@ss\n")
	if err != nil {
		t.Fatal(err)
	}
	want := "HTTPS_PROXY=http://athena:p%40ss@egress.swarmageddon-platform.svc:3128"
	if proxy[0] != want {
		t.Errorf("proxy %q", proxy[0])
	}
	if proxy[2] != "NO_PROXY=localhost,127.0.0.1,::1,.svc,.cluster.local,forgejo.swarmageddon-platform.svc,gateway.swarmageddon-platform.svc,forgejo-ssh.example,registry.local" {
		t.Errorf("no_proxy %q", proxy[2])
	}
	for _, mode := range []string{"full", "none", ""} {
		room.Network.Mode = mode
		if p, _ := ProxyEnv(room, "a:b"); p != nil {
			t.Errorf("%s mode: proxy %q", mode, p)
		}
	}

	root := t.TempDir()
	u := &proc.User{Name: "athena", Home: filepath.Join(root, "home"), UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	os.WriteFile(filepath.Join(root, bashrc), []byte("# distro bashrc\n"), 0o644)
	for range 2 {
		if err := WriteShellEnv(u, append(env, proxy...), root); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(u.Home, ".swarm", "env"))
	if !strings.Contains(string(b), `export API_TOKEN='s3cr'\''et'`) || !strings.Contains(string(b), "export https_proxy='http://athena:p%40ss@") {
		t.Errorf("env file:\n%s", b)
	}
	rc, _ := os.ReadFile(filepath.Join(root, bashrc))
	if strings.Count(string(rc), sourceLine) != 1 || !strings.Contains(string(rc), "# distro bashrc") {
		t.Errorf("bashrc:\n%s", rc)
	}
	conf := filepath.Join(root, "containers.conf.d", "60.conf")
	WritePodmanProxy(conf, proxy)
	c, _ := os.ReadFile(conf)
	if !strings.Contains(string(c), `env = ["HTTPS_PROXY=http://athena:p%40ss@egress.swarmageddon-platform.svc:3128", `) {
		t.Errorf("containers.conf:\n%s", c)
	}
	if err := WritePodmanProxy(conf, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(conf); !os.IsNotExist(err) {
		t.Error("drop-in not removed")
	}
}
