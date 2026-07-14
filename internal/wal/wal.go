// Package wal implements hookmill's write-ahead log: an append-only,
// checksummed, line-oriented record file that is the single source of
// truth for all state. Every state change is one fsynced line; state is
// rebuilt by replaying the file from the top.
//
// Framing: each line is "<crc32 hex, 8 chars> <record json>\n" where the
// checksum covers the JSON bytes. A torn final line (partial write after
// a crash or power loss) is detected, reported, and truncated away on
// open; a bad checksum anywhere *before* the final line means real
// corruption and is refused rather than silently skipped. Repair also
// refuses to discard the log's ONLY record (nothing valid would remain,
// so it is corruption of the whole state, not a torn append).
package wal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"time"
)

// Record is one entry in the log. Data is an opaque JSON payload whose
// shape depends on Type; the store layer owns those shapes.
type Record struct {
	Seq  uint64          `json:"seq"`
	At   time.Time       `json:"at"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// ErrCorrupt is wrapped by replay errors for damage that truncation
// cannot repair (a bad line that is not the final line).
var ErrCorrupt = errors.New("wal corrupt")

// Log is an open write-ahead log ready for appends.
type Log struct {
	path    string
	f       *os.File
	nextSeq uint64
	count   int
}

// ReplayResult reports what Open recovered from disk.
type ReplayResult struct {
	Records  []Record
	TornTail bool // a partial final line was found and truncated
}

// Open replays the log at path (creating it if absent) and returns it
// opened for appends. A torn final line is truncated; any earlier
// damage returns ErrCorrupt.
func Open(path string) (*Log, ReplayResult, error) {
	var res ReplayResult
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, res, fmt.Errorf("read wal: %w", err)
	}

	goodEnd := 0 // byte offset just past the last valid line
	offset := 0
	var lastSeq uint64
	for offset < len(raw) {
		nl := bytes.IndexByte(raw[offset:], '\n')
		final := nl < 0
		var line []byte
		if final {
			line = raw[offset:]
		} else {
			line = raw[offset : offset+nl]
		}
		rec, perr := parseLine(line)
		if perr == nil && rec.Seq != lastSeq+1 {
			// A torn write can never yield a checksum-valid record with
			// the wrong sequence — this is real damage, wherever it sits.
			return nil, res, fmt.Errorf("%w: line %d: sequence jump: got %d, want %d",
				ErrCorrupt, len(res.Records)+1, rec.Seq, lastSeq+1)
		}
		if perr != nil {
			// Only the final line may be damaged (torn write); anything
			// earlier is corruption we must not paper over.
			if final || offset+nl+1 >= len(raw) {
				res.TornTail = true
				break
			}
			return nil, res, fmt.Errorf("%w: line %d: %v", ErrCorrupt, len(res.Records)+1, perr)
		}
		lastSeq = rec.Seq
		res.Records = append(res.Records, rec)
		if final {
			// Valid record but the trailing newline is missing: the write
			// was cut mid-flush. Keep the record; repair the framing.
			res.TornTail = true
			goodEnd = len(raw)
			break
		}
		offset += nl + 1
		goodEnd = offset
	}

	if res.TornTail {
		if len(res.Records) == 0 {
			// The damaged line is the only line: "repair" would empty the
			// log and silently erase all state (a compacted log is exactly
			// one snapshot record). That is whole-state corruption, not a
			// torn append — refuse and leave the file untouched.
			return nil, res, fmt.Errorf("%w: the log's only record is damaged; refusing a repair that would discard all state", ErrCorrupt)
		}
		if err := os.Truncate(path, int64(goodEnd)); err != nil {
			return nil, res, fmt.Errorf("truncate torn tail: %w", err)
		}
		if goodEnd == len(raw) {
			// The tail was a valid record missing only its newline;
			// re-terminate it so the next append starts a fresh line.
			if err := appendRaw(path, []byte{'\n'}); err != nil {
				return nil, res, err
			}
		}
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, res, fmt.Errorf("open wal for append: %w", err)
	}
	return &Log{path: path, f: f, nextSeq: lastSeq + 1, count: len(res.Records)}, res, nil
}

func appendRaw(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		return err
	}
	return f.Sync()
}

func parseLine(line []byte) (Record, error) {
	var rec Record
	sp := bytes.IndexByte(line, ' ')
	if sp != 8 {
		return rec, errors.New("missing checksum prefix")
	}
	var want uint32
	if _, err := fmt.Sscanf(string(line[:sp]), "%08x", &want); err != nil {
		return rec, fmt.Errorf("bad checksum prefix: %w", err)
	}
	payload := line[sp+1:]
	if got := crc32.ChecksumIEEE(payload); got != want {
		return rec, fmt.Errorf("checksum mismatch: recorded %08x, computed %08x", want, got)
	}
	if err := json.Unmarshal(payload, &rec); err != nil {
		return rec, fmt.Errorf("bad record json: %w", err)
	}
	return rec, nil
}

func encodeLine(rec Record) ([]byte, error) {
	payload, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("marshal record: %w", err)
	}
	line := make([]byte, 0, len(payload)+10)
	line = fmt.Appendf(line, "%08x ", crc32.ChecksumIEEE(payload))
	line = append(line, payload...)
	return append(line, '\n'), nil
}

// Append durably writes one record (marshal, checksum, write, fsync)
// and returns it with its assigned sequence number.
func (l *Log) Append(at time.Time, typ string, data any) (Record, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Record{}, fmt.Errorf("marshal %s data: %w", typ, err)
	}
	rec := Record{Seq: l.nextSeq, At: at.UTC(), Type: typ, Data: raw}
	line, err := encodeLine(rec)
	if err != nil {
		return Record{}, err
	}
	if _, err := l.f.Write(line); err != nil {
		return Record{}, fmt.Errorf("append wal: %w", err)
	}
	if err := l.f.Sync(); err != nil {
		return Record{}, fmt.Errorf("sync wal: %w", err)
	}
	l.nextSeq++
	l.count++
	return rec, nil
}

// Close releases the underlying file.
func (l *Log) Close() error { return l.f.Close() }

// Path returns the log's file path.
func (l *Log) Path() string { return l.path }

// Count returns the number of records currently in the log.
func (l *Log) Count() int { return l.count }

// Size returns the log's current size in bytes.
func (l *Log) Size() (int64, error) {
	fi, err := os.Stat(l.path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// Rewrite atomically replaces the log at path with a fresh one holding a
// single record (sequence 1) — the compaction primitive. The new file is
// fully written and fsynced before it is renamed over the old one, so a
// crash mid-compaction leaves the previous log intact.
func Rewrite(path string, at time.Time, typ string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal %s data: %w", typ, err)
	}
	line, err := encodeLine(Record{Seq: 1, At: at.UTC(), Type: typ, Data: raw})
	if err != nil {
		return err
	}
	tmp := path + ".compact"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create compaction file: %w", err)
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		return fmt.Errorf("write compaction file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync compaction file: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("swap compacted wal: %w", err)
	}
	return syncDir(filepath.Dir(path))
}

// syncDir fsyncs a directory so a rename within it is durable. Best
// effort: some filesystems refuse directory fsync, which is fine.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer d.Close()
	_ = d.Sync()
	return nil
}
