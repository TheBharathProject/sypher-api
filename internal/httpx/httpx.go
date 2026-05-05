// Package httpx holds HTTP plumbing helpers shared across the service —
// JSON response writers, client-IP extraction, error envelopes.
//
// Lives outside `internal/server` so tool packages (waitlist, future
// reel-hooks, etc.) can use these without importing server (which would
// be a cycle: server imports tools, tools imports server).
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
)

// ErrorBody is the canonical shape of an error response body.
// Keeping the struct exported means clients (and tests) can unmarshal into it.
type ErrorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// WriteJSON serializes v as JSON with the given status. Encoding errors
// are logged and dropped — by the time we're encoding, headers are gone
// and there's nothing useful to send back to the client.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("writing json response", "err", err)
	}
}

// WriteError sends the canonical error envelope. Keeping the shape uniform
// across all handlers makes client-side error handling predictable.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, ErrorBody{Error: code, Message: message})
}

// ClientIP returns the most likely real client IP, honouring trusted
// proxy headers in this priority order:
//
//  1. X-Forwarded-For (left-most, in case of a chain)
//  2. X-Real-IP
//  3. r.RemoteAddr (TCP peer)
//
// We trust X-Forwarded-For because Caddy is the only public ingress on
// the VM and it sets this header. If you ever expose this service to a
// different proxy chain, revisit.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if rip := r.Header.Get("X-Real-IP"); rip != "" {
		return rip
	}
	if h := r.RemoteAddr; h != "" {
		// RemoteAddr is "host:port"; strip the port.
		if i := strings.LastIndex(h, ":"); i > 0 {
			return h[:i]
		}
	}
	return ""
}
