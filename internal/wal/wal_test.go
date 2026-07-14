// Tests for the write-ahead log: framing, checksums, torn-tail repair,
// corruption refusal, and atomic rewrite. Durability claims live or die
// here, so several tests damage real files on disk and reopen them.
package wal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)

func openTemp(t *testing.T) (*Log, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wal.log")
	l, res, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Records) != 0 || res.TornTail {
		t.Fatalf("fresh log should be empty and whole: %+v", res)
	}
	return l, path
}

type payload struct {
	N int    `json:"n"`
	S string `json:"s,omitempty"`
}

func TestAppendThenReplayRoundTrips(t *testing.T) {
	l, path := openTemp(t)
	for i := 1; i <= 3; i++ {
		if _, err := l.Append(t0, "tick", payload{N: i}); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()

	_, res, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Records) != 3 {
		t.Fatalf("replayed %d records, want 3", len(res.Records))
	}
	for i, rec := range res.Records {
		if rec.Seq != uint64(i+1) || rec.Type != "tick" {
			t.Fatalf("record %d: seq=%d type=%q", i, rec.Seq, rec.Type)
		}
		if !rec.At.Equal(t0) {
			t.Fatalf("record %d: at=%v, want %v", i, rec.At, t0)
		}
	}
}

func TestAppendAssignsSequentialSeqs(t *testing.T) {
	l, _ := openTemp(t)
	defer l.Close()
	r1, _ := l.Append(t0, "a", payload{N: 1})
	r2, _ := l.Append(t0, "b", payload{N: 2})
	if r1.Seq != 1 || r2.Seq != 2 {
		t.Fatalf("seqs = %d, %d; want 1, 2", r1.Seq, r2.Seq)
	}
}

func TestCountAndSizeGrow(t *testing.T) {
	l, _ := openTemp(t)
	defer l.Close()
	if l.Count() != 0 {
		t.Fatalf("fresh count = %d", l.Count())
	}
	l.Append(t0, "a", payload{N: 1})
	size1, _ := l.Size()
	l.Append(t0, "a", payload{N: 2})
	size2, _ := l.Size()
	if l.Count() != 2 || size2 <= size1 || size1 <= 0 {
		t.Fatalf("count=%d size1=%d size2=%d", l.Count(), size1, size2)
	}
}

func TestReopenContinuesSequence(t *testing.T) {
	l, path := openTemp(t)
	l.Append(t0, "a", payload{N: 1})
	l.Close()

	l2, _, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	rec, err := l2.Append(t0, "a", payload{N: 2})
	if err != nil || rec.Seq != 2 {
		t.Fatalf("append after reopen: seq=%d err=%v", rec.Seq, err)
	}
}

func TestTornPartialTailIsTruncated(t *testing.T) {
	l, path := openTemp(t)
	l.Append(t0, "a", payload{N: 1})
	l.Append(t0, "a", payload{N: 2})
	l.Close()

	// Simulate a crash mid-append: half a line, no newline, garbage crc.
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	f.WriteString(`deadbeef {"seq":3,"ty`)
	f.Close()

	l2, res, err := Open(path)
	if err != nil {
		t.Fatalf("torn tail must be repaired, not fatal: %v", err)
	}
	defer l2.Close()
	if !res.TornTail || len(res.Records) != 2 {
		t.Fatalf("want TornTail with 2 records, got torn=%v n=%d", res.TornTail, len(res.Records))
	}
	// The log must be appendable again with the right next sequence.
	rec, err := l2.Append(t0, "a", payload{N: 3})
	if err != nil || rec.Seq != 3 {
		t.Fatalf("append after repair: seq=%d err=%v", rec.Seq, err)
	}
}

func TestTornMissingNewlineKeepsValidRecord(t *testing.T) {
	// The final record was fully written except its newline: it must be
	// KEPT (the data is intact) and the framing repaired.
	l, path := openTemp(t)
	l.Append(t0, "a", payload{N: 1})
	l.Close()
	raw, _ := os.ReadFile(path)
	os.WriteFile(path, raw[:len(raw)-1], 0o644) // chop the trailing \n

	l2, res, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TornTail || len(res.Records) != 1 {
		t.Fatalf("want torn=true with the record kept, got torn=%v n=%d", res.TornTail, len(res.Records))
	}
	rec, err := l2.Append(t0, "a", payload{N: 2})
	l2.Close()
	if err != nil || rec.Seq != 2 {
		t.Fatalf("append after newline repair: seq=%d err=%v", rec.Seq, err)
	}
	// After the repair the file must replay clean, with both records.
	_, res2, err := Open(path)
	if err != nil || res2.TornTail || len(res2.Records) != 2 {
		t.Fatalf("post-repair replay: torn=%v n=%d err=%v", res2.TornTail, len(res2.Records), err)
	}
}

