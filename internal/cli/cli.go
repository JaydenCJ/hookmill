// Package cli implements the hookmill command-line interface: argument
// parsing, command dispatch, and human/JSON rendering. All I/O flows
// through injected writers so the full CLI is testable in-process.
//
// Exit codes: 0 success, 1 verification/check failure, 2 usage error,
// 3 runtime error.
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/JaydenCJ/hookmill/internal/backoff"
	"github.com/JaydenCJ/hookmill/internal/store"
	"github.com/JaydenCJ/hookmill/internal/version"
)

// Exit codes.
const (
	exitOK      = 0
	exitFail    = 1
	exitUsage   = 2
	exitRuntime = 3
)

// maxBodyBytes caps payloads read from --data or stdin.
const maxBodyBytes = 1 << 20

const usageText = `hookmill — outbound webhook delivery: sign, retry, dead-letter, file-backed

Usage: hookmill <command> [flags]

Queue:
  init                    create a data directory (default .hookmill, or $HOOKMILL_DIR)
  endpoint add|list|remove|rotate
                          manage delivery targets and signing secrets
  enqueue <endpoint>      queue a webhook (--type, --data or stdin)
  deliver                 attempt everything due now (--drain to loop)
  status                  queue totals and next due time
  inspect <message-id>    full message detail with attempt history
  dead                    list dead-lettered messages
  requeue <id>...|--all   put dead messages back in the queue
  compact                 rewrite the WAL as one snapshot record

Signing:
  sign                    sign a payload from stdin, print the headers
  verify                  verify headers + stdin payload (exit 1 on mismatch)
  listen                  loopback receiver that verifies incoming deliveries

Other:
  version                 print the version

Run 'hookmill <command> -h' for command flags.`

// Run executes one CLI invocation and returns its exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usageText)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "hookmill %s\n", version.Version)
		return exitOK
	case "help", "--help", "-h":
		fmt.Fprintln(stdout, usageText)
		return exitOK
	case "init":
		return cmdInit(rest, stdout, stderr)
	case "endpoint":
		return cmdEndpoint(rest, stdout, stderr)
	case "enqueue":
		return cmdEnqueue(rest, stdout, stderr)
	case "deliver":
		return cmdDeliver(rest, stdout, stderr)
	case "status":
		return cmdStatus(rest, stdout, stderr)
	case "inspect":
		return cmdInspect(rest, stdout, stderr)
	case "dead":
		return cmdDead(rest, stdout, stderr)
	case "requeue":
		return cmdRequeue(rest, stdout, stderr)
	case "compact":
		return cmdCompact(rest, stdout, stderr)
	case "sign":
		return cmdSign(rest, stdout, stderr)
	case "verify":
		return cmdVerify(rest, stdout, stderr)
	case "listen":
		return cmdListen(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "hookmill: unknown command %q\n\n%s\n", cmd, usageText)
		return exitUsage
	}
}

