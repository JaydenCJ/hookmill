// Package deliver is hookmill's delivery engine: it takes due messages
// from the store, POSTs them with signed headers, classifies the result,
// and records the attempt (delivered / retry with backoff / dead) back
// through the WAL. The HTTP client and clock are injected so the whole
// engine is testable in-process with zero real waiting.
package deliver

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/JaydenCJ/hookmill/internal/signature"
	"github.com/JaydenCJ/hookmill/internal/store"
	"github.com/JaydenCJ/hookmill/internal/version"
)

// Request headers hookmill sends with every delivery.
const (
	HeaderID        = "Hookmill-Id"
	HeaderTimestamp = "Hookmill-Timestamp"
	HeaderSignature = "Hookmill-Signature"
	HeaderEvent     = "Hookmill-Event"
)

// maxResponseBody caps how much of a receiver's response is drained so
// keep-alive connections are reused without trusting the peer's size.
const maxResponseBody = 64 << 10

// maxDrainRounds bounds Drain against pathological zero-delay schedules.
const maxDrainRounds = 1000

// Doer is the HTTP client seam (satisfied by *http.Client).
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Engine delivers due messages for one store.
type Engine struct {
	Store  *store.Store
	Client Doer
	// Now supplies the clock; defaults to time.Now.
	Now func() time.Time
}

// Result describes one attempt the engine made.
type Result struct {
	MessageID string
	Endpoint  string
	AttemptNo int
	Status    int    // HTTP status, 0 when the request never completed
	Err       string // transport error, empty on an HTTP response
	Outcome   string // store.OutcomeDelivered | OutcomeRetry | OutcomeDead
	NextDue   time.Time
	Duration  time.Duration
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// RunOnce attempts every message due right now, at most limit of them
// (0 = no limit), one attempt each. It returns a result per attempt.
func (e *Engine) RunOnce(limit int) ([]Result, error) {
	due := e.Store.Due(e.now())
	if limit > 0 && len(due) > limit {
		due = due[:limit]
	}
	results := make([]Result, 0, len(due))
	for _, m := range due {
		res, err := e.attempt(m)
		if err != nil {
			return results, err
		}
		results = append(results, res)
	}
	return results, nil
}

// Drain keeps running rounds until nothing is due — useful with
// zero-delay schedules and in tests with a controlled clock. With real
// schedules it does NOT wait for future due times; it only drains what
// is due at the moment of each round.
func (e *Engine) Drain(limit int) ([]Result, error) {
	var all []Result
	for round := 0; round < maxDrainRounds; round++ {
		res, err := e.RunOnce(limit)
		all = append(all, res...)
		if err != nil || len(res) == 0 {
			return all, err
		}
	}
	return all, fmt.Errorf("drain exceeded %d rounds; is the schedule all zeros with a failing endpoint?", maxDrainRounds)
}

// attempt performs one signed POST for one message and records the
// outcome. Store write errors abort; delivery failures never do — they
// are the normal case the retry machinery exists for.
func (e *Engine) attempt(m *store.Message) (Result, error) {
	ep := e.Store.Endpoint(m.Endpoint)
	if ep == nil {
		// Cannot happen through the public API (removal dead-letters
		// pending messages), but guard against a hand-edited WAL.
		return Result{}, fmt.Errorf("message %s targets unknown endpoint %q", m.ID, m.Endpoint)
	}
	start := e.now()
	ts := start.Unix()

	req, err := http.NewRequest(http.MethodPost, ep.URL, bytes.NewReader(m.Body))
	if err != nil {
		return Result{}, fmt.Errorf("build request for %s: %w", m.ID, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "hookmill/"+version.Version)
	req.Header.Set(HeaderID, m.ID)
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(HeaderEvent, m.Type)
	req.Header.Set(HeaderSignature, signature.SignAll(ep.Secrets, m.ID, ts, m.Body))

	att := store.Attempt{At: start.UTC()}
	resp, doErr := e.Client.Do(req)
	if doErr != nil {
		att.Err = doErr.Error()
	} else {
		att.Status = resp.StatusCode
		// Drain (bounded) so the transport can reuse the connection.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
		resp.Body.Close()
	}
	end := e.now()
	att.DurationMs = end.Sub(start).Milliseconds()

	res := Result{
		MessageID: m.ID,
		Endpoint:  m.Endpoint,
		AttemptNo: len(m.Attempts) + 1,
		Status:    att.Status,
		Err:       att.Err,
		Duration:  end.Sub(start),
	}

	var nextDue time.Time
	switch {
	case doErr == nil && att.Status >= 200 && att.Status <= 299:
		res.Outcome = store.OutcomeDelivered
	default:
		due, ok := e.Store.Schedule.NextDue(m.FailStreak+1, end)
		if ok {
			res.Outcome = store.OutcomeRetry
			res.NextDue = due
			nextDue = due
		} else {
			res.Outcome = store.OutcomeDead
		}
	}
	if err := e.Store.MarkAttempt(m.ID, att, res.Outcome, nextDue, end); err != nil {
		return res, err
	}
	return res, nil
}
