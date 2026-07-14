# hookmill examples

Both examples are self-contained, stay on 127.0.0.1, and need only Go.

## `receiver/` — embed verification in your own service

A ~40-line webhook consumer using the importable
[`verify`](../verify/verify.go) package. Run it, register it as an
endpoint, and deliver to it:

```bash
go run ./examples/receiver -secret hmsec_yoursecret &
hookmill init
hookmill endpoint add demo --url http://127.0.0.1:9911/hooks --secret hmsec_yoursecret
hookmill enqueue demo --type user.created --data '{"user":"u_1001"}'
hookmill deliver
```

The receiver logs each authenticated event and rejects anything with a
bad signature, a stale timestamp, or a tampered body with 401.

## `end-to-end.sh` — the full reliability cycle in one script

Builds the binary, creates a throwaway queue with a zero-delay retry
schedule, and uses the built-in `hookmill listen --fail-first 1`
receiver to force one synthetic failure — so you can watch a delivery
fail, retry, and land verified, then read the attempt history with
`hookmill status`:

```bash
bash examples/end-to-end.sh
```
