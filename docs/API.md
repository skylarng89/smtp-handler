# HTTP API

Errors are [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) `application/problem+json`:

```json
{
  "type": "https://smtp-handler.dev/problems/validation-failed",
  "title": "Bad Request",
  "status": 400,
  "detail": "data does not match the template",
  "errors": [
    { "field": "data.email", "message": "must be a valid email address" }
  ],
  "request_id": "01J…"
}
```

The last path segment of `type` is a stable machine-readable code. Every response carries
`X-Request-Id`. 5xx responses never include internal details; look up `request_id` in the logs.

## Authentication

`Authorization: Bearer <key>` or `X-API-Key: <key>`.

- Publishable keys (`pk_…`) require a matching `Origin` header (browsers send it automatically).
- Secret keys (`sk_…`) are rejected (`secret-key-in-browser`) if the request carries an `Origin`.

## `POST /v1/messages`

Queue one email. Returns `202` once the message is durably stored.

Headers: `Content-Type: application/json`; optional `Idempotency-Key` (1–128 chars of
`[A-Za-z0-9_.:-]`); optional `X-Captcha-Token` (or `captcha_token` in the body).

**Template mode** (publishable or secret keys):

| Field           | Meaning                                                                                              |
| --------------- | ---------------------------------------------------------------------------------------------------- |
| `template`      | template name                                                                                        |
| `data`          | flat object of strings, numbers, booleans validated against the template's `fields`                  |
| `to`            | recipient alias (`alias` mode) or addresses (`request` mode, secret keys only); rejected for `fixed` |
| `reply_to`      | optional single address (overrides `reply_to_field`)                                                 |
| `captcha_token` | captcha response when the project requires it                                                        |

**Raw mode** (secret keys only):

| Field             | Meaning                                                                                                     |
| ----------------- | ----------------------------------------------------------------------------------------------------------- |
| `to`, `cc`, `bcc` | a string (`"a@b.c"`, `"Name <a@b.c>"`, comma-separated), an object `{"email","name"}`, or an array of those |
| `from`            | optional; must be in the project's `allowed_from`                                                           |
| `reply_to`        | optional single address                                                                                     |
| `subject`         | required, single line                                                                                       |
| `text`, `html`    | at least one; the missing alternative is derived (HTML → text)                                              |
| `attachments`     | `[{ "filename": "a.pdf", "content": "<base64>" }]`                                                          |

Unknown fields are rejected so client bugs surface immediately.

Response `202`:

```json
{ "id": "01J…", "status": "queued" }
```

### Idempotency

| Situation                             | Result                                                           |
| ------------------------------------- | ---------------------------------------------------------------- |
| new key                               | `202`, new message                                               |
| same key, same body                   | `202` with the **original** `id` and `Idempotent-Replayed: true` |
| same key, different body              | `422 idempotency-key-reused`                                     |
| concurrent requests with the same key | exactly one message; the rest replay it                          |

Keys are scoped per project and expire after 24 hours. Replays are answered before captcha, so a
retry does not need a fresh captcha token.

### Status codes

`400 validation-failed`, `401 unauthorized`, `403` (`origin-not-allowed`, `raw-mode-requires-secret-key`,
`from-not-allowed`, `recipient-domain-not-allowed`, `captcha-required`, `captcha-failed`, …),
`404 not-found` (unknown **or non-public** template), `413 payload-too-large`,
`415 unsupported-media-type`, `422` (`template-render-failed`, `idempotency-key-reused`),
`429 rate-limited` (with `Retry-After`), `503 service-unavailable` (with `Retry-After`; retry with the same `Idempotency-Key`).

Browser-facing error responses include CORS headers so your frontend can read them.
A honeypot hit returns a normal-looking `202` and sends nothing.

## `GET /v1/messages/{id}`

Secret key only, scoped to the key's own project.

```json
{
  "id": "01J…",
  "status": "sent",
  "attempts": 1,
  "created_at": "…",
  "sent_at": "…"
}
```

`status` is one of `queued`, `sending`, `retry_scheduled`, `sent`, `failed`, `canceled`.
Failures include `last_error_class` (`transient`, `permanent`, `config`, `unknown`) and `last_error_code`.

## Operations listener

| Path              | Purpose                                                                                                                                                           |
| ----------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `GET /healthz`    | liveness (never touches dependencies)                                                                                                                             |
| `GET /readyz`     | `200` when the store is reachable and the schema matches; `503` while draining                                                                                    |
| `GET /metrics`    | Prometheus: `smtph_messages_accepted_total`, `smtph_deliveries_total`, `smtph_queue_messages`, `smtph_http_request_duration_seconds`, `smtph_lease_lost_total`, … |
| `/admin/api/v1/*` | admin API (when `admin.enabled`)                                                                                                                                  |
| `/ui/`            | web UI (when `admin.ui`)                                                                                                                                          |

## Admin API (`/admin/api/v1`)

Cookie session from `POST /login` `{username,password}` (rate limited; `HttpOnly`, `SameSite=Strict`).
Cross-origin state-changing browser requests are refused.

| Endpoint                                                       |                                                                                                                 |
| -------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------- |
| `GET /projects`, `GET /stats`                                  | overview                                                                                                        |
| `GET /messages?project=&state=failed,queued&q=&before=&limit=` | list; keyset pagination via `next_cursor`                                                                       |
| `GET /messages/{id}`                                           | detail, attempts, preview; returns `ETag` (the version)                                                         |
| `POST /messages/{id}/retry`, `/cancel`                         | require `If-Match: "<version>"`; `412` if stale, `409` if the state forbids it                                  |
| `POST /messages/bulk`                                          | `{"action":"retry\|cancel","items":[{"id","version"}]}`; per-item results, one stale item never aborts the rest |
