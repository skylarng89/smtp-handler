// Package message holds the canonical, validated representation of an
// outgoing email. Everything downstream of payload normalization (store,
// workers, transports) only ever sees this type.
package message

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

// Address is a single mailbox.
type Address struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

func (a Address) String() string {
	return (&mail.Address{Name: a.Name, Address: a.Email}).String()
}

// Net converts to the standard library type.
func (a Address) Net() *mail.Address {
	return &mail.Address{Name: a.Name, Address: a.Email}
}

// Domain returns the lower-cased domain part.
func (a Address) Domain() string {
	if i := strings.LastIndexByte(a.Email, '@'); i >= 0 {
		return a.Email[i+1:]
	}
	return ""
}

type Attachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Data        []byte `json:"data"`
}

// Message is the canonical outgoing email.
type Message struct {
	ID        string       `json:"id"`
	MessageID string       `json:"message_id"` // RFC 5322 msg-id without angle brackets
	Date      time.Time    `json:"date"`
	From      Address      `json:"from"`
	ReplyTo   []Address    `json:"reply_to,omitempty"`
	To        []Address    `json:"to"`
	CC        []Address    `json:"cc,omitempty"`
	BCC       []Address    `json:"bcc,omitempty"`
	Subject   string       `json:"subject"`
	Text      string       `json:"text,omitempty"`
	HTML      string       `json:"html,omitempty"`
	Attach    []Attachment `json:"attachments,omitempty"`
}

// Recipients returns To, CC and BCC in envelope order.
func (m *Message) Recipients() []Address {
	out := make([]Address, 0, len(m.To)+len(m.CC)+len(m.BCC))
	out = append(out, m.To...)
	out = append(out, m.CC...)
	return append(out, m.BCC...)
}

const (
	maxAddressLen   = 254
	maxLocalPartLen = 64
	maxNameLen      = 200
)

var errNoDomain = errors.New("address needs a domain with at least one dot")

// ContainsControl reports whether s has any control character other than
// those in allow. Header-bound text must never contain CR, LF or NUL.
func ContainsControl(s string, allow ...rune) bool {
	for _, r := range s {
		if !unicode.IsControl(r) {
			continue
		}
		permitted := false
		for _, a := range allow {
			if r == a {
				permitted = true
				break
			}
		}
		if !permitted {
			return true
		}
	}
	return false
}

// ParseAddress parses and normalizes one mailbox ("a@b.c" or "Name <a@b.c>").
// The domain is lower-cased and converted to its ASCII (IDNA) form.
func ParseAddress(s string) (Address, error) {
	if ContainsControl(s) || !utf8.ValidString(s) {
		return Address{}, errors.New("address contains invalid characters")
	}
	parsed, err := mail.ParseAddress(strings.TrimSpace(s))
	if err != nil {
		return Address{}, errors.New("not a valid email address")
	}
	return normalize(parsed)
}

// ParseAddressList parses a comma-separated list of mailboxes.
func ParseAddressList(s string) ([]Address, error) {
	if ContainsControl(s) || !utf8.ValidString(s) {
		return nil, errors.New("address list contains invalid characters")
	}
	parsed, err := mail.ParseAddressList(s)
	if err != nil {
		return nil, errors.New("not a valid email address list")
	}
	out := make([]Address, 0, len(parsed))
	for _, p := range parsed {
		a, err := normalize(p)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// NewAddress validates a structured {name, email} pair.
func NewAddress(name, email string) (Address, error) {
	if ContainsControl(name) || ContainsControl(email) {
		return Address{}, errors.New("address contains invalid characters")
	}
	if strings.ContainsAny(email, "<>\" ") {
		return Address{}, errors.New("not a valid email address")
	}
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Name != "" {
		return Address{}, errors.New("not a valid email address")
	}
	parsed.Name = strings.TrimSpace(name)
	return normalize(parsed)
}

func normalize(p *mail.Address) (Address, error) {
	at := strings.LastIndexByte(p.Address, '@')
	if at <= 0 {
		return Address{}, errors.New("not a valid email address")
	}
	local, domain := p.Address[:at], p.Address[at+1:]
	if len(local) > maxLocalPartLen {
		return Address{}, errors.New("address local part is too long")
	}
	ascii, err := idna.Lookup.ToASCII(domain)
	if err != nil {
		return Address{}, errNoDomain
	}
	ascii = strings.ToLower(ascii)
	if !strings.Contains(ascii, ".") || strings.HasPrefix(ascii, "[") {
		return Address{}, errNoDomain
	}
	email := local + "@" + ascii
	if len(email) > maxAddressLen {
		return Address{}, errors.New("address is too long")
	}
	name := strings.TrimSpace(p.Name)
	if utf8.RuneCountInString(name) > maxNameLen {
		return Address{}, fmt.Errorf("display name exceeds %d characters", maxNameLen)
	}
	return Address{Name: name, Email: email}, nil
}

// SanitizeSubject collapses line breaks and control characters so rendered
// subjects can never inject headers.
func SanitizeSubject(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			r = ' '
		}
		if r == ' ' {
			if space {
				continue
			}
			space = true
		} else {
			space = false
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}
