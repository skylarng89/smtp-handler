package payload

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/skylarng89/smtp-handler/internal/apperr"
	"github.com/skylarng89/smtp-handler/internal/message"
)

func TestDecodeIsStrict(t *testing.T) {
	good := `{"template":"t","data":{"a":"x","n":3,"b":true,"z":null}}`
	req, err := Decode(strings.NewReader(good))
	if err != nil || req.Template != "t" || len(req.Data) != 4 {
		t.Fatalf("%v %+v", err, req)
	}

	for name, body := range map[string]string{
		"unknown field":    `{"template":"t","wat":1}`,
		"nested data":      `{"data":{"a":{"b":1}}}`,
		"array data":       `{"data":{"a":[1]}}`,
		"wrong type":       `{"subject":5}`,
		"trailing value":   `{"template":"t"}{}`,
		"trailing garbage": `{"template":"t"} x`,
		"not an object":    `"hello"`,
		"empty":            ``,
		"truncated":        `{"template":`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Decode(strings.NewReader(body))
			var ae *apperr.Error
			if !errors.As(err, &ae) || ae.Kind != apperr.KindValidation {
				t.Fatalf("want a validation error, got %v", err)
			}
		})
	}
}

func TestDecodeMapsOversizedBodyTo413(t *testing.T) {
	r := http.MaxBytesReader(nil, nopCloser{strings.NewReader(`{"template":"` + strings.Repeat("x", 100) + `"}`)}, 20)
	_, err := Decode(r)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Status() != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %v", err)
	}
}

type nopCloser struct{ *strings.Reader }

func (nopCloser) Close() error { return nil }

func TestAddressForms(t *testing.T) {
	tests := []struct {
		in   string
		want []string
		bad  bool
	}{
		{in: `"a@b.com"`, want: []string{"a@b.com"}},
		{in: `"Alice <a@b.com>"`, want: []string{"a@b.com"}},
		{in: `"a@b.com, c@d.org"`, want: []string{"a@b.com", "c@d.org"}},
		{in: `{"email":"a@b.com","name":"Alice"}`, want: []string{"a@b.com"}},
		{in: `["a@b.com", {"email":"c@d.org"}, "Bob <e@f.net>"]`, want: []string{"a@b.com", "c@d.org", "e@f.net"}},
		{in: `["a@b.com","A@B.COM"]`, want: []string{"a@b.com", "A@b.com"}}, // local part is case-sensitive
		{in: `["a@b.com","a@B.com"]`, want: []string{"a@b.com"}},            // duplicates collapse
		{in: `"a@münchen.de"`, want: []string{"a@xn--mnchen-3ya.de"}},       // IDN -> punycode
		{in: `null`, want: nil},
		{in: `"nope"`, bad: true},
		{in: `"a@localhost"`, bad: true},
		{in: `"a@b"`, bad: true},
		{in: `5`, bad: true},
		{in: `{"email":"a@b.com","extra":1}`, bad: true},
		{in: `{"email":"A <a@b.com>"}`, bad: true},
		{in: `["ok@b.com", ""]`, bad: true},
		{in: `"a@b.com\r\nBcc: evil@x.com"`, bad: true},
		{in: `{"email":"a@b.com","name":"X\r\nBcc: e@x.com"}`, bad: true},
		{in: `"a@b.com\u0000"`, bad: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := Addresses(json.RawMessage(tc.in))
			if tc.bad {
				if err == nil {
					t.Fatalf("accepted %s -> %v", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var emails []string
			for _, a := range got {
				emails = append(emails, a.Email)
			}
			if strings.Join(emails, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("got %v, want %v", emails, tc.want)
			}
		})
	}
}

func TestFingerprint(t *testing.T) {
	mk := func(js string) *Request {
		r, err := Decode(strings.NewReader(js))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	a := mk(`{"template":"t","data":{"a":"1","b":"2"},"captcha_token":"one"}`)
	b := mk(`{ "captcha_token":"two", "data":{"b":"2","a":"1"}, "template":"t" }`)
	if a.Fingerprint("p") != b.Fingerprint("p") {
		t.Fatal("key order, whitespace and captcha token must not change the fingerprint")
	}
	if a.Fingerprint("p") == a.Fingerprint("q") {
		t.Fatal("fingerprint must be scoped to the project")
	}
	c := mk(`{"template":"t","data":{"a":"1","b":"3"}}`)
	if a.Fingerprint("p") == c.Fingerprint("p") {
		t.Fatal("different data must change the fingerprint")
	}
	d := mk(`{"to":" a@b.com ","subject":"s","text":"t","attachments":[{"filename":"a.txt","content":"YQ=="}]}`)
	e := mk(`{"to":" a@b.com ","subject":"s","text":"t","attachments":[{"filename":"a.txt","content":"Yg=="}]}`)
	if d.Fingerprint("p") == e.Fingerprint("p") {
		t.Fatal("attachment bytes must change the fingerprint")
	}
}

// FuzzDecode asserts decoding never panics and that no accepted address can
// carry a header-injection payload.
func FuzzDecode(f *testing.F) {
	for _, seed := range []string{
		`{"template":"t","data":{"a":"x"}}`,
		`{"to":["a@b.com"],"subject":"s","text":"t"}`,
		`{"to":"a@b.com\r\nBcc: x@y.z"}`,
		`{"reply_to":{"email":"a@b.com","name":"\n"}}`,
		`{"attachments":[{"filename":"../../x","content":"AAAA"}]}`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, body string) {
		req, err := Decode(strings.NewReader(body))
		if err != nil {
			return
		}
		for _, raw := range []json.RawMessage{req.To, req.CC, req.BCC, req.From, req.ReplyTo} {
			addrs, err := Addresses(raw)
			if err != nil {
				continue
			}
			for _, a := range addrs {
				if message.ContainsControl(a.Email) || message.ContainsControl(a.Name) || strings.ContainsAny(a.Email, "<>\" ,;") {
					t.Fatalf("unsafe address accepted: %+v", a)
				}
			}
		}
	})
}

func FuzzParseAddress(f *testing.F) {
	for _, seed := range []string{"a@b.com", "A <a@b.com>", "\"a b\"@c.de", "a@[1.2.3.4]", "a@b.c\r\n", "a@\u00fcnic\u00f6de.test", "=?utf-8?q?a?= <a@b.com>"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		a, err := message.ParseAddress(s)
		if err != nil {
			return
		}
		if message.ContainsControl(a.Email) || message.ContainsControl(a.Name) || !strings.Contains(a.Email, "@") {
			t.Fatalf("unsafe address from %q: %+v", s, a)
		}
		// The rendered form must remain a single line.
		if strings.ContainsAny(a.String(), "\r\n") {
			t.Fatalf("address renders across lines: %q", a.String())
		}
	})
}
