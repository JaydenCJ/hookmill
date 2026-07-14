# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-07-13

### Added

- File-backed write-ahead log as the single source of truth: CRC32-framed
  JSON records, fsync on every append, torn-tail detection and repair,
  refusal of mid-file corruption and sequence gaps, and atomic
  compaction into a single snapshot record (`hookmill compact`).
- HMAC-SHA256 signing scheme over `id.timestamp.body` with
  `Hookmill-Id` / `Hookmill-Timestamp` / `Hookmill-Signature` /
  `Hookmill-Event` headers, space-separated `v1=` multi-signature
  support, and `hmsec_`-prefixed 192-bit secret generation.
- Endpoint management (`endpoint add|list|remove|rotate`) with secret
  rotation that signs with both the new and previous secret so
  receivers switch without dropping deliveries; removal dead-letters
  the endpoint's pending messages instead of hiding them.
- Delivery engine with explicit retry schedules (default
  `5s,30s,2m,10m,1h,6h,24h`, or `none`), per-message fail streaks,
  dead-lettering on schedule exhaustion, `deliver --drain`, and
  `requeue` (single id or `--all`) that preserves attempt history.
- Queue operations: `enqueue` (flag or stdin payloads, stored
  byte-for-byte), `status`, `inspect`, and `dead`, each with
  `--format json`.
- Receiver-side verification: the importable `verify` Go package
  (`Request`, `Payload`, `Handler`, tolerance and rotation options),
  the `hookmill sign` / `hookmill verify` CLI pair, and a built-in
  loopback test receiver (`hookmill listen`, with `--fail-first` for
  retry drills).
- Runnable examples (`examples/receiver`, `examples/end-to-end.sh`)
  and format references (`docs/signing.md`, `docs/wal-format.md`).
- 89 deterministic offline tests (unit + in-process CLI integration
  against 127.0.0.1 receivers) and `scripts/smoke.sh`.

[0.1.0]: https://github.com/JaydenCJ/hookmill/releases/tag/v0.1.0
