// Package backoff models hookmill's retry schedule: an explicit list of
// delays applied after each failed delivery attempt. Explicit schedules
// (rather than a formula) make retry behavior inspectable, testable, and
// exactly reproducible from the WAL.
package backoff

import (
	"fmt"
	"strings"
	"time"
)

// maxEntries bounds a parsed schedule so a typo cannot create an
// effectively unbounded retry queue.
const maxEntries = 32

// Schedule is the ordered list of waits after the 1st, 2nd, … failure.
// A message gets len(Schedule)+1 total attempts before dead-lettering.
type Schedule []time.Duration

// Default is the schedule written by `hookmill init` when none is given:
// quick early retries for blips, then hourly-scale waits for outages.
func Default() Schedule {
	return Schedule{
		5 * time.Second,
		30 * time.Second,
		2 * time.Minute,
		10 * time.Minute,
		1 * time.Hour,
		6 * time.Hour,
		24 * time.Hour,
	}
}

// Parse reads a comma-separated list of Go durations ("5s,30s,2m").
// The keyword "none" yields an empty schedule (no retries: one attempt,
// then dead-letter). Negative entries are rejected; zero entries are
// allowed and mean "retry immediately", which tests and drain loops use.
func Parse(s string) (Schedule, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil, fmt.Errorf("empty schedule (use %q for no retries)", "none")
	}
	if trimmed == "none" {
		return Schedule{}, nil
	}
	parts := strings.Split(trimmed, ",")
	if len(parts) > maxEntries {
		return nil, fmt.Errorf("schedule has %d entries; max is %d", len(parts), maxEntries)
	}
	out := make(Schedule, 0, len(parts))
	for _, p := range parts {
		d, err := time.ParseDuration(strings.TrimSpace(p))
		if err != nil {
			return nil, fmt.Errorf("bad schedule entry %q: %w", strings.TrimSpace(p), err)
		}
		if d < 0 {
			return nil, fmt.Errorf("bad schedule entry %q: negative durations are not allowed", strings.TrimSpace(p))
		}
		out = append(out, d)
	}
	return out, nil
}

// String renders the schedule back into Parse's input format, with
// zero-valued trailing units trimmed ("2m0s" → "2m", "1h0m0s" → "1h").
func (s Schedule) String() string {
	if len(s) == 0 {
		return "none"
	}
	parts := make([]string, len(s))
	for i, d := range s {
		parts[i] = fmtDuration(d)
	}
	return strings.Join(parts, ",")
}

func fmtDuration(d time.Duration) string {
	str := d.String()
	if strings.HasSuffix(str, "m0s") {
		str = str[:len(str)-2]
	}
	if strings.HasSuffix(str, "h0m") {
		str = str[:len(str)-2]
	}
	return str
}

// MaxAttempts is the total number of delivery attempts a message gets
// before it is dead-lettered.
func (s Schedule) MaxAttempts() int { return len(s) + 1 }

// NextDue returns when the next attempt should run given that
// failedAttempts have already happened (including the one that just
// failed). ok is false when the schedule is exhausted and the message
// must be dead-lettered.
func (s Schedule) NextDue(failedAttempts int, now time.Time) (due time.Time, ok bool) {
	if failedAttempts < 1 || failedAttempts > len(s) {
		return time.Time{}, false
	}
	return now.Add(s[failedAttempts-1]), true
}
