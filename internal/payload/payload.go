// Package payload decodes and normalizes the JSON a client sends to
// POST /v1/messages. Decoding is strict (unknown fields are rejected) so
// frontend bugs surface immediately instead of silently dropping data.
package payload

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/skylarng89/smtp-handler/internal/apperr"
	"github.com/skylarng89/smtp-handler/internal/message"
)

const (
	maxDataKeys   = 100
	maxRecipients = 1000 // hard cap before policy applies the project limit
)

// Request is the decoded body. Exactly one of Template or the raw content
// fields (Subject/Text/HTML) is used; the composer enforces that.
type Request struct {
	Template     string          `json:"template,omitempty"`
	Data         map[string]any  `json:"data,omitempty"`
	To           json.RawMessage `json:"to,omitempty"`
	CC           json.RawMessage `json:"cc,omitempty"`
	BCC          json.RawMessage `json:"bcc,omitempty"`
	From         json.RawMessage `json:"from,omitempty"`
	ReplyTo      json.RawMessage `json:"reply_to,omitempty"`
	Subject      string          `json:"subject,omitempty"`
	Text         string          `json:"text,omitempty"`
	HTML         string          `json:"html,omitempty"`
	Attachments  []Attachment    `json:"attachments,omitempty"`
	CaptchaToken string          `json:"captcha_token,omitempty"`
}

type Attachment struct {
	Filename string `json:"filename"`
	Content  []byte `json:"content"` // base64 on the wire
}

// IsTemplate reports whether the request targets a server-side template.
func (r *Request) IsTemplate() bool { return r.Template != "" }

// Decode reads and validates the JSON envelope. The reader should already be
// size-limited; a *http.MaxBytesError is mapped to 413.
func Decode(rd io.Reader) (*Request, error) {
	dec := json.NewDecoder(rd)
	dec.DisallowUnknownFields()
	dec.UseNumber()

	var req Request
	if err := dec.Decode(&req); err != nil {
		return nil, mapDecodeError(err)
	}
	// Exactly one JSON value is allowed.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, tooLarge()
		}
		return nil, apperr.Validation("request body must contain a single JSON object")
	}
	if err := req.checkData(); err != nil {
		return nil, err
	}
	return &req, nil
}

func tooLarge() *apperr.Error {
	return apperr.New(apperr.KindPayloadTooLarge, "payload-too-large", "request body exceeds the size limit")
}

