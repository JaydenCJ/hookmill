// Package store is hookmill's state layer: endpoints and messages,
// materialized entirely by replaying the write-ahead log. Every mutation
// is append-one-record-then-apply-it, so the in-memory state after a
// command is byte-for-byte reproducible by reopening the data directory.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/JaydenCJ/hookmill/internal/backoff"
	"github.com/JaydenCJ/hookmill/internal/signature"
	"github.com/JaydenCJ/hookmill/internal/wal"
)

// Message states.
const (
	StatePending   = "pending"
	StateDelivered = "delivered"
	StateDead      = "dead"
)

// Attempt outcomes recorded in the WAL.
const (
	OutcomeDelivered = "delivered"
	OutcomeRetry     = "retry"
	OutcomeDead      = "dead"
)

// WAL record types.
const (
	recTypeConfig         = "config"
	recTypeEndpointAdd    = "endpoint_add"
	recTypeEndpointRemove = "endpoint_remove"
	recTypeEndpointRotate = "endpoint_rotate"
	recTypeEnqueue        = "enqueue"
	recTypeAttempt        = "attempt"
	recTypeRequeue        = "requeue"
	recTypeSnapshot       = "snapshot"
)

// maxSecrets is how many secrets an endpoint keeps after rotations: the
// active one plus the previous one, so in-flight deliveries signed with
// the old secret still verify.
const maxSecrets = 2

// WALFile is the write-ahead log filename inside the data directory.
const WALFile = "wal.log"

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Endpoint is a named delivery target with its signing secrets
// (newest first).
type Endpoint struct {
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Secrets   []string  `json:"secrets"`
	CreatedAt time.Time `json:"created_at"`
}

// Attempt is one delivery try. Status is the HTTP status code (0 when
// the request never completed, in which case Err says why).
type Attempt struct {
	At         time.Time `json:"at"`
	Status     int       `json:"status,omitempty"`
	Err        string    `json:"error,omitempty"`
	DurationMs int64     `json:"duration_ms"`
}

// Message is one queued webhook with its full attempt history. Body is
// opaque bytes, stored / signed / delivered byte-for-byte (it round-trips
// through the WAL as base64, never re-encoded as JSON).
type Message struct {
	ID         string    `json:"id"`
	Endpoint   string    `json:"endpoint"`
	Type       string    `json:"type"`
	Body       []byte    `json:"body"`
	EnqueuedAt time.Time `json:"enqueued_at"`
	State      string    `json:"state"`
	NextDue    time.Time `json:"next_due,omitempty"`
	// FailStreak counts consecutive failures since enqueue or the last
	// requeue; it indexes the backoff schedule. Requeue resets it while
	// Attempts keeps the full history.
	FailStreak int       `json:"fail_streak,omitempty"`
	Attempts   []Attempt `json:"attempts,omitempty"`
	DeadReason string    `json:"dead_reason,omitempty"`
}

// Store is the materialized state plus its open WAL.
type Store struct {
	dir      string
	log      *wal.Log
	Schedule backoff.Schedule
	TornTail bool

	endpoints map[string]*Endpoint
	epOrder   []string
	messages  map[string]*Message
	msgOrder  []string
}

// Record payloads (the Data field of wal.Record).

type recConfig struct {
	Schedule string `json:"schedule"`
}

type recEndpointAdd struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Secret string `json:"secret"`
}

type recEndpointRemove struct {
	Name string `json:"name"`
}

type recEndpointRotate struct {
	Name      string `json:"name"`
	NewSecret string `json:"new_secret"`
}

type recEnqueue struct {
	ID       string `json:"id"`
	Endpoint string `json:"endpoint"`
	Type     string `json:"type"`
	Body     []byte `json:"body"`
}

type recAttempt struct {
	ID      string    `json:"id"`
	Attempt Attempt   `json:"attempt"`
	Outcome string    `json:"outcome"`
	NextDue time.Time `json:"next_due,omitempty"`
}

type recRequeue struct {
	ID string `json:"id"`
}

type recSnapshot struct {
	Schedule  string      `json:"schedule"`
	Endpoints []*Endpoint `json:"endpoints"`
	Messages  []*Message  `json:"messages"`
}

