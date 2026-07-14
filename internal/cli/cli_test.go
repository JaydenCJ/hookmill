// In-process integration tests for the CLI: every command runs through
// cli.Run with captured writers, real temp data dirs, and httptest
// receivers on 127.0.0.1. Zero-delay schedules keep retry tests instant.
package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/JaydenCJ/hookmill/verify"
)

// run invokes the CLI in-process, optionally with injected stdin.
func run(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut strings.Builder
	prev := stdinReader
	if stdin != "" {
		stdinReader = strings.NewReader(stdin)
	}
	code := Run(args, &out, &errOut)
	stdinReader = prev
	return code, out.String(), errOut.String()
}

// mustRun asserts exit 0 and returns stdout.
func mustRun(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	code, out, errOut := run(t, stdin, args...)
	if code != 0 {
		t.Fatalf("hookmill %v exited %d\nstdout: %s\nstderr: %s", args, code, out, errOut)
	}
	return out
}

// initDir creates a fresh data dir with the given schedule.
func initDir(t *testing.T, schedule string) string {
	t.Helper()
	dir := t.TempDir() + "/data"
	mustRun(t, "", "init", "--dir", dir, "--schedule", schedule)
	return dir
}

// addEndpoint registers an endpoint and returns nothing; the fixed
// secret keeps assertions simple.
func addEndpoint(t *testing.T, dir, name, url, secret string) {
	t.Helper()
	mustRun(t, "", "endpoint", "add", name, "--dir", dir, "--url", url, "--secret", secret)
}

var msgIDRe = regexp.MustCompile(`msg_[0-9a-f]{16}`)

func enqueue(t *testing.T, dir, endpoint, eventType, data string) string {
	t.Helper()
	out := mustRun(t, "", "enqueue", endpoint, "--dir", dir, "--type", eventType, "--data", data)
	id := msgIDRe.FindString(out)
	if id == "" {
		t.Fatalf("no message id in enqueue output: %s", out)
	}
	return id
}

func TestVersionPrintsSemver(t *testing.T) {
	out := mustRun(t, "", "version")
	if out != "hookmill 0.1.0\n" {
		t.Fatalf("version output = %q", out)
	}
	if out2 := mustRun(t, "", "--version"); out2 != out {
		t.Fatalf("--version differs: %q", out2)
	}
}

func TestHelpAndUsageErrors(t *testing.T) {
	out := mustRun(t, "", "help")
	if !strings.Contains(out, "hookmill <command>") {
		t.Fatalf("help output = %q", out)
	}
	code, _, errOut := run(t, "")
	if code != 2 || !strings.Contains(errOut, "Usage") {
		t.Fatalf("no args: code=%d stderr=%q", code, errOut)
	}
	code, _, errOut = run(t, "", "explode")
	if code != 2 || !strings.Contains(errOut, "unknown command") {
		t.Fatalf("unknown command: code=%d stderr=%q", code, errOut)
	}
	// Asking a subcommand for help is a success, not a usage error.
	code, _, errOut = run(t, "", "deliver", "-h")
	if code != 0 || !strings.Contains(errOut, "-drain") {
		t.Fatalf("deliver -h: code=%d stderr=%q", code, errOut)
	}
}

func TestInitReportsSchedule(t *testing.T) {
	dir := t.TempDir() + "/data"
	out := mustRun(t, "", "init", "--dir", dir, "--schedule", "5s,30s")
	if !strings.Contains(out, "5s,30s") || !strings.Contains(out, "max 3 attempts") {
		t.Fatalf("init output = %q", out)
	}
}

func TestInitErrors(t *testing.T) {
	dir := initDir(t, "5s")
	code, _, errOut := run(t, "", "init", "--dir", dir)
	if code != 3 || !strings.Contains(errOut, "already initialized") {
		t.Fatalf("double init: code=%d stderr=%q", code, errOut)
	}
	if code, _, _ := run(t, "", "init", "--dir", t.TempDir()+"/x", "--schedule", "eventually"); code != 2 {
		t.Fatalf("bad schedule should exit 2, got %d", code)
	}
}