func mapDecodeError(err error) error {
	var (
		mbe    *http.MaxBytesError
		syntax *json.SyntaxError
		typ    *json.UnmarshalTypeError
	)
	switch {
	case errors.As(err, &mbe):
		return tooLarge()
	case errors.Is(err, io.EOF):
		return apperr.Validation("request body is empty")
	case errors.As(err, &syntax), errors.Is(err, io.ErrUnexpectedEOF):
		return apperr.Validation("request body is not valid JSON")
	case errors.As(err, &typ):
		field := typ.Field
		if field == "" {
			field = "body"
		}
		return apperr.Validation("a field has the wrong type",
			apperr.FieldError{Field: field, Message: fmt.Sprintf("expected %s", typ.Type)})
	case strings.HasPrefix(err.Error(), "json: unknown field"):
		name := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field"), ` "`)
		return apperr.Validation("unknown field",
			apperr.FieldError{Field: name, Message: "this field is not supported"})
	default:
		return apperr.Validation("request body could not be decoded")
	}
}

// checkData enforces the shape of template data: flat key/value pairs of
// strings, numbers and booleans. Nested structures are rejected so template
// input stays bounded and predictable.
func (r *Request) checkData() error {
	if len(r.Data) > maxDataKeys {
		return apperr.Validation(fmt.Sprintf("data has more than %d keys", maxDataKeys))
	}
	var fields []apperr.FieldError
	for k, v := range r.Data {
		switch v.(type) {
		case string, json.Number, bool, nil:
		default:
			fields = append(fields, apperr.FieldError{Field: "data." + k, Message: "must be a string, number or boolean"})
		}
	}
	if len(fields) > 0 {
		return apperr.Validation("invalid data values", fields...)
	}
	return nil
}

// Addresses interprets a raw address field. It accepts a string ("a@b.c",
// "Name <a@b.c>", or a comma-separated list), an object {"email","name"}, or
// an array of strings/objects.
func Addresses(raw json.RawMessage) ([]message.Address, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var out []message.Address
	switch raw[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, errors.New("invalid address")
		}
		list, err := message.ParseAddressList(s)
		if err != nil {
			return nil, err
		}
		out = list
	case '{':
		a, err := addressObject(raw)
		if err != nil {
			return nil, err
		}
		out = []message.Address{a}
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, errors.New("invalid address list")
		}
		if len(items) > maxRecipients {
			return nil, fmt.Errorf("more than %d addresses", maxRecipients)
		}
		for _, item := range items {
			list, err := Addresses(item)
			if err != nil {
				return nil, err
			}
			if len(list) == 0 {
				return nil, errors.New("empty address in list")
			}
			out = append(out, list...)
		}
	default:
		return nil, errors.New("address must be a string, object or array")
	}
	if len(out) > maxRecipients {
		return nil, fmt.Errorf("more than %d addresses", maxRecipients)
	}
	return dedupe(out), nil
}

func addressObject(raw json.RawMessage) (message.Address, error) {
	var obj struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&obj); err != nil {
		return message.Address{}, errors.New(`address object must be {"email": "...", "name": "..."}`)
	}
	return message.NewAddress(obj.Name, obj.Email)
}

// Alias interprets a raw field as a single string (used for recipient aliases).
func Alias(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(bytes.TrimSpace(raw), &s); err != nil {
		return "", errors.New("must be a string")
	}
	return strings.TrimSpace(s), nil
}

func dedupe(in []message.Address) []message.Address {
	seen := make(map[string]struct{}, len(in))
	out := in[:0:0]
	for _, a := range in {
		if _, dup := seen[a.Email]; dup {
			continue
		}
		seen[a.Email] = struct{}{}
		out = append(out, a)
	}
	return out
}

// Fingerprint is a stable SHA-256 over everything that defines the request's
// effect. The captcha token is excluded (it changes on every retry) and
// attachment bytes are hashed separately so large uploads are not re-encoded.
func (r *Request) Fingerprint(projectID string) string {
	type attachmentView struct {
		Filename string `json:"filename"`
		SHA256   string `json:"sha256"`
	}
	view := struct {
		Project     string           `json:"project"`
		Template    string           `json:"template,omitempty"`
		Data        map[string]any   `json:"data,omitempty"`
		To          json.RawMessage  `json:"to,omitempty"`
		CC          json.RawMessage  `json:"cc,omitempty"`
		BCC         json.RawMessage  `json:"bcc,omitempty"`
		From        json.RawMessage  `json:"from,omitempty"`
		ReplyTo     json.RawMessage  `json:"reply_to,omitempty"`
		Subject     string           `json:"subject,omitempty"`
		Text        string           `json:"text,omitempty"`
		HTML        string           `json:"html,omitempty"`
		Attachments []attachmentView `json:"attachments,omitempty"`
	}{
		Project: projectID, Template: r.Template, Data: r.Data,
		To: compact(r.To), CC: compact(r.CC), BCC: compact(r.BCC),
		From: compact(r.From), ReplyTo: compact(r.ReplyTo),
		Subject: r.Subject, Text: r.Text, HTML: r.HTML,
	}
	for _, a := range r.Attachments {
		sum := sha256.Sum256(a.Content)
		view.Attachments = append(view.Attachments, attachmentView{a.Filename, hex.EncodeToString(sum[:])})
	}

	h := sha256.New()
	_ = json.NewEncoder(h).Encode(view) // map keys are sorted by encoding/json
	return hex.EncodeToString(h.Sum(nil))
}

func compact(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return buf.Bytes()
}