// newFlagSet builds a command flag set with the shared --dir flag.
func newFlagSet(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("hookmill "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", defaultDir(), "data directory")
	return fs, dir
}

// parseArgs parses flags while collecting positional arguments, so
// `hookmill inspect msg_x --dir data` works — Go's flag package alone
// stops at the first positional.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

// parseErr maps a flag-parse error to an exit code: `-h`/`--help` is a
// successful request for help (flag already printed the usage), not a
// usage error.
func parseErr(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	return exitUsage
}

// plural renders a count with its noun ("1 record", "3 records") so no
// message ever reads "1 records".
func plural(n int64, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func defaultDir() string {
	if d := os.Getenv("HOOKMILL_DIR"); d != "" {
		return d
	}
	return ".hookmill"
}

// openStore opens the data directory, surfacing torn-tail repairs as a
// warning rather than a failure (that is the WAL doing its job).
func openStore(dir string, stderr io.Writer) (*store.Store, int) {
	s, err := store.Open(dir)
	if err != nil {
		fmt.Fprintf(stderr, "hookmill: %v\n", err)
		return nil, exitRuntime
	}
	if s.TornTail {
		fmt.Fprintln(stderr, "hookmill: warning: repaired torn WAL tail (partial final record discarded)")
	}
	return s, exitOK
}

func runtimeErr(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "hookmill: %v\n", err)
	return exitRuntime
}

func usageErr(stderr io.Writer, format string, args ...any) int {
	fmt.Fprintf(stderr, "hookmill: "+format+"\n", args...)
	return exitUsage
}

func printJSON(w io.Writer, v any) int {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return runtimeErr(w, err)
	}
	fmt.Fprintln(w, string(out))
	return exitOK
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func cmdInit(args []string, stdout, stderr io.Writer) int {
	fs, dir := newFlagSet("init", stderr)
	schedFlag := fs.String("schedule", backoff.Default().String(),
		"retry schedule: comma-separated delays after each failure, or 'none'")
	if err := fs.Parse(args); err != nil {
		return parseErr(err)
	}
	sched, err := backoff.Parse(*schedFlag)
	if err != nil {
		return usageErr(stderr, "%v", err)
	}
	if err := store.Init(*dir, sched, time.Now()); err != nil {
		return runtimeErr(stderr, err)
	}
	fmt.Fprintf(stdout, "initialized %s (schedule %s — max %s per message)\n",
		*dir, sched, plural(int64(sched.MaxAttempts()), "attempt"))
	return exitOK
}

func cmdStatus(args []string, stdout, stderr io.Writer) int {
	fs, dir := newFlagSet("status", stderr)
	format := fs.String("format", "text", "output format: text or json")
	if err := fs.Parse(args); err != nil {
		return parseErr(err)
	}
	s, code := openStore(*dir, stderr)
	if s == nil {
		return code
	}
	defer s.Close()
	pending, delivered, dead, nextDue := s.Counts()
	size, err := s.WAL().Size()
	if err != nil {
		return runtimeErr(stderr, err)
	}
	switch *format {
	case "json":
		return printJSON(stdout, map[string]any{
			"data_dir": s.Dir(), "endpoints": len(s.EndpointList()),
			"pending": pending, "delivered": delivered, "dead": dead,
			"next_due": fmtTime(nextDue), "schedule": s.Schedule.String(),
			"max_attempts": s.Schedule.MaxAttempts(),
			"wal_records":  s.WAL().Count(), "wal_bytes": size,
		})
	case "text":
		fmt.Fprintf(stdout, "data dir    %s\n", s.Dir())
		fmt.Fprintf(stdout, "endpoints   %d\n", len(s.EndpointList()))
		if pending > 0 {
			fmt.Fprintf(stdout, "pending     %d  (next due %s)\n", pending, fmtTime(nextDue))
		} else {
			fmt.Fprintf(stdout, "pending     0\n")
		}
		fmt.Fprintf(stdout, "delivered   %d\n", delivered)
		fmt.Fprintf(stdout, "dead        %d\n", dead)
		fmt.Fprintf(stdout, "schedule    %s  (max %s)\n", s.Schedule, plural(int64(s.Schedule.MaxAttempts()), "attempt"))
		fmt.Fprintf(stdout, "wal         %s, %s\n", plural(int64(s.WAL().Count()), "record"), plural(size, "byte"))
		return exitOK
	default:
		return usageErr(stderr, "unknown --format %q (want text or json)", *format)
	}
}

func cmdInspect(args []string, stdout, stderr io.Writer) int {
	fs, dir := newFlagSet("inspect", stderr)
	format := fs.String("format", "text", "output format: text or json")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return parseErr(err)
	}
	if len(pos) != 1 {
		return usageErr(stderr, "usage: hookmill inspect <message-id>")
	}
	s, code := openStore(*dir, stderr)
	if s == nil {
		return code
	}
	defer s.Close()
	m := s.Message(pos[0])
	if m == nil {
		return runtimeErr(stderr, fmt.Errorf("unknown message %q", pos[0]))
	}
	if *format == "json" {
		return printJSON(stdout, m)
	}
	fmt.Fprintf(stdout, "id         %s\n", m.ID)
	fmt.Fprintf(stdout, "endpoint   %s\n", m.Endpoint)
	fmt.Fprintf(stdout, "type       %s\n", m.Type)
	fmt.Fprintf(stdout, "state      %s\n", m.State)
	if m.DeadReason != "" {
		fmt.Fprintf(stdout, "reason     %s\n", m.DeadReason)
	}
	fmt.Fprintf(stdout, "enqueued   %s\n", fmtTime(m.EnqueuedAt))
	if m.State == store.StatePending {
		fmt.Fprintf(stdout, "next due   %s\n", fmtTime(m.NextDue))
	}
	fmt.Fprintf(stdout, "body       %s\n", string(m.Body))
	if len(m.Attempts) > 0 {
		fmt.Fprintln(stdout, "attempts")
		for i, a := range m.Attempts {
			outcome := fmt.Sprintf("%d", a.Status)
			if a.Err != "" {
				outcome = "error: " + a.Err
			}
			fmt.Fprintf(stdout, "  %d  %s  %s  (%dms)\n", i+1, fmtTime(a.At), outcome, a.DurationMs)
		}
	}
	return exitOK
}

