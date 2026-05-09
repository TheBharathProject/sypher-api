package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWebhookRejectsBadSignature verifies that an unsigned or
// wrongly-signed payload bounces with 401 — the signature check is the
// trust boundary; if it passes, the dispatcher trusts the body.
func TestWebhookRejectsBadSignature(t *testing.T) {
	h := NewWebhookHandler(nil, "wh_secret_xxx", slog.Default())

	body := `{"event":"payment.captured","id":"evt_x","payload":{}}`

	cases := []struct {
		name     string
		sig      string
		wantCode int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"wrong", "0000000000000000", http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/webhooks/razorpay", strings.NewReader(body))
			if c.sig != "" {
				req.Header.Set("X-Razorpay-Signature", c.sig)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != c.wantCode {
				t.Errorf("got %d, want %d", rr.Code, c.wantCode)
			}
		})
	}
}

// TestRzpNotesUnmarshal is the regression for the dashboard's "Test
// webhook" 400 we hit on first wire-up: Razorpay's PHP backend
// serializes empty notes as `[]` (a numerically-indexed empty array),
// which doesn't decode into map[string]string. The custom
// UnmarshalJSON has to accept all five shapes the wire produces.
func TestRzpNotesUnmarshal(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want rzpNotes
	}{
		{"empty array (PHP)", `[]`, rzpNotes{}},
		{"empty object", `{}`, rzpNotes{}},
		{"null", `null`, rzpNotes{}},
		{"populated", `{"kind":"credits","user_id":"abc"}`,
			rzpNotes{"kind": "credits", "user_id": "abc"}},
		{"mixed values keeps strings", `{"kind":"credits","credits":500}`,
			rzpNotes{"kind": "credits"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var n rzpNotes
			if err := n.UnmarshalJSON([]byte(c.in)); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(n) != len(c.want) {
				t.Errorf("got %v, want %v", n, c.want)
				return
			}
			for k, v := range c.want {
				if n[k] != v {
					t.Errorf("key %q: got %q, want %q", k, n[k], v)
				}
			}
		})
	}
}

// TestWebhookAcceptsValidSignatureUnknownEvent verifies that a
// well-signed body for an event we don't handle (e.g. "refund.created")
// still returns 200 — Razorpay should stop retrying for unhandled
// events too. This requires a real DB to test the full path; here we
// only assert the signature gate passes for unknown events.
func TestWebhookSignatureValid(t *testing.T) {
	secret := "wh_secret_xxx"
	body := []byte(`{"event":"refund.created","id":"evt_unknown","payload":{}}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))

	if !VerifyWebhookSignature(body, sig, secret) {
		t.Fatal("expected signature to verify")
	}
	if VerifyWebhookSignature(body, sig, "different_secret") {
		t.Fatal("signature verified with wrong secret")
	}
}
