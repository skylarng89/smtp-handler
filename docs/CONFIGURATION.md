# Configuration

Configuration comes from, in increasing priority: built-in defaults → the YAML file
(`-config FILE` or `SMTPH_CONFIG`) → `SMTPH_*` environment variables. The file is parsed
**strictly**: a misspelled key is an error, not silently ignored. Validate with
`smtp-handler config validate -config FILE`; every problem is reported at once.

Secrets are never expanded from the file. Use `*_env` / `*_file` references (e.g.
`password_env`, `password_file`) or the `SMTPH_*_FILE` variants, which suit Docker/Kubernetes
secrets. API keys are stored only as `sha256:<hex>` hashes.

Durations accept Go syntax plus days: `30s`, `15m`, `720h`, `30d`.

## Single-app mode (environment only)

If `SMTPH_SMTP_HOST` is set, a project named `default` is built from these variables
(and appended to any YAML projects):

| Variable                                                   | Meaning                                                | Default                        |
| ---------------------------------------------------------- | ------------------------------------------------------ | ------------------------------ |
| `SMTPH_SMTP_HOST` / `_PORT`                                | SMTP server                                            | port by TLS mode               |
| `SMTPH_SMTP_USERNAME`, `SMTPH_SMTP_PASSWORD` (`_FILE`)     | credentials                                            | none                           |
| `SMTPH_SMTP_TLS`                                           | `starttls` (587), `tls` (465), `none` (local dev only) | `starttls`                     |
| `SMTPH_SMTP_AUTH`                                          | `auto`, `plain`, `login`, `cram-md5`, `none`           | `auto`                         |
| `SMTPH_SMTP_MAX_CONNECTIONS`                               | pooled connections per node                            | `4`                            |
| `SMTPH_SMTP_GLOBAL_MAX_CONNECTIONS`                        | cap across **all** replicas (PostgreSQL)               | unlimited                      |
| `SMTPH_FROM`                                               | default sender, e.g. `App <no-reply@example.com>`      | required                       |
| `SMTPH_PUBLISHABLE_KEY_HASHES`                             | comma-separated `sha256:…`                             |                                |
| `SMTPH_ALLOWED_ORIGINS`                                    | comma-separated origins for those keys                 | required with publishable keys |
| `SMTPH_SECRET_KEY_HASHES`                                  | comma-separated `sha256:…`                             |                                |
| `SMTPH_TEMPLATES_DIR`                                      | directory of `*.yaml` templates                        |                                |
| `SMTPH_RECIPIENT_DOMAINS`                                  | limits raw recipients to these domains                 | any                            |
| `SMTPH_RAW_RECIPIENTS`                                     | `any`, `domain_allowlist`, `none`                      | `any`                          |
| `SMTPH_CAPTCHA_PROVIDER`, `SMTPH_CAPTCHA_SECRET` (`_FILE`) | `turnstile` or `hcaptcha`                              | off                            |
| `SMTPH_DELIVERY_SEMANTICS`                                 | `at_least_once` / `at_most_once`                       | `at_least_once`                |
| `SMTPH_DEDUPE_WINDOW`                                      | collapse identical keyless requests                    | off                            |

## Global settings