// Init creates the data directory and its WAL with a config record.
// It refuses to re-initialize an existing data directory.
func Init(dir string, schedule backoff.Schedule, now time.Time) error {
	path := filepath.Join(dir, WALFile)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already initialized (%s exists)", dir, path)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	log, _, err := wal.Open(path)
	if err != nil {
		return err
	}
	defer log.Close()
	_, err = log.Append(now, recTypeConfig, recConfig{Schedule: schedule.String()})
	return err
}

// Open replays the WAL in dir and returns the materialized store.
func Open(dir string) (*Store, error) {
	path := filepath.Join(dir, WALFile)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s is not a hookmill data dir (run `hookmill init` first)", dir)
	}
	log, replay, err := wal.Open(path)
	if err != nil {
		return nil, err
	}
	s := &Store{
		dir:       dir,
		log:       log,
		Schedule:  backoff.Default(),
		TornTail:  replay.TornTail,
		endpoints: map[string]*Endpoint{},
		messages:  map[string]*Message{},
	}
	for _, rec := range replay.Records {
		if err := s.apply(rec); err != nil {
			log.Close()
			return nil, fmt.Errorf("replay wal record %d (%s): %w", rec.Seq, rec.Type, err)
		}
	}
	return s, nil
}

// Close releases the WAL file handle.
func (s *Store) Close() error { return s.log.Close() }

// Dir returns the data directory path.
func (s *Store) Dir() string { return s.dir }

// WAL returns the underlying log (for size/count reporting).
func (s *Store) WAL() *wal.Log { return s.log }

// commit appends a record and applies it, keeping disk and memory in
// lockstep. Apply errors after a successful append are programmer
// errors (the mutation was validated first), so they are returned as-is.
func (s *Store) commit(now time.Time, typ string, data any) error {
	rec, err := s.log.Append(now, typ, data)
	if err != nil {
		return err
	}
	return s.apply(rec)
}

// apply mutates state from one WAL record. It is the single code path
// shared by live mutations and replay, which is what guarantees that a
// reopened store matches the one that wrote the log.
func (s *Store) apply(rec wal.Record) error {
	switch rec.Type {
	case recTypeConfig:
		var d recConfig
		if err := json.Unmarshal(rec.Data, &d); err != nil {
			return err
		}
		sched, err := backoff.Parse(d.Schedule)
		if err != nil {
			return err
		}
		s.Schedule = sched
	case recTypeEndpointAdd:
		var d recEndpointAdd
		if err := json.Unmarshal(rec.Data, &d); err != nil {
			return err
		}
		s.endpoints[d.Name] = &Endpoint{Name: d.Name, URL: d.URL, Secrets: []string{d.Secret}, CreatedAt: rec.At}
		s.epOrder = append(s.epOrder, d.Name)
	case recTypeEndpointRemove:
		var d recEndpointRemove
		if err := json.Unmarshal(rec.Data, &d); err != nil {
			return err
		}
		delete(s.endpoints, d.Name)
		s.epOrder = remove(s.epOrder, d.Name)
		// Pending messages for a removed endpoint can never deliver:
		// dead-letter them now so they surface instead of hiding.
		for _, id := range s.msgOrder {
			m := s.messages[id]
			if m.Endpoint == d.Name && m.State == StatePending {
				m.State = StateDead
				m.DeadReason = "endpoint removed"
				m.NextDue = time.Time{}
			}
		}
	case recTypeEndpointRotate:
		var d recEndpointRotate
		if err := json.Unmarshal(rec.Data, &d); err != nil {
			return err
		}
		ep, ok := s.endpoints[d.Name]
		if !ok {
			return fmt.Errorf("rotate: unknown endpoint %q", d.Name)
		}
		ep.Secrets = append([]string{d.NewSecret}, ep.Secrets...)
		if len(ep.Secrets) > maxSecrets {
			ep.Secrets = ep.Secrets[:maxSecrets]
		}
	case recTypeEnqueue:
		var d recEnqueue
		if err := json.Unmarshal(rec.Data, &d); err != nil {
			return err
		}
		m := &Message{
			ID: d.ID, Endpoint: d.Endpoint, Type: d.Type, Body: d.Body,
			EnqueuedAt: rec.At, State: StatePending, NextDue: rec.At,
		}
		s.messages[d.ID] = m
		s.msgOrder = append(s.msgOrder, d.ID)
	case recTypeAttempt:
		var d recAttempt
		if err := json.Unmarshal(rec.Data, &d); err != nil {
			return err
		}
		m, ok := s.messages[d.ID]
		if !ok {
			return fmt.Errorf("attempt: unknown message %q", d.ID)
		}
		m.Attempts = append(m.Attempts, d.Attempt)
		switch d.Outcome {
		case OutcomeDelivered:
			m.State = StateDelivered
			m.FailStreak = 0
			m.NextDue = time.Time{}
		case OutcomeRetry:
			m.FailStreak++
			m.NextDue = d.NextDue
		case OutcomeDead:
			m.State = StateDead
			m.FailStreak++
			m.DeadReason = "retry schedule exhausted"
			m.NextDue = time.Time{}
		default:
			return fmt.Errorf("attempt: unknown outcome %q", d.Outcome)
		}
	case recTypeRequeue:
		var d recRequeue
		if err := json.Unmarshal(rec.Data, &d); err != nil {
			return err
		}
		m, ok := s.messages[d.ID]
		if !ok {
			return fmt.Errorf("requeue: unknown message %q", d.ID)
		}
		m.State = StatePending
		m.FailStreak = 0
		m.DeadReason = ""
		m.NextDue = rec.At
	case recTypeSnapshot:
		var d recSnapshot
		if err := json.Unmarshal(rec.Data, &d); err != nil {
			return err
		}
		sched, err := backoff.Parse(d.Schedule)
		if err != nil {
			return err
		}
		s.Schedule = sched
		s.endpoints = map[string]*Endpoint{}
		s.epOrder = nil
		for _, ep := range d.Endpoints {
			s.endpoints[ep.Name] = ep
			s.epOrder = append(s.epOrder, ep.Name)
		}
		s.messages = map[string]*Message{}
		s.msgOrder = nil
		for _, m := range d.Messages {
			s.messages[m.ID] = m
			s.msgOrder = append(s.msgOrder, m.ID)
		}
	default:
		return fmt.Errorf("unknown record type %q", rec.Type)
	}
	return nil
}

