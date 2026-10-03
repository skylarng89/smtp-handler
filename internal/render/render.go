// Package render compiles project templates once at startup, validates
// client-supplied data against each template's field schema, and renders
// subject/HTML/text. HTML bodies use html/template so data is auto-escaped.
package render

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	htmltemplate "html/template"
	"regexp"
	"strconv"
	"strings"
	texttemplate "text/template"
	"unicode/utf8"

	"github.com/skylarng89/smtp-handler/internal/apperr"
	"github.com/skylarng89/smtp-handler/internal/config"
	"github.com/skylarng89/smtp-handler/internal/message"
)

// ErrHoneypot signals that a bot filled the honeypot field. Callers should
// pretend success so the bot learns nothing.
var ErrHoneypot = errors.New("honeypot field was filled")

const (
	defaultStringMax = 200
	defaultTextMax   = 5000
	defaultEmailMax  = 254
	freeFormValueMax = 100_000 // schema-less templates (secret keys only)
)

// Rendered is the output of a template.
type Rendered struct {
	Subject string
	HTML    string
	Text    string
}

// Template is a compiled template plus its field schema.
type Template struct {
	Cfg     *config.Template
	subject *texttemplate.Template
	html    *htmltemplate.Template
	text    *texttemplate.Template
	fields  map[string]compiledField
}

type compiledField struct {
	config.Field
	pattern *regexp.Regexp
}

// Registry holds every compiled template, keyed by project then name.
// It is immutable after construction and therefore safe for concurrent use.
type Registry struct {
	byProject map[string]map[string]*Template
}

// NewRegistry compiles all templates, returning every compile error at once.
func NewRegistry(cfg *config.Config) (*Registry, error) {
	r := &Registry{byProject: make(map[string]map[string]*Template, len(cfg.Projects))}
	var errs []error
	for i := range cfg.Projects {
		p := &cfg.Projects[i]
		m := make(map[string]*Template, len(p.Templates))
		for j := range p.Templates {
			t, err := compile(&p.Templates[j])
			if err != nil {
				errs = append(errs, fmt.Errorf("project %q template %q: %w", p.ID, p.Templates[j].Name, err))
				continue
			}
			m[t.Cfg.Name] = t
		}
		r.byProject[p.ID] = m
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return r, nil
}

// Get returns a template by project and name.
func (r *Registry) Get(projectID, name string) (*Template, bool) {
	t, ok := r.byProject[projectID][name]
	return t, ok
}

func compile(cfg *config.Template) (*Template, error) {
	t := &Template{Cfg: cfg, fields: make(map[string]compiledField, len(cfg.Fields))}
	var err error
	if t.subject, err = texttemplate.New("subject").Option("missingkey=error").Parse(cfg.Subject); err != nil {
		return nil, fmt.Errorf("subject: %w", err)
	}
	if strings.TrimSpace(cfg.HTML) != "" {
		if t.html, err = htmltemplate.New("html").Option("missingkey=error").Parse(cfg.HTML); err != nil {
			return nil, fmt.Errorf("html: %w", err)
		}
	}
	if strings.TrimSpace(cfg.Text) != "" {
		if t.text, err = texttemplate.New("text").Option("missingkey=error").Parse(cfg.Text); err != nil {
			return nil, fmt.Errorf("text: %w", err)
		}
	}
	for name, f := range cfg.Fields {
		cf := compiledField{Field: f}
		if f.Pattern != "" {
			// RE2 guarantees linear-time matching, so client input cannot trigger ReDoS.
			if cf.pattern, err = regexp.Compile("^(?:" + f.Pattern + ")$"); err != nil {
				return nil, fmt.Errorf("field %q pattern: %w", name, err)
			}
		}
		t.fields[name] = cf
	}
	return t, nil
}

// Validate checks client data against the field schema and returns the
// normalized values to render with. Declared-but-absent optional fields are
// set to "" so templates can reference them safely.
func (t *Template) Validate(data map[string]any) (map[string]any, error) {
	if len(t.fields) == 0 {
		return t.validateFreeForm(data)
	}

	out := make(map[string]any, len(t.fields))
	var problems []apperr.FieldError
	bad := func(name, msg string) {
		problems = append(problems, apperr.FieldError{Field: "data." + name, Message: msg})
	}

	for name := range data {
		if _, declared := t.fields[name]; !declared && name != t.Cfg.Honeypot {
			bad(name, "unknown field")
		}
	}
	if hp := t.Cfg.Honeypot; hp != "" {
		if v, ok := data[hp]; ok && fmt.Sprint(v) != "" && v != nil {
			return nil, ErrHoneypot
		}
	}

	for name, f := range t.fields {
		raw, present := data[name]
		if !present || raw == nil || raw == "" {
			if f.Required {
				bad(name, "is required")
			}
			out[name] = zeroFor(f.Type)
			continue
		}
		val, msg := f.check(raw)
		if msg != "" {
			bad(name, msg)
			continue
		}
		out[name] = val
	}
	if len(problems) > 0 {
		return nil, apperr.Validation("data does not match the template", problems...)
	}
	return out, nil
}

func zeroFor(typ string) any {
	switch typ {
	case "number":
		return json.Number("0")
	case "boolean":
		return false
	default:
		return ""
	}
}

func (t *Template) validateFreeForm(data map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(data))
	var problems []apperr.FieldError
	for k, v := range data {
		if s, ok := v.(string); ok {
			if utf8.RuneCountInString(s) > freeFormValueMax || !validText(s, true) {
				problems = append(problems, apperr.FieldError{Field: "data." + k, Message: "invalid or too long"})
				continue
			}
		}
		out[k] = v
	}
	if len(problems) > 0 {
		return nil, apperr.Validation("invalid data values", problems...)
	}
	return out, nil
}

