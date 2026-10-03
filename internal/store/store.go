// Package store defines the persistence contract for the mail queue.
//
// Every operation that several replicas (or a worker and an operator) could
// race on is expressed as a single guarded statement in the implementation:
// unique constraints for idempotency, atomic claims with fencing tokens for
// workers, and version checks for admin actions. Implementations must never
// rely on in-process locks for cross-node correctness, and must take all
// timestamps from the database clock.
package store

import (
	"context"
	"errors"
	"time"
)

// State is a message's position in the delivery state machine.
//
//	queued ─► sending ─► sent
//	            │  ▲
//	            ▼  │ (lease expiry / release)
//	      retry_scheduled ─► sending
//	            │
//	            ▼
//	         failed ──(admin retry)──► queued
//	queued / retry_scheduled / failed ──(admin cancel)──► canceled
type State string

const (
	StateQueued         State = "queued"
	StateSending        State = "sending"
	StateRetryScheduled State = "retry_scheduled"
	StateSent           State = "sent"
	StateFailed         State = "failed"
	StateCanceled       State = "canceled"
)

// AllStates lists every state, for validation and stats.
var AllStates = []State{StateQueued, StateSending, StateRetryScheduled, StateSent, StateFailed, StateCanceled}

func (s State) Valid() bool {
	for _, v := range AllStates {
		if s == v {
			return true
		}
	}
	return false
}

// Sentinel errors returned by Store methods.
var (
	ErrNotFound = errors.New("store: not found")
	// ErrConflict means the message is not in a state that allows the action.
	ErrConflict = errors.New("store: state conflict")
	// ErrVersionMismatch means the caller's version is stale (optimistic lock).
	ErrVersionMismatch = errors.New("store: version mismatch")
	// ErrLeaseLost means the caller no longer owns the job's lease; its
	// result must be discarded because another worker may have taken over.
	ErrLeaseLost = errors.New("store: lease lost")
	// ErrIdempotencyMismatch means the idempotency key was used with a
	// different request payload.
	ErrIdempotencyMismatch = errors.New("store: idempotency key reused with a different payload")
	// ErrPayloadGone means the message body was dropped by retention policy.
	ErrPayloadGone = errors.New("store: message body no longer available")
)

// Message is a queue row without its (potentially large) body.
type Message struct {
	ID              string     `json:"id"`
	ProjectID       string     `json:"project_id"`
	Template        string     `json:"template,omitempty"`
	KeyID           string     `json:"key_id,omitempty"`
	State           State      `json:"state"`
	Version         int64      `json:"version"`
	Attempt         int        `json:"attempt"`
	MaxAttempts     int        `json:"max_attempts"`
	NextAttemptAt   time.Time  `json:"next_attempt_at"`
	DeadlineAt      time.Time  `json:"deadline_at"`
	LeaseOwner      string     `json:"lease_owner,omitempty"`
	LeaseUntil      *time.Time `json:"lease_until,omitempty"`
	MessageIDHeader string     `json:"message_id"`
	Subject         string     `json:"subject"`
	From            string     `json:"from"`
	To              string     `json:"to"`
	BodyDropped     bool       `json:"body_dropped"`
	LastErrorClass  string     `json:"last_error_class,omitempty"`
	LastErrorCode   int        `json:"last_error_code,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	SentAt          *time.Time `json:"sent_at,omitempty"`
}

// NewMessage is the input for Enqueue.
type NewMessage struct {
	ID              string
	ProjectID       string
	Template        string
	KeyID           string
	MessageIDHeader string
	Subject         string
	From            string
	To              string // comma-separated, for display and search
	Payload         []byte // JSON-encoded message.Message
	MaxAttempts     int
	MaxAge          time.Duration
}

// Idempotency describes the de-duplication key for an Enqueue call.
type Idempotency struct {
	Key         string
	Fingerprint string
	TTL         time.Duration
}

// EnqueueResult reports the stored message ID and whether it already existed.
type EnqueueResult struct {
	ID       string
	Replayed bool
}

// Job is a claimed message, owned by the claimer until its lease expires.
type Job struct {
	Message
	Payload    []byte
	LeaseToken string
}

// Attempt is the audit record of one delivery try.
type Attempt struct {
	MessageID    string    `json:"message_id,omitempty"`
	Attempt      int       `json:"attempt"`
	Node         string    `json:"node"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	Outcome      string    `json:"outcome"`
	SMTPCode     int       `json:"smtp_code,omitempty"`
	EnhancedCode string    `json:"enhanced_code,omitempty"`
	Error        string    `json:"error,omitempty"`
}

// Outcome describes why a job left the sending state.
type Outcome struct {
	Class        string // e.g. "transient", "permanent", "config", "unknown"
	SMTPCode     int
	EnhancedCode string
	Detail       string
}

