#!/usr/bin/env bash
# End-to-end walkthrough: build hookmill, create a queue in a temp dir,
# stand up the built-in loopback receiver, and watch a signed delivery
# survive one synthetic failure before landing. Everything stays on
# 127.0.0.1 and is cleaned up on exit.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

BIN="$WORKDIR/hookmill"
export HOOKMILL_DIR="$WORKDIR/queue"
PORT=18877
SECRET="hmsec_example-walkthrough"

(cd "$ROOT" && go build -o "$BIN" ./cmd/hookmill)

"$BIN" init --schedule 0s,0s
"$BIN" endpoint add demo --url "http://127.0.0.1:$PORT/hooks" --secret "$SECRET"
"$BIN" enqueue demo --type user.created --data '{"user":"u_1001","plan":"pro"}'

# Receiver: verify signatures, fail the first delivery on purpose so
# the retry machinery has something to do, exit after one success.
"$BIN" listen --addr "127.0.0.1:$PORT" --secret "$SECRET" --fail-first 1 --max 1 &
LISTEN_PID=$!
for _ in $(seq 1 50); do # wait for the receiver socket to accept
  if (exec 3<>"/dev/tcp/127.0.0.1/$PORT") 2>/dev/null; then break; fi
  sleep 0.1
done

"$BIN" deliver --drain
wait "$LISTEN_PID"

echo
"$BIN" status
echo
echo "walkthrough complete: one failure, one retry, one verified delivery"
