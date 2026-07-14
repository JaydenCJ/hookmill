package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/JaydenCJ/hookmill/verify"
)

// cmdListen runs a loopback test receiver: it verifies every incoming
// delivery against the given secret and prints one line per request.
// It exists so you can watch real signed deliveries land without
// writing a receiver first — and so the smoke test has a peer.
func cmdListen(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hookmill listen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:8811", "listen address (loopback only unless --allow-remote)")
	secret := fs.String("secret", "", "secret to verify against (required)")
	maxOK := fs.Int("max", 0, "exit after this many successful (2xx) verified deliveries; --fail-first 500s do not count (0 = run until killed)")
	tolerance := fs.Duration("tolerance", verify.DefaultTolerance, "accepted clock skew")
	failFirst := fs.Int("fail-first", 0, "respond 500 to the first N verified deliveries (for testing retries)")
	allowRemote := fs.Bool("allow-remote", false, "permit binding a non-loopback address")
	if err := fs.Parse(args); err != nil {
		return parseErr(err)
	}
	if *secret == "" {
		return usageErr(stderr, "listen: --secret is required")
	}
	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return usageErr(stderr, "bad --addr %q: %v", *addr, err)
	}
	if !*allowRemote && !isLoopbackHost(host) {
		return usageErr(stderr, "refusing to bind non-loopback address %q (pass --allow-remote to override)", *addr)
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return runtimeErr(stderr, err)
	}
	fmt.Fprintf(stdout, "listening on http://%s (verifying hookmill deliveries)\n", ln.Addr())

	var (
		mu     sync.Mutex
		okSeen int
		failed int
		srv    = &http.Server{ReadHeaderTimeout: 10 * time.Second}
	)
	opts := &verify.Options{Tolerance: *tolerance}
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ev, verr := verify.Request(r, *secret, opts)
		mu.Lock()
		defer mu.Unlock()
		if verr != nil {
			fmt.Fprintf(stdout, "bad  %v\n", verr)
			http.Error(w, "signature verification failed", http.StatusUnauthorized)
			return
		}
		if failed < *failFirst {
			failed++
			fmt.Fprintf(stdout, "500  %s  %s  (--fail-first %d/%d)\n", ev.ID, ev.Type, failed, *failFirst)
			http.Error(w, "synthetic failure", http.StatusInternalServerError)
			return
		}
		okSeen++
		fmt.Fprintf(stdout, "ok   %s  %s  %s\n", ev.ID, ev.Type, plural(int64(len(ev.Body)), "byte"))
		w.WriteHeader(http.StatusNoContent)
		if *maxOK > 0 && okSeen >= *maxOK {
			go srv.Shutdown(context.Background())
		}
	})
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return runtimeErr(stderr, err)
	}
	return exitOK
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
