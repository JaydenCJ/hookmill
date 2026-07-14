package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/JaydenCJ/hookmill/internal/signature"
)

func osStdin() *os.File { return os.Stdin }

// signFlagSet builds the flags shared by `sign` and `verify` (no --dir:
// both are pure functions over their inputs and never touch the store).
func signFlagSet(name string, stderr io.Writer) (*flag.FlagSet, *string, *string, *string) {
	fs := flag.NewFlagSet("hookmill "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	secret := fs.String("secret", "", "signing secret (required)")
	id := fs.String("id", "", "message id (required)")
	ts := fs.String("timestamp", "", "unix seconds (sign: default now)")
	return fs, secret, id, ts
}

func cmdSign(args []string, stdout, stderr io.Writer) int {
	fs, secret, id, tsRaw := signFlagSet("sign", stderr)
	if err := fs.Parse(args); err != nil {
		return parseErr(err)
	}
	if *secret == "" || *id == "" {
		return usageErr(stderr, "usage: hookmill sign --secret S --id ID [--timestamp T] < payload")
	}
	ts := time.Now().Unix()
	if *tsRaw != "" {
		var err error
		if ts, err = signature.ParseTimestamp(*tsRaw); err != nil {
			return usageErr(stderr, "%v", err)
		}
	}
	body, err := readStdin()
	if err != nil {
		return runtimeErr(stderr, err)
	}
	fmt.Fprintf(stdout, "Hookmill-Id: %s\n", *id)
	fmt.Fprintf(stdout, "Hookmill-Timestamp: %d\n", ts)
	fmt.Fprintf(stdout, "Hookmill-Signature: %s\n", signature.Sign(*secret, *id, ts, body))
	return exitOK
}

func cmdVerify(args []string, stdout, stderr io.Writer) int {
	fs, secret, id, tsRaw := signFlagSet("verify", stderr)
	sig := fs.String("signature", "", "Hookmill-Signature header value (required)")
	tolerance := fs.String("tolerance", "5m", "max clock skew, or 'none' to skip the timestamp check")
	if err := fs.Parse(args); err != nil {
		return parseErr(err)
	}
	if *secret == "" || *id == "" || *tsRaw == "" || *sig == "" {
		return usageErr(stderr, "usage: hookmill verify --secret S --id ID --timestamp T --signature SIG < payload")
	}
	ts, err := signature.ParseTimestamp(*tsRaw)
	if err != nil {
		return usageErr(stderr, "%v", err)
	}
	body, err := readStdin()
	if err != nil {
		return runtimeErr(stderr, err)
	}
	secrets := []string{*secret}
	if *tolerance == "none" {
		err = signature.Verify(secrets, *id, ts, *sig, body)
	} else {
		tol, perr := time.ParseDuration(*tolerance)
		if perr != nil {
			return usageErr(stderr, "bad --tolerance %q: %v", *tolerance, perr)
		}
		err = signature.VerifyAt(secrets, *id, ts, *sig, body, time.Now(), tol)
	}
	if err != nil {
		fmt.Fprintf(stdout, "signature INVALID: %v\n", err)
		return exitFail
	}
	fmt.Fprintf(stdout, "signature OK (%s, ts %d, %s)\n", *id, ts, plural(int64(len(body)), "byte"))
	return exitOK
}
