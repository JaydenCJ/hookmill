// Tests for the public receiver-side helpers — the code third parties
// embed in their services, so the failure modes (missing headers, stale
// timestamps, oversized bodies) get as much attention as the happy path.
package verify

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JaydenCJ/hookmill/internal/signature"
)

const (
	secret = "hmsec_receiver-test"
	msgID  = "msg_00ff00ff00ff00ff"
)

var (
	t0   = time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	body = []byte(`{"event":"invoice.paid","total":42}`)
)

// signedRequest fabricates exactly what hookmill's engine sends.
func signedRequest(ts int64, sigBody []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/hooks", bytes.NewReader(body))
	r.Header.Set(HeaderID, msgID)
	r.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	r.Header.Set(HeaderEvent, "invoice.paid")
	r.Header.Set(HeaderSignature, signature.Sign(secret, msgID, ts, sigBody))
	return r
}

func opts() *Options {
	return &Options{Now: func() time.Time { return t0 }}
}

func TestRequestAcceptsAuthenticDelivery(t *testing.T) {
	ev, err := Request(signedRequest(t0.Unix(), body), secret, opts())
	if err != nil {
		t.Fatal(err)
	}
	if ev.ID != msgID || ev.Type != "invoice.paid" || string(ev.Body) != string(body) {
		t.Fatalf("event = %+v", ev)
	}
	if !ev.Timestamp.Equal(time.Unix(t0.Unix(), 0)) {
		t.Fatalf("timestamp = %v", ev.Timestamp)
	}
}

func TestRequestBodyIsRereadableAfterVerification(t *testing.T) {
	r := signedRequest(t0.Unix(), body)
	if _, err := Request(r, secret, opts()); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(body))
	n, _ := r.Body.Read(got)
	if string(got[:n]) != string(body) {
		t.Fatalf("body not re-readable: %q", got[:n])
	}
}

func TestRequestRejectsForgery(t *testing.T) {
	if _, err := Request(signedRequest(t0.Unix(), body), "hmsec_wrong", opts()); err == nil {
		t.Fatal("wrong secret must fail")
	}
	// Signature computed over different bytes than the request carries.
	r := signedRequest(t0.Unix(), []byte(`{"event":"invoice.paid","total":43}`))
	if _, err := Request(r, secret, opts()); err == nil {
		t.Fatal("tampered body must fail")
	}
}

func TestRequestRejectsStaleTimestamp(t *testing.T) {
	stale := t0.Add(-time.Hour).Unix()
	if _, err := Request(signedRequest(stale, body), secret, opts()); err == nil {
		t.Fatal("hour-old delivery must fail the default 5m tolerance")
	}
}

func TestRequestCustomToleranceAcceptsOlder(t *testing.T) {
	stale := t0.Add(-time.Hour).Unix()
	o := opts()
	o.Tolerance = 2 * time.Hour
	if _, err := Request(signedRequest(stale, body), secret, o); err != nil {
		t.Fatalf("2h tolerance should accept 1h-old delivery: %v", err)
	}
}

func TestRequestRejectsMissingHeaders(t *testing.T) {
	for _, drop := range []string{HeaderID, HeaderTimestamp, HeaderSignature} {
		r := signedRequest(t0.Unix(), body)
		r.Header.Del(drop)
		_, err := Request(r, secret, opts())
		if err == nil || !strings.Contains(err.Error(), "missing") {
			t.Fatalf("dropping %s: want missing-header error, got %v", drop, err)
		}
	}
}

func TestRequestRejectsOversizedBody(t *testing.T) {
	o := opts()
	o.MaxBody = 8
	if _, err := Request(signedRequest(t0.Unix(), body), secret, o); err == nil {
		t.Fatal("body over MaxBody must fail before verification")
	}
}

func TestRotationSecretInOptionsVerifies(t *testing.T) {
	// Receiver still configured with the OLD secret; sender signs with
	// old+new during the rotation window. Options.Secrets bridges it.
	newSecret := "hmsec_rotated"
	r := signedRequest(t0.Unix(), body)
	r.Header.Set(HeaderSignature, signature.SignAll([]string{newSecret, secret}, msgID, t0.Unix(), body))
	if _, err := Request(r, secret, opts()); err != nil {
		t.Fatalf("old secret should still verify: %v", err)
	}
	o := opts()
	o.Secrets = []string{newSecret}
	r2 := signedRequest(t0.Unix(), body)
	r2.Header.Set(HeaderSignature, signature.Sign(newSecret, msgID, t0.Unix(), body))
	if _, err := Request(r2, "hmsec_already-retired", o); err != nil {
		t.Fatalf("extra secret in Options should verify: %v", err)
	}
}

func TestHandlerPassesAuthenticAndBlocksForged(t *testing.T) {
	var reached bool
	h := HandlerWithOptions(secret, opts(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signedRequest(t0.Unix(), body))
	if !reached || rec.Code != http.StatusNoContent {
		t.Fatalf("authentic request: reached=%v code=%d", reached, rec.Code)
	}

	reached = false
	forged := signedRequest(t0.Unix(), body)
	forged.Header.Set(HeaderSignature, "v1=Zm9yZ2Vk")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, forged)
	if reached || rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged request: reached=%v code=%d", reached, rec.Code)
	}
}

func TestPayloadDirectUse(t *testing.T) {
	ts := t0.Unix()
	sig := signature.Sign(secret, msgID, ts, body)
	if err := Payload(secret, msgID, ts, sig, body, opts()); err != nil {
		t.Fatalf("Payload should accept valid pieces: %v", err)
	}
	if err := Payload(secret, msgID, ts, sig, []byte("{}"), opts()); err == nil {
		t.Fatal("Payload must reject a different body")
	}
}
