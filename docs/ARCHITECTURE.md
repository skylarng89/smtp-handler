# Architecture

```
 Browser (pk_) / Backend (sk_)
          │  HTTPS  POST /v1/messages
          ▼
┌──────────────────────── public listener (stateless) ─────────────────────────┐
│ request-id → recover → metrics/log → CORS → per-IP limit                      │
│ → auth (key hash) → origin binding → key/IP limits → strict decode            │
│ → idempotency replay → captcha → compose (template/policy) → recipient limits │
│ → ONE transaction: idempotency record + message → 202                        │
└───────────────────────────────┬──────────────────────────────────────────────┘
                                ▼
                    Store (SQLite | PostgreSQL)
                                ▼
┌──────────────────────── dispatcher (per node) ───────────────────────────────┐
│ claim (lease + fencing token) → build MIME → pooled SMTP send                 │
│ → classify → sent | retry (backoff) | dead-letter | config pause | release    │
└──────────────────────────────────────────────────────────────────────────────┘
 operations listener: /healthz /readyz /metrics  [+ /admin/api/v1, /ui/ when enabled]
```

## Packages

| Package                        | Role                                                                                  |
| ------------------------------ | ------------------------------------------------------------------------------------- |
| `config`                       | YAML + env loading, defaults, secret resolution, strict validation                    |
| `payload`                      | strict JSON decode, address parsing/normalization, request fingerprint                |
| `render`                       | compiled templates, field-schema validation, HTML→text                                |
| `policy`, `compose`            | recipient/`From`/attachment rules; turns a request into a canonical `message.Message` |
| `message`                      | canonical model; address parsing that rejects header injection                        |
| `auth`, `ratelimit`, `captcha` | key and origin checks, session/password primitives, fixed-window limiters, siteverify |
| `store` / `store/sqlstore`     | the persistence contract and one SQL implementation for SQLite and PostgreSQL         |
| `transport`                    | `Transport` interface; pooled SMTP via go-mail; outcome classification                |
| `worker`                       | claiming, delivery, retry/backoff, circuit breaker, graceful drain                    |
| `httpapi`, `adminapi`, `ui`    | public API, operator API, embedded Vue app                                            |
| `app`                          | wiring and shutdown order                                                             |

## Message state machine

```
queued ─► sending ─► sent
            │  ▲
            ▼  │ (lease expiry / release)
      retry_scheduled ─► sending
            │
            ▼
         failed ──(admin retry)──► queued
queued / retry_scheduled / failed ──(admin cancel)──► canceled
```

Every transition is a single guarded `UPDATE … WHERE state IN (…) [AND version = ?] RETURNING`.
There is no read-then-write window between replicas, workers and operators.

## Running replicas

All coordination happens in the database in one statement. There are no in-process locks that
matter across nodes and no separate lock service.

