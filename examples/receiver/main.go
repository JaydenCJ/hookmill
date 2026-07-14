// Command receiver is a minimal webhook consumer built on hookmill's
// importable verify package: it authenticates every delivery, prints
// the event, and acknowledges with 204. Point an endpoint at it:
//
//	go run ./examples/receiver -addr 127.0.0.1:9911 -secret hmsec_yoursecret
//	hookmill endpoint add demo --url http://127.0.0.1:9911/hooks --secret hmsec_yoursecret
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/JaydenCJ/hookmill/verify"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9911", "listen address")
	secret := flag.String("secret", "", "endpoint signing secret (required)")
	flag.Parse()
	if *secret == "" {
		fmt.Fprintln(os.Stderr, "receiver: -secret is required")
		os.Exit(2)
	}

	http.HandleFunc("/hooks", func(w http.ResponseWriter, r *http.Request) {
		// One call authenticates the delivery: constant-time HMAC check,
		// replay protection via the timestamp, rotation-aware.
		ev, err := verify.Request(r, *secret, nil)
		if err != nil {
			log.Printf("rejected: %v", err)
			http.Error(w, "signature verification failed", http.StatusUnauthorized)
			return
		}
		log.Printf("event %s (%s): %s", ev.ID, ev.Type, ev.Body)
		// Only a 2xx acknowledges; anything else makes hookmill retry.
		w.WriteHeader(http.StatusNoContent)
	})

	log.Printf("receiving hookmill deliveries on http://%s/hooks", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
