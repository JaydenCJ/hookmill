# hookmill WAL format

All hookmill state — endpoints, secrets, messages, attempt history —
lives in a single append-only file, `<data dir>/wal.log`. There is no
database and no cache: opening the data directory replays the log from
the top, and any state a command shows you is exactly what a fresh
replay reconstructs. The file is line-oriented and human-inspectable
with standard tools.

## Framing

One record per line:

```
<crc32 of the JSON, 8 lowercase hex chars> <space> <record JSON> <newline>
```

The JSON envelope is:

```json
{"seq":3,"at":"2026-07-13T05:19:37Z","type":"enqueue","data":{…}}
```

- `seq` starts at 1 and must increase by exactly 1 per record.
- `at` is the UTC wall-clock time the record was written.
- `data` is the type-specific payload; message bodies inside it are
  base64 so arbitrary bytes round-trip without re-encoding.

Every append is flushed with `fsync` before the command reports
success.

## Crash safety

On open, hookmill verifies every line's checksum and sequence:

- **Damage confined to the final line** (a partial write after a crash
  or power loss — including a missing trailing newline) is repaired by
  truncating to the last good record. Commands print a one-line warning
  when this happens.
- **Damage before the final line, or any sequence gap,** cannot be the
  result of a torn append. hookmill refuses to open the log rather than
  silently drop records — a dropped `attempt` record would resurrect a
  delivered message and double-send it.
- **Damage to the log's only record** (a compacted log is exactly one
  snapshot line) is likewise refused, never repaired: truncating it
  would silently erase all state. The file is left untouched as
  evidence.

## Record types

| `type` | Written by | Effect on replay |
|---|---|---|
| `config` | `init` | sets the retry schedule |
| `endpoint_add` | `endpoint add` | registers name, URL, initial secret |
| `endpoint_remove` | `endpoint remove` | drops the endpoint, dead-letters its pending messages |
| `endpoint_rotate` | `endpoint rotate` | prepends a new secret, keeps the previous one |
| `enqueue` | `enqueue` | adds a pending message, due immediately |
| `attempt` | `deliver` | appends one attempt; outcome `delivered` / `retry` (with `next_due`) / `dead` |
| `requeue` | `requeue` | dead → pending, fail streak reset, history kept |
| `snapshot` | `compact` | replaces all state with the embedded snapshot |

## Compaction

`hookmill compact` serializes the current state into one `snapshot`
record, writes it to `wal.log.compact` (fully fsynced), and renames it
over the old log — so a crash mid-compaction leaves the previous log
intact. Sequence numbering restarts at 1. Attempt history is part of
the snapshot; compaction reclaims space taken by superseded records,
not your audit trail.

There is no automatic compaction in 0.1.0: the queue's disk footprint
is fully in your control and observable via `hookmill status`.
