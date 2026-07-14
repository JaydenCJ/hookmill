// Command hookmill is an outbound webhook delivery service in a single
// binary: HMAC-signed deliveries, retry with backoff, a dead-letter
// queue, and a receiver-side verifier — all state in one file-backed
// write-ahead log.
package main

import (
	"os"

	"github.com/JaydenCJ/hookmill/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
