// Tests for the signing scheme. The header format and signed-content
// layout are wire compatibility: several tests pin them down with
// independently-computed vectors so a refactor cannot silently change
// what receivers must verify.
package signature

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	testSecret = "hmsec_test-secret"
	testID     = "msg_0123456789abcdef"
	testTS     = int64(1752380000)
)

var testBody = []byte(`{"hello":"world"}`)

// independentSig recomputes the expected signature without going
// through the code under test, locking the id.ts.body content format.
func independentSig(secret, id string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(id + "." + "1752380000" + "."))
	if ts != testTS {
		panic("independentSig only supports the fixed test timestamp")
	}
	mac.Write(body)
	return "v1=" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func TestSignMatchesIndependentComputation(t *testing.T) {
	got := Sign(testSecret, testID, testTS, testBody)
	if want := independentSig(testSecret, testID, testTS, testBody); got != want {
		t.Fatalf("Sign = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "v1=") {
		t.Fatalf("signature %q lacks v1= prefix", got)
	}
	if again := Sign(testSecret, testID, testTS, testBody); again != got {
		t.Fatalf("same inputs signed differently: %q vs %q", again, got)
	}
}

func TestVerifyAcceptsValidSignature(t *testing.T) {
	header := Sign(testSecret, testID, testTS, testBody)
	if err := Verify([]string{testSecret}, testID, testTS, header, testBody); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
}

func TestVerifyRejectsAnyMutation(t *testing.T) {
	// Every component is bound into the MAC: a captured signature must
	// not authenticate a different secret, body, message id (replay
	// under a new id), or timestamp.
	header := Sign(testSecret, testID, testTS, testBody)
	cases := []struct {
		name       string
		secret, id string
		ts         int64
		body       []byte
	}{
		{"wrong secret", "hmsec_other", testID, testTS, testBody},
		{"tampered body", testSecret, testID, testTS, []byte(`{"hello":"worle"}`)},
		{"id swap", testSecret, "msg_ffffffffffffffff", testTS, testBody},
		{"timestamp shift", testSecret, testID, testTS + 1, testBody},
	}
	for _, c := range cases {
		err := Verify([]string{c.secret}, c.id, c.ts, header, c.body)
		if !errors.Is(err, ErrNoMatch) {
			t.Fatalf("%s: want ErrNoMatch, got %v", c.name, err)
		}
	}
}

func TestVerifyMalformedHeaders(t *testing.T) {
	err := Verify([]string{testSecret}, testID, testTS, "   ", testBody)
	if !errors.Is(err, ErrMissingSignature) {
		t.Fatalf("want ErrMissingSignature, got %v", err)
	}
	for _, header := range []string{"v2=abcd", "nonsense", "v1=!!!not-base64!!!"} {
		err := Verify([]string{testSecret}, testID, testTS, header, testBody)
		if !errors.Is(err, ErrMalformedHeader) {
			t.Fatalf("header %q: want ErrMalformedHeader, got %v", header, err)
		}
	}
}

func TestSignAllRotationWindow(t *testing.T) {
	// During rotation the header carries one signature per secret and a
	// receiver knowing EITHER secret must succeed.
	oldSecret := "hmsec_old"
	header := SignAll([]string{testSecret, oldSecret}, testID, testTS, testBody)
	if got := len(strings.Fields(header)); got != 2 {
		t.Fatalf("want 2 signatures in header, got %d (%q)", got, header)
	}
	if err := Verify([]string{testSecret}, testID, testTS, header, testBody); err != nil {
		t.Fatalf("new secret should verify: %v", err)
	}
	if err := Verify([]string{oldSecret}, testID, testTS, header, testBody); err != nil {
		t.Fatalf("old secret should verify during rotation: %v", err)
	}
}

func TestVerifyEdgeCaseBodies(t *testing.T) {
	// Empty and multi-byte-unicode bodies must sign and verify
	// byte-exactly — the scheme treats the body as opaque bytes.
	for _, body := range [][]byte{nil, []byte(`{"msg":"支払い完了 ✅"}`)} {
		header := Sign(testSecret, testID, testTS, body)
		if err := Verify([]string{testSecret}, testID, testTS, header, body); err != nil {
			t.Fatalf("body %q should verify: %v", body, err)
		}
	}
}

func TestVerifyAtWithinTolerance(t *testing.T) {
	now := time.Unix(testTS+120, 0)
	header := Sign(testSecret, testID, testTS, testBody)
	if err := VerifyAt([]string{testSecret}, testID, testTS, header, testBody, now, 5*time.Minute); err != nil {
		t.Fatalf("2 minutes of skew inside a 5m tolerance must pass: %v", err)
	}
}

func TestVerifyAtRejectsSkewInBothDirections(t *testing.T) {
	// A stale timestamp is a replay risk; one far in the future is just
	// as suspicious. Tolerance applies symmetrically.
	header := Sign(testSecret, testID, testTS, testBody)
	for _, now := range []time.Time{time.Unix(testTS+3600, 0), time.Unix(testTS-3600, 0)} {
		err := VerifyAt([]string{testSecret}, testID, testTS, header, testBody, now, 5*time.Minute)
		if !errors.Is(err, ErrTimestampSkew) {
			t.Fatalf("now=%v: want ErrTimestampSkew, got %v", now, err)
		}
	}
}

func TestVerifyAtZeroToleranceUsesDefault(t *testing.T) {
	now := time.Unix(testTS+240, 0) // 4 minutes of skew, inside the 5m default
	header := Sign(testSecret, testID, testTS, testBody)
	if err := VerifyAt([]string{testSecret}, testID, testTS, header, testBody, now, 0); err != nil {
		t.Fatalf("tolerance 0 should fall back to the 5m default: %v", err)
	}
}

func TestGenerateSecretShapeAndUniqueness(t *testing.T) {
	a, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a, SecretPrefix) {
		t.Fatalf("secret %q lacks %q prefix", a, SecretPrefix)
	}
	if len(a) != len(SecretPrefix)+32 { // 24 bytes → 32 base64url chars
		t.Fatalf("secret length = %d, want %d", len(a), len(SecretPrefix)+32)
	}
	if a == b {
		t.Fatal("two generated secrets are identical")
	}
}

func TestParseTimestamp(t *testing.T) {
	ts, err := ParseTimestamp(" 1752380000 ")
	if err != nil || ts != testTS {
		t.Fatalf("ParseTimestamp = %d, %v", ts, err)
	}
	if _, err := ParseTimestamp("not-a-number"); err == nil {
		t.Fatal("garbage timestamp must error")
	}
}
