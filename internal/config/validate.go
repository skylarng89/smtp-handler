package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
)

var (
	identRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	hashRe     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	fieldKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	argonRe    = regexp.MustCompile(`^\$argon2id\$v=\d+\$m=\d+,t=\d+,p=\d+\$[A-Za-z0-9+/]+\$[A-Za-z0-9+/]+$`)
)

// Validate checks the fully-defaulted configuration. All problems are
// reported together so an operator can fix them in one pass.
func Validate(c *Config) error {
	v := &validator{}
	v.server(c)
	v.admin(c)
	v.store(c)
	v.delivery(c)

	if len(c.Projects) == 0 {
		v.add("no projects configured: set SMTPH_SMTP_HOST (single-app mode) or define projects in SMTPH_CONFIG")
	}
	seenProject := map[string]bool{}
	seenHash := map[string]string{}
	for i := range c.Projects {
		p := &c.Projects[i]
		if seenProject[p.ID] {
			v.add("duplicate project id %q", p.ID)
		}
		seenProject[p.ID] = true
		v.project(c, p, seenHash)
	}

	c.Warnings = append(c.Warnings, v.warnings...)
	return errors.Join(v.errs...)
}

type validator struct {
	errs     []error
	warnings []string
}

func (v *validator) add(format string, args ...any) {
	v.errs = append(v.errs, fmt.Errorf(format, args...))
}

func (v *validator) warn(format string, args ...any) {
	v.warnings = append(v.warnings, fmt.Sprintf(format, args...))
}

func oneOf(s string, options ...string) bool {
	for _, o := range options {
		if s == o {
			return true
		}
	}
	return false
}

func (v *validator) server(c *Config) {
	for _, cidr := range c.Server.TrustedProxies {
		if _, err := parseCIDROrIP(cidr); err != nil {
			v.add("server.trusted_proxies: %q is not a valid IP or CIDR", cidr)
		}
	}
	if (c.Server.TLSCertFile == "") != (c.Server.TLSKeyFile == "") {
		v.add("server.tls_cert_file and server.tls_key_file must be set together")
	}
	if c.Server.PreAuthRate.Limit < 0 {
		v.add("server.pre_auth_rate.limit must not be negative")
	}
	if c.Server.MaxBodyPublishable <= 0 || c.Server.MaxBodySecret <= 0 {
		v.add("server body limits must be positive")
	}
	if !oneOf(c.Logging.Level, "debug", "info", "warn", "error") {
		v.add("logging.level must be debug, info, warn or error")
	}
	if !oneOf(c.Logging.Format, "json", "text") {
		v.add("logging.format must be json or text")
	}
}

func (v *validator) admin(c *Config) {
	a := &c.Admin
	if a.UI && !a.Enabled {
		v.add("admin.ui requires admin.enabled (the UI is a client of the admin API)")
	}
	if !a.Enabled {
		return
	}
	if !argonRe.MatchString(a.PasswordHash) {
		v.add("admin.password_hash must be an argon2id hash (generate one with `smtp-handler hash-password`)")
	}
	if a.SessionSecret != "" && len(a.SessionSecret) < 32 {
		v.add("admin.session_secret must be at least 32 characters")
	}
	if a.SessionSecret == "" {
		v.warn("admin.session_secret is unset: a random secret is generated per process, so admin sessions will not work across replicas")
	}
	if a.InsecureCookies {
		v.warn("admin.insecure_cookies is on: session cookies are sent without the Secure flag")
	}
}

func (v *validator) store(c *Config) {
	switch c.Store.Driver {
	case "sqlite":
		if c.Store.DSN == "" {
			v.add("store.dsn is required")
		}
	case "postgres":
		if !strings.HasPrefix(c.Store.DSN, "postgres://") && !strings.HasPrefix(c.Store.DSN, "postgresql://") {
			v.add("store.dsn must be a postgres:// URL when driver is postgres")
		}
	default:
		v.add("store.driver must be sqlite or postgres")
	}
	if !oneOf(c.RateLimit.Backend, "auto", "memory", "redis", "store") {
		v.add("rate_limit.backend must be auto, memory, redis or store")
	}
	if c.RateLimit.Backend == "redis" && c.Redis.URL == "" {
		v.add("rate_limit.backend is redis but redis.url is empty")
	}
	if c.Store.Driver == "sqlite" && c.RateLimit.Backend == "store" {
		v.warn("rate_limit.backend=store with sqlite only shares limits on a single node")
	}
}

