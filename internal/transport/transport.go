// Package transport delivers canonical messages. The SMTP implementation
// pools connections per project; other providers (SES, Postmark, ...) can be
// added behind the same interface without touching the worker pipeline.
package transport

import (
	"context"

	"github.com/skylarng89/smtp-handler/internal/message"
)

// Class is how the worker should react to a delivery result.
type Class string

const (
	ClassSent      Class = "sent"
	ClassTransient Class = "transient" // retry with backoff
	ClassPermanent Class = "permanent" // dead-letter
	// ClassConfig means our side is misconfigured or refused (auth, TLS,
	// banned sender). Retrying immediately is pointless and the failure says
	// nothing about the message, so it does not consume an attempt.
	ClassConfig Class = "config"
	// ClassUnknown means the connection broke after the message body was sent
	// but before the server answered: it may or may not have been accepted.
	ClassUnknown Class = "unknown"
	// ClassBusy means no connection slot was available; the message was
	// never attempted and does not consume an attempt.
	ClassBusy Class = "busy"
)

// Result is the outcome of one delivery try.
type Result struct {
	Class    Class
	Code     int    // SMTP reply code, 0 if none
	Enhanced string // RFC 3463 enhanced status code, if reported
	Detail   string
}

func (r Result) OK() bool { return r.Class == ClassSent }

// Transport delivers messages. Implementations must be safe for concurrent use.
type Transport interface {
	Send(ctx context.Context, m *message.Message) Result
	Close() error
}
