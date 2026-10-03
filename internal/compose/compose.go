// Package compose turns a decoded client request into a canonical
// message.Message, applying template rendering and project policy. It is the
// single place that decides what a given key type may send and to whom.
package compose

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/skylarng89/smtp-handler/internal/apperr"
	"github.com/skylarng89/smtp-handler/internal/config"
	"github.com/skylarng89/smtp-handler/internal/message"
	"github.com/skylarng89/smtp-handler/internal/payload"
	"github.com/skylarng89/smtp-handler/internal/policy"
	"github.com/skylarng89/smtp-handler/internal/render"
)

const maxSubjectLen = 300

// Result is a ready-to-enqueue message.
type Result struct {
	Msg      *message.Message
	Template string // empty for raw sends
	// Honeypot is true when a bot filled the honeypot field; the caller
	// should acknowledge the request without sending anything.
	Honeypot bool
}

type project struct {
	cfg         *config.Project
	policy      *policy.Policy
	defaultFrom message.Address
	templates   map[string]*templateRecipients
}

// templateRecipients holds a template's config-defined recipients, parsed
// once at startup.
type templateRecipients struct {
	to      []message.Address
	cc, bcc []message.Address
	aliases map[string][]message.Address
}

// Composer is immutable after construction and safe for concurrent use.
type Composer struct {
	reg      *render.Registry
	projects map[string]*project
}

func New(cfg *config.Config, reg *render.Registry) (*Composer, error) {
	c := &Composer{reg: reg, projects: make(map[string]*project, len(cfg.Projects))}
	for i := range cfg.Projects {
		p := &cfg.Projects[i]
		from, err := message.ParseAddress(p.From)
		if err != nil {
			return nil, fmt.Errorf("project %q from: %w", p.ID, err)
		}
		entry := &project{
			cfg: p, policy: policy.New(p), defaultFrom: from,
			templates: make(map[string]*templateRecipients, len(p.Templates)),
		}
		for j := range p.Templates {
			tr, err := parseRecipients(&p.Templates[j])
			if err != nil {
				return nil, fmt.Errorf("project %q template %q: %w", p.ID, p.Templates[j].Name, err)
			}
			entry.templates[p.Templates[j].Name] = tr
		}
		c.projects[p.ID] = entry
	}
	return c, nil
}

func parseRecipients(t *config.Template) (*templateRecipients, error) {
	parse := func(list []string) ([]message.Address, error) {
		out := make([]message.Address, 0, len(list))
		for _, s := range list {
			a, err := message.ParseAddress(s)
			if err != nil {
				return nil, fmt.Errorf("%q: %w", s, err)
			}
			out = append(out, a)
		}
		return out, nil
	}
	var (
		tr  templateRecipients
		err error
	)
	if tr.to, err = parse(t.Recipients.To); err != nil {
		return nil, err
	}
	if tr.cc, err = parse(t.Recipients.CC); err != nil {
		return nil, err
	}
	if tr.bcc, err = parse(t.Recipients.BCC); err != nil {
		return nil, err
	}
	if len(t.Recipients.Aliases) > 0 {
		tr.aliases = make(map[string][]message.Address, len(t.Recipients.Aliases))
		for name, list := range t.Recipients.Aliases {
			if tr.aliases[name], err = parse(list); err != nil {
				return nil, err
			}
		}
	}
	return &tr, nil
}

// Prepare builds the message for a request. secret reports whether the
// caller authenticated with a secret (server-side) key.
func (c *Composer) Prepare(projectID string, secret bool, req *payload.Request, now time.Time) (*Result, error) {
	p, ok := c.projects[projectID]
	if !ok {
		return nil, apperr.NotFound("project not found")
	}
	if req.IsTemplate() {
		return c.fromTemplate(p, secret, req, now)
	}
	if !secret {
		return nil, apperr.Forbidden("raw-mode-requires-secret-key",
			"publishable keys can only send templates; use a secret key from your backend for raw content")
	}
	return c.fromRaw(p, req, now)
}