func TestMidFileCorruptionIsRefused(t *testing.T) {
	// Damage before the final line is NOT a torn write; silently
	// dropping it would resurrect delivered messages. Must refuse.
	l, path := openTemp(t)
	l.Append(t0, "a", payload{N: 1})
	l.Append(t0, "a", payload{N: 2})
	l.Append(t0, "a", payload{N: 3})
	l.Close()

	raw, _ := os.ReadFile(path)
	lines := strings.SplitAfter(string(raw), "\n")
	lines[1] = strings.Replace(lines[1], `"n":2`, `"n":9`, 1) // flip a byte, keep old crc
	os.WriteFile(path, []byte(strings.Join(lines, "")), 0o644)

	_, _, err := Open(path)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}
}

func TestSequenceGapIsRefused(t *testing.T) {
	// A hand-edited log with a missing record must not replay quietly.
	l, path := openTemp(t)
	l.Append(t0, "a", payload{N: 1})
	l.Append(t0, "a", payload{N: 2})
	l.Append(t0, "a", payload{N: 3})
	l.Close()
	raw, _ := os.ReadFile(path)
	lines := strings.SplitAfter(string(raw), "\n")
	os.WriteFile(path, []byte(lines[0]+lines[2]), 0o644)

	_, _, err := Open(path)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt for a seq gap, got %v", err)
	}
}

func TestChecksumCatchesBitFlipInFinalLine(t *testing.T) {
	// Damage confined to the final line is repairable by truncation —
	// the record is lost but the log stays usable.
	l, path := openTemp(t)
	l.Append(t0, "a", payload{N: 1})
	l.Append(t0, "a", payload{N: 2, S: "target"})
	l.Close()
	raw, _ := os.ReadFile(path)
	flipped := strings.Replace(string(raw), "target", "tarGet", 1)
	os.WriteFile(path, []byte(flipped), 0o644)

	l2, res, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if !res.TornTail || len(res.Records) != 1 {
		t.Fatalf("want final-line damage dropped, got torn=%v n=%d", res.TornTail, len(res.Records))
	}
}

func TestDamagedOnlyRecordIsRefusedNotEmptied(t *testing.T) {
	// A compacted log is exactly one snapshot record. If that record is
	// damaged, "torn tail repair" would truncate the file to zero bytes
	// and silently erase all state. Must refuse and leave the file alone.
	l, path := openTemp(t)
	l.Append(t0, "snapshot", payload{N: 1, S: "everything"})
	l.Close()
	raw, _ := os.ReadFile(path)
	flipped := strings.Replace(string(raw), "everything", "everyThing", 1)
	os.WriteFile(path, []byte(flipped), 0o644)

	_, _, err := Open(path)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt for a damaged only record, got %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != flipped {
		t.Fatal("refusal must not modify the file (evidence preserved)")
	}
}

func TestLargeRecordSurvives(t *testing.T) {
	l, path := openTemp(t)
	big := strings.Repeat("x", 256<<10)
	if _, err := l.Append(t0, "big", payload{N: 1, S: big}); err != nil {
		t.Fatal(err)
	}
	l.Close()
	_, res, err := Open(path)
	if err != nil || len(res.Records) != 1 {
		t.Fatalf("big record replay: n=%d err=%v", len(res.Records), err)
	}
}

func TestRewriteReplacesLogAtomically(t *testing.T) {
	l, path := openTemp(t)
	for i := 1; i <= 5; i++ {
		l.Append(t0, "a", payload{N: i})
	}
	l.Close()

	if err := Rewrite(path, t0, "snapshot", payload{N: 99}); err != nil {
		t.Fatal(err)
	}
	l2, res, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if len(res.Records) != 1 || res.Records[0].Type != "snapshot" || res.Records[0].Seq != 1 {
		t.Fatalf("rewritten log = %+v", res.Records)
	}
	if _, err := os.Stat(path + ".compact"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temp compaction file must not linger")
	}
}

func TestAppendAfterRewriteContinues(t *testing.T) {
	l, path := openTemp(t)
	l.Append(t0, "a", payload{N: 1})
	l.Close()
	Rewrite(path, t0, "snapshot", payload{N: 1})
	l2, _, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	rec, err := l2.Append(t0, "a", payload{N: 2})
	if err != nil || rec.Seq != 2 {
		t.Fatalf("append after rewrite: seq=%d err=%v", rec.Seq, err)
	}
}
