// Package identity turns the generic room image into one room: the agent
// user, its SSH and GPG keys, git signing and tea's login (design §6.3 and
// §9.2), from the room's Secret mounted at /run/swarm/identity.
package identity

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/usr-lex/swarmageddon/internal/redact"
	"github.com/usr-lex/swarmageddon/internal/supervisor/proc"
)

// EnsureUser creates the agent user name with uid (and a group of the same
// name), its home, and optionally passwordless sudo. Another user holding
// the uid (e.g. Ubuntu's "ubuntu") is removed first. Returns the user.
func EnsureUser(name string, uid int, home string, sudo bool) (*proc.User, error) {
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil && u.Username != name {
		if out, err := exec.Command("userdel", u.Username).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("userdel %s: %v: %s", u.Username, err, out)
		}
		if g, err := user.LookupGroupId(strconv.Itoa(uid)); err == nil && g.Name != name {
			_ = exec.Command("groupdel", g.Name).Run()
		}
	}
	if _, err := user.Lookup(name); err != nil {
		args := []string{"-M", "-u", strconv.Itoa(uid), "-U", "-s", "/bin/bash", "-d", home}
		if out, err := exec.Command("useradd", append(args, name)...).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("useradd: %v: %s", err, out)
		}
	}
	u, err := user.Lookup(name)
	if err != nil {
		return nil, err
	}
	gid, _ := strconv.Atoi(u.Gid)
	sudoers := "/etc/sudoers.d/room"
	if sudo {
		if err := os.WriteFile(sudoers, []byte(name+" ALL=(ALL) NOPASSWD: ALL\n"), 0o440); err != nil {
			return nil, err
		}
	} else if err := os.Remove(sudoers); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(home, 0o750); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(home, ".bashrc")); errors.Is(err, fs.ErrNotExist) {
		_ = exec.Command("cp", "-a", "/etc/skel/.", home+"/").Run()
		_ = exec.Command("chown", "-R", fmt.Sprintf("%d:%d", uid, gid), home).Run()
	}
	if err := os.Chown(home, uid, gid); err != nil {
		return nil, err
	}
	if err := os.Chmod(home, 0o750); err != nil {
		return nil, err
	}
	var groups []uint32
	if ids, err := u.GroupIds(); err == nil {
		for _, g := range ids {
			n, _ := strconv.Atoi(g)
			groups = append(groups, uint32(n))
		}
	}
	return &proc.User{UID: uint32(uid), GID: uint32(gid), Groups: groups, Name: name, Home: home}, nil
}

// Setup is everything Install needs.
type Setup struct {
	User *proc.User
	// SecretDir is /run/swarm/identity.
	SecretDir string
	GitName   string
	GitEmail  string
	ForgeURL  string
	// ForgeSSH is host[:port] of Forgejo's SSH service.
	ForgeSSH  string
	ForgeUser string
}

// Install writes keys and configuration into the agent's home. It is safe to
// run on every start; values it owns are replaced.
func Install(s Setup) error {
	var errs []error
	if err := s.ssh(); err != nil {
		errs = append(errs, fmt.Errorf("ssh: %w", err))
	}
	if err := s.git(); err != nil {
		errs = append(errs, fmt.Errorf("git: %w", err))
	}
	if err := s.tea(); err != nil {
		errs = append(errs, fmt.Errorf("tea: %w", err))
	}
	return errors.Join(errs...)
}

func (s Setup) home(p ...string) string { return filepath.Join(append([]string{s.User.Home}, p...)...) }

func (s Setup) secret(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.SecretDir, name))
}

// writeFile writes a file owned by the agent user.
func (s Setup) writeFile(path string, data []byte, mode os.FileMode) error {
	if err := s.mkdir(filepath.Dir(path)); err != nil {
		return err
	}
	tmp := path + ".swarm.tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := s.chown(tmp); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s Setup) mkdir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return s.chown(dir)
}

func (s Setup) chown(p string) error {
	if os.Getuid() != 0 {
		return nil
	}
	return os.Chown(p, int(s.User.UID), int(s.User.GID))
}

const blockBegin = "# swarm:begin (written by the room supervisor on every start)"
const blockEnd = "# swarm:end"

func (s Setup) ssh() error {
	key, err := s.secret("id_ed25519")
	if err != nil {
		return err
	}
	if err := s.writeFile(s.home(".ssh", "id_ed25519"), key, 0o600); err != nil {
		return err
	}
	if pub, err := s.secret("id_ed25519.pub"); err == nil {
		if err := s.writeFile(s.home(".ssh", "id_ed25519.pub"), pub, 0o644); err != nil {
			return err
		}
	}
	known := s.home(".ssh", "known_hosts_forge")
	if kh, err := s.secret("forge_known_hosts"); err == nil {
		if err := s.writeFile(known, kh, 0o644); err != nil {
			return err
		}
	}
	host, port := s.ForgeSSH, ""
	if h, p, ok := strings.Cut(s.ForgeSSH, ":"); ok {
		host, port = h, p
	}
	var b strings.Builder
	if host != "" {
		fmt.Fprintf(&b, "Host %s\n    User git\n    IdentityFile ~/.ssh/id_ed25519\n    UserKnownHostsFile %s\n    StrictHostKeyChecking yes\n", host, known)
		if port != "" {
			fmt.Fprintf(&b, "    Port %s\n", port)
		}
	}
	// OpenSSH 10 marks packets as expedited; some NATs drop them (remote-labs).
	b.WriteString("Host *\n    IPQoS none\n")
	cfg := s.home(".ssh", "config")
	old, _ := os.ReadFile(cfg)
	return s.writeFile(cfg, []byte(ReplaceBlock(string(old), blockBegin, blockEnd, b.String())), 0o600)
}