func TestCommandsWithoutInitPointAtInit(t *testing.T) {
	code, _, errOut := run(t, "", "status", "--dir", t.TempDir()+"/nope")
	if code != 3 || !strings.Contains(errOut, "hookmill init") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

func TestEndpointAddPrintsSecretOnce(t *testing.T) {
	dir := initDir(t, "5s")
	out := mustRun(t, "", "endpoint", "add", "orders", "--dir", dir, "--url", "http://127.0.0.1:9/hooks")
	if !strings.Contains(out, "secret  hmsec_") {
		t.Fatalf("generated secret missing from output: %s", out)
	}
	// list hides secrets unless asked
	list := mustRun(t, "", "endpoint", "list", "--dir", dir)
	if strings.Contains(list, "hmsec_") {
		t.Fatalf("plain list must not leak secrets: %s", list)
	}
	withSecrets := mustRun(t, "", "endpoint", "list", "--dir", dir, "--show-secrets")
	if !strings.Contains(withSecrets, "hmsec_") {
		t.Fatalf("--show-secrets should show them: %s", withSecrets)
	}
	// Rotation prints the replacement secret the same way.
	rotate := mustRun(t, "", "endpoint", "rotate", "orders", "--dir", dir)
	if !strings.Contains(rotate, "new secret  hmsec_") {
		t.Fatalf("rotate output = %s", rotate)
	}
}

func TestEndpointListJSON(t *testing.T) {
	dir := initDir(t, "5s")
	addEndpoint(t, dir, "orders", "http://127.0.0.1:9/hooks", "hmsec_x")
	out := mustRun(t, "", "endpoint", "list", "--dir", dir, "--format", "json")
	if !strings.Contains(out, `"name": "orders"`) || !strings.Contains(out, `"num_secrets": 1`) {
		t.Fatalf("json list = %s", out)
	}
	if strings.Contains(out, "hmsec_x") {
		t.Fatalf("json list must not leak secrets by default: %s", out)
	}
}

func TestEndpointAddRequiresURL(t *testing.T) {
	dir := initDir(t, "5s")
	code, _, errOut := run(t, "", "endpoint", "add", "orders", "--dir", dir)
	if code != 2 || !strings.Contains(errOut, "--url is required") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

func TestEnqueueDeliverRoundTripWithReceiverVerification(t *testing.T) {
	const secret = "hmsec_cli-test"
	var received *verify.Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ev, err := verify.Request(r, secret, nil)
		if err != nil {
			t.Errorf("receiver-side verification failed: %v", err)
			http.Error(w, "bad", http.StatusUnauthorized)
			return
		}
		received = ev
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	dir := initDir(t, "5s")
	addEndpoint(t, dir, "orders", srv.URL, secret)
	id := enqueue(t, dir, "orders", "invoice.paid", `{"total":42}`)

	out := mustRun(t, "", "deliver", "--dir", dir)
	if !strings.Contains(out, "delivered") || !strings.Contains(out, "summary: 1 delivered, 0 retried, 0 dead") {
		t.Fatalf("deliver output = %s", out)
	}
	if received == nil || received.ID != id || received.Type != "invoice.paid" || string(received.Body) != `{"total":42}` {
		t.Fatalf("received = %+v", received)
	}
}

func TestEnqueueReadsStdinWhenNoData(t *testing.T) {
	dir := initDir(t, "5s")
	addEndpoint(t, dir, "orders", "http://127.0.0.1:9/h", "hmsec_x")
	out := mustRun(t, `{"from":"stdin"}`, "enqueue", "orders", "--dir", dir, "--type", "e.t")
	if !strings.Contains(out, "16 bytes") {
		t.Fatalf("stdin body length not reflected: %s", out)
	}
}

func TestFailRetryDeadRequeueRecoverCycle(t *testing.T) {
	// The whole reliability story in one test: a receiver that fails
	// twice then recovers, a 0s,0s schedule (3 attempts), dead-letter
	// after a permanent failure, requeue, and final success.
	var calls int
	fail := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if fail {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	dir := initDir(t, "0s,0s")
	addEndpoint(t, dir, "orders", srv.URL, "hmsec_x")
	id := enqueue(t, dir, "orders", "job.done", `{}`)

	// Drain while the receiver is down: 3 attempts, then dead.
	out := mustRun(t, "", "deliver", "--dir", dir, "--drain")
	if !strings.Contains(out, "summary: 0 delivered, 2 retried, 1 dead") {
		t.Fatalf("drain output = %s", out)
	}
	if calls != 3 {
		t.Fatalf("receiver saw %d calls, want 3", calls)
	}

	deadOut := mustRun(t, "", "dead", "--dir", dir)
	if !strings.Contains(deadOut, id) || !strings.Contains(deadOut, "retry schedule exhausted") {
		t.Fatalf("dead list = %s", deadOut)
	}

	// Receiver recovers; requeue and deliver.
	fail = false
	mustRun(t, "", "requeue", id, "--dir", dir)
	out = mustRun(t, "", "deliver", "--dir", dir)
	if !strings.Contains(out, "delivered") {
		t.Fatalf("post-requeue deliver = %s", out)
	}
	if empty := mustRun(t, "", "dead", "--dir", dir); !strings.Contains(empty, "dead-letter queue is empty") {
		t.Fatalf("dead list after recovery = %s", empty)
	}
	// With everything delivered, another run has nothing to do.
	out = mustRun(t, "", "deliver", "--dir", dir)
	if !strings.Contains(out, "nothing due (queue empty)") {
		t.Fatalf("deliver output = %s", out)
	}
}

func TestInspectShowsAttemptHistory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()

	dir := initDir(t, "1h")
	addEndpoint(t, dir, "orders", srv.URL, "hmsec_x")
	id := enqueue(t, dir, "orders", "e.t", `{"n":1}`)
	mustRun(t, "", "deliver", "--dir", dir)

	out := mustRun(t, "", "inspect", id, "--dir", dir)
	for _, want := range []string{"state      pending", "attempts", "502", `{"n":1}`} {
		if !strings.Contains(out, want) {
			t.Fatalf("inspect missing %q:\n%s", want, out)
		}
	}
	jsonOut := mustRun(t, "", "inspect", id, "--dir", dir, "--format", "json")
	if !strings.Contains(jsonOut, `"fail_streak": 1`) {
		t.Fatalf("inspect json = %s", jsonOut)
	}
}

func TestStatusCountsAndJSON(t *testing.T) {
	dir := initDir(t, "5s,30s")
	addEndpoint(t, dir, "orders", "http://127.0.0.1:9/h", "hmsec_x")
	enqueue(t, dir, "orders", "e.t", `{}`)
	out := mustRun(t, "", "status", "--dir", dir)
	for _, want := range []string{"endpoints   1", "pending     1", "schedule    5s,30s  (max 3 attempts)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status missing %q:\n%s", want, out)
		}
	}
	jsonOut := mustRun(t, "", "status", "--dir", dir, "--format", "json")
	if !strings.Contains(jsonOut, `"pending": 1`) || !strings.Contains(jsonOut, `"wal_records": 3`) {
		t.Fatalf("status json = %s", jsonOut)
	}
	if code, _, _ := run(t, "", "status", "--dir", dir, "--format", "yaml"); code != 2 {
		t.Fatalf("unknown format should exit 2, got %d", code)
	}
}