func newMessage(now time.Time, from message.Address) *message.Message {
	id := ulid.Make().String()
	return &message.Message{
		ID:        id,
		MessageID: id + "@" + from.Domain(),
		Date:      now.UTC().Truncate(time.Second),
		From:      from,
	}
}

func (c *Composer) fromTemplate(p *project, secret bool, req *payload.Request, now time.Time) (*Result, error) {
	notFound := apperr.NotFound("template not found")
	tpl, ok := c.reg.Get(p.cfg.ID, req.Template)
	if !ok {
		return nil, notFound
	}
	// Non-public templates are indistinguishable from missing ones for
	// browser keys, so template names are not enumerable.
	if !secret && !tpl.Cfg.IsPublic() {
		return nil, notFound
	}
	recipients := p.templates[tpl.Cfg.Name]

	if err := rejectRawFields(req); err != nil {
		return nil, err
	}

	to := recipients.to
	switch tpl.Cfg.Recipients.Mode {
	case "alias":
		alias, err := payload.Alias(req.To)
		if err != nil || alias == "" {
			return nil, apperr.Validation("a recipient alias is required",
				apperr.FieldError{Field: "to", Message: "must be one of the template's recipient aliases"})
		}
		if to, ok = recipients.aliases[alias]; !ok {
			return nil, apperr.Validation("unknown recipient alias",
				apperr.FieldError{Field: "to", Message: "is not a known alias"})
		}
	case "request":
		// Only secret keys can get here (validated at load + non-public check
		// above); the project's recipient policy still applies.
		list, err := payload.Addresses(req.To)
		if err != nil {
			return nil, apperr.Validation("invalid recipients", apperr.FieldError{Field: "to", Message: err.Error()})
		}
		if err := p.policy.CheckRawRecipients(list); err != nil {
			return nil, err
		}
		to = list
	default:
		if len(req.To) > 0 {
			return nil, apperr.Validation("recipients are fixed for this template",
				apperr.FieldError{Field: "to", Message: "is not accepted for this template"})
		}
	}

	data, err := tpl.Validate(req.Data)
	if err != nil {
		if errors.Is(err, render.ErrHoneypot) {
			return &Result{Honeypot: true, Template: tpl.Cfg.Name}, nil
		}
		return nil, err
	}
	rendered, err := tpl.Render(data)
	if err != nil {
		return nil, err
	}

	from := p.defaultFrom
	if tpl.Cfg.From != "" {
		if from, err = message.ParseAddress(tpl.Cfg.From); err != nil {
			return nil, apperr.Internal(err)
		}
	}
	msg := newMessage(now, from)
	msg.To, msg.CC, msg.BCC = to, recipients.cc, recipients.bcc
	msg.Subject, msg.Text, msg.HTML = rendered.Subject, rendered.Text, rendered.HTML

	replyTo, err := replyToFor(req, tpl.Cfg, data)
	if err != nil {
		return nil, err
	}
	msg.ReplyTo = replyTo
	return &Result{Msg: msg, Template: tpl.Cfg.Name}, nil
}

func replyToFor(req *payload.Request, cfg *config.Template, data map[string]any) ([]message.Address, error) {
	if len(req.ReplyTo) > 0 {
		list, err := payload.Addresses(req.ReplyTo)
		if err != nil {
			return nil, apperr.Validation("invalid reply_to", apperr.FieldError{Field: "reply_to", Message: err.Error()})
		}
		if len(list) > 1 {
			return nil, apperr.Validation("invalid reply_to", apperr.FieldError{Field: "reply_to", Message: "accepts a single address"})
		}
		return list, nil
	}
	if cfg.ReplyToField == "" {
		return nil, nil
	}
	email, _ := data[cfg.ReplyToField].(string)
	if email == "" {
		return nil, nil
	}
	name, _ := data[cfg.ReplyToNameKey].(string)
	a, err := message.NewAddress(name, email)
	if err != nil {
		return nil, apperr.Validation("invalid reply-to field",
			apperr.FieldError{Field: "data." + cfg.ReplyToField, Message: err.Error()})
	}
	return []message.Address{a}, nil
}

