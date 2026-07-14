# hookmill signing scheme

Every delivery hookmill makes is authenticated with HMAC-SHA256. This
document is the wire reference for receivers implementing verification
in any language. The Go implementation lives in the importable
[`verify`](../verify/verify.go) package; its behavior is pinned by
tests with independently-computed vectors.

## Request headers

| Header | Example | Meaning |
|---|---|---|
| `Hookmill-Id` | `msg_c97eb11956d7be70` | unique message id, stable across retries |
| `Hookmill-Timestamp` | `1784092777` | unix seconds when *this attempt* was signed |
| `Hookmill-Signature` | `v1=oanwo9nMB4Yy…=` | one or more signatures, space-separated |
| `Hookmill-Event` | `invoice.paid` | the event type given at enqueue |
| `Content-Type` | `application/json` | always |
| `User-Agent` | `hookmill/0.1.0` | tool + version |

## Signed content

The MAC covers the exact byte sequence

```
<Hookmill-Id> "." <Hookmill-Timestamp> "." <raw request body>
```

with the secret string (including its `hmsec_` prefix) used verbatim as
the HMAC key. Binding the id and timestamp into the MAC means a
captured body cannot be replayed under a different message id or at a
different time. The body is opaque bytes: hookmill stores, signs, and
delivers it byte-for-byte, so receivers must verify against the raw
body *before* any JSON parsing or re-serialization.

Each signature is `v1=` + standard base64 of the 32-byte MAC. During a
secret rotation the header carries one signature per active secret
(newest first), space-separated; a receiver should accept the delivery
if **any** entry matches a secret it knows.

## Verification steps (any language)

1. Reject if `Hookmill-Id`, `Hookmill-Timestamp`, or
   `Hookmill-Signature` is missing.
2. Parse the timestamp as unix seconds; reject if
   `|now − timestamp|` exceeds your tolerance (hookmill's default is
   **5 minutes**, both directions — retries are re-signed with a fresh
   timestamp, so a tight window is safe).
3. Recompute HMAC-SHA256 over `id.timestamp.body` for each secret you
   hold, and compare — **constant-time** — against every `v1=` entry.
4. Accept on any match; otherwise respond 401 and do not process.

Respond with any 2xx status to acknowledge. Anything else (including
3xx) makes hookmill retry on its backoff schedule.

## Testing a receiver by hand

```bash
printf '{"total":42}' | hookmill sign --secret hmsec_yoursecret --id msg_1
# Hookmill-Id: msg_1
# Hookmill-Timestamp: 1784092777
# Hookmill-Signature: v1=M322j02A9FgvUO+YW2u4ZHorEC5LgFSV0+DKhsRV938=

printf '{"total":42}' | hookmill verify --secret hmsec_yoursecret --id msg_1 \
  --timestamp 1784092777 --tolerance none \
  --signature 'v1=M322j02A9FgvUO+YW2u4ZHorEC5LgFSV0+DKhsRV938='
# signature OK (msg_1, ts 1784092777, 12 bytes)   — exit code 0; mismatches exit 1
# (--tolerance none skips the skew check, since this example timestamp is fixed)
```

`hookmill listen` runs a loopback receiver with this verification built
in, which is the fastest way to watch real signed deliveries land.