func TestRequeueUsageErrors(t *testing.T) {
	dir := initDir(t, "5s")
	if code, _, _ := run(t, "", "requeue", "--dir", dir); code != 2 {
		t.Fatal("requeue with neither ids nor --all must exit 2")
	}
	if code, _, _ := run(t, "", "requeue", "msg_x", "--all", "--dir", dir); code != 2 {
		t.Fatal("requeue with both ids and --all must exit 2")
	}
}

func TestCompactPreservesQueueAcrossCommands(t *testing.T) {
	dir := initDir(t, "5s")
	addEndpoint(t, dir, "orders", "http://127.0.0.1:9/h", "hmsec_x")
	for i := 0; i < 4; i++ {
		enqueue(t, dir, "orders", "e.t", fmt.Sprintf(`{"n":%d}`, i))
	}
	before := mustRun(t, "", "status", "--dir", dir)
	out := mustRun(t, "", "compact", "--dir", dir)
	if !strings.Contains(out, "6 records → 1 snapshot") {
		t.Fatalf("compact output = %s", out)
	}
	after := mustRun(t, "", "status", "--dir", dir)
	// Same counts, only the wal line differs.
	trim := func(s string) string {
		lines := strings.Split(s, "\n")
		return strings.Join(lines[:len(lines)-2], "\n")
	}
	if trim(before) != trim(after) {
		t.Fatalf("compact changed visible state:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestSignVerifyRoundTripThroughCLI(t *testing.T) {
	payload := `{"total":42}`
	out := mustRun(t, payload, "sign", "--secret", "hmsec_s", "--id", "msg_1", "--timestamp", "1752380000")
	var sig string
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "Hookmill-Signature: "); ok {
			sig = v
		}
	}
	if sig == "" {
		t.Fatalf("no signature line in sign output: %s", out)
	}
	vOut := mustRun(t, payload, "verify", "--secret", "hmsec_s", "--id", "msg_1",
		"--timestamp", "1752380000", "--signature", sig, "--tolerance", "none")
	if !strings.Contains(vOut, "signature OK") {
		t.Fatalf("verify output = %s", vOut)
	}
}

