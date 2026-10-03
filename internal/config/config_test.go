package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const hash64 = "sha256:0000000000000000000000000000000000000000000000000000000000000001"

func env(m map[string]string) Getenv { return func(k string) string { return m[k] } }

func TestSingleAppModeFromEnvironment(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "contact.yaml"), []byte(`
subject: "Hi {{.name}}"
text_file: contact.txt
recipients: {to: [support@example.com]}
fields:
  name: {type: string, required: true}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "contact.txt"), []byte("Hello {{.name}}"), 0o600)
	pwFile := filepath.Join(dir, "smtp-password")
	_ = os.WriteFile(pwFile, []byte("s3cret\n"), 0o600)

	cfg, err := Load("", env(map[string]string{
		"SMTPH_SMTP_HOST": "smtp.example.com", "SMTPH_SMTP_USERNAME": "mailer", "SMTPH_SMTP_PASSWORD_FILE": pwFile,
		"SMTPH_FROM": "App <noreply@example.com>", "SMTPH_PUBLISHABLE_KEY_HASHES": hash64,
		"SMTPH_ALLOWED_ORIGINS": "https://app.example.com, https://www.example.com",
		"SMTPH_TEMPLATES_DIR":   dir, "SMTPH_RETENTION_SENT": "7d", "SMTPH_WORKERS": "3",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Projects) != 1 {
		t.Fatalf("projects = %d", len(cfg.Projects))
	}
	p := cfg.Projects[0]
	if p.ID != "default" || p.SMTP.Host != "smtp.example.com" || p.SMTP.Port != 587 || p.SMTP.TLS != "starttls" {
		t.Fatalf("smtp defaults: %+v", p.SMTP)
	}
	if p.SMTP.Password != "s3cret" {
		t.Fatalf("password from file = %q (must be trimmed)", p.SMTP.Password)
	}
	if len(p.Keys) != 1 || len(p.Keys[0].AllowedOrigins) != 2 {
		t.Fatalf("keys: %+v", p.Keys)
	}
	if len(p.Templates) != 1 || p.Templates[0].Name != "contact" || p.Templates[0].Text != "Hello {{.name}}" {
		t.Fatalf("templates: %+v", p.Templates)
	}
	if !p.Templates[0].IsPublic() {
		t.Fatal("a template with fields should be public by default")
	}
	if cfg.Delivery.Workers != 3 || cfg.Retention.Sent.Std() != 7*24*time.Hour || cfg.Retention.Failed.Std() != 30*24*time.Hour {
		t.Fatalf("retention/workers: %+v %+v", cfg.Retention, cfg.Delivery)
	}
	if cfg.Store.Driver != "sqlite" || cfg.Server.AdminAddr != "127.0.0.1:9090" || cfg.RateLimit.Backend != "auto" {
		t.Fatalf("defaults: %+v", cfg)
	}
	if !p.Policy.Attachments.hasExt(".pdf") || p.RateLimits.Publishable.PerIP.Limit != 10 {
		t.Fatalf("project defaults: %+v", p.Policy)
	}
}

func (a Attachments) hasExt(e string) bool {
	for _, x := range a.AllowedExtensions {
		if x == e {
			return true
		}
	}
	return false
}

func TestEmptyConfigExplainsWhatIsMissing(t *testing.T) {
	_, err := Load("", env(nil))
	if err == nil || !strings.Contains(err.Error(), "no projects configured") {
		t.Fatalf("err = %v", err)
	}
}

func TestYAMLIsStrict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(path, []byte("projcts: []\n"), 0o600)
	if _, err := Load(path, env(nil)); err == nil || !strings.Contains(err.Error(), "projcts") {
		t.Fatalf("typos in keys must be rejected, got %v", err)
	}
}

func TestValidationReportsEveryProblem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(path, []byte(`
store: {driver: mysql}
admin: {enabled: true, password_hash: plaintext}
projects:
  - id: "Bad ID"
    from: not-an-address
    smtp: {host: "", tls: smtps}
    keys:
      - {type: publishable, hash: abc}
      - {type: secret, hash: `+hash64+`, allowed_origins: ["https://x.test"]}
    templates:
      - {name: t, subject: "", recipients: {mode: fixed}}
`), 0o600)
	_, err := Load(path, env(nil))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{
		"store.driver", "argon2id", "id must match", "smtp.host is required", "smtp.tls must be",
		"is not a valid address", "hash must be sha256", "allowed_origins only applies", "subject is required",
		"recipients.to is required",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}

func TestSecurityRulesInValidation(t *testing.T) {
	base := func(mutate string) string {
		return `
projects:
  - id: p
    from: a@example.com
    smtp: {host: h}
    keys: [{type: publishable, hash: ` + hash64 + `, allowed_origins: ["https://x.test"]}]
    templates:
      - name: t
        subject: s
        text: body
        recipients: {to: [a@example.com]}
` + mutate
	}
	tests := []struct{ name, extra, wantErr string }{
		{"public template needs fields", "        public: true\n", "must declare fields"},
		{"publishable key needs origins", "", ""},
		{"honeypot cannot shadow a field", "        fields: {x: {type: string}}\n        honeypot: x\n", "must not also be a declared field"},
		{"bad regexp", "        fields: {x: {type: string, pattern: \"(\"}}\n", "invalid pattern"},
		{"reply-to must be email", "        fields: {x: {type: string}}\n        reply_to_field: x\n", "must be of type email"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "c.yaml")
			_ = os.WriteFile(path, []byte(base(tc.extra)), 0o600)
			_, err := Load(path, env(nil))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("want %q, got %v", tc.wantErr, err)
			}
		})
	}

	path := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(path, []byte(strings.Replace(base(""), `allowed_origins: ["https://x.test"]`, "", 1)), 0o600)
	if _, err := Load(path, env(nil)); err == nil || !strings.Contains(err.Error(), "publishable keys need allowed_origins") {
		t.Fatalf("publishable key without origins must be rejected: %v", err)
	}
}

func TestDuplicateKeyHashAcrossProjectsIsRejected(t *testing.T) {
	proj := func(id string) string {
		return `  - id: ` + id + "\n    from: a@example.com\n    smtp: {host: h}\n    keys: [{type: secret, hash: " + hash64 + "}]\n"
	}
	path := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(path, []byte("projects:\n"+proj("a")+proj("b")), 0o600)
	if _, err := Load(path, env(nil)); err == nil || !strings.Contains(err.Error(), "duplicates a key hash") {
		t.Fatalf("one key must never authenticate against two projects: %v", err)
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"30d": 30 * 24 * time.Hour, "1.5d": 36 * time.Hour, "90m": 90 * time.Minute, "720h": 720 * time.Hour} {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "d", "abc", "5x"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("%q should fail", in)
		}
	}
}

func TestBadEnvironmentValuesAreAllReported(t *testing.T) {
	_, err := Load("", env(map[string]string{"SMTPH_WORKERS": "many", "SMTPH_ADMIN_ENABLED": "maybe", "SMTPH_RETENTION_SENT": "soon"}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"SMTPH_WORKERS", "SMTPH_ADMIN_ENABLED", "SMTPH_RETENTION_SENT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %s in %v", want, err)
		}
	}
}

func TestPerProjectRetentionOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(path, []byte(`
retention: {sent: 30d, failed: 30d}
projects:
  - id: strict
    from: a@example.com
    smtp: {host: h}
    keys: [{type: secret, hash: `+hash64+`}]
    retention: {sent: 24h, drop_body_on_sent: true}
`), 0o600)
	cfg, err := Load(path, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Projects[0]
	if p.SentRetention(cfg.Retention) != 24*time.Hour || p.FailedRetention(cfg.Retention) != 30*24*time.Hour || !p.DropBody(cfg.Retention) {
		t.Fatalf("overrides not applied: %+v", p.Retention)
	}
}

func TestRequestRecipientModeIsSecretKeyOnly(t *testing.T) {
	load := func(tpl string) error {
		path := filepath.Join(t.TempDir(), "c.yaml")
		_ = os.WriteFile(path, []byte(`
projects:
  - id: p
    from: a@example.com
    smtp: {host: h}
    keys: [{type: secret, hash: `+hash64+`}]
    templates:
      - name: t
        subject: s
        text: body
`+tpl), 0o600)
		_, err := Load(path, env(nil))
		return err
	}

	if err := load("        public: false\n        recipients: {mode: request}\n"); err != nil {
		t.Fatalf("valid request template rejected: %v", err)
	}
	// Declaring fields makes a template public by default; a public template
	// that lets the caller choose recipients would let browsers email anyone.
	err := load("        fields: {x: {type: string}}\n        recipients: {mode: request}\n")
	if err == nil || !strings.Contains(err.Error(), "requires `public: false`") {
		t.Fatalf("a public request-recipient template must be rejected: %v", err)
	}
	err = load("        public: true\n        fields: {x: {type: string}}\n        recipients: {mode: request}\n")
	if err == nil {
		t.Fatal("explicitly public request template accepted")
	}
	if err := load("        public: false\n        recipients: {mode: request, to: [x@example.com]}\n"); err == nil {
		t.Fatal("recipients.to with mode request must be rejected")
	}
}

func TestAdminPasswordHashMustBeWellFormed(t *testing.T) {
	// Produced by `smtp-handler hash-password`.
	valid := "$argon2id$v=19$m=65536,t=3,p=2$c29tZXNhbHRzb21lc2FsdA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	for hash, ok := range map[string]bool{
		valid:       true,
		"":          false,
		"plaintext": false,
		"$argon2id$REPLACE_WITH_OUTPUT_OF_hash-password": false,
		"$bcrypt$v=19$m=1,t=1,p=1$a$b":                   false,
	} {
		_, err := Load("", env(map[string]string{
			"SMTPH_SMTP_HOST": "h", "SMTPH_FROM": "a@example.com", "SMTPH_SECRET_KEY_HASHES": hash64,
			"SMTPH_ADMIN_ENABLED": "true", "SMTPH_ADMIN_PASSWORD_HASH": hash,
			"SMTPH_ADMIN_SESSION_SECRET": "0123456789abcdef0123456789abcdef",
		}))
		if ok && err != nil {
			t.Errorf("%q rejected: %v", hash, err)
		}
		if !ok && (err == nil || !strings.Contains(err.Error(), "argon2id")) {
			t.Errorf("%q accepted or wrong error: %v", hash, err)
		}
	}
}
