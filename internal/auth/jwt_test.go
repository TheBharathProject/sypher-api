package auth

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestJWTRoundTrip(t *testing.T) {
	secret := "test-secret-do-not-use-in-prod"
	issuer := "sypher.test"
	audience := "sypher.test"
	uid := uuid.New()
	email := "user@example.com"

	tok, err := IssueJWT(secret, issuer, audience, uid, email, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if tok == "" {
		t.Fatal("empty token")
	}

	gotID, gotEmail, err := VerifyJWT(secret, issuer, audience, tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if gotID != uid {
		t.Errorf("uid mismatch: got %v want %v", gotID, uid)
	}
	if gotEmail != email {
		t.Errorf("email mismatch: got %q want %q", gotEmail, email)
	}
}

func TestJWTBadSecret(t *testing.T) {
	tok, err := IssueJWT("right-secret", "iss", "aud", uuid.New(), "u@x.com", time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, _, err := VerifyJWT("wrong-secret", "iss", "aud", tok); err == nil {
		t.Fatal("verify with wrong secret should fail")
	}
}

func TestJWTExpired(t *testing.T) {
	tok, err := IssueJWT("s", "iss", "aud", uuid.New(), "u@x.com", -time.Second)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, _, err := VerifyJWT("s", "iss", "aud", tok); err == nil {
		t.Fatal("expired token should fail verification")
	}
}

func TestJWTAudienceMismatch(t *testing.T) {
	tok, err := IssueJWT("s", "iss", "aud-a", uuid.New(), "u@x.com", time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, _, err := VerifyJWT("s", "iss", "aud-b", tok); err == nil {
		t.Fatal("audience mismatch should fail")
	}
}
