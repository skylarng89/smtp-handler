package message

import (
	"strings"
	"testing"
)

func TestSanitizeSubject(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                    "plain",
		"  padded  ":               "padded",
		"line1\r\nBcc: evil@x.com": "line1 Bcc: evil@x.com",
		"tab\there":                "tab here",
		"many   spaces":            "many spaces",
		"unicode separator":        "unicode separator",
		"\x00nul\x1bescape":        "nul escape",
		"emoji ✓ stays":            "emoji ✓ stays",
		"\r\n":                     "",
	} {
		got := SanitizeSubject(in)
		if got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
		if strings.ContainsAny(got, "\r\n\x00") {
			t.Errorf("%q: control characters survived: %q", in, got)
		}
	}
}

func TestParseAddressNormalization(t *testing.T) {
	a, err := ParseAddress(`"Jane Q. Doe" <Jane@Example.COM>`)
	if err != nil || a.Email != "Jane@example.com" || a.Name != "Jane Q. Doe" || a.Domain() != "example.com" {
		t.Fatalf("%+v %v", a, err)
	}
	if got := a.String(); !strings.Contains(got, "Jane@example.com") {
		t.Fatal(got)
	}
	for _, bad := range []string{"", "a", "a@", "@b.com", "a@b", "a@localhost", "a b@c.com", "a@b.com\n", strings.Repeat("x", 65) + "@b.com", "a@" + strings.Repeat("d", 250) + ".com"} {
		if _, err := ParseAddress(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := NewAddress(strings.Repeat("n", 201), "a@b.com"); err == nil {
		t.Error("over-long display name accepted")
	}
	if _, err := NewAddress("ok", "Name <a@b.com>"); err == nil {
		t.Error("a structured email field must be a bare address")
	}
}

func TestContainsControl(t *testing.T) {
	if ContainsControl("clean text ✓") || !ContainsControl("a\nb") || !ContainsControl("a\x00") {
		t.Fatal("basic detection wrong")
	}
	if ContainsControl("a\nb", '\n') || !ContainsControl("a\rb", '\n') {
		t.Fatal("allow list not honoured")
	}
}

func TestRecipientsOrder(t *testing.T) {
	m := &Message{To: []Address{{Email: "to@x.co"}}, CC: []Address{{Email: "cc@x.co"}}, BCC: []Address{{Email: "bcc@x.co"}}}
	got := m.Recipients()
	if len(got) != 3 || got[0].Email != "to@x.co" || got[2].Email != "bcc@x.co" {
		t.Fatalf("%+v", got)
	}
}
