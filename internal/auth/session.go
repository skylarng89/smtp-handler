package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Sessions are stateless HMAC-signed tokens, so any replica can validate a
// session issued by another without shared session storage. The trade-off is
// that a session cannot be revoked before it expires; keep the TTL short.

type sessionClaims struct {
	User    string `json:"u"`
	Expires int64  `json:"e"`
}

var errBadSession = errors.New("invalid session")

// SignSession issues a token for user valid for ttl.
func SignSession(secret []byte, user string, ttl time.Duration, now time.Time) string {
	body, _ := json.Marshal(sessionClaims{User: user, Expires: now.Add(ttl).Unix()})
	payload := base64.RawURLEncoding.EncodeToString(body)
	return "v1." + payload + "." + sign(secret, payload)
}

// VerifySession returns the user if the token is authentic and unexpired.
func VerifySession(secret []byte, token string, now time.Time) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return "", errBadSession
	}
	if !hmac.Equal([]byte(sign(secret, parts[1])), []byte(parts[2])) {
		return "", errBadSession
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errBadSession
	}
	var c sessionClaims
	if err := json.Unmarshal(raw, &c); err != nil || c.User == "" {
		return "", errBadSession
	}
	if now.Unix() >= c.Expires {
		return "", errors.New("session expired")
	}
	return c.User, nil
}

func sign(secret []byte, payload string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("smtph-session|" + payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
