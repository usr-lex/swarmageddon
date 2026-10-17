package tokenhash

import (
	"strings"
	"testing"
)

func TestFormatParse(t *testing.T) {
	forge := "0123456789abcdef0123456789abcdef01234567"
	key := "sk-Ab_cD-efGh1234567890x"
	v := Format([]string{key, forge, "", key})
	lines := strings.Split(v, "\n")
	if len(lines) != 2 || lines[0] > lines[1] {
		t.Fatalf("value = %q", v)
	}
	for _, l := range lines {
		if len(l) != 64 || strings.ToLower(l) != l {
			t.Fatalf("line %q is not lowercase hex SHA-256", l)
		}
	}
	if strings.Contains(v, forge) || strings.Contains(v, key) {
		t.Fatal("a token is in the value")
	}
	if s := Parse(v + "\n\nnot hex\n"); len(s) != 2 {
		t.Fatalf("parsed %d digests", len(s))
	}
}

func TestContains(t *testing.T) {
	forge := "0123456789abcdef0123456789abcdef01234567"
	key := "sk-Ab_cD-efGh1234567890x"
	edge := "_abcdefghijklmnopq-" // a token may start or end with - or _
	s := Parse(Format([]string{forge, key, edge}))
	for _, body := range []string{
		forge,
		"here is my token: " + forge + ".",
		"export ANTHROPIC_AUTH_TOKEN=" + key,
		"Authorization: Bearer " + key + "\n",
		`{"key":"` + key + `"}`,
		"git clone https://athena:" + forge + "@forgejo/acme/api",
		"*" + key + "*",
		"_" + key + "_", // markdown emphasis
		"x " + edge + " y",
	} {
		if !s.Contains(body) {
			t.Errorf("missed the token in %q", body)
		}
	}
	for _, body := range []string{
		"",
		"nothing secret here, just a long word: antidisestablishmentarianism",
		forge[:39],             // shorter
		forge + "8",            // longer
		"x" + key,              // part of a longer word
		strings.ToUpper(forge), // tokens are case-sensitive
	} {
		if s.Contains(body) {
			t.Errorf("false match in %q", body)
		}
	}
	if (Set{}).Contains(forge) || Set(nil).Contains(forge) {
		t.Error("an empty set matched")
	}
}

func BenchmarkContains(b *testing.B) {
	s := Parse(Format([]string{"0123456789abcdef0123456789abcdef01234567"}))
	body := strings.Repeat("a reasonably ordinary sentence with some_identifiers-and words in it. ", 900) // ~64 KB
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		if s.Contains(body) {
			b.Fatal("match")
		}
	}
}
