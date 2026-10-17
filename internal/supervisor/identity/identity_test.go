package identity

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/usr-lex/swarmageddon/internal/supervisor/proc"
)

func TestReplaceBlock(t *testing.T) {
	got := ReplaceBlock("Host mine\n  User me\n", "#b", "#e", "Host *\n  IPQoS none\n")
	want := "#b\nHost *\n  IPQoS none\n#e\n\nHost mine\n  User me\n"
	if got != want {
		t.Fatalf("got %q", got)
	}
	again := ReplaceBlock(got, "#b", "#e", "new\n")
	if again != "#b\nnew\n#e\n\nHost mine\n  User me\n" {
		t.Fatalf("replace: %q", again)
	}
}

func TestFingerprint(t *testing.T) {
	out := "sec:u:255:22:ABC:1:::::::scESC:::+:::23::0:\nfpr:::::::::0123456789ABCDEF0123456789ABCDEF01234567:\n"
	if Fingerprint(out) != "0123456789ABCDEF0123456789ABCDEF01234567" {
		t.Fatal(Fingerprint(out))
	}
}

func TestInstallAsCurrentUser(t *testing.T) {
	for _, b := range []string{"git", "gpg"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skip("needs " + b)
		}
	}
	home, secrets := t.TempDir(), t.TempDir()
	u := &proc.User{UID: uint32(os.Getuid()), GID: uint32(os.Getgid()), Name: "athena", Home: home}
	gpgHome := t.TempDir()
	gen := exec.Command("gpg", "--batch", "--homedir", gpgHome, "--passphrase", "", "--quick-gen-key", "Athena <athena@agents.acme.invalid>", "ed25519", "sign", "never")
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("gpg keygen: %v %s", err, out)
	}
	asc, err := exec.Command("gpg", "--batch", "--homedir", gpgHome, "--armor", "--export-secret-keys").Output()
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"id_ed25519": "PRIVATE\n", "id_ed25519.pub": "ssh-ed25519 AAA\n", "forge_token": "tok123\n",
		"forge_known_hosts": "forgejo-ssh ssh-ed25519 AAAB\n", "gpg_secret.asc": string(asc),
	}
	for n, c := range files {
		os.WriteFile(filepath.Join(secrets, n), []byte(c), 0o600)
	}
	err = Install(Setup{User: u, SecretDir: secrets, GitName: "Athena", GitEmail: "athena@agents.acme.invalid",
		ForgeURL: "http://forgejo:3000", ForgeSSH: "forgejo-ssh:2222", ForgeUser: "athena"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := os.ReadFile(filepath.Join(home, ".ssh/config"))
	for _, want := range []string{"IPQoS none", "Host forgejo-ssh", "Port 2222", "known_hosts_forge"} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("ssh config lacks %q:\n%s", want, cfg)
		}
	}
	gitcfg, _ := os.ReadFile(filepath.Join(home, ".gitconfig"))
	for _, want := range []string{"gpgsign = true", "format = openpgp", "signingkey = ", "email = athena@agents.acme.invalid"} {
		if !strings.Contains(string(gitcfg), want) {
			t.Errorf("gitconfig lacks %q:\n%s", want, gitcfg)
		}
	}
	tea, _ := os.ReadFile(filepath.Join(home, ".config/tea/config.yml"))
	if !strings.Contains(string(tea), `token: "tok123"`) || !strings.Contains(string(tea), `url: "http://forgejo:3000"`) {
		t.Errorf("tea config:\n%s", tea)
	}
}

func TestWriteFileOwnsParentDirs(t *testing.T) {
	home := t.TempDir()
	s := Setup{User: &proc.User{Home: home, UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}}
	path := filepath.Join(home, ".config", "tea", "config.yml")
	if err := s.writeFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join(home, ".config"), filepath.Join(home, ".config", "tea")} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Errorf("%s: %v", d, err)
		}
	}
}
