package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestVerifyWebhookSignature(t *testing.T) {
	body := []byte(`{"event":"payment.captured","payload":{}}`)
	secret := "wh_secret_123"

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	good := hex.EncodeToString(mac.Sum(nil))

	cases := []struct {
		name   string
		body   []byte
		sig    string
		secret string
		want   bool
	}{
		{"valid", body, good, secret, true},
		{"empty sig", body, "", secret, false},
		{"empty secret", body, good, "", false},
		{"wrong sig", body, "deadbeef", secret, false},
		{"tampered body", []byte(`{"event":"hacked"}`), good, secret, false},
		{"uppercase hex still valid", body, hexUpper(good), secret, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := VerifyWebhookSignature(c.body, c.sig, c.secret)
			if got != c.want {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}

func hexUpper(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'f' {
			c = c - 'a' + 'A'
		}
		out[i] = c
	}
	return string(out)
}

func TestPackByID(t *testing.T) {
	if _, err := PackByID("starter"); err != nil {
		t.Fatalf("starter should resolve: %v", err)
	}
	if _, err := PackByID("nope"); err == nil {
		t.Error("unknown pack should error")
	}
	packs := CreditPacks()
	if len(packs) != 3 {
		t.Errorf("expected 3 packs, got %d", len(packs))
	}
	for _, p := range packs {
		if p.AmountPaise <= 0 || p.Credits <= 0 {
			t.Errorf("pack %q has bad pricing: %+v", p.ID, p)
		}
	}
}