func cmdDead(args []string, stdout, stderr io.Writer) int {
	fs, dir := newFlagSet("dead", stderr)
	format := fs.String("format", "text", "output format: text or json")
	if err := fs.Parse(args); err != nil {
		return parseErr(err)
	}
	s, code := openStore(*dir, stderr)
	if s == nil {
		return code
	}
	defer s.Close()
	var dead []*store.Message
	for _, m := range s.MessageList() {
		if m.State == store.StateDead {
			dead = append(dead, m)
		}
	}
	if *format == "json" {
		if dead == nil {
			dead = []*store.Message{}
		}
		return printJSON(stdout, dead)
	}
	if len(dead) == 0 {
		fmt.Fprintln(stdout, "dead-letter queue is empty")
		return exitOK
	}
	fmt.Fprintf(stdout, "%-22s %-14s %-20s %-9s %s\n", "id", "endpoint", "type", "attempts", "reason")
	for _, m := range dead {
		fmt.Fprintf(stdout, "%-22s %-14s %-20s %-9d %s\n",
			m.ID, m.Endpoint, m.Type, len(m.Attempts), m.DeadReason)
	}
	return exitOK
}

func cmdRequeue(args []string, stdout, stderr io.Writer) int {
	fs, dir := newFlagSet("requeue", stderr)
	all := fs.Bool("all", false, "requeue every dead message")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return parseErr(err)
	}
	if *all == (len(pos) > 0) {
		return usageErr(stderr, "usage: hookmill requeue <message-id>... | --all")
	}
	s, code := openStore(*dir, stderr)
	if s == nil {
		return code
	}
	defer s.Close()
	ids := pos
	if *all {
		for _, m := range s.MessageList() {
			if m.State == store.StateDead {
				ids = append(ids, m.ID)
			}
		}
		if len(ids) == 0 {
			fmt.Fprintln(stdout, "dead-letter queue is empty")
			return exitOK
		}
	}
	for _, id := range ids {
		if _, err := s.Requeue(id, time.Now()); err != nil {
			return runtimeErr(stderr, err)
		}
		fmt.Fprintf(stdout, "requeued %s (due now)\n", id)
	}
	return exitOK
}

func cmdCompact(args []string, stdout, stderr io.Writer) int {
	fs, dir := newFlagSet("compact", stderr)
	if err := fs.Parse(args); err != nil {
		return parseErr(err)
	}
	s, code := openStore(*dir, stderr)
	if s == nil {
		return code
	}
	defer s.Close()
	before := s.WAL().Count()
	beforeSize, err := s.WAL().Size()
	if err != nil {
		return runtimeErr(stderr, err)
	}
	if err := s.Compact(time.Now()); err != nil {
		return runtimeErr(stderr, err)
	}
	afterSize, err := s.WAL().Size()
	if err != nil {
		return runtimeErr(stderr, err)
	}
	fmt.Fprintf(stdout, "compacted %s: %s → 1 snapshot (%d → %d bytes)\n",
		s.WAL().Path(), plural(int64(before), "record"), beforeSize, afterSize)
	return exitOK
}
