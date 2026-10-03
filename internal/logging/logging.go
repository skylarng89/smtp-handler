// Package logging configures structured logging and redacts personal data.
package logging

import (
	"io"
	"log/slog"
	"regexp"
	"strings"

	"github.com/skylarng89/smtp-handler/internal/config"
)

// New builds the process logger.
func New(w io.Writer, c config.Logging) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(c.Level))
	opts := &slog.HandlerOptions{Level: level}
	if c.Format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

// RedactEmail keeps just enough of an address to correlate in logs
// ("j***@example.com") without storing personal data.
func RedactEmail(addr string) string {
	local, domain, ok := strings.Cut(addr, "@")
	if !ok || local == "" {
		return "***"
	}
	return local[:1] + "***@" + domain
}

var emailRe = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+`)

// RedactText masks every email address in free text such as SMTP server
// replies, which commonly echo the recipient ("550 <a@b.c> no such user").
func RedactText(s string) string {
	return emailRe.ReplaceAllStringFunc(s, RedactEmail)
}
