package waitlist

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/TheBharathProject/sypher-api/internal/httpx"
	"github.com/TheBharathProject/sypher-api/internal/security"
)

// Handler bundles the deps the POST /waitlist handler needs. Construct
// once, register with the router, reuse across all requests.
//
// "Accept interfaces, return structs" — when we add a test that needs
// to mock the Store, define a small interface here (e.g. Inserter) and
// take that instead of *Store. Until then, the concrete dep is fine.
type Handler struct {
	store  *Store
	salt   string
	logger *slog.Logger
}

func NewHandler(store *Store, ipSalt string, logger *slog.Logger) *Handler {
	return &Handler{store: store, salt: ipSalt, logger: logger}
}

// ServeHTTP makes Handler satisfy http.Handler. The router calls this once
// per request. Handlers in Go are typed by *interface*, not by inheritance.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Cap the body — anyone POSTing > 4 KB is up to no good.
	body, err := io.ReadAll(io.LimitReader(r.Body, 4*1024))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "could not read body")
		return
	}
	defer r.Body.Close()

	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_json", "invalid json body")
		return
	}

	// Honeypot — bots that scrape the form often blindly fill every field.
	// We pretend everything's fine so they don't learn anything.
	if req.HP != "" {
		httpx.WriteJSON(w, http.StatusOK, Response{OK: true, New: false})
		return
	}

	req.Normalize()
	if err := req.Validate(); err != nil {
		// errors.Is keeps the door open for richer error types later
		// without changing this branch.
		if errors.Is(err, ErrInvalidEmail) {
			httpx.WriteError(w, http.StatusBadRequest, "bad_email", "invalid email")
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	ipHash := security.HashIP(h.salt, httpx.ClientIP(r))
	userAgent := truncate(r.UserAgent(), 512)

	created, err := h.store.Insert(r.Context(), req.Email, req.Source, req.Referrer, userAgent, ipHash)
	if err != nil {
		h.logger.Error("waitlist insert", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "db_error", "could not save signup")
		return
	}

	httpx.WriteJSON(w, http.StatusOK, Response{OK: true, New: created})
}

// Compile-time assertion: *Handler satisfies http.Handler. If we ever
// break the interface, this fails to compile rather than at request time.
var _ http.Handler = (*Handler)(nil)
