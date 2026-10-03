# SMTP Handler

A small, fast service that sends email **on behalf of your frontend applications**, so SMTP
credentials never ship in browser code.

```
Browser ──(publishable key + template + data)──▶ SMTP Handler ──(SMTP, pooled)──▶ your mail provider
Backend ──(secret key + raw or template)───────▶      │
                                                      └─ queue · retries · rate limits · captcha · admin UI
```

- **Credentials stay server-side.** The browser holds only a _publishable_ key that can do almost nothing.
- **Safe by construction.** Browser keys can only send **server-defined templates** to
  **recipients fixed in config**, from **allowed origins**, behind rate limits and optional captcha.
  A leaked browser key can at worst email your own inbox a bounded number of times.
- **Fast.** The API validates, writes to a durable queue and answers `202` in about a millisecond.
  Workers deliver in the background over pooled, reused SMTP connections.
- **Reliable.** Retries with backoff, dead-letter queue, idempotency keys, graceful shutdown,
  and safe to run as several replicas.
- **Generic.** One binary serves one app (configured with environment variables only) or many
  apps in a company (YAML projects). Optional web UI to inspect the queue and retry failures.
- **One static binary**, Go, no CGO. SQLite by default, PostgreSQL for multi-node.

## Quick start (single app)

```bash
# 1. Create keys. Only the hashes go into configuration.
docker run --rm smtp-handler keys generate -type publishable    # use in your frontend
docker run --rm smtp-handler keys generate -type secret         # use from your backend

# 2. Configure and run.
cp .env.example .env            # fill in SMTP host/credentials, FROM, key hashes, allowed origins
docker compose -f deploy/compose/single-app.yaml up --build
```

Define what the browser may send as a template (`templates/contact-form.yaml`, see
[`templates/`](templates)). Then, from your frontend:

```js
const res = await fetch("https://mail.example.com/v1/messages", {
  method: "POST",
  headers: {
    Authorization: "Bearer pk_...", // publishable key: safe to ship
    "Content-Type": "application/json",
    "Idempotency-Key": crypto.randomUUID(), // makes retries safe
  },
  body: JSON.stringify({
    template: "contact-form",
    data: { name: "Ada", email: "ada@example.org", message: "Hello!" },
    captcha_token: turnstileToken, // if captcha is enabled
  }),
});
// 202 { "id": "01J…", "status": "queued" }
```

From your backend, with a secret key, you can also send raw content or a template to any
recipient your project policy allows:

```bash
curl -X POST https://mail.example.com/v1/messages \
  -H "Authorization: Bearer sk_..." -H "Content-Type: application/json" \
  -d '{"to":"user@customers.example","subject":"Your receipt","html":"<h1>Thanks!</h1>"}'
```

Without Docker: `make ui build && ./bin/smtp-handler serve -config configs/example.yaml`
(requires Go 1.27+; Node.js only if you want the admin UI built in).

## How requests are checked

Every request runs these guards, cheapest first. A rejection never writes to the queue.

| #   | Guard                                                                                                                 | Result                  |
| --- | --------------------------------------------------------------------------------------------------------------------- | ----------------------- |
| 1   | per-IP limit before any key is checked                                                                                | `429`                   |
| 2   | API key (SHA-256 lookup)                                                                                              | `401`                   |
| 3   | **Publishable keys**: `Origin` must match the key's allowed origins. **Secret keys**: must _not_ come from a browser. | `403`                   |
| 4   | per-key and per-IP limits                                                                                             | `429` + `Retry-After`   |
| 5   | strict JSON decode (unknown fields, nesting, size limits)                                                             | `400`/`413`             |
| 6   | idempotency replay (answered _before_ captcha, since tokens are single-use)                                           | `202` replay            |
| 7   | captcha (browser keys only; fails closed if the provider is down)                                                     | `403`/`503`             |
| 8   | template/schema validation, recipient and `From` policy, attachment checks                                            | `400`/`403`/`404`/`422` |
| 9   | per-recipient and daily limits                                                                                        | `429`                   |
| 10  | transactional enqueue (message + idempotency record together)                                                         | `202`                   |