// ReplaceBlock puts block between begin and end markers at the top of
// content, replacing an earlier block and keeping everything else.
func ReplaceBlock(content, begin, end, block string) string {
	rest := content
	if i := strings.Index(content, begin); i >= 0 {
		if j := strings.Index(content[i:], end); j >= 0 {
			rest = content[:i] + content[i+j+len(end):]
		}
	}
	rest = strings.TrimLeft(rest, "\n")
	out := begin + "\n" + strings.TrimRight(block, "\n") + "\n" + end + "\n"
	if strings.TrimSpace(rest) != "" {
		out += "\n" + rest
	}
	return out
}

// asUser runs a command as the agent user with its HOME.
func (s Setup) asUser(stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = append(proc.BaseEnv(s.User), "GNUPGHOME="+s.home(".gnupg"))
	cmd.SysProcAttr = proc.SysProcAttr(s.User)
	cmd.Dir = s.User.Home
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (s Setup) gitSet(key, value string) error {
	_, err := s.asUser(nil, "git", "config", "--global", "--replace-all", key, value)
	return err
}

func (s Setup) git() error {
	var errs []error
	set := func(k, v string) {
		if err := s.gitSet(k, v); err != nil {
			errs = append(errs, err)
		}
	}
	set("user.name", s.GitName)
	set("user.email", s.GitEmail)
	set("init.defaultBranch", "main")
	key, err := s.secret("gpg_secret.asc")
	if err != nil {
		errs = append(errs, fmt.Errorf("no gpg key; commits will not be signed: %w", err))
		return errors.Join(errs...)
	}
	if err := s.mkdir(s.home(".gnupg")); err != nil {
		return err
	}
	if _, err := s.asUser(key, "gpg", "--batch", "--quiet", "--import"); err != nil {
		errs = append(errs, err)
	}
	out, err := s.asUser(nil, "gpg", "--batch", "--with-colons", "--list-secret-keys")
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	fpr := Fingerprint(string(out))
	if fpr == "" {
		return errors.Join(append(errs, errors.New("imported gpg key has no fingerprint"))...)
	}
	if _, err := s.asUser([]byte(fpr+":6:\n"), "gpg", "--batch", "--import-ownertrust"); err != nil {
		errs = append(errs, err)
	}
	// gpg.format must be explicit: a global gpg.format=ssh would otherwise
	// make SSH signatures, which Forgejo marks unverified.
	set("gpg.format", "openpgp")
	set("user.signingkey", fpr)
	set("commit.gpgsign", "true")
	set("tag.gpgsign", "true")
	return errors.Join(errs...)
}

// Fingerprint returns the first fpr in gpg --with-colons output.
func Fingerprint(colons string) string {
	for _, line := range strings.Split(colons, "\n") {
		f := strings.Split(line, ":")
		if len(f) > 9 && f[0] == "fpr" {
			return f[9]
		}
	}
	return ""
}

func (s Setup) tea() error {
	tok, err := s.secret("forge_token")
	if err != nil {
		return err
	}
	if s.ForgeURL == "" {
		return errors.New("no forge URL")
	}
	host := s.ForgeSSH
	if host == "" {
		host = strings.TrimPrefix(strings.TrimPrefix(s.ForgeURL, "https://"), "http://")
	}
	redact.Add(string(tok))
	cfg := TeaConfig(s.ForgeURL, strings.TrimSpace(string(tok)), s.ForgeUser, host)
	return s.writeFile(s.home(".config", "tea", "config.yml"), []byte(cfg), 0o600)
}

// TeaConfig renders tea's config.yml with one default login named "forge".
func TeaConfig(url, token, user, sshHost string) string {
	q := strconv.Quote
	return "logins:\n" +
		"  - name: forge\n" +
		"    url: " + q(url) + "\n" +
		"    token: " + q(token) + "\n" +
		"    default: true\n" +
		"    ssh_host: " + q(sshHost) + "\n" +
		"    ssh_key: \"\"\n" +
		"    insecure: false\n" +
		"    version_check: false\n" +
		"    user: " + q(user) + "\n" +
		"preferences:\n" +
		"  editor: false\n" +
		"  flag_defaults:\n" +
		"    remote: \"\"\n"
}
