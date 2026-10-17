package images

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsFollowVersion(t *testing.T) {
	d := Defaults("v1.2.3")
	if d.Room != Registry+"/room:v1.2.3" || d.Operator != Registry+"/operator:v1.2.3" {
		t.Fatalf("own images: %+v", d)
	}
	if !strings.Contains(d.Forgejo, "forgejo:17.0.0-rootless@sha256:") {
		t.Fatalf("forgejo: %s", d.Forgejo)
	}
	if Defaults("dev").Room != Registry+"/room:latest" {
		t.Fatal("dev should map to latest")
	}
}

func TestLoadOverrides(t *testing.T) {
	p := filepath.Join(t.TempDir(), "images.yaml")
	if err := os.WriteFile(p, []byte("room: localhost:5000/room:dev\npostgres: mirror/postgres:18\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Room != "localhost:5000/room:dev" || got.Postgres != "mirror/postgres:18" {
		t.Fatalf("overrides not applied: %+v", got)
	}
	if got.Forgejo != Defaults("v1").Forgejo {
		t.Fatal("unset entries must keep defaults")
	}
	if err := os.WriteFile(p, []byte("rooom: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p, "v1"); err == nil {
		t.Fatal("unknown key accepted")
	}
}

func TestOwnFromChart(t *testing.T) {
	i := Defaults("v1")
	i.Own("k3d-swarm-registry.localhost:5005", "dev")
	for name, got := range map[string]string{"messaging": i.Messaging, "devices": i.Devices, "runner": i.Runner, "warden": i.Warden} {
		if want := "k3d-swarm-registry.localhost:5005/" + name + ":dev"; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if i.Postgres != Defaults("v1").Postgres {
		t.Errorf("Own changed a third-party image: %q", i.Postgres)
	}
	p := filepath.Join(t.TempDir(), "images.yaml")
	if err := os.WriteFile(p, []byte("devices: example.com/devices:x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadOnto(i, p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Devices != "example.com/devices:x" || got.Messaging != i.Messaging {
		t.Errorf("override file: devices %q, messaging %q", got.Devices, got.Messaging)
	}
}
