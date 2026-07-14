// Tests for the retry schedule: parsing, rendering, and the
// failure-count → next-due mapping that decides when a message dies.
package backoff

import (
	"strings"
	"testing"
	"time"
)

func TestParseSimpleSchedule(t *testing.T) {
	s, err := Parse("5s,30s,2m")
	if err != nil {
		t.Fatal(err)
	}
	want := Schedule{5 * time.Second, 30 * time.Second, 2 * time.Minute}
	if len(s) != len(want) {
		t.Fatalf("len = %d, want %d", len(s), len(want))
	}
	for i := range want {
		if s[i] != want[i] {
			t.Fatalf("entry %d = %s, want %s", i, s[i], want[i])
		}
	}
	if spaced, err := Parse(" 5s , 30s "); err != nil || len(spaced) != 2 {
		t.Fatalf("spaced input should parse: %v (%v)", spaced, err)
	}
}

func TestParseSpecialSchedules(t *testing.T) {
	// 'none' = no retries (one attempt total); zero delays are the
	// deterministic-test / drain-loop workhorse.
	s, err := Parse("none")
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != 0 || s.MaxAttempts() != 1 {
		t.Fatalf("'none' should mean one attempt total, got %d entries / %d attempts", len(s), s.MaxAttempts())
	}
	if z, err := Parse("0s,0s"); err != nil || len(z) != 2 {
		t.Fatalf("zero delays should parse: %v (%v)", z, err)
	}
}

func TestParseRejectsInvalidInput(t *testing.T) {
	long := strings.TrimSuffix(strings.Repeat("1s,", 40), ",")
	for _, in := range []string{
		"   ",     // empty: 'none' must be explicit
		"5s,-1s",  // negative delay
		"5s,soon", // not a duration
		long,      // over the entry cap
	} {
		if _, err := Parse(in); err == nil {
			t.Fatalf("Parse(%q) should fail", in)
		}
	}
}

func TestStringRoundTrips(t *testing.T) {
	// String output must be compact (no "2m0s"/"1h0m0s" noise) and
	// re-parseable to the same schedule.
	for in, want := range map[string]string{
		"5s,30s,2m": "5s,30s,2m",
		"none":      "none",
		"1h,6h,24h": "1h,6h,24h",
		"90s,1h30m": "1m30s,1h30m",
	} {
		s, err := Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		if s.String() != want {
			t.Fatalf("Parse(%q).String() = %q, want %q", in, s.String(), want)
		}
		back, err := Parse(s.String())
		if err != nil || back.String() != s.String() {
			t.Fatalf("round trip of %q gave %q (%v)", in, back.String(), err)
		}
	}
}

func TestDefaultScheduleShape(t *testing.T) {
	d := Default()
	if d.MaxAttempts() != 8 {
		t.Fatalf("default gives %d attempts, want 8", d.MaxAttempts())
	}
	for i := 1; i < len(d); i++ {
		if d[i] <= d[i-1] {
			t.Fatalf("default schedule must be strictly increasing; entry %d (%s) <= entry %d (%s)", i, d[i], i-1, d[i-1])
		}
	}
}

func TestNextDueWalksTheSchedule(t *testing.T) {
	s := Schedule{5 * time.Second, 30 * time.Second}
	now := time.Unix(1_000_000, 0)
	due, ok := s.NextDue(1, now)
	if !ok || !due.Equal(now.Add(5*time.Second)) {
		t.Fatalf("failure 1: due=%v ok=%v", due, ok)
	}
	due, ok = s.NextDue(2, now)
	if !ok || !due.Equal(now.Add(30*time.Second)) {
		t.Fatalf("failure 2: due=%v ok=%v", due, ok)
	}
}

func TestNextDueExhaustsToDead(t *testing.T) {
	s := Schedule{5 * time.Second}
	if _, ok := s.NextDue(2, time.Now()); ok {
		t.Fatal("failure 2 on a 1-entry schedule must be terminal")
	}
	empty := Schedule{}
	if _, ok := empty.NextDue(1, time.Now()); ok {
		t.Fatal("any failure on an empty schedule must be terminal")
	}
	if _, ok := s.NextDue(0, time.Now()); ok {
		t.Fatal("failedAttempts=0 makes no sense and must not schedule")
	}
}
