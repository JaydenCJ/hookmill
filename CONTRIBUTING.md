# Contributing to hookmill

Issues, discussions and pull requests are all welcome.

## Getting started

You need Go ≥1.22; nothing else — no database, no message broker.

```bash
git clone https://github.com/JaydenCJ/hookmill && cd hookmill
go build ./...
go test ./...
bash scripts/smoke.sh
```

`scripts/smoke.sh` builds the binary, spins up the built-in loopback
receiver, and pushes real signed deliveries through the whole
enqueue → retry → dead-letter → requeue cycle; it must finish by
printing `SMOKE OK`.

## Before you open a pull request

1. `gofmt -l .` reports nothing (formatting is enforced).
2. `go vet ./...` passes with no findings.
3. `go test ./...` passes (89 deterministic tests, no network beyond
   127.0.0.1 loopback in the CLI suite).
4. `bash scripts/smoke.sh` prints `SMOKE OK`.
5. Add tests for behavior changes; keep logic in pure, unit-testable
   modules (only `deliver` and the `listen` command touch sockets —
   everything else is functions over bytes and time values).

## Ground rules

- Keep dependencies at zero; adding one needs strong justification in
  the PR. The standard library is the whole toolbox.
- No telemetry, no network at startup. hookmill only ever connects to
  the endpoint URLs the user configured, and `listen` binds loopback
  unless explicitly overridden.
- The WAL is the source of truth: every state change must be a record
  applied through `store.apply`, never a direct map mutation, so replay
  equivalence holds. The replay-equivalence tests are non-negotiable.
- Wire compatibility is sacred: the signed-content format, header
  names, and WAL framing are pinned by tests with independent vectors.
  Changing them is a breaking change and needs a scheme version bump.
- Code comments and doc comments are written in English.

## Reporting bugs

Include the output of `hookmill version`, the full command you ran, and
— for delivery issues — the `hookmill inspect <message-id>` output for
the affected message (redact URLs and secrets), since that is the exact
attempt history the engine recorded.

## Security

Please do not open public issues for security problems (especially
anything touching signature verification); use GitHub's private
vulnerability reporting on this repository instead.
