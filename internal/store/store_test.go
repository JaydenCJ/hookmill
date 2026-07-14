// Tests for the state layer. The load-bearing property is replay
// equivalence: after any sequence of mutations, closing and reopening
// the data directory must reconstruct identical state — several tests
// assert exactly that.
package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/JaydenCJ/hookmill/internal/backoff"
)

var t0 = time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)

func newStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	if err := Init(dir, backoff.Default(), t0); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// reopen closes the store and replays its WAL from scratch.
func reopen(t *testing.T, s *Store) *Store {
	t.Helper()
	dir := s.Dir()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s2.Close() })
	return s2
}

func addEndpoint(t *testing.T, s *Store, name string) *Endpoint {
	t.Helper()
	ep, err := s.AddEndpoint(name, "http://127.0.0.1:9/hooks", "hmsec_"+name, t0)
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

func enqueue(t *testing.T, s *Store, ep string) *Message {
	t.Helper()
	m, err := s.Enqueue(ep, "user.created", []byte(`{"n":1}`), t0)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestInitAndOpenGuards(t *testing.T) {
	_, err := Open(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "hookmill init") {
		t.Fatalf("uninitialized dir should point at `hookmill init`, got %v", err)
	}
	dir := t.TempDir()
	if err := Init(dir, backoff.Default(), t0); err != nil {
		t.Fatal(err)
	}
	if err := Init(dir, backoff.Default(), t0); err == nil {
		t.Fatal("second init must refuse to clobber the WAL")
	}
}

func TestInitPersistsSchedule(t *testing.T) {
	dir := t.TempDir()
	sched, _ := backoff.Parse("1s,2s")
	if err := Init(dir, sched, t0); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Schedule.String() != "1s,2s" {
		t.Fatalf("schedule = %s, want 1s,2s", s.Schedule)
	}
}

func TestAddEndpointGeneratesSecretWhenEmpty(t *testing.T) {
	s := newStore(t)
	ep, err := s.AddEndpoint("orders", "http://127.0.0.1:9/h", "", t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ep.Secrets) != 1 || !strings.HasPrefix(ep.Secrets[0], "hmsec_") {
		t.Fatalf("generated secret missing: %v", ep.Secrets)
	}
}

func TestAddEndpointValidation(t *testing.T) {
	s := newStore(t)
	cases := []struct{ name, url string }{
		{"Bad Name", "http://127.0.0.1/h"}, // uppercase + space
		{"", "http://127.0.0.1/h"},         // empty name
		{"ok", "ftp://127.0.0.1/h"},        // bad scheme
		{"ok", "not a url"},                // unparseable
		{"ok", "/relative"},                // no host
	}
	for _, c := range cases {
		if _, err := s.AddEndpoint(c.name, c.url, "", t0); err == nil {
			t.Fatalf("AddEndpoint(%q, %q) should fail", c.name, c.url)
		}
	}
	addEndpoint(t, s, "orders")
	if _, err := s.AddEndpoint("orders", "http://127.0.0.1:9/h2", "", t0); err == nil {
		t.Fatal("duplicate endpoint name must fail")
	}
}

func TestEndpointListKeepsCreationOrder(t *testing.T) {
	s := newStore(t)
	for _, n := range []string{"zeta", "alpha", "mid"} {
		addEndpoint(t, s, n)
	}
	var got []string
	for _, ep := range s.EndpointList() {
		got = append(got, ep.Name)
	}
	if !reflect.DeepEqual(got, []string{"zeta", "alpha", "mid"}) {
		t.Fatalf("order = %v", got)
	}
}

func TestRotateKeepsPreviousSecretOnly(t *testing.T) {
	s := newStore(t)
	addEndpoint(t, s, "orders")
	first := s.Endpoint("orders").Secrets[0]
	sec2, err := s.RotateSecret("orders", t0)
	if err != nil {
		t.Fatal(err)
	}
	got := s.Endpoint("orders").Secrets
	if !reflect.DeepEqual(got, []string{sec2, first}) {
		t.Fatalf("after 1 rotation: %v", got)
	}
	sec3, _ := s.RotateSecret("orders", t0)
	got = s.Endpoint("orders").Secrets
	if !reflect.DeepEqual(got, []string{sec3, sec2}) {
		t.Fatalf("after 2 rotations the first secret must be retired: %v", got)
	}
}

func TestEnqueueRequiresKnownEndpointAndType(t *testing.T) {
	s := newStore(t)
	addEndpoint(t, s, "orders")
	if _, err := s.Enqueue("ghost", "e.t", nil, t0); err == nil {
		t.Fatal("unknown endpoint must fail")
	}
	if _, err := s.Enqueue("orders", "", nil, t0); err == nil {
		t.Fatal("empty event type must fail")
	}
}

func TestEnqueueIsDueImmediately(t *testing.T) {
	s := newStore(t)
	addEndpoint(t, s, "orders")
	m := enqueue(t, s, "orders")
	if m.State != StatePending || !m.NextDue.Equal(t0) {
		t.Fatalf("state=%s nextDue=%v", m.State, m.NextDue)
	}
	if due := s.Due(t0); len(due) != 1 || due[0].ID != m.ID {
		t.Fatalf("Due = %v", due)
	}
}

func TestDueOrdersBySoonestAndSkipsFuture(t *testing.T) {
	s := newStore(t)
	addEndpoint(t, s, "orders")
	m1 := enqueue(t, s, "orders")
	m2 := enqueue(t, s, "orders")
	// Fail m1 so its retry lands in the future.
	err := s.MarkAttempt(m1.ID, Attempt{At: t0, Status: 500}, OutcomeRetry, t0.Add(time.Hour), t0)
	if err != nil {
		t.Fatal(err)
	}
	due := s.Due(t0)
	if len(due) != 1 || due[0].ID != m2.ID {
		t.Fatalf("only m2 should be due now: %v", due)
	}
	due = s.Due(t0.Add(2 * time.Hour))
	if len(due) != 2 || due[0].ID != m2.ID || due[1].ID != m1.ID {
		t.Fatalf("later both are due, soonest first: %v", due)
	}
}

func TestAttemptOutcomesTransitionState(t *testing.T) {
	s := newStore(t)
	addEndpoint(t, s, "orders")
	m := enqueue(t, s, "orders")

	if err := s.MarkAttempt(m.ID, Attempt{At: t0, Status: 500}, OutcomeRetry, t0.Add(time.Minute), t0); err != nil {
		t.Fatal(err)
	}
	if m.State != StatePending || m.FailStreak != 1 || len(m.Attempts) != 1 {
		t.Fatalf("after retry: %+v", m)
	}
	if err := s.MarkAttempt(m.ID, Attempt{At: t0, Status: 204}, OutcomeDelivered, time.Time{}, t0); err != nil {
		t.Fatal(err)
	}
	if m.State != StateDelivered || m.FailStreak != 0 || !m.NextDue.IsZero() {
		t.Fatalf("after delivery: %+v", m)
	}
	// Terminal messages must reject further attempts.
	if err := s.MarkAttempt(m.ID, Attempt{At: t0}, OutcomeRetry, t0, t0); err == nil {
		t.Fatal("attempt on a delivered message must fail")
	}
	// And the dead outcome records why.
	m2 := enqueue(t, s, "orders")
	if err := s.MarkAttempt(m2.ID, Attempt{At: t0, Err: "connection refused"}, OutcomeDead, time.Time{}, t0); err != nil {
		t.Fatal(err)
	}
	if m2.State != StateDead || m2.DeadReason == "" {
		t.Fatalf("after dead: state=%s reason=%q", m2.State, m2.DeadReason)
	}
}

func TestRequeueResetsStreakKeepsHistory(t *testing.T) {
	s := newStore(t)
	addEndpoint(t, s, "orders")
	m := enqueue(t, s, "orders")
	s.MarkAttempt(m.ID, Attempt{At: t0, Status: 500}, OutcomeRetry, t0, t0)
	s.MarkAttempt(m.ID, Attempt{At: t0, Status: 500}, OutcomeDead, time.Time{}, t0)

	later := t0.Add(time.Hour)
	if _, err := s.Requeue(m.ID, later); err != nil {
		t.Fatal(err)
	}
	if m.State != StatePending || m.FailStreak != 0 || !m.NextDue.Equal(later) {
		t.Fatalf("after requeue: %+v", m)
	}
	if len(m.Attempts) != 2 {
		t.Fatalf("attempt history must survive requeue, got %d", len(m.Attempts))
	}
	// Only dead messages are requeueable (m is pending again now).
	if _, err := s.Requeue(m.ID, t0); err == nil {
		t.Fatal("requeue of a pending message must fail")
	}
	if _, err := s.Requeue("msg_nope", t0); err == nil {
		t.Fatal("requeue of an unknown id must fail")
	}
}

func TestRemoveEndpointDeadLettersPending(t *testing.T) {
	s := newStore(t)
	addEndpoint(t, s, "orders")
	m := enqueue(t, s, "orders")
	n, err := s.RemoveEndpoint("orders", t0)
	if err != nil || n != 1 {
		t.Fatalf("RemoveEndpoint = %d, %v", n, err)
	}
	if m.State != StateDead || m.DeadReason != "endpoint removed" {
		t.Fatalf("pending message should be dead-lettered: %+v", m)
	}
	if s.Endpoint("orders") != nil {
		t.Fatal("endpoint should be gone")
	}
	// And its dead messages cannot be requeued into the void.
	if _, err := s.Requeue(m.ID, t0); err == nil {
		t.Fatal("requeue toward a removed endpoint must fail")
	}
}

func TestReplayEquivalenceAfterFullLifecycle(t *testing.T) {
	s := newStore(t)
	addEndpoint(t, s, "orders")
	addEndpoint(t, s, "billing")
	m1 := enqueue(t, s, "orders")
	m2 := enqueue(t, s, "billing")
	s.RotateSecret("orders", t0)
	s.MarkAttempt(m1.ID, Attempt{At: t0, Status: 500, DurationMs: 3}, OutcomeRetry, t0.Add(5*time.Second), t0)
	s.MarkAttempt(m1.ID, Attempt{At: t0, Status: 204, DurationMs: 2}, OutcomeDelivered, time.Time{}, t0)
	s.MarkAttempt(m2.ID, Attempt{At: t0, Err: "connection refused"}, OutcomeDead, time.Time{}, t0)
	s.Requeue(m2.ID, t0)

	before := snapshotJSON(t, s)
	s2 := reopen(t, s)
	if after := snapshotJSON(t, s2); before != after {
		t.Fatalf("replayed state diverges:\nbefore: %s\nafter:  %s", before, after)
	}
}

func snapshotJSON(t *testing.T, s *Store) string {
	t.Helper()
	out, err := json.Marshal(map[string]any{
		"schedule":  s.Schedule.String(),
		"endpoints": s.EndpointList(),
		"messages":  s.MessageList(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestCompactPreservesStateAndShrinksWAL(t *testing.T) {
	s := newStore(t)
	addEndpoint(t, s, "orders")
	m := enqueue(t, s, "orders")
	for i := 0; i < 5; i++ {
		s.MarkAttempt(m.ID, Attempt{At: t0, Status: 500}, OutcomeRetry, t0, t0)
	}
	before := snapshotJSON(t, s)
	records := s.WAL().Count()
	if err := s.Compact(t0); err != nil {
		t.Fatal(err)
	}
	if s.WAL().Count() != 1 {
		t.Fatalf("compacted WAL has %d records, want 1 (was %d)", s.WAL().Count(), records)
	}
	if after := snapshotJSON(t, s); before != after {
		t.Fatal("compaction changed live state")
	}
	// The snapshot must also replay from disk identically, and further
	// mutations appended after the snapshot must survive another replay.
	s2 := reopen(t, s)
	if after := snapshotJSON(t, s2); before != after {
		t.Fatal("compacted WAL replays to different state")
	}
	enqueue(t, s2, "orders")
	before2 := snapshotJSON(t, s2)
	s3 := reopen(t, s2)
	if after := snapshotJSON(t, s3); before2 != after {
		t.Fatal("post-compaction mutations lost on replay")
	}
}

func TestCountsByState(t *testing.T) {
	s := newStore(t)
	addEndpoint(t, s, "orders")
	m1 := enqueue(t, s, "orders")
	m2 := enqueue(t, s, "orders")
	enqueue(t, s, "orders")
	s.MarkAttempt(m1.ID, Attempt{At: t0, Status: 200}, OutcomeDelivered, time.Time{}, t0)
	s.MarkAttempt(m2.ID, Attempt{At: t0, Status: 500}, OutcomeDead, time.Time{}, t0)
	pending, delivered, dead, next := s.Counts()
	if pending != 1 || delivered != 1 || dead != 1 {
		t.Fatalf("counts = %d/%d/%d", pending, delivered, dead)
	}
	if !next.Equal(t0) {
		t.Fatalf("next due = %v, want %v", next, t0)
	}
}

func TestMessageIDShape(t *testing.T) {
	s := newStore(t)
	addEndpoint(t, s, "orders")
	m := enqueue(t, s, "orders")
	if !strings.HasPrefix(m.ID, "msg_") || len(m.ID) != 20 {
		t.Fatalf("id %q should be msg_ + 16 hex chars", m.ID)
	}
}

func TestBodyStoredVerbatim(t *testing.T) {
	s := newStore(t)
	addEndpoint(t, s, "orders")
	body := []byte("  {\"keep\":\t\"whitespace 名前\"}  ")
	m, err := s.Enqueue("orders", "e.t", body, t0)
	if err != nil {
		t.Fatal(err)
	}
	s2 := reopen(t, s)
	got := s2.Message(m.ID).Body
	if string(got) != string(body) {
		t.Fatalf("body mutated across replay: %q vs %q", got, body)
	}
}