func rejectRawFields(req *payload.Request) error {
	var fields []apperr.FieldError
	add := func(name string, present bool) {
		if present {
			fields = append(fields, apperr.FieldError{Field: name, Message: "is not accepted when a template is used"})
		}
	}
	add("cc", len(req.CC) > 0)
	add("bcc", len(req.BCC) > 0)
	add("from", len(req.From) > 0)
	add("subject", req.Subject != "")
	add("text", req.Text != "")
	add("html", req.HTML != "")
	add("attachments", len(req.Attachments) > 0)
	if len(fields) > 0 {
		return apperr.Validation("raw content fields cannot be combined with a template", fields...)
	}
	return nil
}

func (c *Composer) fromRaw(p *project, req *payload.Request, now time.Time) (*Result, error) {
	if len(req.Data) > 0 {
		return nil, apperr.Validation("data is only accepted together with a template",
			apperr.FieldError{Field: "data", Message: "requires template"})
	}

	var fields []apperr.FieldError
	parse := func(name string, raw []byte) []message.Address {
		list, err := payload.Addresses(raw)
		if err != nil {
			fields = append(fields, apperr.FieldError{Field: name, Message: err.Error()})
		}
		return list
	}
	to, cc, bcc := parse("to", req.To), parse("cc", req.CC), parse("bcc", req.BCC)
	replyTo := parse("reply_to", req.ReplyTo)

	from := p.defaultFrom
	if len(req.From) > 0 {
		list := parse("from", req.From)
		if len(list) != 1 {
			fields = append(fields, apperr.FieldError{Field: "from", Message: "must be a single address"})
		} else {
			from = list[0]
			if err := p.policy.CheckFrom(from); err != nil {
				return nil, err
			}
		}
	}

	subject := strings.TrimSpace(req.Subject)
	switch {
	case subject == "":
		fields = append(fields, apperr.FieldError{Field: "subject", Message: "is required"})
	case message.ContainsControl(subject):
		fields = append(fields, apperr.FieldError{Field: "subject", Message: "must be a single line"})
	case len([]rune(subject)) > maxSubjectLen:
		fields = append(fields, apperr.FieldError{Field: "subject", Message: fmt.Sprintf("must be at most %d characters", maxSubjectLen)})
	}
	if strings.TrimSpace(req.Text) == "" && strings.TrimSpace(req.HTML) == "" {
		fields = append(fields, apperr.FieldError{Field: "text", Message: "text or html is required"})
	}
	if len(replyTo) > 1 {
		fields = append(fields, apperr.FieldError{Field: "reply_to", Message: "accepts a single address"})
	}
	if len(fields) > 0 {
		return nil, apperr.Validation("invalid message", fields...)
	}

	all := make([]message.Address, 0, len(to)+len(cc)+len(bcc))
	all = append(append(append(all, to...), cc...), bcc...)
	if err := p.policy.CheckRawRecipients(all); err != nil {
		return nil, err
	}
	if len(to) == 0 {
		return nil, apperr.Validation("at least one recipient is required",
			apperr.FieldError{Field: "to", Message: "is required"})
	}
	attachments, err := p.policy.CheckAttachments(req.Attachments)
	if err != nil {
		return nil, err
	}

	msg := newMessage(now, from)
	msg.To, msg.CC, msg.BCC, msg.ReplyTo = to, cc, bcc, replyTo
	msg.Subject, msg.Text, msg.HTML, msg.Attach = subject, req.Text, req.HTML, attachments
	if msg.Text == "" {
		msg.Text = render.HTMLToText(msg.HTML)
	}
	return &Result{Msg: msg}, nil
}
