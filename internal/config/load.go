package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Getenv abstracts os.Getenv so tests can inject an environment.
type Getenv func(string) string

// Load builds the effective configuration. path may be empty, in which case
// the service is configured entirely from SMTPH_* environment variables
// (single-app mode).
func Load(path string, getenv Getenv) (*Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if path == "" {
		path = getenv("SMTPH_CONFIG")
	}

	cfg := &Config{}
	baseDir := "."
	if path != "" {
		f, err := os.Open(path) //nolint:gosec // G304: the config path is the operator's own flag/env
		if err != nil {
			return nil, fmt.Errorf("open config: %w", err)
		}
		defer func() { _ = f.Close() }()
		if err := decodeStrict(f, cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		baseDir = filepath.Dir(path)
	}

	env := envReader{get: getenv}
	applyEnv(cfg, &env)
	if p, ok := defaultProjectFromEnv(&env); ok {
		cfg.Projects = append(cfg.Projects, p)
	}
	if err := env.err(); err != nil {
		return nil, err
	}

	applyDefaults(cfg)
	if err := resolveFiles(cfg, baseDir, getenv); err != nil {
		return nil, err
	}
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func decodeStrict(r io.Reader, out any) error {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// envReader collects parse errors so every bad variable is reported at once.
type envReader struct {
	get  Getenv
	errs []error
}

func (e *envReader) err() error { return errors.Join(e.errs...) }

func (e *envReader) str(name string, dst *string) {
	if v := e.get(name); v != "" {
		*dst = v
	}
}

// secret reads NAME, or the contents of the file named by NAME_FILE.
func (e *envReader) secret(name string, dst *string) {
	if v := e.get(name); v != "" {
		*dst = v
		return
	}
	if path := e.get(name + "_FILE"); path != "" {
		b, err := os.ReadFile(path) //nolint:gosec // G304: operator-supplied *_FILE secret path
		if err != nil {
			e.errs = append(e.errs, fmt.Errorf("%s_FILE: %w", name, err))
			return
		}
		*dst = strings.TrimSpace(string(b))
	}
}

func (e *envReader) integer(name string, dst *int) {
	if v := e.get(name); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			e.errs = append(e.errs, fmt.Errorf("%s: %q is not an integer", name, v))
			return
		}
		*dst = n
	}
}

func (e *envReader) boolean(name string, dst *bool) {
	if v := e.get(name); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			e.errs = append(e.errs, fmt.Errorf("%s: %q is not a boolean", name, v))
			return
		}
		*dst = b
	}
}

func (e *envReader) duration(name string, dst *Duration) {
	if v := e.get(name); v != "" {
		d, err := ParseDuration(v)
		if err != nil {
			e.errs = append(e.errs, fmt.Errorf("%s: %w", name, err))
			return
		}
		*dst = Duration(d)
	}
}