func remove(list []string, v string) []string {
	out := list[:0]
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// AddEndpoint registers a delivery target. An empty secret generates a
// fresh one. The URL must be absolute http(s).
func (s *Store) AddEndpoint(name, rawURL, secret string, now time.Time) (*Endpoint, error) {
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("invalid endpoint name %q (want lowercase letters, digits, - and _, max 64 chars)", name)
	}
	if _, exists := s.endpoints[name]; exists {
		return nil, fmt.Errorf("endpoint %q already exists", name)
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid endpoint url %q (want absolute http:// or https://)", rawURL)
	}
	if secret == "" {
		secret, err = signature.GenerateSecret()
		if err != nil {
			return nil, err
		}
	}
	if err := s.commit(now, recTypeEndpointAdd, recEndpointAdd{Name: name, URL: rawURL, Secret: secret}); err != nil {
		return nil, err
	}
	return s.endpoints[name], nil
}

// RemoveEndpoint deletes an endpoint and dead-letters its pending
// messages, returning how many were dead-lettered.
func (s *Store) RemoveEndpoint(name string, now time.Time) (int, error) {
	if _, ok := s.endpoints[name]; !ok {
		return 0, fmt.Errorf("unknown endpoint %q", name)
	}
	pending := 0
	for _, id := range s.msgOrder {
		m := s.messages[id]
		if m.Endpoint == name && m.State == StatePending {
			pending++
		}
	}
	if err := s.commit(now, recTypeEndpointRemove, recEndpointRemove{Name: name}); err != nil {
		return 0, err
	}
	return pending, nil
}

// RotateSecret generates a new signing secret for the endpoint. The
// previous secret keeps signing (and verifying) alongside the new one
// until the next rotation, giving receivers a seamless switch window.
func (s *Store) RotateSecret(name string, now time.Time) (string, error) {
	if _, ok := s.endpoints[name]; !ok {
		return "", fmt.Errorf("unknown endpoint %q", name)
	}
	secret, err := signature.GenerateSecret()
	if err != nil {
		return "", err
	}
	if err := s.commit(now, recTypeEndpointRotate, recEndpointRotate{Name: name, NewSecret: secret}); err != nil {
		return "", err
	}
	return secret, nil
}

// newMessageID returns "msg_" + 16 hex chars of crypto randomness.
var newMessageID = func() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate message id: %w", err)
	}
	return "msg_" + hex.EncodeToString(raw), nil
}

