// Package auth authenticates API keys and binds publishable keys to origins.
//
// Keys are random 256-bit values, so a plain SHA-256 is a sufficient (and
// fast) verifier: the configuration only ever stores hashes, and lookup is by
// hash, which leaks nothing useful through timing.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"

	"github.com/skylarng89/smtp-handler/internal/apperr"
	"github.com/skylarng89/smtp-handler/internal/config"
)

type KeyType string

const (
	Publishable KeyType = "publishable"
	Secret      KeyType = "secret"
)

const (
	prefixPublishable = "pk_"
	prefixSecret      = "sk_"
)

// Principal is an authenticated key.
type Principal struct {
	ProjectID string
	Type      KeyType
	KeyID     string // first hash bytes; safe to log and store
	Label     string

	anyOrigin bool
	origins   []originPattern
}

func (p *Principal) IsSecret() bool { return p.Type == Secret }

// OriginAllowed reports whether a browser Origin may use this key.
func (p *Principal) OriginAllowed(origin string) bool {
	if p.anyOrigin {
		return true
	}
	return matchAny(p.origins, origin)
}

// Authenticator resolves request credentials to principals. It is immutable
// after construction.
type Authenticator struct {
	byHash     map[[32]byte]*Principal
	allOrigins []originPattern // union across publishable keys, for CORS preflight
	anyOrigin  bool
}

func New(cfg *config.Config) *Authenticator {
	a := &Authenticator{byHash: map[[32]byte]*Principal{}}
	for i := range cfg.Projects {
		p := &cfg.Projects[i]
		for _, k := range p.Keys {
			raw, err := hex.DecodeString(strings.TrimPrefix(k.Hash, "sha256:"))
			if err != nil || len(raw) != sha256.Size {
				continue // validated at config load
			}
			pr := &Principal{
				ProjectID: p.ID, Type: KeyType(k.Type), Label: k.Label,
				KeyID: hex.EncodeToString(raw[:4]),
			}
			if pr.Type == Publishable {
				for _, o := range k.AllowedOrigins {
					if o == "*" {
						pr.anyOrigin, a.anyOrigin = true, true
						continue
					}
					op := parseOrigin(o)
					pr.origins = append(pr.origins, op)
					a.allOrigins = append(a.allOrigins, op)
				}
			}
			a.byHash[[32]byte(raw)] = pr
		}
	}
	return a
}

// OriginKnown reports whether any publishable key accepts the origin. It
// answers CORS preflights, which carry no credentials.
func (a *Authenticator) OriginKnown(origin string) bool {
	return a.anyOrigin || matchAny(a.allOrigins, origin)
}

// Authenticate extracts the key from "Authorization: Bearer" or "X-API-Key".
func (a *Authenticator) Authenticate(r *http.Request) (*Principal, error) {
	token := ""
	if h := r.Header.Get("Authorization"); h != "" {
		scheme, rest, _ := strings.Cut(h, " ")
		if strings.EqualFold(scheme, "Bearer") {
			token = strings.TrimSpace(rest)
		}
	}
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("X-API-Key"))
	}
	if token == "" {
		return nil, unauthorized("missing API key")
	}
	if p, ok := a.byHash[sha256.Sum256([]byte(token))]; ok {
		return p, nil
	}
	return nil, unauthorized("invalid API key")
}

func unauthorized(detail string) *apperr.Error { return apperr.Unauthorized(detail) }

// HashKey returns the config representation of a key.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// GenerateKey creates a new random key of the given type and its hash.
func GenerateKey(t KeyType) (key, hash string, err error) {
	var b [32]byte
	if _, err = rand.Read(b[:]); err != nil {
		return "", "", err
	}
	prefix := prefixSecret
	if t == Publishable {
		prefix = prefixPublishable
	}
	key = prefix + base64.RawURLEncoding.EncodeToString(b[:])
	return key, HashKey(key), nil
}

// originPattern matches scheme://host[:port], optionally with a leading
// "*." wildcard on the host (which requires at least one extra label).
type originPattern struct {
	scheme, host, port string
	wildcard           bool
}

func parseOrigin(s string) originPattern {
	wildcard := strings.Contains(s, "://*.")
	u, err := url.Parse(strings.Replace(s, "://*.", "://", 1))
	if err != nil {
		return originPattern{}
	}
	return originPattern{scheme: u.Scheme, host: strings.ToLower(u.Hostname()), port: u.Port(), wildcard: wildcard}
}

func matchAny(patterns []originPattern, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Path != "" || u.User != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, p := range patterns {
		if p.scheme != u.Scheme || p.port != u.Port() {
			continue
		}
		if p.wildcard {
			// "*.example.com" needs at least one extra label; the apex is not implied.
			if strings.HasSuffix(host, "."+p.host) {
				return true
			}
			continue
		}
		if host == p.host {
			return true
		}
	}
	return false
}