func (f *compiledField) check(raw any) (any, string) {
	switch f.Type {
	case "boolean":
		b, ok := raw.(bool)
		if !ok {
			return nil, "must be a boolean"
		}
		return b, ""
	case "number":
		n, ok := raw.(json.Number)
		if !ok {
			return nil, "must be a number"
		}
		v, err := strconv.ParseFloat(n.String(), 64)
		if err != nil {
			return nil, "must be a number"
		}
		if f.Min != 0 && v < float64(f.Min) {
			return nil, fmt.Sprintf("must be at least %d", f.Min)
		}
		if f.Max != 0 && v > float64(f.Max) {
			return nil, fmt.Sprintf("must be at most %d", f.Max)
		}
		return n, ""
	}

	s, ok := raw.(string)
	if !ok {
		return nil, "must be a string"
	}
	multiline := f.Type == "text"
	if !validText(s, multiline) {
		return nil, "contains invalid characters"
	}
	max := f.Max
	if max == 0 {
		switch f.Type {
		case "text":
			max = defaultTextMax
		case "email":
			max = defaultEmailMax
		default:
			max = defaultStringMax
		}
	}
	n := utf8.RuneCountInString(s)
	if n > max {
		return nil, fmt.Sprintf("must be at most %d characters", max)
	}
	if f.Min > 0 && n < f.Min {
		return nil, fmt.Sprintf("must be at least %d characters", f.Min)
	}
	if f.Required && strings.TrimSpace(s) == "" {
		return nil, "is required"
	}
	if f.Type == "email" {
		a, err := message.NewAddress("", strings.TrimSpace(s))
		if err != nil {
			return nil, "must be a valid email address"
		}
		return a.Email, ""
	}
	if len(f.Enum) > 0 && !contains(f.Enum, s) {
		return nil, "must be one of: " + strings.Join(f.Enum, ", ")
	}
	if f.pattern != nil && !f.pattern.MatchString(s) {
		return nil, "has an invalid format"
	}
	return s, ""
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// validText requires valid UTF-8 without control characters; multi-line
// fields may additionally contain CR, LF and tab.
func validText(s string, multiline bool) bool {
	if !utf8.ValidString(s) {
		return false
	}
	if multiline {
		return !message.ContainsControl(s, '\n', '\r', '\t')
	}
	return !message.ContainsControl(s, '\t')
}

// Render executes the template. The text part is derived from the HTML when
// the template only defines HTML.
func (t *Template) Render(data map[string]any) (*Rendered, error) {
	var out Rendered

	var buf bytes.Buffer
	if err := t.subject.Execute(&buf, data); err != nil {
		return nil, renderError("subject", err)
	}
	out.Subject = message.SanitizeSubject(buf.String())
	if out.Subject == "" {
		return nil, apperr.New(apperr.KindUnprocessable, "empty-subject", "the template rendered an empty subject")
	}

	if t.html != nil {
		buf.Reset()
		if err := t.html.Execute(&buf, data); err != nil {
			return nil, renderError("html", err)
		}
		out.HTML = buf.String()
	}
	if t.text != nil {
		buf.Reset()
		if err := t.text.Execute(&buf, data); err != nil {
			return nil, renderError("text", err)
		}
		out.Text = buf.String()
	} else if out.HTML != "" {
		out.Text = HTMLToText(out.HTML)
	}
	return &out, nil
}

func renderError(part string, err error) error {
	return &apperr.Error{
		Kind:   apperr.KindUnprocessable,
		Code:   "template-render-failed",
		Detail: "the template could not be rendered with the supplied data (" + part + ")",
		Cause:  err,
	}
}