## Concepts

- **Project**: one application's settings: SMTP account, keys, templates, policy, limits.
  Env-only deployments get a single project named `default`.
- **Publishable key (`pk_…`)**: for browsers. Template mode only, to configured recipients,
  from allowed origins. Not a secret, so it is bound to origins, rate limits and captcha instead.
- **Secret key (`sk_…`)**: for your backend. Raw content or any template, within the project's
  recipient policy. Rejected if used from a browser.
- **Template**: subject/body with `{{.field}}` placeholders (HTML is auto-escaped), a field
  schema that bounds what clients may send, and a recipient rule:
  `fixed` (config recipients), `alias` (client picks a named alias such as `sales`), or
  `request` (the **caller** supplies recipients, so secret keys only).

See [docs/CONFIGURATION.md](docs/CONFIGURATION.md) for every setting, [docs/API.md](docs/API.md)
for the HTTP API, [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for how it works (including how
replicas stay correct) and [docs/SECURITY.md](docs/SECURITY.md) for the threat model.

## Deployment shapes

|             | Single app                                          | Shared company service                                              |
| ----------- | --------------------------------------------------- | ------------------------------------------------------------------- |
| Config      | env vars only                                       | YAML with N projects                                                |
| Store       | SQLite file on a volume                             | PostgreSQL                                                          |
| Replicas    | 1 (a lock file refuses a second)                    | any number                                                          |
| Rate limits | in memory                                           | shared in PostgreSQL (or Redis)                                     |
| Compose     | [`single-app.yaml`](deploy/compose/single-app.yaml) | [`multi-app-postgres.yaml`](deploy/compose/multi-app-postgres.yaml) |

Put a TLS-terminating reverse proxy in front of the public port (or set `tls_cert_file`/`tls_key_file`)
and keep the **operations port** (health, metrics, admin) on an internal network.

## Delivery guarantees

Email cannot be exactly-once: if a worker dies between the server's `250 OK` and our database
write, we must choose between resending and possibly losing the message. SMTP Handler delivers
**at least once** by default and keeps that window tiny; resends carry the same `Message-ID`
so mail systems can de-duplicate. Projects can opt into `at_most_once` instead.

- Temporary failures (4xx, network) retry with exponential backoff and jitter.
- Permanent failures (5xx) go to the dead-letter queue (visible and retryable in the UI).
- Misconfiguration (bad credentials, TLS problems) pauses the project and **does not burn attempts**.
- `Idempotency-Key` makes client retries safe, including under concurrency and across replicas.

## Admin UI (optional)

Off by default. Enable with `SMTPH_ADMIN_ENABLED=true SMTPH_UI_ENABLED=true` plus
`SMTPH_ADMIN_PASSWORD_HASH` (from `smtp-handler hash-password`). Served on the **operations**
listener at `/ui/`. Browse and search the queue, inspect attempts and a safe sandboxed preview,
and retry or cancel one or many messages. Retry/cancel use optimistic versioning, so a stale
view can never overwrite newer state. The admin API (`/admin/api/v1`) works without the UI.

## Operations

- `GET /healthz` (liveness), `GET /readyz` (store, schema; turns unready as soon as shutdown begins),
  `GET /metrics` (Prometheus) on the operations port.
- Structured JSON logs. Recipient addresses are not logged.
- `smtp-handler config validate` checks a configuration (including templates) without starting.
- `SIGTERM`: stop reporting ready → finish in-flight HTTP → finish or release in-flight sends → exit.

## Development

```bash
make test-race      # all Go tests with the race detector
make fuzz           # fuzz the payload and address parsers
make ui             # build the admin UI into internal/ui/dist
cd web && npm test  # UI tests
```

The PostgreSQL contract tests run when `SMTPH_TEST_POSTGRES_DSN` is set (CI provides a service);
without it only SQLite is exercised.

## License

MIT, see [LICENSE](LICENSE).
