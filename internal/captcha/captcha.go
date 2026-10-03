// Package captcha verifies bot-protection tokens (Cloudflare Turnstile,
// hCaptcha) before browser requests are accepted.
package captcha

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/skylarng89/smtp-handler/internal/apperr"
	"github.com/skylarng89/smtp-handler/internal/config"
)

// Verifier checks a client-supplied token.
type Verifier interface {
	Verify(ctx context.Context, token, remoteIP string) error
}

var defaultURLs = map[string]string{
	"turnstile": "https://challenges.cloudflare.com/turnstile/v0/siteverify",
	"hcaptcha":  "https://api.hcaptcha.com/siteverify",
}

const maxResponseBytes = 64 << 10

// Siteverify implements the shared "siteverify" protocol used by Turnstile
// and hCaptcha: POST secret+response(+remoteip), receive {"success": bool}.
type Siteverify struct {
	url    string
	secret string
	client *http.Client
}

// New builds a verifier for the project, or nil when captcha is not enabled.
func New(c config.Captcha) Verifier {
	if c.Provider == "" {
		return nil
	}
	endpoint := c.VerifyURL
	if endpoint == "" {
		endpoint = defaultURLs[c.Provider]
	}
	return &Siteverify{
		url: endpoint, secret: c.Secret,
		client: &http.Client{Timeout: c.Timeout.Std()},
	}
}

func (s *Siteverify) Verify(ctx context.Context, token, remoteIP string) error {
	if strings.TrimSpace(token) == "" {
		return apperr.Forbidden("captcha-required", "a captcha token is required")
	}
	form := url.Values{"secret": {s.secret}, "response": {token}}
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, strings.NewReader(form.Encode()))
	if err != nil {
		return apperr.Internal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.client.Do(req)
	if err != nil {
		// Fail closed: an unreachable provider must not become a bypass.
		return apperr.Unavailable("captcha verification is temporarily unavailable", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 500 {
		return apperr.Unavailable("captcha verification is temporarily unavailable",
			fmt.Errorf("provider status %d", resp.StatusCode))
	}

	var out struct {
		Success bool     `json:"success"`
		Errors  []string `json:"error-codes"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&out); err != nil {
		return apperr.Unavailable("captcha verification is temporarily unavailable", err)
	}
	if !out.Success {
		return apperr.Forbidden("captcha-failed", "captcha verification failed")
	}
	return nil
}
