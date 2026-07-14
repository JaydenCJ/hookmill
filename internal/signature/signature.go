// Package signature implements hookmill's HMAC-SHA256 webhook signing
// scheme: the signed content format, header encoding, secret generation,
// and verification with timestamp tolerance and multi-secret rotation.
//
// The signed content is the exact byte sequence
//
//	<message id> "." <unix timestamp> "." <raw body>
//
// and the signature header carries one or more space-separated
// "v1=<base64>" entries — one per active secret — so receivers keep
// verifying during a secret rotation window.
package signature

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Scheme is the signature version tag receivers must expect.
const Scheme = "v1"

// SecretPrefix marks hookmill-generated secrets. Verification treats the
// whole secret string (prefix included) as the HMAC key, so any opaque
// string works as a secret.
const SecretPrefix = "hmsec_"

// DefaultTolerance is the maximum accepted clock skew between the signed
// timestamp and the receiver's clock, in either direction.
const DefaultTolerance = 5 * time.Minute

// Verification failure reasons, distinguishable with errors.Is.
var (
	ErrMissingSignature = errors.New("signature header is empty")
	ErrMalformedHeader  = errors.New("signature header is malformed")
	ErrNoMatch          = errors.New("no signature matched any known secret")
	ErrTimestampSkew    = errors.New("timestamp outside tolerance")
)

// GenerateSecret returns a fresh 192-bit random secret with the hmsec_
// prefix, e.g. "hmsec_3q2-7f…".
func GenerateSecret() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	return SecretPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// signedContent builds the byte sequence that is signed. The message id
// and timestamp are bound into the MAC so a captured body cannot be
// replayed under a different id or at a different time.
func signedContent(id string, ts int64, body []byte) []byte {
	prefix := id + "." + strconv.FormatInt(ts, 10) + "."
	out := make([]byte, 0, len(prefix)+len(body))
	out = append(out, prefix...)
	return append(out, body...)
}

// Sign returns a single "v1=<base64>" signature for one secret.
func Sign(secret, id string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(signedContent(id, ts, body))
	return Scheme + "=" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// SignAll returns the full header value: one signature per secret,
// space-separated, in the given order (newest secret first by convention).
func SignAll(secrets []string, id string, ts int64, body []byte) string {
	parts := make([]string, 0, len(secrets))
	for _, s := range secrets {
		parts = append(parts, Sign(s, id, ts, body))
	}
	return strings.Join(parts, " ")
}

// ParseTimestamp parses the Hookmill-Timestamp header value (unix seconds).
func ParseTimestamp(s string) (int64, error) {
	ts, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid timestamp %q: %w", s, err)
	}
	return ts, nil
}

// Verify checks the signature header against every provided secret and
// returns nil when any "v1=" entry matches any secret. It does NOT check
// the timestamp; use VerifyAt for full receiver-side verification.
func Verify(secrets []string, id string, ts int64, header string, body []byte) error {
	fields := strings.Fields(header)
	if len(fields) == 0 {
		return ErrMissingSignature
	}
	candidates := make([][]byte, 0, len(fields))
	for _, f := range fields {
		val, ok := strings.CutPrefix(f, Scheme+"=")
		if !ok {
			return fmt.Errorf("%w: entry %q lacks %q prefix", ErrMalformedHeader, f, Scheme+"=")
		}
		raw, err := base64.StdEncoding.DecodeString(val)
		if err != nil {
			return fmt.Errorf("%w: entry is not base64: %v", ErrMalformedHeader, err)
		}
		candidates = append(candidates, raw)
	}
	content := signedContent(id, ts, body)
	for _, secret := range secrets {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(content)
		want := mac.Sum(nil)
		for _, got := range candidates {
			if hmac.Equal(want, got) {
				return nil
			}
		}
	}
	return ErrNoMatch
}

// VerifyAt performs full verification as a receiver should: timestamp
// within tolerance of now (both directions, so slightly-fast sender
// clocks are fine), then MAC match. tolerance <= 0 selects
// DefaultTolerance.
func VerifyAt(secrets []string, id string, ts int64, header string, body []byte, now time.Time, tolerance time.Duration) error {
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	skew := now.Unix() - ts
	if skew < 0 {
		skew = -skew
	}
	if skew > int64(tolerance/time.Second) {
		return fmt.Errorf("%w: signed at %d, now %d (tolerance %s)", ErrTimestampSkew, ts, now.Unix(), tolerance)
	}
	return Verify(secrets, id, ts, header, body)
}