func (v *validator) delivery(c *Config) {
	d := &c.Delivery
	if d.Workers < 1 || d.BatchSize < 1 || d.MaxAttempts < 1 {
		v.add("delivery.workers, batch_size and max_attempts must be at least 1")
	}
	if d.BackoffMax < d.BackoffBase {
		v.add("delivery.backoff_max must be >= backoff_base")
	}
	if d.LeaseDuration < d.SendTimeout {
		v.add("delivery.lease_duration (%s) must be >= send_timeout (%s)", d.LeaseDuration.Std(), d.SendTimeout.Std())
	}
	if c.Retention.Sent < 0 || c.Retention.Failed < 0 {
		v.add("retention durations must not be negative")
	}
}

func (v *validator) project(c *Config, p *Project, seenHash map[string]string) {
	pfx := fmt.Sprintf("project %q: ", p.ID)
	fail := func(format string, args ...any) { v.add(pfx+format, args...) }

	if !identRe.MatchString(p.ID) {
		fail("id must match %s", identRe)
	}

	s := &p.SMTP
	if s.Host == "" {
		fail("smtp.host is required")
	}
	if s.Port < 1 || s.Port > 65535 {
		fail("smtp.port out of range")
	}
	if !oneOf(s.TLS, "starttls", "tls", "none") {
		fail("smtp.tls must be starttls, tls or none")
	}
	if s.TLS == "none" {
		v.warn("%ssmtp.tls=none sends mail and credentials unencrypted; use only for local development", pfx)
	}
	if !oneOf(s.AuthMethod, "auto", "plain", "login", "cram-md5", "none") {
		fail("smtp.auth_method must be auto, plain, login, cram-md5 or none")
	}
	if s.AuthMethod != "none" && s.Username != "" && s.Password == "" {
		fail("smtp.username is set but no password was provided (password, password_env or password_file)")
	}
	if s.MaxConnections < 1 {
		fail("smtp.max_connections must be at least 1")
	}
	if s.GlobalMaxConnections < 0 {
		fail("smtp.global_max_connections must not be negative")
	}

	from, err := mail.ParseAddress(p.From)
	if err != nil {
		fail("from %q is not a valid address", p.From)
	} else {
		found := false
		for _, a := range p.AllowedFrom {
			if _, err := mail.ParseAddress(a); err != nil {
				fail("allowed_from %q is not a valid address", a)
			}
			if strings.EqualFold(addrOnly(a), from.Address) {
				found = true
			}
		}
		if !found {
			p.AllowedFrom = append(p.AllowedFrom, from.Address)
		}
	}

	v.keys(p, seenHash, fail)
	v.policy(p, fail)
	v.rates(p, fail)

	if p.Captcha.Provider != "" {
		if !oneOf(p.Captcha.Provider, "turnstile", "hcaptcha") {
			fail("captcha.provider must be turnstile or hcaptcha")
		}
		if p.Captcha.Secret == "" {
			fail("captcha.provider is set but no secret was provided")
		}
		if p.Captcha.VerifyURL != "" {
			if u, err := url.Parse(p.Captcha.VerifyURL); err != nil || u.Scheme == "" || u.Host == "" {
				fail("captcha.verify_url is not a valid URL")
			}
		}
	}
	if !oneOf(p.Delivery.Semantics, "at_least_once", "at_most_once") {
		fail("delivery.semantics must be at_least_once or at_most_once")
	}
	if p.Delivery.MaxAttempts < 0 {
		fail("delivery.max_attempts must not be negative")
	}

	names := map[string]bool{}
	hasPublicTemplate := false
	for i := range p.Templates {
		t := &p.Templates[i]
		if names[t.Name] {
			fail("duplicate template %q", t.Name)
		}
		names[t.Name] = true
		v.template(p, t, fail)
		hasPublicTemplate = hasPublicTemplate || t.IsPublic()
	}
	for _, k := range p.Keys {
		if k.Type == "publishable" {
			if !hasPublicTemplate {
				v.warn("%shas a publishable key but no public template; browser requests will always be rejected", pfx)
			}
			break
		}
	}
	if len(p.Templates) == 0 {
		v.warn("%sdefines no templates; only raw sends with secret keys are possible", pfx)
	}
}

