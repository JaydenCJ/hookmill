#!/usr/bin/env bash
# End-to-end smoke test for hookmill: builds the binary, runs a real
# loopback receiver, and asserts on real CLI output across the whole
# enqueue → sign → deliver → retry → dead-letter → requeue cycle.
# Everything stays on 127.0.0.1, is idempotent, and finishes in seconds.
#
# Assertions capture output first and grep the variable — piping a
# command straight into `grep -q` under pipefail flakes, because grep
# exits at the first match and the writer dies with SIGPIPE.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

fail() {
  echo "SMOKE FAIL: $*" >&2
  exit 1
}

# expect <label> <pattern> <command...>: run the command, assert its
# combined output contains the pattern.
expect() {
  local label="$1" pattern="$2" out
  shift 2
  out="$("$@" 2>&1)" || fail "$label: command failed: $out"
  echo "$out" | grep -q "$pattern" || fail "$label: wanted \"$pattern\" in: $out"
}

BIN="$WORKDIR/hookmill"
export HOOKMILL_DIR="$WORKDIR/queue"
PORT=18811
SECRET="hmsec_smoke-test-secret"

wait_for_port() {
  for _ in $(seq 1 100); do
    if (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; then return 0; fi
    sleep 0.05
  done
  fail "receiver on port $1 never came up"
}

echo "1. build"
(cd "$ROOT" && go build -o "$BIN" ./cmd/hookmill) || fail "go build failed"

echo "2. version matches manifest"
"$BIN" version | grep -qx "hookmill 0.1.0" || fail "version mismatch"

echo "3. init with a zero-delay schedule (3 attempts per message)"
expect "init" "max 3 attempts" "$BIN" init --schedule 0s,0s
[ -f "$HOOKMILL_DIR/wal.log" ] || fail "wal.log not created"

echo "4. endpoint add / list"
expect "endpoint add" "secret  $SECRET" \
  "$BIN" endpoint add billing --url "http://127.0.0.1:$PORT/hooks" --secret "$SECRET"
LIST="$("$BIN" endpoint list)"
echo "$LIST" | grep -q "billing" || fail "endpoint missing from list"
echo "$LIST" | grep -q "$SECRET" && fail "plain list must not leak secrets"

echo "5. enqueue via flag and via stdin"
expect "enqueue (flag)" "due now" \
  "$BIN" enqueue billing --type invoice.paid --data '{"invoice":"inv_1042"}'
STDIN_OUT="$(printf '{"invoice":"inv_1043"}' | "$BIN" enqueue billing --type invoice.paid)"
echo "$STDIN_OUT" | grep -q "22 bytes" || fail "enqueue (stdin) failed: $STDIN_OUT"

echo "6. deliver through a verifying receiver that fails first"
"$BIN" listen --addr "127.0.0.1:$PORT" --secret "$SECRET" --fail-first 1 --max 2 \
  > "$WORKDIR/listen.out" 2>&1 &
LISTEN_PID=$!
wait_for_port "$PORT"
DRAIN="$("$BIN" deliver --drain)"
wait "$LISTEN_PID" || fail "listen exited non-zero"
echo "$DRAIN" | grep -q "summary: 2 delivered, 1 retried, 0 dead" \
  || fail "drain summary wrong: $DRAIN"
grep -q "^500  msg_" "$WORKDIR/listen.out" || fail "synthetic failure not seen"
[ "$(grep -c "^ok   msg_" "$WORKDIR/listen.out")" -eq 2 ] \
  || fail "receiver did not verify 2 deliveries"

echo "7. status reflects the queue"
expect "status" "delivered   2" "$BIN" status
expect "status json" '"pending": 0' "$BIN" status --format json

echo "8. unreachable endpoint exhausts the schedule into the dead letter queue"
"$BIN" endpoint add void --url "http://127.0.0.1:1/hooks" --secret "$SECRET" >/dev/null
MSG="$("$BIN" enqueue void --type job.failed --data '{}' | grep -o 'msg_[0-9a-f]*')"
expect "dead-letter drain" "summary: 0 delivered, 2 retried, 1 dead" "$BIN" deliver --drain
expect "dead list" "$MSG" "$BIN" dead
expect "inspect dead" "state      dead" "$BIN" inspect "$MSG"

echo "9. requeue puts it back, attempt history intact"
expect "requeue" "requeued $MSG" "$BIN" requeue "$MSG"
INSPECT="$("$BIN" inspect "$MSG")"
echo "$INSPECT" | grep -q "state      pending" || fail "requeued message not pending: $INSPECT"
echo "$INSPECT" | grep -q "connection refused" || fail "attempt history lost"

echo "10. sign / verify round-trip, tamper detection, exit codes"
SIG="$(printf '{"total":42}' | "$BIN" sign --secret "$SECRET" --id msg_smoke --timestamp 1784092777 \
  | grep '^Hookmill-Signature: ' | cut -d' ' -f2-)"
VOUT="$(printf '{"total":42}' | "$BIN" verify --secret "$SECRET" --id msg_smoke \
  --timestamp 1784092777 --signature "$SIG" --tolerance none)"
echo "$VOUT" | grep -q "signature OK" || fail "verify rejected a valid signature: $VOUT"
if printf '{"total":43}' | "$BIN" verify --secret "$SECRET" --id msg_smoke \
  --timestamp 1784092777 --signature "$SIG" --tolerance none >/dev/null; then
  fail "verify accepted a tampered body"
fi

echo "11. compact rewrites the WAL without losing state"
expect "compact" "1 snapshot" "$BIN" compact
expect "state after compact" "delivered   2" "$BIN" status
expect "dead after compact" "dead-letter queue is empty" "$BIN" dead

echo "12. usage errors exit 2"
set +e
"$BIN" enqueue billing >/dev/null 2>&1
[ $? -eq 2 ] || fail "enqueue without --type should exit 2"
"$BIN" listen --secret s --addr 0.0.0.0:1 >/dev/null 2>&1
[ $? -eq 2 ] || fail "non-loopback listen should exit 2"
set -e

echo "SMOKE OK"
