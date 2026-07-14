// Package verify gives webhook receivers a dependency-free way to
// authenticate hookmill deliveries: constant-time HMAC-SHA256 checks,
// replay protection via timestamp tolerance, and rotation support
// (any signature in the header may match).
//
// Typical usage inside an http.Handler:
//
//	ev, err := verify.Request(r, secret, nil)
//	if err != nil { http.Error(w, "bad signature", 401); return }
//	// ev.ID, ev.Type, ev.Body are now authenticated.
//
// Or wrap a whole handler:
//
//	mux.Handle("/hooks", verify.Handler(secret, myHandler))
package verify

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/JaydenCJ/hookmill/internal/signature"
)

// Header names hookmill sends with every delivery.
const (
	HeaderID        = "Hookmill-Id"
	HeaderTimestamp = "Hookmill-Timestamp"
	HeaderSignature = "Hookmill-Signature"
	HeaderEvent     = "Hookmill-Event"
)

// DefaultTolerance is the accepted clock skew when Options.Tolerance is
// unset: 5 minutes in either direction.
const DefaultTolerance = signature.DefaultTolerance

// DefaultMaxBody caps how many request-body bytes Request reads when
// Options.MaxBody is unset (1 MiB).
const DefaultMaxBody = int64(1 << 20)

// Options tunes verification. The zero value (or nil) uses the defaults.
type Options struct {
	// Tolerance is the maximum |now - signed timestamp| accepted.
	Tolerance time.Duration
	// Now supplies the clock (tests); defaults to time.Now.
	Now func() time.Time
	// MaxBody caps the request body size Request will read.
	MaxBody int64
	// Secrets lists additional accepted secrets (rotation windows).
	Secrets []string
}

// Event is a successfully verified delivery.
type Event struct {
	ID        string
	Type      string
	Timestamp time.Time
	Body      []byte
}

func (o *Options) now() time.Time {
	if o != nil && o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *Options) tolerance() time.Duration {
	if o != nil && o.Tolerance > 0 {
		return o.Tolerance
	}
	return DefaultTolerance
}

func (o *Options) maxBody() int64 {
	if o != nil && o.MaxBody > 0 {
		return o.MaxBody
	}
	return DefaultMaxBody
}

func (o *Options) secrets(primary string) []string {
	out := []string{primary}
	if o != nil {
		out = append(out, o.Secrets...)
	}
	return out
}

// Payload verifies raw pieces (already-extracted headers and body).
// It returns nil only when the timestamp is within tolerance AND at
// least one signature in sigHeader matches one accepted secret.
func Payload(secret, id string, ts int64, sigHeader string, body []byte, opt *Options) error {
	return signature.VerifyAt(opt.secrets(secret), id, ts, sigHeader, body, opt.now(), opt.tolerance())
}

// Request reads and verifies an incoming *http.Request. On success the
// request body is replaced with a re-readable copy and the authenticated
// event is returned; on failure the error says which check failed.
func Request(r *http.Request, secret string, opt *Options) (*Event, error) {
	id := r.Header.Get(HeaderID)
	if id == "" {
		return nil, fmt.Errorf("missing %s header", HeaderID)
	}
	tsRaw := r.Header.Get(HeaderTimestamp)
	if tsRaw == "" {
		return nil, fmt.Errorf("missing %s header", HeaderTimestamp)
	}
	ts, err := signature.ParseTimestamp(tsRaw)
	if err != nil {
		return nil, err
	}
	sig := r.Header.Get(HeaderSignature)
	if sig == "" {
		return nil, fmt.Errorf("missing %s header", HeaderSignature)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, opt.maxBody()+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > opt.maxBody() {
		return nil, fmt.Errorf("body exceeds %d bytes", opt.maxBody())
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err := Payload(secret, id, ts, sig, body, opt); err != nil {
		return nil, err
	}
	return &Event{
		ID:        id,
		Type:      r.Header.Get(HeaderEvent),
		Timestamp: time.Unix(ts, 0).UTC(),
		Body:      body,
	}, nil
}

// Handler wraps next so it only runs for authentic deliveries; anything
// else gets 401 with no detail (details would help an attacker probe).
// The verified body is re-readable from r.Body inside next.
func Handler(secret string, next http.Handler) http.Handler {
	return HandlerWithOptions(secret, nil, next)
}

// HandlerWithOptions is Handler with explicit Options.
func HandlerWithOptions(secret string, opt *Options, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := Request(r, secret, opt); err != nil {
			http.Error(w, "signature verification failed", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
