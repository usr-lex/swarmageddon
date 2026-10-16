package auth

import (
	"testing"
	"time"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

func TestMapRole(t *testing.T) {
	m := []v1alpha1.RoleMapping{{Group: "ops", Role: "operator"}, {Group: "admins", Role: "admin"}, {Group: "all", Role: "viewer"}}
	cases := []struct {
		groups []string
		def    v1alpha1.Role
		want   Role
	}{
		{[]string{"all", "admins"}, "", RoleAdmin},
		{[]string{"ops", "all"}, "", RoleOperator},
		{[]string{"other"}, "viewer", RoleViewer},
		{[]string{"other"}, "", RoleNone},
		{nil, "", RoleNone},
	}
	for _, c := range cases {
		if got := MapRole(c.groups, m, c.def); got != c.want {
			t.Errorf("MapRole(%v, %q) = %v, want %v", c.groups, c.def, got, c.want)
		}
	}
}

func TestRBAC(t *testing.T) {
	admin := &Principal{Role: RoleAdmin}
	op := &Principal{Role: RoleOperator, Email: "alice@x", Handle: "alice", Workspaces: []string{"beta"}}
	viewer := &Principal{Role: RoleViewer, Handle: "vic", Workspaces: []string{"acme"}}
	owners := []string{"Alice@X"}

	cases := []struct {
		name          string
		p             *Principal
		ws            string
		owners        []string
		view, operate bool
	}{
		{"admin any", admin, "acme", nil, true, true},
		{"operator owner", op, "acme", owners, true, true},
		{"operator assigned", op, "beta", nil, true, true},
		{"operator other", op, "gamma", nil, false, false},
		{"viewer assigned", viewer, "acme", nil, true, false},
		{"viewer other", viewer, "beta", nil, false, false},
		{"viewer named owner still viewer", &Principal{Role: RoleViewer, Handle: "alice"}, "acme", []string{"alice"}, true, false},
		{"nil", nil, "acme", nil, false, false},
	}
	for _, c := range cases {
		if got := c.p.CanView(c.ws, c.owners); got != c.view {
			t.Errorf("%s: CanView = %v", c.name, got)
		}
		if got := c.p.CanOperate(c.ws, c.owners); got != c.operate {
			t.Errorf("%s: CanOperate = %v", c.name, got)
		}
	}
}

func TestLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewLimiter()
	l.Now = func() time.Time { return now }
	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow("1.2.3.4"); !ok {
			t.Fatalf("attempt %d refused", i+1)
		}
	}
	ok, wait := l.Allow("1.2.3.4")
	if ok || wait != time.Minute {
		t.Fatalf("6th attempt: ok=%v wait=%v", ok, wait)
	}
	if ok, _ := l.Allow("5.6.7.8"); !ok {
		t.Fatal("other IP limited")
	}
	now = now.Add(61 * time.Second)
	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow("1.2.3.4"); !ok {
			t.Fatalf("after lockout attempt %d refused", i+1)
		}
	}
	if ok, wait := l.Allow("1.2.3.4"); ok || wait != 2*time.Minute {
		t.Fatalf("second lockout should double: ok=%v wait=%v", ok, wait)
	}
	now = now.Add(30 * time.Second)
	if ok, _ := l.Allow("1.2.3.4"); ok {
		t.Fatal("allowed during lockout")
	}
	l.Reset("1.2.3.4")
	if ok, _ := l.Allow("1.2.3.4"); !ok {
		t.Fatal("refused after reset")
	}
}
