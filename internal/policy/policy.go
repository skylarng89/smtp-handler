// Package policy enforces per-project sending rules: which recipients and
// From addresses are allowed, and which attachments may be sent.
package policy

import (
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/skylarng89/smtp-handler/internal/apperr"
	"github.com/skylarng89/smtp-handler/internal/config"
	"github.com/skylarng89/smtp-handler/internal/message"
	"github.com/skylarng89/smtp-handler/internal/payload"
)

// Policy is the compiled policy for one project.
type Policy struct {
	cfg         config.Policy
	domains     map[string]struct{}
	allowedFrom map[string]struct{}
	extensions  map[string]struct{}
}

func New(p *config.Project) *Policy {
	pol := &Policy{
		cfg:         p.Policy,
		domains:     make(map[string]struct{}, len(p.Policy.AllowedRecipientDomains)),
		allowedFrom: make(map[string]struct{}, len(p.AllowedFrom)),
		extensions:  make(map[string]struct{}, len(p.Policy.Attachments.AllowedExtensions)),
	}
	for _, d := range p.Policy.AllowedRecipientDomains {
		pol.domains[strings.ToLower(d)] = struct{}{}
	}
	for _, a := range p.AllowedFrom {
		if addr, err := message.ParseAddress(a); err == nil {
			pol.allowedFrom[addr.Email] = struct{}{}
		}
	}
	for _, e := range p.Policy.Attachments.AllowedExtensions {
		pol.extensions[strings.ToLower(e)] = struct{}{}
	}
	return pol
}

// CheckFrom verifies a client-requested From address is on the allow list.
func (p *Policy) CheckFrom(a message.Address) error {
	if _, ok := p.allowedFrom[a.Email]; !ok {
		return apperr.Forbidden("from-not-allowed", "the from address is not allowed for this project")
	}
	return nil
}

// CheckRawRecipients applies the project's rule for arbitrary recipients,
// which only secret keys may use. Template recipients come from config and
// are not subject to this check.
func (p *Policy) CheckRawRecipients(all []message.Address) error {
	if len(all) == 0 {
		return apperr.Validation("at least one recipient is required",
			apperr.FieldError{Field: "to", Message: "is required"})
	}
	if len(all) > p.cfg.MaxRecipients {
		return apperr.Validation(fmt.Sprintf("too many recipients (maximum %d)", p.cfg.MaxRecipients))
	}
	switch p.cfg.RawRecipients {
	case "none":
		return apperr.Forbidden("recipients-not-allowed", "this project does not allow arbitrary recipients")
	case "domain_allowlist":
		for _, a := range all {
			if _, ok := p.domains[a.Domain()]; !ok {
				return apperr.Forbidden("recipient-domain-not-allowed",
					"a recipient's domain is not allowed for this project")
			}
		}
	}
	return nil
}

// extensionTypes lists, per extension, the sniffed content-type prefix that
// must match so a renamed file cannot pass as a different type.
var extensionTypes = map[string]string{
	".pdf": "application/pdf", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp",
	".docx": "application/zip", ".xlsx": "application/zip", ".pptx": "application/zip",
	".txt": "text/", ".csv": "text/", ".ics": "text/",
}

// CheckAttachments validates and converts client attachments. The content
// type is detected from the bytes; the client's claim is never trusted.
func (p *Policy) CheckAttachments(in []payload.Attachment) ([]message.Attachment, error) {
	if len(in) == 0 {
		return nil, nil
	}
	limits := p.cfg.Attachments
	if len(in) > limits.MaxCount {
		return nil, apperr.Validation(fmt.Sprintf("too many attachments (maximum %d)", limits.MaxCount))
	}

	out := make([]message.Attachment, 0, len(in))
	var total int64
	for i, a := range in {
		field := fmt.Sprintf("attachments[%d]", i)
		name := sanitizeFilename(a.Filename)
		if name == "" {
			return nil, fieldErr(field+".filename", "is required and must be a plain file name")
		}
		if len(a.Content) == 0 {
			return nil, fieldErr(field+".content", "is empty")
		}
		if int64(len(a.Content)) > limits.MaxBytes {
			return nil, fieldErr(field+".content", fmt.Sprintf("exceeds %d bytes", limits.MaxBytes))
		}
		total += int64(len(a.Content))
		if total > limits.MaxBytes*int64(limits.MaxCount) {
			return nil, apperr.Validation("attachments are too large in total")
		}

		ext := strings.ToLower(filepath.Ext(name))
		if _, ok := p.extensions[ext]; !ok {
			return nil, fieldErr(field+".filename", fmt.Sprintf("extension %q is not allowed", ext))
		}
		sniffed := http.DetectContentType(a.Content[:min(len(a.Content), 512)])
		if want, ok := extensionTypes[ext]; ok && !strings.HasPrefix(sniffed, want) {
			return nil, fieldErr(field+".content", "content does not match the file extension")
		}
		ct := sniffed
		if ct == "application/octet-stream" || strings.HasPrefix(ct, "text/plain") {
			if byExt := mime.TypeByExtension(ext); byExt != "" {
				ct = byExt
			}
		}
		out = append(out, message.Attachment{Filename: name, ContentType: ct, Data: a.Content})
	}
	return out, nil
}

func fieldErr(field, msg string) error {
	return apperr.Validation("invalid attachment", apperr.FieldError{Field: field, Message: msg})
}

// sanitizeFilename keeps only a plain base name; path separators, quotes and
// control characters are rejected (returning "") because they end up in
// Content-Disposition headers.
func sanitizeFilename(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 255 || s == "." || s == ".." {
		return ""
	}
	for _, r := range s {
		if unicode.IsControl(r) || strings.ContainsRune(`/\"<>|:*?`, r) {
			return ""
		}
	}
	return s
}