| Hazard                                                                    | Guard                                                                                                                                                                                                                                                                     |
| ------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| A client retries or double-submits                                        | **Idempotency**: primary key `(project_id, idem_key)` plus a request fingerprint, written in the _same transaction_ as the message. Concurrent duplicates block on the unique index until the first commits, then replay its result. `202` is only returned after commit. |
| Two nodes send the same job                                               | **Atomic claim**: `UPDATE … WHERE id IN (SELECT … FOR UPDATE SKIP LOCKED LIMIT n) RETURNING` (PostgreSQL) or a single-writer transaction (SQLite) sets owner, lease expiry and a random `lease_token`.                                                                    |
| A slow worker finishes after its lease expired and another node took over | **Fencing**: every completion (`MarkSent/Retry/Failed/Release`, lease renewal) requires `lease_token = ?`. A stale worker changes nothing; the loss is logged and counted (`smtph_lease_lost_total`). Long sends renew their lease; losing it aborts the send.            |
| A node crashes mid-send                                                   | The expired lease is reclaimed by the next claim query itself (no separate janitor to race). A message that exhausts its attempts through repeated crashes is failed, not looped (poison-message protection).                                                             |
| Crash after the server's `250 OK` but before our commit                   | Documented at-least-once window. The resend keeps the same `Message-ID`/`Date`. Use `delivery.semantics: at_most_once` to dead-letter instead.                                                                                                                            |
| Connection drops after the body was sent, before the reply                | Classified `unknown`: retried under `at_least_once`, dead-lettered under `at_most_once`. Cancelling an in-flight send is also `unknown`.                                                                                                                                  |
| Two operators (or an operator and a worker) act on one message            | State machine in SQL + optimistic `version`; admin retry/cancel require `If-Match`. Bulk actions report per item.                                                                                                                                                         |
| Rate limits read-then-write                                               | Atomic increments: `INSERT … ON CONFLICT DO UPDATE SET count = count + 1 RETURNING` (database) or a Lua script (Redis). Backend failure degrades to per-node limits.                                                                                                      |
| Purge/migrations on every replica                                         | Migrations and purge take a PostgreSQL advisory lock and are idempotent and batched. `readyz` is unready until the schema version matches the binary.                                                                                                                     |
| Clock skew between replicas                                               | Leases, schedules, idempotency expiry and retention all use the **database clock** (`{now}`), never the node clock.                                                                                                                                                       |
| `N replicas × M connections` exceeds the SMTP provider's cap              | `max_connections` per node plus `global_max_connections` enforced with leased slot rows. Waiting for a slot never consumes an attempt.                                                                                                                                    |
| Two processes on one SQLite file                                          | An exclusive `flock` on `<db>.lock` refuses the second process.                                                                                                                                                                                                           |
| Rolling deploys / scale-down                                              | On `SIGTERM`: readiness off → HTTP drains → in-flight sends finish (or are aborted at `drain_timeout`) → remaining leases are released so other replicas take over immediately.                                                                                           |
| Replicas on different config versions                                     | The fully rendered message is stored at enqueue, so whichever node sends it delivers exactly what was accepted. A node that does not know the project releases the lease without consuming an attempt.                                                                    |

### Delivery outcomes

| Class       | Examples                                                        | Action                                                                                        |
| ----------- | --------------------------------------------------------------- | --------------------------------------------------------------------------------------------- |
| `transient` | 4xx, 421, network error, timeout before DATA                    | retry with exponential backoff + jitter                                                       |
| `permanent` | 5xx                                                             | dead-letter (retryable by an operator)                                                        |
| `config`    | 530/534/535/538, TLS verification failure, STARTTLS unavailable | pause the project (circuit breaker); the message is released **without** consuming an attempt |
| `unknown`   | connection lost after DATA, cancelled mid-send                  | per `delivery.semantics`                                                                      |
| `busy`      | no cluster-wide connection slot                                 | released shortly, no attempt used                                                             |

## Storage

One `Store` interface (queue, status, idempotency, counters, slots, retention) with a single SQL
implementation parameterised by a small dialect (placeholders, database clock, row locking).
Both drivers pass the same contract suite (`internal/store/storetest`). SQLite uses one
connection in WAL mode; PostgreSQL uses a pool and `SKIP LOCKED`. The queue lives in SQL (not
Redis/NATS) so the admin UI can filter, search and page it.

## Design decisions and refinements

- **Browser keys are scoped, not trusted.** A browser cannot keep a secret, so the publishable
  key is bounded by origin, template schema, fixed recipients, rate limits and captcha.
- **Template recipients** are `fixed`, `alias` or `request`; only secret keys can supply recipients.
- **Fixed-window rate limits** (identical semantics across memory, database and Redis) rather than
  token buckets; a burst of up to 2× the limit across a window boundary is possible.
- **Retry jitter** is "equal jitter" (half the exponential delay fixed, half random) so a retry
  is never immediate.
- **Concurrent duplicate requests** block briefly on the idempotency unique index and replay,
  instead of returning `409`.
- **Sessions** are stateless signed cookies so any replica can validate them; they cannot be
  revoked before expiry (default 12h).
- The admin UI is a plain Vue 3 + Vite + TypeScript SPA embedded with `go:embed`. A Go-only build
  without the UI still works (it serves an explanatory page).

## Known limits

- SMTP is not exactly-once (see above).
- SQLite is single-process by design; use PostgreSQL for replicas.
- Admin sessions cannot be revoked server-side before they expire.
- Attachments are held in memory and stored in the queue; keep `max_bytes` modest.
- Only SMTP transports exist today; the `Transport` interface is the extension point for HTTP-API providers.
