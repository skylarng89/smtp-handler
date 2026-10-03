# Security

## Threat model

| Actor                                                           | Can                                                                                 | Cannot                                                                                                    |
| --------------------------------------------------------------- | ----------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| Anyone on the internet                                          | call the public API                                                                 | do anything without a key (per-IP limit applies first)                                                    |
| Holder of a **publishable** key (it is public in your frontend) | send _public templates_ with schema-valid data, from allowed origins, within limits | choose recipients, send raw content, attach files, read status, or exceed the per-key/IP/recipient limits |
| Holder of a **secret** key                                      | send raw content or templates within the project's recipient policy                 | use it from a browser (rejected), act on other projects                                                   |
| Admin                                                           | view and retry/cancel the queue                                                     | read credentials (they are never returned)                                                                |

A leaked publishable key's worst case is a bounded number of messages to the recipients your
templates already define, which is why captcha and per-recipient limits are recommended for
public forms. **Origin checks are not authentication**: a non-browser client can send any
`Origin`. They stop other _websites_ from using your key from visitors' browsers.

## Controls

- **No credentials in the browser**; SMTP secrets are referenced by env/file, never logged or returned.
- **Header injection**: CR/LF/NUL are rejected in every address, name, subject and single-line
  field; rendered subjects are sanitized; fuzz tests assert accepted addresses stay single-line.
- **Template safety**: `html/template` auto-escaping, schema validation (types, lengths, enums, RE2 patterns), flat data only.
- **Strict decoding**, request size limits per key type, content-type enforcement.
- **Keys**: 256-bit random, only SHA-256 hashes stored, lookup by hash.
- **Rate limits**: per IP (pre-auth), key, IP, recipient and daily; client IP is taken from
  `X-Forwarded-For` only from `trusted_proxies`.
- **Captcha** fails closed; replays do not consume tokens.
- **Honeypot** fields silently drop bots.
- **Attachments**: extension allowlist, sniffed content must match the extension, size/count limits, filenames sanitized.
- **SMTP**: TLS 1.2+ with certificate verification; `starttls` mode is _mandatory_ (no downgrade);
  `insecure_skip_verify` and `tls: none` need explicit opt-in and produce warnings.
- **Admin**: argon2id password, signed `HttpOnly` `SameSite=Strict` `Secure` cookies,
  Go's `CrossOriginProtection` for state-changing requests, login throttling, constant-time-ish
  failure path, optimistic versioning, audit log lines. Email previews render in a fully
  sandboxed iframe (no scripts, no same-origin) that inherits a CSP blocking remote content,
  so previewing a stored email cannot run code or fire tracking pixels.
- **Operations**: separate listener (bind it to an internal interface); logs redact recipient addresses;
  distroless non-root image; read-only filesystem and dropped capabilities in the compose examples.

## Hardening checklist

1. Terminate TLS in front of the public port (or set `tls_cert_file`/`tls_key_file`).
2. Bind `admin_addr` to localhost or an internal network; never expose it publicly.
3. Set `trusted_proxies` to your proxy's address range, or per-IP limits can be spoofed.
4. Give every public template a tight `fields` schema, a `honeypot`, and enable captcha.
5. Restrict `allowed_origins` to your real origins (avoid `"*"`).
6. Use `policy.raw_recipients: domain_allowlist` unless your backend must email arbitrary users.
7. Configure SPF, DKIM and DMARC for your sending domain with your mail provider.
8. Run behind PostgreSQL if you need replicas; set a shared `admin.session_secret`.
9. Rotate keys by adding a new hash, switching clients, then removing the old hash.

## Reporting a vulnerability

Please report suspected vulnerabilities privately to the maintainer (GitHub security advisories
on the repository) rather than opening a public issue.
