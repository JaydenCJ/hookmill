// Tests for the delivery engine, run fully in-process: a fake Doer
// captures requests and scripts responses, and a manual clock makes
// backoff arithmetic exact. No sockets, no sleeping.
package deliver

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JaydenCJ/hookmill/internal/backoff"
	"github.com/JaydenCJ/hookmill/internal/signature"
	"github.com/JaydenCJ/hookmill/internal/store"
	"github.com/JaydenCJ/hookmill/internal/version"
)

var t0 = time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)

const epSecret = "hmsec_engine-test"

// fakeDoer replies with a scripted sequence of statuses (0 = transport
// error) and records every request it saw.
type fakeDoer struct {
	statuses []int
	calls    int
	requests []*http.Request
	bodies   []string
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	f.requests = append(f.requests, req)
	f.bodies = append(f.bodies, string(body))
	status := 200
	if f.calls < len(f.statuses) {
		status = f.statuses[f.calls]
	}
	f.calls++
	if status == 0 {
		return nil, errors.New("dial tcp 127.0.0.1:9: connection refused")
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("ok"))}, nil
}

// fixture builds a store with one endpoint, one enqueued message, and
// an engine wired to the fake doer and a manual clock.
func fixture(t *testing.T, schedule string, statuses ...int) (*Engine, *fakeDoer, *store.Message, *time.Time) {
	t.Helper()
	dir := t.TempDir()
	sched, err := backoff.Parse(schedule)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(dir, sched, t0); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.AddEndpoint("orders", "http://127.0.0.1:9/hooks", epSecret, t0); err != nil {
		t.Fatal(err)
	}
	m, err := s.Enqueue("orders", "invoice.paid", []byte(`{"total":42}`), t0)
	if err != nil {
		t.Fatal(err)
	}
	now := t0
	doer := &fakeDoer{statuses: statuses}
	engine := &Engine{Store: s, Client: doer, Now: func() time.Time { return now }}
	return engine, doer, m, &now
}