// ListFilter selects messages for the admin API. Zero values are ignored.
type ListFilter struct {
	ProjectID string
	States    []State
	Query     string // substring of subject, recipients or message ID
	Before    string // keyset cursor: return IDs strictly less than this
	Limit     int
}

// Count is one cell of the stats breakdown.
type Count struct {
	ProjectID string `json:"project_id"`
	State     State  `json:"state"`
	Count     int64  `json:"count"`
}

// RetentionRule defines how long one project's messages are kept.
type RetentionRule struct {
	ProjectID string
	Sent      time.Duration
	Failed    time.Duration
}

// PurgeResult summarizes a retention sweep.
type PurgeResult struct {
	Messages    int64
	Idempotency int64
	Counters    int64
	Slots       int64
	Skipped     bool // another node holds the purge lock
}

// Store is the persistence contract. All methods are safe for concurrent use
// by multiple goroutines and multiple processes.
type Store interface {
	// Enqueue inserts a message and, when idem is non-nil, its idempotency
	// record in a single transaction. A request that repeats a key with the
	// same fingerprint returns the original ID with Replayed set; a different
	// fingerprint returns ErrIdempotencyMismatch.
	Enqueue(ctx context.Context, m NewMessage, idem *Idempotency) (EnqueueResult, error)

	// FindIdempotent looks up a live idempotency record without writing.
	// It returns ErrNotFound when none exists, ErrIdempotencyMismatch when
	// the fingerprint differs.
	FindIdempotent(ctx context.Context, projectID, key, fingerprint string) (string, error)

	// Claim atomically leases up to limit due jobs to owner. Jobs whose lease
	// expired (crashed worker) are reclaimed; reclaimed jobs that already used
	// all attempts are failed instead (poison-message protection).
	Claim(ctx context.Context, owner string, limit int, lease time.Duration, excludeProjects []string) ([]Job, error)

	// RenewLease extends a lease; ErrLeaseLost if the token no longer matches.
	RenewLease(ctx context.Context, id, token string, lease time.Duration) error

	// The following complete a claimed job. Each is fenced on the lease token
	// and returns ErrLeaseLost when the caller no longer owns the job.
	MarkSent(ctx context.Context, id, token string, a Attempt, dropBody bool) error
	// MarkRetry schedules another attempt after delay, or fails the message if
	// attempts or max age are exhausted. It returns the resulting state.
	MarkRetry(ctx context.Context, id, token string, delay time.Duration, a Attempt) (State, error)
	MarkFailed(ctx context.Context, id, token string, a Attempt) error
	// Release returns the job to the queue after delay without consuming an
	// attempt (configuration problem, unknown project, shutdown).
	Release(ctx context.Context, id, token string, delay time.Duration, a *Attempt) error
	// ReleaseOwner releases every lease held by owner (graceful shutdown).
	ReleaseOwner(ctx context.Context, owner string) (int64, error)

	Get(ctx context.Context, id string) (*Message, error)
	// GetPayload returns the stored message body, or ErrPayloadGone.
	GetPayload(ctx context.Context, id string) ([]byte, error)
	Attempts(ctx context.Context, id string) ([]Attempt, error)
	List(ctx context.Context, f ListFilter) ([]Message, error)
	Stats(ctx context.Context) ([]Count, error)

	// Retry requeues a failed, canceled or scheduled message for immediate
	// delivery with a fresh attempt budget. version < 0 skips the version
	// check. Returns ErrNotFound, ErrConflict, ErrVersionMismatch or
	// ErrPayloadGone.
	Retry(ctx context.Context, id string, version int64) (*Message, error)
	// Cancel discards a queued, scheduled or failed message.
	Cancel(ctx context.Context, id string, version int64) (*Message, error)

	// Incr atomically increments a fixed-window counter and returns the new
	// count and the time remaining in the window.
	Incr(ctx context.Context, key string, window time.Duration) (int64, time.Duration, error)

	// Global SMTP connection slots, shared across replicas.
	AcquireSlot(ctx context.Context, projectID string, max int, owner string, ttl time.Duration) (int, bool, error)
	RenewSlot(ctx context.Context, projectID string, slot int, owner string, ttl time.Duration) (bool, error)
	ReleaseSlot(ctx context.Context, projectID string, slot int, owner string) error

	// Purge applies retention rules. Only one node runs it at a time.
	Purge(ctx context.Context, rules []RetentionRule, def RetentionRule, batch int) (PurgeResult, error)

	Ping(ctx context.Context) error
	// SchemaOK reports whether the database schema matches this binary.
	SchemaOK(ctx context.Context) error
	Driver() string
	Close() error
}