func (e *envReader) list(name string) []string {
	return splitList(e.get(name))
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func applyEnv(cfg *Config, e *envReader) {
	e.str("SMTPH_ADDR", &cfg.Server.Addr)
	e.str("SMTPH_ADMIN_ADDR", &cfg.Server.AdminAddr)
	if v := e.list("SMTPH_TRUSTED_PROXIES"); v != nil {
		cfg.Server.TrustedProxies = v
	}

	e.boolean("SMTPH_ADMIN_ENABLED", &cfg.Admin.Enabled)
	e.boolean("SMTPH_UI_ENABLED", &cfg.Admin.UI)
	e.str("SMTPH_ADMIN_USERNAME", &cfg.Admin.Username)
	e.secret("SMTPH_ADMIN_PASSWORD_HASH", &cfg.Admin.PasswordHash)
	e.secret("SMTPH_ADMIN_SESSION_SECRET", &cfg.Admin.SessionSecret)
	e.boolean("SMTPH_ADMIN_INSECURE_COOKIES", &cfg.Admin.InsecureCookies)

	e.duration("SMTPH_SHUTDOWN_DELAY", &cfg.Server.ShutdownDelay)
	e.str("SMTPH_TLS_CERT_FILE", &cfg.Server.TLSCertFile)
	e.str("SMTPH_TLS_KEY_FILE", &cfg.Server.TLSKeyFile)
	if v := e.get("SMTPH_AUTO_MIGRATE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			e.errs = append(e.errs, fmt.Errorf("SMTPH_AUTO_MIGRATE: %q is not a boolean", v))
		}
		cfg.Store.AutoMigrate = &b
	}
	e.str("SMTPH_STORE_DRIVER", &cfg.Store.Driver)
	e.secret("SMTPH_STORE_DSN", &cfg.Store.DSN)
	e.secret("SMTPH_REDIS_URL", &cfg.Redis.URL)
	e.str("SMTPH_RATELIMIT_BACKEND", &cfg.RateLimit.Backend)

	e.integer("SMTPH_WORKERS", &cfg.Delivery.Workers)
	e.integer("SMTPH_MAX_ATTEMPTS", &cfg.Delivery.MaxAttempts)
	e.duration("SMTPH_MAX_AGE", &cfg.Delivery.MaxAge)

	e.duration("SMTPH_RETENTION_SENT", &cfg.Retention.Sent)
	e.duration("SMTPH_RETENTION_FAILED", &cfg.Retention.Failed)
	e.boolean("SMTPH_DROP_BODY_ON_SENT", &cfg.Retention.DropBodyOnSent)

	e.str("SMTPH_LOG_LEVEL", &cfg.Logging.Level)
	e.str("SMTPH_LOG_FORMAT", &cfg.Logging.Format)
}

// defaultProjectFromEnv builds the single "default" project used by
// env-only deployments. It is only created when SMTPH_SMTP_HOST is set.
func defaultProjectFromEnv(e *envReader) (Project, bool) {
	host := e.get("SMTPH_SMTP_HOST")
	if host == "" {
		return Project{}, false
	}
	p := Project{ID: "default", Name: "default"}
	p.SMTP.Host = host
	e.integer("SMTPH_SMTP_PORT", &p.SMTP.Port)
	e.str("SMTPH_SMTP_USERNAME", &p.SMTP.Username)
	e.secret("SMTPH_SMTP_PASSWORD", &p.SMTP.Password)
	e.str("SMTPH_SMTP_TLS", &p.SMTP.TLS)
	e.str("SMTPH_SMTP_AUTH", &p.SMTP.AuthMethod)
	e.integer("SMTPH_SMTP_MAX_CONNECTIONS", &p.SMTP.MaxConnections)
	e.integer("SMTPH_SMTP_GLOBAL_MAX_CONNECTIONS", &p.SMTP.GlobalMaxConnections)
	e.str("SMTPH_FROM", &p.From)
	e.str("SMTPH_TEMPLATES_DIR", &p.TemplatesDir)
	e.str("SMTPH_DELIVERY_SEMANTICS", &p.Delivery.Semantics)
	e.duration("SMTPH_DEDUPE_WINDOW", &p.DedupeWindow)

	origins := e.list("SMTPH_ALLOWED_ORIGINS")
	for _, h := range e.list("SMTPH_PUBLISHABLE_KEY_HASHES") {
		p.Keys = append(p.Keys, Key{Type: "publishable", Hash: h, AllowedOrigins: origins})
	}
	for _, h := range e.list("SMTPH_SECRET_KEY_HASHES") {
		p.Keys = append(p.Keys, Key{Type: "secret", Hash: h})
	}

	if domains := e.list("SMTPH_RECIPIENT_DOMAINS"); domains != nil {
		p.Policy.RawRecipients = "domain_allowlist"
		p.Policy.AllowedRecipientDomains = domains
	}
	e.str("SMTPH_RAW_RECIPIENTS", &p.Policy.RawRecipients)

	e.str("SMTPH_CAPTCHA_PROVIDER", &p.Captcha.Provider)
	e.secret("SMTPH_CAPTCHA_SECRET", &p.Captcha.Secret)
	return p, true
}

