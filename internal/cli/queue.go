package cli

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/JaydenCJ/hookmill/internal/deliver"
	"github.com/JaydenCJ/hookmill/internal/store"
)

func cmdEnqueue(args []string, stdout, stderr io.Writer) int {
	fs, dir := newFlagSet("enqueue", stderr)
	eventType := fs.String("type", "", "event type, e.g. user.created (required)")
	data := fs.String("data", "", "payload; omit (or use '-') to read from stdin")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return parseErr(err)
	}
	if len(pos) != 1 {
		return usageErr(stderr, "usage: hookmill enqueue <endpoint> --type T [--data JSON]")
	}
	if *eventType == "" {
		return usageErr(stderr, "enqueue: --type is required")
	}
	body := []byte(*data)
	if *data == "" || *data == "-" {
		body, err = readStdin()
		if err != nil {
			return runtimeErr(stderr, err)
		}
	}
	s, code := openStore(*dir, stderr)
	if s == nil {
		return code
	}
	defer s.Close()
	m, err := s.Enqueue(pos[0], *eventType, body, time.Now())
	if err != nil {
		return runtimeErr(stderr, err)
	}
	fmt.Fprintf(stdout, "enqueued %s → %s (%s, %s, due now)\n",
		m.ID, m.Endpoint, m.Type, plural(int64(len(m.Body)), "byte"))
	return exitOK
}

// stdinReader is swapped in tests so enqueue/sign/verify can be driven
// without a real pipe.
var stdinReader io.Reader

func readStdin() ([]byte, error) {
	r := stdinReader
	if r == nil {
		r = io.Reader(osStdin())
	}
	body, err := io.ReadAll(io.LimitReader(r, maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read stdin: %w", err)
	}
	if len(body) > maxBodyBytes {
		return nil, fmt.Errorf("payload exceeds %d bytes", maxBodyBytes)
	}
	return body, nil
}

func cmdDeliver(args []string, stdout, stderr io.Writer) int {
	fs, dir := newFlagSet("deliver", stderr)
	limit := fs.Int("limit", 0, "max messages to attempt this run (0 = all due)")
	drain := fs.Bool("drain", false, "keep running rounds until nothing is due")
	timeout := fs.Duration("timeout", 10*time.Second, "per-request timeout")
	if err := fs.Parse(args); err != nil {
		return parseErr(err)
	}
	s, code := openStore(*dir, stderr)
	if s == nil {
		return code
	}
	defer s.Close()
	engine := &deliver.Engine{
		Store: s,
		// No proxy: hookmill talks only to the URLs you configured.
		Client: &http.Client{Timeout: *timeout, Transport: &http.Transport{Proxy: nil}},
	}
	var results []deliver.Result
	var err error
	if *drain {
		results, err = engine.Drain(*limit)
	} else {
		results, err = engine.RunOnce(*limit)
	}
	for _, r := range results {
		printResult(stdout, r)
	}
	if err != nil {
		return runtimeErr(stderr, err)
	}
	if len(results) == 0 {
		pending, _, _, nextDue := s.Counts()
		if pending == 0 {
			fmt.Fprintln(stdout, "nothing due (queue empty)")
		} else {
			fmt.Fprintf(stdout, "nothing due (%d pending, next due %s)\n", pending, fmtTime(nextDue))
		}
		return exitOK
	}
	var delivered, retried, dead int
	for _, r := range results {
		switch r.Outcome {
		case store.OutcomeDelivered:
			delivered++
		case store.OutcomeRetry:
			retried++
		case store.OutcomeDead:
			dead++
		}
	}
	fmt.Fprintf(stdout, "summary: %d delivered, %d retried, %d dead\n", delivered, retried, dead)
	return exitOK
}

func printResult(w io.Writer, r deliver.Result) {
	status := fmt.Sprintf("%d", r.Status)
	if r.Err != "" {
		status = "error: " + r.Err
	}
	switch r.Outcome {
	case store.OutcomeDelivered:
		fmt.Fprintf(w, "%s  %s  attempt %d  →  %s  delivered  (%dms)\n",
			r.MessageID, r.Endpoint, r.AttemptNo, status, r.Duration.Milliseconds())
	case store.OutcomeRetry:
		fmt.Fprintf(w, "%s  %s  attempt %d  →  %s  retry (due %s)\n",
			r.MessageID, r.Endpoint, r.AttemptNo, status, fmtTime(r.NextDue))
	case store.OutcomeDead:
		fmt.Fprintf(w, "%s  %s  attempt %d  →  %s  dead (schedule exhausted)\n",
			r.MessageID, r.Endpoint, r.AttemptNo, status)
	}
}
