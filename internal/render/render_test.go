package render

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/skylarng89/smtp-handler/internal/apperr"
	"github.com/skylarng89/smtp-handler/internal/config"
)

func compileOne(t *testing.T, tpl config.Template) *Template {
	t.Helper()
	cfg := &config.Config{Projects: []config.Project{{ID: "p", Templates: []config.Template{tpl}}}}
	reg, err := NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reg.Get("p", tpl.Name)
	if !ok {
		t.Fatal("template missing")
	}
	return got
}

func fieldErrors(t *testing.T, err error) map[string]string {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("not an apperr: %v", err)
	}
	out := map[string]string{}
	for _, f := range ae.Fields {
		out[f.Field] = f.Message
	}
	return out
}

func TestValidateEnforcesSchema(t *testing.T) {
	tpl := compileOne(t, config.Template{
		Name: "t", Subject: "s", Text: "x",
		Honeypot: "website",
		Fields: map[string]config.Field{
			"name":  {Type: "string", Required: true, Max: 5},
			"email": {Type: "email", Required: true},
			"msg":   {Type: "text", Min: 3},
			"age":   {Type: "number", Min: 18, Max: 99},
			"agree": {Type: "boolean", Required: true},
			"kind":  {Type: "string", Enum: []string{"a", "b"}},
			"zip":   {Type: "string", Pattern: `[0-9]{5}`},
		},
	})
	ok := map[string]any{"name": "Al", "email": "Al@Example.COM", "agree": true, "age": json.Number("20"), "kind": "a", "zip": "12345", "msg": "hello\nworld"}
	out, err := tpl.Validate(ok)
	if err != nil {
		t.Fatalf("valid data rejected: %v", err)
	}
	if out["email"] != "Al@example.com" {
		t.Fatalf("email not normalized: %v", out["email"])
	}

	cases := map[string]map[string]any{
		"data.name":  {"name": "toolong", "email": "a@b.co", "agree": true},
		"data.email": {"name": "a", "email": "x", "agree": true},
		"data.age":   {"name": "a", "email": "a@b.co", "agree": true, "age": json.Number("5")},
		"data.agree": {"name": "a", "email": "a@b.co", "agree": "yes"},
		"data.kind":  {"name": "a", "email": "a@b.co", "agree": true, "kind": "c"},
		"data.zip":   {"name": "a", "email": "a@b.co", "agree": true, "zip": "1234x"},
		"data.msg":   {"name": "a", "email": "a@b.co", "agree": true, "msg": "hi"},
		"data.extra": {"name": "a", "email": "a@b.co", "agree": true, "extra": "x"},
	}
	for field, data := range cases {
		_, err := tpl.Validate(data)
		if _, bad := fieldErrors(t, err)[field]; !bad {
			t.Errorf("%s: expected an error, got %v", field, err)
		}
	}

	// Control characters are rejected in single-line fields but allowed in text.
	_, err = tpl.Validate(map[string]any{"name": "a\nb", "email": "a@b.co", "agree": true})
	if _, bad := fieldErrors(t, err)["data.name"]; !bad {
		t.Error("newline accepted in a single-line field")
	}
	_, err = tpl.Validate(map[string]any{"name": "a\x00", "email": "a@b.co", "agree": true, "msg": "ok ok"})
	if err == nil {
		t.Error("NUL accepted")
	}

	if _, err := tpl.Validate(map[string]any{"name": "a", "email": "a@b.co", "agree": true, "website": "http://spam"}); !errors.Is(err, ErrHoneypot) {
		t.Fatalf("honeypot not detected: %v", err)
	}
	if _, err := tpl.Validate(map[string]any{"name": "a", "email": "a@b.co", "agree": true, "website": ""}); err != nil {
		t.Fatalf("an empty honeypot must be accepted: %v", err)
	}
}

func TestRenderEscapesHTMLAndSanitizesSubject(t *testing.T) {
	tpl := compileOne(t, config.Template{
		Name: "t", Subject: "Hi {{.name}}",
		HTML:   `<p>{{.msg}}</p><a href="{{.url}}">x</a>`,
		Fields: map[string]config.Field{"name": {Type: "text"}, "msg": {Type: "text"}, "url": {Type: "string"}},
	})
	out, err := tpl.Render(map[string]any{"name": "A\r\nBcc: evil@x.com", "msg": "<img src=x onerror=alert(1)>", "url": `javascript:alert(1)`})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.Subject, "\r\n") {
		t.Fatalf("header injection via subject: %q", out.Subject)
	}
	if strings.Contains(out.HTML, "<img") || !strings.Contains(out.HTML, "&lt;img") {
		t.Fatalf("html not escaped: %s", out.HTML)
	}
	if strings.Contains(out.HTML, `href="javascript:`) {
		t.Fatalf("javascript: URL survived in an href: %s", out.HTML)
	}
	if out.Text == "" {
		t.Fatal("text alternative not generated")
	}
}

func TestRenderFailuresAreClientVisibleNotCrashes(t *testing.T) {
	tpl := compileOne(t, config.Template{Name: "t", Subject: "{{.missing}}", Text: "x"})
	_, err := tpl.Render(map[string]any{})
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Kind != apperr.KindUnprocessable {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(ae.Detail, "missing") {
		t.Fatalf("internal detail leaked to the client: %s", ae.Detail)
	}

	empty := compileOne(t, config.Template{Name: "e", Subject: "{{.s}}", Text: "x"})
	if _, err := empty.Render(map[string]any{"s": "  \r\n "}); err == nil {
		t.Fatal("empty subject accepted")
	}
}

func TestRegistryReportsAllCompileErrors(t *testing.T) {
	cfg := &config.Config{Projects: []config.Project{{ID: "p", Templates: []config.Template{
		{Name: "a", Subject: "{{.x", Text: "ok"},
		{Name: "b", Subject: "ok", HTML: "{{range}}"},
	}}}}
	_, err := NewRegistry(cfg)
	if err == nil || !strings.Contains(err.Error(), `template "a"`) || !strings.Contains(err.Error(), `template "b"`) {
		t.Fatalf("got %v", err)
	}
}

func TestHTMLToText(t *testing.T) {
	got := HTMLToText(`<html><head><title>no</title><style>p{}</style></head><body>
		<h1>Title</h1><p>Hello&nbsp;<b>world</b> &amp; <a href="https://x.test/a">link</a></p>
		<script>alert(1)</script><ul><li>one</li><li>two</li></ul><a href="https://same.test">https://same.test</a></body></html>`)
	for _, want := range []string{"Title", "Hello", "world & link (https://x.test/a)", "- one", "- two", "https://same.test"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, bad := range []string{"alert", "no\n", "p{}", "<", "(https://same.test)"} {
		if strings.Contains(got, bad) {
			t.Errorf("unexpected %q in:\n%s", bad, got)
		}
	}
	if HTMLToText("") != "\n" {
		t.Errorf("empty input: %q", HTMLToText(""))
	}
	_ = HTMLToText("<<<>>><a href=")
}