func applyDefaults(c *Config) {
	setStr := func(dst *string, def string) {
		if *dst == "" {
			*dst = def
		}
	}
	setInt := func(dst *int, def int) {
		if *dst == 0 {
			*dst = def
		}
	}
	setDur := func(dst *Duration, def time.Duration) {
		if *dst == 0 {
			*dst = Duration(def)
		}
	}

	s := &c.Server
	setStr(&s.Addr, ":8080")
	setStr(&s.AdminAddr, "127.0.0.1:9090")
	setDur(&s.ReadHeaderTimeout, 5*time.Second)
	setDur(&s.ReadTimeout, 15*time.Second)
	setDur(&s.WriteTimeout, 30*time.Second)
	setDur(&s.IdleTimeout, 60*time.Second)
	setDur(&s.ShutdownTimeout, 30*time.Second)
	if s.PreAuthRate.Limit == 0 && s.PreAuthRate.Window == 0 {
		s.PreAuthRate = Rule{Limit: 600, Window: Duration(time.Minute)}
	}
	if s.MaxBodyPublishable == 0 {
		s.MaxBodyPublishable = 64 << 10
	}
	if s.MaxBodySecret == 0 {
		s.MaxBodySecret = 25 << 20
	}

	a := &c.Admin
	setStr(&a.Username, "admin")
	setDur(&a.SessionTTL, 12*time.Hour)

	setStr(&c.Store.Driver, "sqlite")
	if c.Store.DSN == "" && c.Store.Driver == "sqlite" {
		c.Store.DSN = "data/smtp-handler.db"
	}
	setInt(&c.Store.MaxOpenConns, 20)
	if c.Store.AutoMigrate == nil {
		on := true
		c.Store.AutoMigrate = &on
	}
	setStr(&c.RateLimit.Backend, "auto")

	d := &c.Delivery
	setInt(&d.Workers, 8)
	setInt(&d.BatchSize, 16)
	setDur(&d.PollInterval, time.Second)
	setDur(&d.LeaseDuration, 2*time.Minute)
	setDur(&d.SendTimeout, 60*time.Second)
	setInt(&d.MaxAttempts, 8)
	setDur(&d.MaxAge, 24*time.Hour)
	setDur(&d.BackoffBase, 30*time.Second)
	setDur(&d.BackoffMax, time.Hour)
	setDur(&d.BreakerCooldown, time.Minute)
	setDur(&d.DrainTimeout, 20*time.Second)

	r := &c.Retention
	setDur(&r.Sent, 30*24*time.Hour)
	setDur(&r.Failed, 30*24*time.Hour)
	setDur(&r.PurgeInterval, time.Hour)

	setStr(&c.Logging.Level, "info")
	setStr(&c.Logging.Format, "json")

	for i := range c.Projects {
		applyProjectDefaults(&c.Projects[i])
	}
}

func applyProjectDefaults(p *Project) {
	if p.Name == "" {
		p.Name = p.ID
	}
	s := &p.SMTP
	if s.TLS == "" {
		s.TLS = "starttls"
	}
	if s.AuthMethod == "" {
		s.AuthMethod = "auto"
	}
	if s.Port == 0 {
		switch s.TLS {
		case "tls":
			s.Port = 465
		case "none":
			s.Port = 25
		default:
			s.Port = 587
		}
	}
	if s.Timeout == 0 {
		s.Timeout = Duration(30 * time.Second)
	}
	if s.MaxConnections == 0 {
		s.MaxConnections = 4
	}
	if s.IdleTimeout == 0 {
		s.IdleTimeout = Duration(30 * time.Second)
	}

	pol := &p.Policy
	if pol.RawRecipients == "" {
		pol.RawRecipients = "any"
	}
	if pol.MaxRecipients == 0 {
		pol.MaxRecipients = 50
	}
	if pol.Attachments.MaxCount == 0 {
		pol.Attachments.MaxCount = 10
	}
	if pol.Attachments.MaxBytes == 0 {
		pol.Attachments.MaxBytes = 10 << 20
	}
	if pol.Attachments.AllowedExtensions == nil {
		pol.Attachments.AllowedExtensions = []string{
			".pdf", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".txt", ".csv",
			".ics", ".docx", ".xlsx", ".pptx",
		}
	}

	rule := func(dst **Rule, limit int, window time.Duration) {
		if *dst == nil {
			*dst = &Rule{Limit: limit, Window: Duration(window)}
		}
	}
	pub := &p.RateLimits.Publishable
	rule(&pub.PerKey, 120, time.Minute)
	rule(&pub.PerIP, 10, time.Minute)
	rule(&pub.PerRecipient, 100, time.Hour)
	rule(&pub.Daily, 0, 24*time.Hour)
	sec := &p.RateLimits.Secret
	rule(&sec.PerKey, 1000, time.Minute)
	rule(&sec.PerIP, 0, time.Minute)
	rule(&sec.PerRecipient, 0, time.Hour)
	rule(&sec.Daily, 0, 24*time.Hour)

	if p.Captcha.Provider != "" && p.Captcha.Timeout == 0 {
		p.Captcha.Timeout = Duration(5 * time.Second)
	}
	if p.Delivery.Semantics == "" {
		p.Delivery.Semantics = "at_least_once"
	}
	for i := range p.Templates {
		t := &p.Templates[i]
		if t.Recipients.Mode == "" {
			t.Recipients.Mode = "fixed"
		}
	}
}

