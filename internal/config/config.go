// Package config loads, defaults and validates SMTP Handler configuration
// from an optional YAML file plus SMTPH_* environment variables.
package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that also accepts a "d" (days) suffix.
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// ParseDuration extends time.ParseDuration with whole-day suffixes ("30d").
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return v, nil
}

// Rule is a fixed-window rate limit: Limit events per Window. Limit 0
// disables the rule.
type Rule struct {
	Limit  int      `yaml:"limit"`
	Window Duration `yaml:"window"`
}

func (r *Rule) Enabled() bool { return r != nil && r.Limit > 0 && r.Window > 0 }

type Config struct {
	Server    Server    `yaml:"server"`
	Admin     Admin     `yaml:"admin"`
	Store     Store     `yaml:"store"`
	Redis     Redis     `yaml:"redis"`
	RateLimit RateLimit `yaml:"rate_limit"`
	Delivery  Delivery  `yaml:"delivery"`
	Retention Retention `yaml:"retention"`
	Logging   Logging   `yaml:"logging"`
	Projects  []Project `yaml:"projects"`

	// Warnings are non-fatal findings from validation, surfaced at startup.
	Warnings []string `yaml:"-"`
}

type Server struct {
	Addr              string   `yaml:"addr"`
	AdminAddr         string   `yaml:"admin_addr"`
	ReadHeaderTimeout Duration `yaml:"read_header_timeout"`
	ReadTimeout       Duration `yaml:"read_timeout"`
	WriteTimeout      Duration `yaml:"write_timeout"`
	IdleTimeout       Duration `yaml:"idle_timeout"`
	ShutdownTimeout   Duration `yaml:"shutdown_timeout"`
	// ShutdownDelay keeps serving (but reports not-ready) for this long after
	// SIGTERM so load balancers can stop routing here before connections drop.
	ShutdownDelay  Duration `yaml:"shutdown_delay"`
	TLSCertFile    string   `yaml:"tls_cert_file"`
	TLSKeyFile     string   `yaml:"tls_key_file"`
	TrustedProxies []string `yaml:"trusted_proxies"`
	// PreAuthRate throttles every client IP before any key is checked, so it
	// also bounds key-guessing attempts.
	PreAuthRate Rule `yaml:"pre_auth_rate"`
	// Body limits are applied per key type after authentication.
	MaxBodyPublishable int64 `yaml:"max_body_publishable"`
	MaxBodySecret      int64 `yaml:"max_body_secret"`
}

type Admin struct {
	Enabled       bool     `yaml:"enabled"`
	UI            bool     `yaml:"ui"`
	Username      string   `yaml:"username"`
	PasswordHash  string   `yaml:"password_hash"`
	SessionSecret string   `yaml:"session_secret"`
	SessionTTL    Duration `yaml:"session_ttl"`
	// InsecureCookies drops the Secure flag from the session cookie. Use only
	// for plain-HTTP development outside localhost.
	InsecureCookies bool `yaml:"insecure_cookies"`
}

type Store struct {
	Driver       string `yaml:"driver"` // sqlite | postgres
	DSN          string `yaml:"dsn"`
	MaxOpenConns int    `yaml:"max_open_conns"`
	// AutoMigrate applies pending migrations at startup (default true). It is
	// safe with replicas: under PostgreSQL an advisory lock serializes them.
	AutoMigrate *bool `yaml:"auto_migrate"`
}

type Redis struct {
	URL string `yaml:"url"`
}

type RateLimit struct {
	Backend string `yaml:"backend"` // auto | memory | redis | store
}

type Delivery struct {
	Workers         int      `yaml:"workers"`
	BatchSize       int      `yaml:"batch_size"`
	PollInterval    Duration `yaml:"poll_interval"`
	LeaseDuration   Duration `yaml:"lease_duration"`
	SendTimeout     Duration `yaml:"send_timeout"`
	MaxAttempts     int      `yaml:"max_attempts"`
	MaxAge          Duration `yaml:"max_age"`
	BackoffBase     Duration `yaml:"backoff_base"`
	BackoffMax      Duration `yaml:"backoff_max"`
	BreakerCooldown Duration `yaml:"breaker_cooldown"`
	DrainTimeout    Duration `yaml:"drain_timeout"`
}

type Retention struct {
	Sent           Duration `yaml:"sent"`
	Failed         Duration `yaml:"failed"`
	DropBodyOnSent bool     `yaml:"drop_body_on_sent"`
	PurgeInterval  Duration `yaml:"purge_interval"`
}

type Logging struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"` // json | text
}

type Project struct {
	ID           string          `yaml:"id"`
	Name         string          `yaml:"name"`
	SMTP         SMTP            `yaml:"smtp"`
	From         string          `yaml:"from"`
	AllowedFrom  []string        `yaml:"allowed_from"`
	Keys         []Key           `yaml:"keys"`
	TemplatesDir string          `yaml:"templates_dir"`
	Templates    []Template      `yaml:"templates"`
	Policy       Policy          `yaml:"policy"`
	RateLimits   ProjectRates    `yaml:"rate_limits"`
	Captcha      Captcha         `yaml:"captcha"`
	Delivery     ProjectDelivery `yaml:"delivery"`
	// DedupeWindow, when set, suppresses identical requests (same payload
	// fingerprint) that carry no Idempotency-Key within the window.
	DedupeWindow Duration         `yaml:"dedupe_window"`
	Retention    ProjectRetention `yaml:"retention"`
}