func (v *validator) keys(p *Project, seenHash map[string]string, fail func(string, ...any)) {
	if len(p.Keys) == 0 {
		fail("at least one key is required (generate one with `smtp-handler keys generate`)")
	}
	for i, k := range p.Keys {
		if !oneOf(k.Type, "publishable", "secret") {
			fail("keys[%d].type must be publishable or secret", i)
		}
		if !hashRe.MatchString(k.Hash) {
			fail("keys[%d].hash must be sha256:<64 hex chars>", i)
		} else if _, err := hex.DecodeString(strings.TrimPrefix(k.Hash, "sha256:")); err == nil {
			if other, dup := seenHash[k.Hash]; dup {
				fail("keys[%d] duplicates a key hash already used by project %q", i, other)
			}
			seenHash[k.Hash] = p.ID
		}
		if k.Type == "publishable" {
			if len(k.AllowedOrigins) == 0 {
				fail("keys[%d]: publishable keys need allowed_origins (use \"*\" to allow any origin explicitly)", i)
			}
			for _, o := range k.AllowedOrigins {
				if o == "*" {
					v.warn("project %q keys[%d]: allowed_origins \"*\" disables origin binding", p.ID, i)
					continue
				}
				if err := checkOrigin(o); err != nil {
					fail("keys[%d].allowed_origins: %v", i, err)
				}
			}
		} else if len(k.AllowedOrigins) > 0 {
			fail("keys[%d]: allowed_origins only applies to publishable keys", i)
		}
	}
}

func (v *validator) policy(p *Project, fail func(string, ...any)) {
	pol := &p.Policy
	if !oneOf(pol.RawRecipients, "any", "domain_allowlist", "none") {
		fail("policy.raw_recipients must be any, domain_allowlist or none")
	}
	if pol.RawRecipients == "domain_allowlist" && len(pol.AllowedRecipientDomains) == 0 {
		fail("policy.allowed_recipient_domains is required for domain_allowlist")
	}
	for _, d := range pol.AllowedRecipientDomains {
		if d == "" || strings.ContainsAny(d, "@ /") {
			fail("policy.allowed_recipient_domains: %q is not a bare domain", d)
		}
	}
	if pol.MaxRecipients < 1 {
		fail("policy.max_recipients must be at least 1")
	}
	if pol.Attachments.MaxCount < 0 || pol.Attachments.MaxBytes < 0 {
		fail("policy.attachments limits must not be negative")
	}
}

func (v *validator) rates(p *Project, fail func(string, ...any)) {
	check := func(name string, r *Rule) {
		if r == nil {
			return
		}
		if r.Limit < 0 || (r.Limit > 0 && r.Window <= 0) {
			fail("rate_limits.%s needs limit >= 0 and a positive window", name)
		}
	}
	for label, set := range map[string]RateSet{"publishable": p.RateLimits.Publishable, "secret": p.RateLimits.Secret} {
		check(label+".per_key", set.PerKey)
		check(label+".per_ip", set.PerIP)
		check(label+".per_recipient", set.PerRecipient)
		check(label+".daily", set.Daily)
	}
}

var fieldTypes = []string{"string", "text", "email", "number", "boolean"}

