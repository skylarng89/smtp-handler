package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/skylarng89/smtp-handler/internal/config"
)

func testConfig(t *testing.T) (*config.Config, string, string) {
	t.Helper()
	pk, pkHash, err := GenerateKey(Publishable)
	if err != nil {
		t.Fatal(err)
	}
	sk, skHash, _ := GenerateKey(Secret)
	return &config.Config{Projects: []config.Project{
		{ID: "a", Keys: []config.Key{
			{Type: "publishable", Hash: pkHash, AllowedOrigins: []string{"https://app.example.com", "https://*.stage.example.com", "http://localhost:5173"}},
			{Type: "secret", Hash: skHash, Label: "backend"},
		}},
		{ID: "b", Keys: []config.Key{{Type: "publishable", Hash: HashKey("pk_other"), AllowedOrigins: []string{"https://other.test"}}}},
	}}, pk, sk
}

func TestAuthenticate(t *testing.T) {
	cfg, pk, sk := testConfig(t)
	a := New(cfg)

	req := func(h, v string) *http.Request {
		r, _ := http.NewRequestWithContext(context.Background(), "POST", "/", nil)
		if h != "" {
			r.Header.Set(h, v)
		}
		return r
	}
	if p, err := a.Authenticate(req("Authorization", "Bearer "+pk)); err != nil || p.ProjectID != "a" || p.Type != Publishable {
		t.Fatalf("%v %+v", err, p)
	}
	if p, err := a.Authenticate(req("X-API-Key", sk)); err != nil || !p.IsSecret() || p.Label != "backend" {
		t.Fatalf("%v %+v", err, p)
	}
	if p, _ := a.Authenticate(req("Authorization", "bearer "+pk)); p == nil {
		t.Fatal("scheme must be case-insensitive")
	}
	for name, r := range map[string]*http.Request{
		"none":         req("", ""),
		"wrong":        req("Authorization", "Bearer pk_wrong"),
		"basic scheme": req("Authorization", "Basic "+pk),
		"prefix only":  req("Authorization", "Bearer "+pk[:10]),
		"padded":       req("Authorization", "Bearer "+pk+"x"),
		"empty bearer": req("Authorization", "Bearer "),
	} {
		if _, err := a.Authenticate(r); err == nil {
			t.Errorf("%s: authenticated", name)
		}
	}
}

func TestOriginMatching(t *testing.T) {
	cfg, pk, _ := testConfig(t)
	a := New(cfg)
	r, _ := http.NewRequestWithContext(context.Background(), "POST", "/", nil)
	r.Header.Set("Authorization", "Bearer "+pk)
	p, _ := a.Authenticate(r)

	for origin, want := range map[string]bool{
		"https://app.example.com":         true,
		"https://APP.example.com":         true,
		"https://pr-1.stage.example.com":  true,
		"https://a.b.stage.example.com":   true,
		"http://localhost:5173":           true,
		"https://stage.example.com":       false, // wildcard needs a label
		"https://evilstage.example.com":   false,
		"https://app.example.com.evil.io": false,
		"http://app.example.com":          false,
		"https://app.example.com:8443":    false,
		"http://localhost:3000":           false,
		"https://other.test":              false, // belongs to a different project's key
		"null":                            false,
		"":                                false,
		"https://app.example.com/path":    false,
		"https://user@app.example.com":    false,
	} {
		if got := p.OriginAllowed(origin); got != want {
			t.Errorf("%q: got %v, want %v", origin, got, want)
		}
	}
	if !a.OriginKnown("https://other.test") || a.OriginKnown("https://nobody.test") {
		t.Fatal("OriginKnown (preflight) must consider every publishable key")
	}
}

func TestWildcardOriginOptIn(t *testing.T) {
	cfg := &config.Config{Projects: []config.Project{{ID: "a", Keys: []config.Key{
		{Type: "publishable", Hash: HashKey("k"), AllowedOrigins: []string{"*"}}}}}}
	a := New(cfg)
	r, _ := http.NewRequestWithContext(context.Background(), "POST", "/", nil)
	r.Header.Set("Authorization", "Bearer k")
	p, err := a.Authenticate(r)
	if err != nil || !p.OriginAllowed("https://anything.test") || !a.OriginKnown("https://anything.test") {
		t.Fatalf("explicit * should allow any origin: %v", err)
	}
}

func TestGenerateKeyFormat(t *testing.T) {
	k1, h1, _ := GenerateKey(Publishable)
	k2, _, _ := GenerateKey(Publishable)
	s1, _, _ := GenerateKey(Secret)
	if !strings.HasPrefix(k1, "pk_") || !strings.HasPrefix(s1, "sk_") || k1 == k2 || len(k1) < 40 {
		t.Fatalf("keys: %s %s %s", k1, k2, s1)
	}
	if h1 != HashKey(k1) || !strings.HasPrefix(h1, "sha256:") || len(h1) != len("sha256:")+64 {
		t.Fatalf("hash %s", h1)
	}
}

func TestPasswordHashing(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil || !strings.HasPrefix(h, "$argon2id$") {
		t.Fatalf("%v %s", err, h)
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Fatal("salts must differ")
	}
	if ok, err := VerifyPassword("correct horse battery", h); !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	if ok, _ := VerifyPassword("Correct horse battery", h); ok {
		t.Fatal("wrong password verified")
	}
	for _, bad := range []string{"", "plain", "$argon2id$v=19$m=65536,t=3,p=2$AAAA", "$bcrypt$x$y$z$w", "$argon2id$v=19$m=99999999,t=3,p=2$AAAA$AAAA", "$argon2id$v=19$m=65536,t=99,p=2$AAAA$AAAA"} {
		if ok, err := VerifyPassword("x", bad); ok || err == nil {
			t.Errorf("%q: ok=%v err=%v", bad, ok, err)
		}
	}
}

func TestSessions(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	now := time.Now()
	tok := SignSession(secret, "ops", time.Hour, now)

	if u, err := VerifySession(secret, tok, now.Add(time.Minute)); err != nil || u != "ops" {
		t.Fatalf("%v %v", u, err)
	}
	if _, err := VerifySession(secret, tok, now.Add(2*time.Hour)); err == nil {
		t.Fatal("expired session accepted")
	}
	if _, err := VerifySession([]byte("another secret another secret!!!"), tok, now); err == nil {
		t.Fatal("token verified with a different secret")
	}
	parts := strings.Split(tok, ".")
	forged := SignSession(secret, "admin", time.Hour, now)
	if _, err := VerifySession(secret, parts[0]+"."+strings.Split(forged, ".")[1]+"."+parts[2], now); err == nil {
		t.Fatal("payload swap accepted")
	}
	for _, bad := range []string{"", "v1", "v1.a", "v1.a.b", "v2." + parts[1] + "." + parts[2], tok + "x"} {
		if _, err := VerifySession(secret, bad, now); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