| Variable                                   | YAML                                                              | Default                  |
| ------------------------------------------ | ----------------------------------------------------------------- | ------------------------ |
| `SMTPH_ADDR`                               | `server.addr`                                                     | `:8080`                  |
| `SMTPH_ADMIN_ADDR`                         | `server.admin_addr` (health, metrics, admin)                      | `127.0.0.1:9090`         |
| `SMTPH_TRUSTED_PROXIES`                    | `server.trusted_proxies` (CIDRs allowed to set `X-Forwarded-For`) | none                     |
| `SMTPH_SHUTDOWN_DELAY`                     | `server.shutdown_delay` (stay up, report unready)                 | `0s`                     |
| `SMTPH_TLS_CERT_FILE` / `_KEY_FILE`        | `server.tls_cert_file` / `tls_key_file`                           | terminate TLS in a proxy |
|                                            | `server.pre_auth_rate`                                            | 600 / minute / IP        |
|                                            | `server.max_body_publishable` / `max_body_secret`                 | 64 KiB / 25 MiB          |
| `SMTPH_STORE_DRIVER`                       | `store.driver` (`sqlite`, `postgres`)                             | `sqlite`                 |
| `SMTPH_STORE_DSN` (`_FILE`)                | `store.dsn`                                                       | `data/smtp-handler.db`   |
| `SMTPH_AUTO_MIGRATE`                       | `store.auto_migrate`                                              | `true`                   |
| `SMTPH_REDIS_URL` (`_FILE`)                | `redis.url`                                                       | none                     |
| `SMTPH_RATELIMIT_BACKEND`                  | `rate_limit.backend` (`auto`, `memory`, `store`, `redis`)         | `auto`                   |
| `SMTPH_WORKERS`                            | `delivery.workers`                                                | `8`                      |
| `SMTPH_MAX_ATTEMPTS`                       | `delivery.max_attempts`                                           | `8`                      |
| `SMTPH_MAX_AGE`                            | `delivery.max_age`                                                | `24h`                    |
|                                            | `delivery.backoff_base` / `backoff_max`                           | `30s` / `1h`             |
|                                            | `delivery.drain_timeout`                                          | `20s`                    |
| `SMTPH_RETENTION_SENT` / `_FAILED`         | `retention.sent` / `failed`                                       | `30d` / `30d`            |
| `SMTPH_DROP_BODY_ON_SENT`                  | `retention.drop_body_on_sent`                                     | `false`                  |
| `SMTPH_LOG_LEVEL` / `_FORMAT`              | `logging.level` / `format`                                        | `info` / `json`          |
| `SMTPH_ADMIN_ENABLED` / `SMTPH_UI_ENABLED` | `admin.enabled` / `admin.ui`                                      | `false` / `false`        |
| `SMTPH_ADMIN_USERNAME`                     | `admin.username`                                                  | `admin`                  |
| `SMTPH_ADMIN_PASSWORD_HASH` (`_FILE`)      | `admin.password_hash`                                             | required when enabled    |
| `SMTPH_ADMIN_SESSION_SECRET` (`_FILE`)     | `admin.session_secret` (≥ 32 chars)                               | random per process       |
| `SMTPH_ADMIN_INSECURE_COOKIES`             | `admin.insecure_cookies`                                          | `false`                  |

The rate-limit backend `auto` resolves to the database when the store is PostgreSQL, else Redis
when `redis.url` is set, else per-node memory. If a shared backend fails at runtime the service
degrades to per-node limits (and logs it) rather than failing open or blocking all mail.

**Retention** is configurable per project (`projects[].retention`) because policies differ.
`drop_body_on_sent: true` removes the message body once delivered (the row and its delivery
history stay until the retention period ends). Messages whose body was dropped cannot be retried.

## Projects (YAML)

See [`configs/example.yaml`](../configs/example.yaml), which is validated by the test suite.

### Keys

```yaml
keys:
  - type: publishable # browser-safe; needs allowed_origins
    hash: "sha256:…"
    allowed_origins:
      ["https://app.example.com", "https://*.preview.example.com"]
  - type: secret # server-side only
    hash: "sha256:…"
```

Origins are `scheme://host[:port]`, with an optional leading `*.` wildcard (which requires at
least one extra label: `https://*.example.com` does **not** match `https://example.com`).
`"*"` allows any origin and is reported as a warning.

### Templates

```yaml
name: contact-form # defaults to the file name
subject: "New message from {{.name}}"
html_file: contact-form.html # or inline `html:`
text_file: contact-form.txt # or inline `text:`; derived from HTML if omitted
recipients: { mode: fixed, to: ["support@example.com"] } # cc/bcc also supported
fields:
  name: { type: string, required: true, max: 80 }
  email: { type: email, required: true }
  message: { type: text, required: true, min: 5, max: 4000 }
reply_to_field: email
honeypot: website
```

- **Fields** types: `string` (single line), `text` (multi-line), `email`, `number`, `boolean`.
  Options: `required`, `min`, `max` (length, or value for numbers), `pattern` (RE2 regexp, full
  match), `enum`. Unknown fields, nested values and control characters are rejected.
- **public**: browsers may use only public templates. Defaults to _true when `fields` are
  declared_, so a template exposed to browsers always has a bounded schema. Set `public: false`
  for backend-only templates.
- **Recipient modes**: `fixed`, `alias` (client sends `"to": "sales"`; `aliases:` maps names to
  addresses) and `request` (caller supplies `to`; **requires `public: false`**; the project's
  `policy.raw_recipients` still applies).
- `html` uses `html/template` (auto-escaping). A template that renders an empty subject, or
  references a missing key, fails with a `422`, never a crash.

### Policy and limits

`policy.raw_recipients` (`any`, `domain_allowlist`, `none`) governs recipients chosen by secret
keys. `policy.attachments` limits count, size and extensions; content is sniffed and must match
the extension. `rate_limits.publishable` / `.secret` each take `per_key`, `per_ip`,
`per_recipient`, `daily`, as `{limit, window}` (`limit: 0` disables a rule). Windows are fixed windows.
