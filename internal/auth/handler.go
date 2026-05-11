package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/TheBharathProject/sypher-api/internal/config"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

// ProfileProvisioner is implemented by the per-tool store(s) that want to
// run setup work the first time a user signs in (e.g. create an empty
// profile row, auto-pick a slug). Optional — auth boots fine without it.
type ProfileProvisioner interface {
	ProvisionProfile(ctx context.Context, userID uuid.UUID) error
}

// Handler bundles dependencies for the /auth/* endpoints.
type Handler struct {
	cfg          *config.Config
	store        *Store
	logger       *slog.Logger
	provisioners []ProfileProvisioner
}

func NewHandler(cfg *config.Config, store *Store, logger *slog.Logger) *Handler {
	return &Handler{cfg: cfg, store: store, logger: logger}
}

// WithProvisioner registers a per-tool post-login hook. Multiple are allowed;
// they're all invoked sequentially after UpsertUser. Failure of one is
// logged but doesn't block login — the lazy ensureSlug path handles
// recovery on the user's next /profile fetch.
func (h *Handler) WithProvisioner(p ProfileProvisioner) *Handler {
	h.provisioners = append(h.provisioners, p)
	return h
}

const oauthStateCookie = "sypher_oauth_state"

// GoogleStart redirects to Google's OAuth consent screen.
// Sets a short-lived state cookie so the callback can verify origin.
func (h *Handler) GoogleStart(w http.ResponseWriter, r *http.Request) {
	state, err := RandomState()
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "could not generate state")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookie,
		Value:    state,
		Path:     "/auth",
		Expires:  time.Now().Add(10 * time.Minute),
		HttpOnly: true,
		Secure:   h.cfg.Env == "prod",
		SameSite: http.SameSiteLaxMode,
	})

	authURL := BuildGoogleAuthURL(h.cfg.GoogleOAuthClientID, h.cfg.GoogleOAuthRedirectURL, state)
	http.Redirect(w, r, authURL, http.StatusFound)
}

// GoogleCallback handles the OAuth redirect: validates state, exchanges
// code, upserts the user, issues a JWT, and redirects to the frontend
// with the JWT in the query string.
func (h *Handler) GoogleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if errStr := q.Get("error"); errStr != "" {
		h.redirectFailure(w, r, errStr)
		return
	}
	code := q.Get("code")
	state := q.Get("state")
	if code == "" || state == "" {
		h.redirectFailure(w, r, "missing_code_or_state")
		return
	}

	cookie, err := r.Cookie(oauthStateCookie)
	if err != nil || cookie.Value == "" || cookie.Value != state {
		h.redirectFailure(w, r, "bad_state")
		return
	}
	// Clear the state cookie ASAP.
	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookie,
		Value:    "",
		Path:     "/auth",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.cfg.Env == "prod",
		SameSite: http.SameSiteLaxMode,
	})

	prof, err := ExchangeCode(r.Context(),
		h.cfg.GoogleOAuthClientID, h.cfg.GoogleOAuthClientSecret, h.cfg.GoogleOAuthRedirectURL, code)
	if err != nil {
		h.logger.Error("oauth exchange", "err", err)
		h.redirectFailure(w, r, "exchange_failed")
		return
	}

	user, err := h.store.UpsertUser(r.Context(), prof.Sub, prof.Email, prof.Name, prof.Picture)
	if err != nil {
		h.logger.Error("upsert user", "err", err)
		h.redirectFailure(w, r, "upsert_failed")
		return
	}

	// Run any per-tool post-login provisioning (profile row, slug, etc.)
	// before redirecting. Failure here is logged but doesn't block login —
	// lazy paths (e.g. ensureSlug in GetProfile) will retry on next access.
	for _, p := range h.provisioners {
		if err := p.ProvisionProfile(r.Context(), user.ID); err != nil {
			h.logger.Warn("post-login provisioner failed",
				"user_id", user.ID, "err", err)
		}
	}

	tok, err := IssueJWT(h.cfg.JWTSecret, h.cfg.JWTIssuer, h.cfg.JWTAudience, user.ID, user.Email, h.cfg.JWTTTL)
	if err != nil {
		h.logger.Error("issue jwt", "err", err)
		h.redirectFailure(w, r, "jwt_failed")
		return
	}

	dest, err := url.Parse(h.cfg.FrontendLoginRedirect)
	if err != nil {
		h.logger.Error("parse frontend redirect", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "bad frontend redirect")
		return
	}
	// Stamp the JWT into the URL FRAGMENT (#) instead of the query
	// string (?) — fragments never hit server access logs, never appear
	// in Referer headers, and aren't sent back upstream when the SPA
	// makes its first fetch. The frontend reads window.location.hash on
	// the callback route and stashes the token in localStorage.
	//
	// Backwards compat note: existing FE callback already prefers
	// fragment over query (with a fallback window), so this rollout is
	// safe to deploy independently of the FE change.
	dest.Fragment = "token=" + tok
	http.Redirect(w, r, dest.String(), http.StatusFound)
}

func (h *Handler) redirectFailure(w http.ResponseWriter, r *http.Request, reason string) {
	dest, err := url.Parse(h.cfg.FrontendLoginRedirect)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "bad frontend redirect")
		return
	}
	qq := dest.Query()
	qq.Set("error", reason)
	dest.RawQuery = qq.Encode()
	http.Redirect(w, r, dest.String(), http.StatusFound)
}

// Logout — JWTs are stateless, so this is a courtesy 204. Frontend clears
// the token from localStorage.
func (h *Handler) Logout(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// CurrentUser is a small helper used by /job-tracker/me — given a context
// gated by RequireUser, returns the User from the DB.
func (h *Handler) CurrentUser(w http.ResponseWriter, r *http.Request) {
	uid := MustUserID(r.Context())
	u, err := h.store.GetUserByID(r.Context(), uid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "user not found")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, u)
}
