package policy

import (
	"bytes"
	"errors"
	"testing"

	"github.com/skylarng89/smtp-handler/internal/apperr"
	"github.com/skylarng89/smtp-handler/internal/config"
	"github.com/skylarng89/smtp-handler/internal/message"
	"github.com/skylarng89/smtp-handler/internal/payload"
)

func newPolicy(mut func(*config.Project)) *Policy {
	p := &config.Project{
		AllowedFrom: []string{"noreply@acme.test", "Billing <billing@acme.test>"},
		Policy: config.Policy{
			RawRecipients: "domain_allowlist", AllowedRecipientDomains: []string{"acme.test", "Customers.Test"},
			MaxRecipients: 3,
			Attachments: config.Attachments{
				MaxCount: 2, MaxBytes: 1024, AllowedExtensions: []string{".pdf", ".png", ".txt", ".csv", ".docx"},
			},
		},
	}
	if mut != nil {
		mut(p)
	}
	return New(p)
}

func addr(e string) message.Address { return message.Address{Email: e} }

func kind(err error) apperr.Kind {
	var ae *apperr.Error
	if errors.As(err, &ae) {
		return ae.Kind
	}
	return -1
}

func TestRecipientPolicy(t *testing.T) {
	p := newPolicy(nil)
	if err := p.CheckRawRecipients([]message.Address{addr("a@acme.test"), addr("b@customers.test")}); err != nil {
		t.Fatalf("allowed domains rejected (case-insensitive): %v", err)
	}
	for name, rcpts := range map[string][]message.Address{
		"outside domain":   {addr("a@evil.test")},
		"mixed":            {addr("a@acme.test"), addr("b@evil.test")},
		"subdomain trick":  {addr("a@evil.acme.test")},
		"suffix trick":     {addr("a@notacme.test")},
		"empty":            nil,
		"over the maximum": {addr("1@acme.test"), addr("2@acme.test"), addr("3@acme.test"), addr("4@acme.test")},
	} {
		if err := p.CheckRawRecipients(rcpts); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := newPolicy(func(p *config.Project) { p.Policy.RawRecipients = "none" }).CheckRawRecipients([]message.Address{addr("a@acme.test")}); kind(err) != apperr.KindForbidden {
		t.Fatalf("none: %v", err)
	}
	if err := newPolicy(func(p *config.Project) { p.Policy.RawRecipients = "any" }).CheckRawRecipients([]message.Address{addr("a@anywhere.test")}); err != nil {
		t.Fatalf("any: %v", err)
	}
}

func TestFromPolicy(t *testing.T) {
	p := newPolicy(nil)
	if err := p.CheckFrom(addr("noreply@acme.test")); err != nil {
		t.Fatal(err)
	}
	if err := p.CheckFrom(addr("billing@acme.test")); err != nil {
		t.Fatalf("display-name entries must match on the address: %v", err)
	}
	if err := p.CheckFrom(addr("ceo@acme.test")); kind(err) != apperr.KindForbidden {
		t.Fatalf("%v", err)
	}
}

func TestAttachmentChecks(t *testing.T) {
	p := newPolicy(nil)
	pdf := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("x"), 100)...)
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 64)...)
	zip := append([]byte("PK\x03\x04"), bytes.Repeat([]byte{0}, 64)...)

	got, err := p.CheckAttachments([]payload.Attachment{{Filename: "a.pdf", Content: pdf}, {Filename: "n.txt", Content: []byte("hello")}})
	if err != nil || len(got) != 2 || got[0].ContentType != "application/pdf" || got[1].ContentType != "text/plain; charset=utf-8" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := p.CheckAttachments([]payload.Attachment{{Filename: "d.docx", Content: zip}}); err != nil {
		t.Fatalf("docx (zip) rejected: %v", err)
	}

	bad := map[string][]payload.Attachment{
		"executable":            {{Filename: "x.exe", Content: []byte("MZ")}},
		"double extension":      {{Filename: "x.pdf.exe", Content: pdf}},
		"html disguised as png": {{Filename: "x.png", Content: []byte("<html><script>alert(1)</script></html>")}},
		"png as pdf":            {{Filename: "x.pdf", Content: png}},
		"path traversal":        {{Filename: "../../etc/passwd.txt", Content: []byte("x")}},
		"backslash path":        {{Filename: `..\x.txt`, Content: []byte("x")}},
		"quote in name":         {{Filename: `a".txt`, Content: []byte("x")}},
		"newline in name":       {{Filename: "a\r\nX: y.txt", Content: []byte("x")}},
		"no extension":          {{Filename: "README", Content: []byte("x")}},
		"empty name":            {{Filename: " ", Content: []byte("x")}},
		"empty content":         {{Filename: "a.txt", Content: nil}},
		"too big":               {{Filename: "a.txt", Content: bytes.Repeat([]byte("x"), 2000)}},
		"too many":              {{Filename: "1.txt", Content: []byte("x")}, {Filename: "2.txt", Content: []byte("x")}, {Filename: "3.txt", Content: []byte("x")}},
	}
	for name, in := range bad {
		if _, err := p.CheckAttachments(in); kind(err) != apperr.KindValidation {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if got, err := p.CheckAttachments(nil); got != nil || err != nil {
		t.Fatalf("no attachments: %v %v", got, err)
	}
}
