package logging

import (
	"strings"
	"testing"
)

func TestRedaction(t *testing.T) {
	if got := RedactEmail("jane.doe@example.com"); got != "j***@example.com" {
		t.Fatalf("%s", got)
	}
	if RedactEmail("nope") != "***" || RedactEmail("@x.y") != "***" {
		t.Fatal("malformed addresses must be fully masked")
	}

	in := "550 5.1.1 <jane.doe@example.com>: user unknown; also tried bob+tag@mail.example.org"
	out := RedactText(in)
	for _, leaked := range []string{"jane.doe", "bob+tag"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("%q leaked in %q", leaked, out)
		}
	}
	if !strings.Contains(out, "5.1.1") || !strings.Contains(out, "user unknown") {
		t.Fatalf("redaction removed diagnostic text: %q", out)
	}
	if RedactText("no addresses here") != "no addresses here" {
		t.Fatal("text without addresses must be unchanged")
	}
}
