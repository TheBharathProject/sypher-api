// Package waitlist owns the /waitlist endpoint end-to-end:
//
//	types.go    request/response shapes (and their validation)
//	store.go    SQL — talks to Postgres
//	handler.go  HTTP — parses, validates, calls store, returns JSON
//
// This three-file split is the convention for every "tool" package in this
// service. Other tools (reel-hooks, markets, ...) follow the same shape.
package waitlist

import (
	"errors"
	"net/mail"
	"strings"
)

// Request mirrors the JSON body POSTed to /waitlist. Each `json:"..."` tag
// tells encoding/json which incoming key maps to which field. Without the
// tag, decoding would expect "Email", "Source" etc. (Go's exported names).
//
// `omitempty` on output structs would skip empty fields; on input it has no
// effect. Including it on optional fields documents intent.
type Request struct {
	Email    string `json:"email"`
	Source   string `json:"source,omitempty"`
	Referrer string `json:"referrer,omitempty"`
	HP       string `json:"hp,omitempty"` // honeypot — real users never set this
}

// Response is what we return on success.
type Response struct {
	OK  bool `json:"ok"`
	New bool `json:"new"`
}

// ErrInvalidEmail is the validation error surfaced when the email doesn't
// parse cleanly. The handler maps this to a 400; future callers can use
// errors.Is to distinguish validation errors from other failures.
var ErrInvalidEmail = errors.New("invalid email")

// Normalize lower-cases and trims the email in place. Idempotent.
func (r *Request) Normalize() {
	r.Email = strings.ToLower(strings.TrimSpace(r.Email))
	// truncate optional strings to safe lengths
	r.Source = truncate(r.Source, 64)
	r.Referrer = truncate(r.Referrer, 512)
}

// Validate enforces that Email is a syntactically valid bare address and
// within RFC 5321's 254-char hard limit. Call after Normalize.
//
// Return semantics: nil on valid; ErrInvalidEmail on any failure. We keep
// the error opaque (no detailed reason) — this is a public endpoint and
// detailed validation errors leak structure to crawlers.
func (r *Request) Validate() error {
	if r.Email == "" || len(r.Email) > 254 {
		return ErrInvalidEmail
	}
	addr, err := mail.ParseAddress(r.Email)
	if err != nil {
		return ErrInvalidEmail
	}
	// ParseAddress accepts "Name <user@host>" too; we want bare addresses.
	if addr.Address != r.Email || !strings.Contains(r.Email, "@") {
		return ErrInvalidEmail
	}
	return nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