func (v *validator) template(p *Project, t *Template, fail func(string, ...any)) {
	tp := fmt.Sprintf("template %q: ", t.Name)
	bad := func(format string, args ...any) { fail(tp+format, args...) }

	if !identRe.MatchString(t.Name) {
		bad("name must match %s", identRe)
	}
	if strings.TrimSpace(t.Subject) == "" {
		bad("subject is required")
	}
	if strings.TrimSpace(t.HTML) == "" && strings.TrimSpace(t.Text) == "" {
		bad("at least one of html/text is required")
	}
	if t.From != "" {
		if a, err := mail.ParseAddress(t.From); err != nil {
			bad("from %q is not a valid address", t.From)
		} else if !containsFold(p.AllowedFrom, a.Address) {
			bad("from %q is not in the project's allowed_from", t.From)
		}
	}

	r := &t.Recipients
	switch r.Mode {
	case "fixed":
		if len(r.To) == 0 {
			bad("recipients.to is required for mode fixed")
		}
		if len(r.Aliases) > 0 {
			bad("recipients.aliases only applies to mode alias")
		}
		v.addresses(bad, "recipients.to", r.To)
	case "alias":
		if len(r.Aliases) == 0 {
			bad("recipients.aliases is required for mode alias")
		}
		for name, addrs := range r.Aliases {
			if !identRe.MatchString(name) {
				bad("alias %q must match %s", name, identRe)
			}
			if len(addrs) == 0 {
				bad("alias %q has no addresses", name)
			}
			v.addresses(bad, "alias "+name, addrs)
		}
		if len(r.To) > 0 {
			bad("recipients.to only applies to mode fixed")
		}
	case "request":
		// The caller chooses the recipients, so browsers must never reach it.
		if t.IsPublic() {
			bad("recipients.mode request lets the caller choose recipients, so it requires `public: false` (secret keys only)")
		}
		if len(r.To) > 0 || len(r.Aliases) > 0 {
			bad("recipients.to and recipients.aliases do not apply to mode request")
		}
	default:
		bad("recipients.mode %q is not supported (use fixed, alias or request)", r.Mode)
	}
	v.addresses(bad, "recipients.cc", r.CC)
	v.addresses(bad, "recipients.bcc", r.BCC)

	if t.IsPublic() && len(t.Fields) == 0 {
		bad("public templates must declare fields so browser input is bounded")
	}
	for name, f := range t.Fields {
		if !fieldKeyRe.MatchString(name) {
			bad("field name %q is invalid", name)
		}
		if !oneOf(f.Type, fieldTypes...) {
			bad("field %q: type must be one of %s", name, strings.Join(fieldTypes, ", "))
		}
		if f.Max < 0 || f.Min < 0 || (f.Max > 0 && f.Min > f.Max) {
			bad("field %q: invalid min/max", name)
		}
		if f.Pattern != "" {
			if _, err := regexp.Compile(f.Pattern); err != nil {
				bad("field %q: invalid pattern: %v", name, err)
			}
		}
		if len(f.Enum) > 0 && f.Type != "string" {
			bad("field %q: enum only applies to type string", name)
		}
	}
	if t.Honeypot != "" {
		if !fieldKeyRe.MatchString(t.Honeypot) {
			bad("honeypot %q is not a valid field name", t.Honeypot)
		}
		if _, clash := t.Fields[t.Honeypot]; clash {
			bad("honeypot %q must not also be a declared field", t.Honeypot)
		}
	}
	for _, ref := range []string{t.ReplyToField, t.ReplyToNameKey} {
		if ref == "" {
			continue
		}
		if _, ok := t.Fields[ref]; !ok && len(t.Fields) > 0 {
			bad("reply_to field %q is not a declared field", ref)
		}
	}
	if t.ReplyToField != "" && len(t.Fields) > 0 && t.Fields[t.ReplyToField].Type != "email" {
		bad("reply_to_field %q must be of type email", t.ReplyToField)
	}
}

func (v *validator) addresses(bad func(string, ...any), where string, addrs []string) {
	for _, a := range addrs {
		if _, err := mail.ParseAddress(a); err != nil {
			bad("%s: %q is not a valid address", where, a)
		}
	}
}

func addrOnly(s string) string {
	if a, err := mail.ParseAddress(s); err == nil {
		return a.Address
	}
	return s
}

func containsFold(list []string, addr string) bool {
	for _, s := range list {
		if strings.EqualFold(addrOnly(s), addr) {
			return true
		}
	}
	return false
}

// checkOrigin accepts "scheme://host[:port]" with an optional leading
// "*." wildcard on the host. Paths, queries and credentials are rejected.
func checkOrigin(o string) error {
	probe := strings.Replace(o, "://*.", "://wildcard.", 1)
	u, err := url.Parse(probe)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q is not a valid origin (expected scheme://host[:port])", o)
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("%q must not contain a path, query or credentials", o)
	}
	return nil
}