// resolveFiles loads secrets referenced by env/file and template files.
func resolveFiles(c *Config, baseDir string, getenv Getenv) error {
	var errs []error
	for i := range c.Projects {
		p := &c.Projects[i]
		if err := resolveSecret(&p.SMTP.Password, p.SMTP.PasswordEnv, p.SMTP.PasswordFile, getenv); err != nil {
			errs = append(errs, fmt.Errorf("project %q smtp password: %w", p.ID, err))
		}
		if err := resolveSecret(&p.Captcha.Secret, p.Captcha.SecretEnv, p.Captcha.SecretFile, getenv); err != nil {
			errs = append(errs, fmt.Errorf("project %q captcha secret: %w", p.ID, err))
		}
		if p.TemplatesDir != "" {
			dir := resolvePath(baseDir, p.TemplatesDir)
			loaded, err := loadTemplateDir(dir)
			if err != nil {
				errs = append(errs, fmt.Errorf("project %q templates_dir: %w", p.ID, err))
			}
			for _, t := range loaded {
				if t.Recipients.Mode == "" {
					t.Recipients.Mode = "fixed"
				}
				p.Templates = append(p.Templates, t)
			}
			baseForFiles := dir
			for j := range p.Templates {
				errs = append(errs, loadTemplateFiles(&p.Templates[j], baseForFiles)...)
			}
			continue
		}
		for j := range p.Templates {
			errs = append(errs, loadTemplateFiles(&p.Templates[j], baseDir)...)
		}
	}
	return errors.Join(errs...)
}

func resolveSecret(dst *string, envName, file string, getenv Getenv) error {
	if *dst != "" {
		return nil
	}
	if envName != "" {
		v := getenv(envName)
		if v == "" {
			return fmt.Errorf("environment variable %s is empty", envName)
		}
		*dst = v
		return nil
	}
	if file != "" {
		b, err := os.ReadFile(file) //nolint:gosec // G304: operator-configured secret/template path
		if err != nil {
			return err
		}
		*dst = strings.TrimSpace(string(b))
	}
	return nil
}

func resolvePath(base, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

func loadTemplateDir(dir string) ([]Template, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if !e.IsDir() && (ext == ".yaml" || ext == ".yml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var (
		out  []Template
		errs []error
	)
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // G304: files inside the operator-configured templates_dir
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var t Template
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&t); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if t.Name == "" {
			t.Name = strings.TrimSuffix(name, filepath.Ext(name))
		}
		out = append(out, t)
	}
	return out, errors.Join(errs...)
}

func loadTemplateFiles(t *Template, base string) []error {
	var errs []error
	load := func(dst *string, file string) {
		if file == "" || *dst != "" {
			return
		}
		b, err := os.ReadFile(resolvePath(base, file))
		if err != nil {
			errs = append(errs, fmt.Errorf("template %q: %w", t.Name, err))
			return
		}
		*dst = string(b)
	}
	load(&t.HTML, t.HTMLFile)
	load(&t.Text, t.TextFile)
	return errs
}