func TestSuccessfulDelivery(t *testing.T) {
	engine, doer, m, _ := fixture(t, "5s", 204)
	results, err := engine.RunOnce(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Outcome != store.OutcomeDelivered || results[0].Status != 204 {
		t.Fatalf("results = %+v", results)
	}
	if m.State != store.StateDelivered {
		t.Fatalf("message state = %s", m.State)
	}
	if doer.bodies[0] != `{"total":42}` {
		t.Fatalf("delivered body = %q", doer.bodies[0])
	}
}

func TestRequestCarriesSignedHeaders(t *testing.T) {
	engine, doer, m, _ := fixture(t, "5s", 200)
	if _, err := engine.RunOnce(0); err != nil {
		t.Fatal(err)
	}
	req := doer.requests[0]
	if got := req.Header.Get(HeaderID); got != m.ID {
		t.Fatalf("%s = %q, want %q", HeaderID, got, m.ID)
	}
	if got := req.Header.Get(HeaderEvent); got != "invoice.paid" {
		t.Fatalf("%s = %q", HeaderEvent, got)
	}
	if got := req.Header.Get("User-Agent"); got != "hookmill/"+version.Version {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	ts, err := strconv.ParseInt(req.Header.Get(HeaderTimestamp), 10, 64)
	if err != nil || ts != t0.Unix() {
		t.Fatalf("timestamp header = %q (%v)", req.Header.Get(HeaderTimestamp), err)
	}
	// The signature must verify with the endpoint's secret — this is
	// the sender/receiver contract in one assertion.
	sig := req.Header.Get(HeaderSignature)
	if err := signature.Verify([]string{epSecret}, m.ID, ts, sig, []byte(doer.bodies[0])); err != nil {
		t.Fatalf("delivered signature does not verify: %v", err)
	}
}

func TestFailureSchedulesRetryWithBackoff(t *testing.T) {
	engine, _, m, now := fixture(t, "5s,30s", 500, 500)
	results, err := engine.RunOnce(0)
	if err != nil {
		t.Fatal(err)
	}
	r := results[0]
	if r.Outcome != store.OutcomeRetry || r.Status != 500 {
		t.Fatalf("result = %+v", r)
	}
	wantDue := t0.Add(5 * time.Second)
	if !r.NextDue.Equal(wantDue) || !m.NextDue.Equal(wantDue) {
		t.Fatalf("next due = %v, want %v", m.NextDue, wantDue)
	}
	if m.State != store.StatePending {
		t.Fatalf("state = %s", m.State)
	}
	// The second failure walks to the second schedule entry.
	*now = t0.Add(5 * time.Second) // clock reaches the first retry
	engine.RunOnce(0)
	wantDue = now.Add(30 * time.Second)
	if !m.NextDue.Equal(wantDue) {
		t.Fatalf("second retry due = %v, want %v", m.NextDue, wantDue)
	}
}

func TestNotDueMeansNotAttempted(t *testing.T) {
	engine, doer, _, _ := fixture(t, "1h", 500)
	engine.RunOnce(0) // fails, reschedules 1h out
	results, err := engine.RunOnce(0)
	if err != nil || len(results) != 0 {
		t.Fatalf("nothing should be due yet: %v (%v)", results, err)
	}
	if doer.calls != 1 {
		t.Fatalf("doer called %d times, want 1", doer.calls)
	}
}

func TestScheduleExhaustionDeadLetters(t *testing.T) {
	engine, _, m, now := fixture(t, "0s", 500, 500)
	engine.RunOnce(0)
	*now = t0.Add(time.Millisecond)
	results, err := engine.RunOnce(0)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Outcome != store.OutcomeDead {
		t.Fatalf("second failure on 0s schedule must dead-letter: %+v", results[0])
	}
	if m.State != store.StateDead || len(m.Attempts) != 2 {
		t.Fatalf("message = %+v", m)
	}
	// And a 'none' schedule dead-letters on the very first failure.
	engine2, _, m2, _ := fixture(t, "none", 503)
	results2, _ := engine2.RunOnce(0)
	if results2[0].Outcome != store.OutcomeDead || m2.State != store.StateDead {
		t.Fatalf("schedule 'none' must dead-letter immediately: %+v", results2[0])
	}
}

func TestTransportErrorIsRecordedAndRetried(t *testing.T) {
	engine, _, m, _ := fixture(t, "5s", 0)
	results, err := engine.RunOnce(0)
	if err != nil {
		t.Fatal(err)
	}
	r := results[0]
	if r.Outcome != store.OutcomeRetry || r.Status != 0 || !strings.Contains(r.Err, "connection refused") {
		t.Fatalf("result = %+v", r)
	}
	if m.Attempts[0].Err == "" {
		t.Fatal("transport error must land in the attempt record")
	}
}

func TestNon2xxStatusesAllRetry(t *testing.T) {
	// 3xx and 4xx are failures too: a receiver that redirects or
	// rejects has not durably accepted the event.
	for _, status := range []int{301, 400, 404, 429, 500, 503} {
		engine, _, m, _ := fixture(t, "5s", status)
		results, _ := engine.RunOnce(0)
		if results[0].Outcome != store.OutcomeRetry {
			t.Fatalf("status %d: outcome = %s, want retry", status, results[0].Outcome)
		}
		if m.State != store.StatePending {
			t.Fatalf("status %d: state = %s", status, m.State)
		}
	}
}

func TestRetryAfterFailureDeliversAndVerifies(t *testing.T) {
	engine, doer, m, now := fixture(t, "0s,0s", 500, 200)
	engine.RunOnce(0)
	*now = t0.Add(time.Second)
	engine.RunOnce(0)
	if m.State != store.StateDelivered || len(m.Attempts) != 2 {
		t.Fatalf("message = %+v", m)
	}
	// The retry is re-signed with the fresh timestamp, not the old one.
	req := doer.requests[1]
	ts, _ := strconv.ParseInt(req.Header.Get(HeaderTimestamp), 10, 64)
	if ts != now.Unix() {
		t.Fatalf("retry timestamp = %d, want %d", ts, now.Unix())
	}
	if err := signature.Verify([]string{epSecret}, m.ID, ts, req.Header.Get(HeaderSignature), []byte(doer.bodies[1])); err != nil {
		t.Fatalf("retry signature does not verify: %v", err)
	}
}

func TestRunOnceHonorsLimit(t *testing.T) {
	engine, doer, _, _ := fixture(t, "5s", 200, 200)
	if _, err := engine.Store.Enqueue("orders", "b", []byte(`{}`), t0); err != nil {
		t.Fatal(err)
	}
	results, err := engine.RunOnce(1)
	if err != nil || len(results) != 1 || doer.calls != 1 {
		t.Fatalf("limit 1: results=%d calls=%d err=%v", len(results), doer.calls, err)
	}
}

func TestDrainProcessesZeroDelayRetriesToTermination(t *testing.T) {
	engine, doer, m, _ := fixture(t, "0s,0s", 500, 500, 204)
	results, err := engine.Drain(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 || doer.calls != 3 {
		t.Fatalf("drain made %d attempts (%d calls)", len(results), doer.calls)
	}
	if m.State != store.StateDelivered {
		t.Fatalf("final state = %s", m.State)
	}
}

func TestRotatedSecretsBothSignTheDelivery(t *testing.T) {
	engine, doer, m, _ := fixture(t, "5s", 200)
	newSecret, err := engine.Store.RotateSecret("orders", t0)
	if err != nil {
		t.Fatal(err)
	}
	engine.RunOnce(0)
	req := doer.requests[0]
	ts, _ := strconv.ParseInt(req.Header.Get(HeaderTimestamp), 10, 64)
	sig := req.Header.Get(HeaderSignature)
	if len(strings.Fields(sig)) != 2 {
		t.Fatalf("want 2 signatures during rotation, got %q", sig)
	}
	for _, secret := range []string{newSecret, epSecret} {
		if err := signature.Verify([]string{secret}, m.ID, ts, sig, []byte(doer.bodies[0])); err != nil {
			t.Fatalf("secret %s should verify: %v", secret, err)
		}
	}
}