// Enqueue appends a message for an existing endpoint; it is due
// immediately. The body is stored verbatim.
func (s *Store) Enqueue(endpoint, eventType string, body []byte, now time.Time) (*Message, error) {
	if _, ok := s.endpoints[endpoint]; !ok {
		return nil, fmt.Errorf("unknown endpoint %q", endpoint)
	}
	if eventType == "" {
		return nil, errors.New("event type must not be empty")
	}
	id, err := newMessageID()
	if err != nil {
		return nil, err
	}
	if err := s.commit(now, recTypeEnqueue, recEnqueue{ID: id, Endpoint: endpoint, Type: eventType, Body: body}); err != nil {
		return nil, err
	}
	return s.messages[id], nil
}

// MarkAttempt records one delivery try and its outcome transition.
func (s *Store) MarkAttempt(id string, a Attempt, outcome string, nextDue, now time.Time) error {
	m, ok := s.messages[id]
	if !ok {
		return fmt.Errorf("unknown message %q", id)
	}
	if m.State != StatePending {
		return fmt.Errorf("message %s is %s, not pending", id, m.State)
	}
	return s.commit(now, recTypeAttempt, recAttempt{ID: id, Attempt: a, Outcome: outcome, NextDue: nextDue})
}

// Requeue puts a dead message back in the pending queue, due
// immediately, with a fresh fail streak (history is kept).
func (s *Store) Requeue(id string, now time.Time) (*Message, error) {
	m, ok := s.messages[id]
	if !ok {
		return nil, fmt.Errorf("unknown message %q", id)
	}
	if m.State != StateDead {
		return nil, fmt.Errorf("message %s is %s; only dead messages can be requeued", id, m.State)
	}
	if _, ok := s.endpoints[m.Endpoint]; !ok {
		return nil, fmt.Errorf("message %s targets removed endpoint %q", id, m.Endpoint)
	}
	if err := s.commit(now, recTypeRequeue, recRequeue{ID: id}); err != nil {
		return nil, err
	}
	return m, nil
}

// Compact rewrites the WAL as a single snapshot record, atomically.
func (s *Store) Compact(now time.Time) error {
	snap := recSnapshot{
		Schedule:  s.Schedule.String(),
		Endpoints: s.EndpointList(),
		Messages:  s.MessageList(),
	}
	path := s.log.Path()
	if err := s.log.Close(); err != nil {
		return err
	}
	if err := wal.Rewrite(path, now, recTypeSnapshot, snap); err != nil {
		return err
	}
	log, _, err := wal.Open(path)
	if err != nil {
		return err
	}
	s.log = log
	return nil
}

// Endpoint returns one endpoint by name (nil when absent).
func (s *Store) Endpoint(name string) *Endpoint { return s.endpoints[name] }

// EndpointList returns endpoints in creation order.
func (s *Store) EndpointList() []*Endpoint {
	out := make([]*Endpoint, 0, len(s.epOrder))
	for _, name := range s.epOrder {
		out = append(out, s.endpoints[name])
	}
	return out
}

// Message returns one message by id (nil when absent).
func (s *Store) Message(id string) *Message { return s.messages[id] }

// MessageList returns messages in enqueue order.
func (s *Store) MessageList() []*Message {
	out := make([]*Message, 0, len(s.msgOrder))
	for _, id := range s.msgOrder {
		out = append(out, s.messages[id])
	}
	return out
}

// Due returns pending messages whose NextDue is at or before now,
// soonest first (enqueue order breaks ties).
func (s *Store) Due(now time.Time) []*Message {
	var out []*Message
	for _, id := range s.msgOrder {
		m := s.messages[id]
		if m.State == StatePending && !m.NextDue.After(now) {
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].NextDue.Before(out[j].NextDue) })
	return out
}

// Counts returns totals by state plus the earliest pending due time.
func (s *Store) Counts() (pending, delivered, dead int, nextDue time.Time) {
	for _, m := range s.messages {
		switch m.State {
		case StatePending:
			pending++
			if nextDue.IsZero() || m.NextDue.Before(nextDue) {
				nextDue = m.NextDue
			}
		case StateDelivered:
			delivered++
		case StateDead:
			dead++
		}
	}
	return
}