func TestVerifyFailuresExitOne(t *testing.T) {
	payload := `{"total":42}`
	out := mustRun(t, payload, "sign", "--secret", "hmsec_s", "--id", "msg_1", "--timestamp", "1752380000")
	var sig string
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "Hookmill-Signature: "); ok {
			sig = v
		}
	}
	// Wrong secret.
	code, vOut, _ := run(t, payload, "verify", "--secret", "hmsec_other", "--id", "msg_1",
		"--timestamp", "1752380000", "--signature", sig, "--tolerance", "none")
	if code != 1 || !strings.Contains(vOut, "signature INVALID") {
		t.Fatalf("wrong secret: code=%d out=%s", code, vOut)
	}
	// Stale timestamp under the default tolerance (2026 vs now).
	code, vOut, _ = run(t, payload, "verify", "--secret", "hmsec_s", "--id", "msg_1",
		"--timestamp", "1752380000", "--signature", sig)
	if code != 1 || !strings.Contains(vOut, "signature INVALID") {
		t.Fatalf("stale timestamp: code=%d out=%s", code, vOut)
	}
}

func TestSignRequiresSecretAndID(t *testing.T) {
	if code, _, _ := run(t, "x", "sign", "--secret", "s"); code != 2 {
		t.Fatal("sign without --id must exit 2")
	}
	if code, _, _ := run(t, "x", "verify", "--secret", "s", "--id", "i"); code != 2 {
		t.Fatal("verify without --timestamp/--signature must exit 2")
	}
}

func TestListenRefusesNonLoopbackAndMissingSecret(t *testing.T) {
	if code, _, _ := run(t, "", "listen"); code != 2 {
		t.Fatal("listen without --secret must exit 2")
	}
	code, _, errOut := run(t, "", "listen", "--secret", "s", "--addr", "0.0.0.0:8899")
	if code != 2 || !strings.Contains(errOut, "non-loopback") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

func TestEndpointRemoveDeadLettersViaCliOutput(t *testing.T) {
	dir := initDir(t, "5s")
	addEndpoint(t, dir, "orders", "http://127.0.0.1:9/h", "hmsec_x")
	enqueue(t, dir, "orders", "e.t", `{}`)
	out := mustRun(t, "", "endpoint", "remove", "orders", "--dir", dir)
	if !strings.Contains(out, "1 pending message dead-lettered") {
		t.Fatalf("remove output = %s", out)
	}
	dead := mustRun(t, "", "dead", "--dir", dir)
	if !strings.Contains(dead, "endpoint removed") {
		t.Fatalf("dead list = %s", dead)
	}
}
