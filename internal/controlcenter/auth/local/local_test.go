package local

import (
	"strings"
	"testing"
)

func TestHashVerify(t *testing.T) {
	h, err := Hash("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("unexpected PHC string %q", h)
	}
	if err := Verify(h, "correct horse battery"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := Verify(h, "wrong"); err != ErrMismatch {
		t.Fatalf("wrong password: %v", err)
	}
	h2, _ := Hash("correct horse battery")
	if h == h2 {
		t.Fatal("salt not random")
	}
	for _, bad := range []string{"", "$bcrypt$x", "$argon2id$v=19$m=0,t=1,p=1$AAAA$AAAA", "$argon2i$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA"} {
		if err := Verify(bad, "x"); err == nil {
			t.Errorf("Verify(%q) accepted", bad)
		}
	}
	if CheckNew("short") == nil || CheckNew("long enough password") != nil {
		t.Fatal("CheckNew")
	}
}
