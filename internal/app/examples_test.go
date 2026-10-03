package app_test

import (
	"path/filepath"
	"testing"

	"github.com/skylarng89/smtp-handler/internal/compose"
	"github.com/skylarng89/smtp-handler/internal/config"
	"github.com/skylarng89/smtp-handler/internal/render"
)

// The shipped example configuration and templates are documentation people
// copy; this keeps them from silently rotting as the schema evolves.
func TestShippedExamplesAreValid(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "example.yaml")
	cfg, err := config.Load(path, func(k string) string {
		if k == "ACME_SMTP_PASSWORD" {
			return "secret"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("configs/example.yaml: %v", err)
	}
	reg, err := render.NewRegistry(cfg)
	if err != nil {
		t.Fatalf("example templates do not compile: %v", err)
	}
	if _, err := compose.New(cfg, reg); err != nil {
		t.Fatal(err)
	}

	p := cfg.Projects[0]
	names := map[string]bool{}
	for _, tpl := range p.Templates {
		names[tpl.Name] = true
	}
	for _, want := range []string{"contact-form", "department", "password-reset"} {
		if !names[want] {
			t.Errorf("example template %q was not loaded", want)
		}
	}
	for _, tpl := range p.Templates {
		if tpl.Name == "password-reset" && tpl.IsPublic() {
			t.Error("the password-reset example must not be reachable from browsers")
		}
	}
}