type SMTP struct {
	Host                 string   `yaml:"host"`
	Port                 int      `yaml:"port"`
	Username             string   `yaml:"username"`
	Password             string   `yaml:"password"`
	PasswordEnv          string   `yaml:"password_env"`
	PasswordFile         string   `yaml:"password_file"`
	TLS                  string   `yaml:"tls"`         // starttls | tls | none
	AuthMethod           string   `yaml:"auth_method"` // auto | plain | login | cram-md5 | none
	HELO                 string   `yaml:"helo"`
	Timeout              Duration `yaml:"timeout"`
	MaxConnections       int      `yaml:"max_connections"`
	GlobalMaxConnections int      `yaml:"global_max_connections"`
	IdleTimeout          Duration `yaml:"idle_timeout"`
	InsecureSkipVerify   bool     `yaml:"insecure_skip_verify"`
}

type Key struct {
	Type           string   `yaml:"type"` // publishable | secret
	Hash           string   `yaml:"hash"` // sha256:<hex>
	Label          string   `yaml:"label"`
	AllowedOrigins []string `yaml:"allowed_origins"`
}

type Template struct {
	Name           string           `yaml:"name"`
	Subject        string           `yaml:"subject"`
	HTML           string           `yaml:"html"`
	HTMLFile       string           `yaml:"html_file"`
	Text           string           `yaml:"text"`
	TextFile       string           `yaml:"text_file"`
	From           string           `yaml:"from"`
	Recipients     Recipients       `yaml:"recipients"`
	Fields         map[string]Field `yaml:"fields"`
	Honeypot       string           `yaml:"honeypot"`
	Public         *bool            `yaml:"public"`
	ReplyToField   string           `yaml:"reply_to_field"`
	ReplyToNameKey string           `yaml:"reply_to_name_field"`
}

// IsPublic reports whether publishable keys may use the template.
func (t *Template) IsPublic() bool {
	if t.Public != nil {
		return *t.Public
	}
	return len(t.Fields) > 0
}

type Recipients struct {
	Mode    string              `yaml:"mode"` // fixed | alias | request
	To      []string            `yaml:"to"`
	CC      []string            `yaml:"cc"`
	BCC     []string            `yaml:"bcc"`
	Aliases map[string][]string `yaml:"aliases"`
}

type Field struct {
	Type     string   `yaml:"type"` // string | text | email | number | boolean
	Required bool     `yaml:"required"`
	Min      int      `yaml:"min"`
	Max      int      `yaml:"max"`
	Pattern  string   `yaml:"pattern"`
	Enum     []string `yaml:"enum"`
}

type Policy struct {
	RawRecipients           string      `yaml:"raw_recipients"` // any | domain_allowlist | none
	AllowedRecipientDomains []string    `yaml:"allowed_recipient_domains"`
	MaxRecipients           int         `yaml:"max_recipients"`
	Attachments             Attachments `yaml:"attachments"`
}

type Attachments struct {
	MaxCount          int      `yaml:"max_count"`
	MaxBytes          int64    `yaml:"max_bytes"`
	AllowedExtensions []string `yaml:"allowed_extensions"`
}

type RateSet struct {
	PerKey       *Rule `yaml:"per_key"`
	PerIP        *Rule `yaml:"per_ip"`
	PerRecipient *Rule `yaml:"per_recipient"`
	Daily        *Rule `yaml:"daily"`
}

type ProjectRates struct {
	Publishable RateSet `yaml:"publishable"`
	Secret      RateSet `yaml:"secret"`
}

type Captcha struct {
	Provider     string   `yaml:"provider"` // turnstile | hcaptcha | ""
	Secret       string   `yaml:"secret"`
	SecretEnv    string   `yaml:"secret_env"`
	SecretFile   string   `yaml:"secret_file"`
	VerifyURL    string   `yaml:"verify_url"`
	Timeout      Duration `yaml:"timeout"`
	AllowMissing bool     `yaml:"allow_missing"`
}

type ProjectDelivery struct {
	// Semantics governs an unknown outcome (connection lost after DATA):
	// at_least_once retries (may duplicate), at_most_once dead-letters it.
	Semantics   string `yaml:"semantics"`
	MaxAttempts int    `yaml:"max_attempts"`
}

type ProjectRetention struct {
	Sent           *Duration `yaml:"sent"`
	Failed         *Duration `yaml:"failed"`
	DropBodyOnSent *bool     `yaml:"drop_body_on_sent"`
}

// Project helpers resolved from defaults.

func (p *Project) SentRetention(def Retention) time.Duration {
	if p.Retention.Sent != nil {
		return p.Retention.Sent.Std()
	}
	return def.Sent.Std()
}

func (p *Project) FailedRetention(def Retention) time.Duration {
	if p.Retention.Failed != nil {
		return p.Retention.Failed.Std()
	}
	return def.Failed.Std()
}

func (p *Project) DropBody(def Retention) bool {
	if p.Retention.DropBodyOnSent != nil {
		return *p.Retention.DropBodyOnSent
	}
	return def.DropBodyOnSent
}

// Project returns the project with the given ID.
func (c *Config) Project(id string) (*Project, bool) {
	for i := range c.Projects {
		if c.Projects[i].ID == id {
			return &c.Projects[i], true
		}
	}
	return nil, false
}
