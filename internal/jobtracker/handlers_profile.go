package jobtracker

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

var slugRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{1,38}[a-z0-9])?$`)

func (h *Handler) GetProfile(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	p, err := h.store.GetProfile(r.Context(), uid)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

func (h *Handler) UpdateProfile(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	var in ProfileInput
	if !readJSON(w, r, &in) {
		return
	}
	if err := h.store.UpdateProfile(r.Context(), uid, in); err != nil {
		writeDBError(w, err)
		return
	}
	h.GetProfile(w, r)
}

// publicProfileBase returns the user-facing base for /u/<slug> URLs.
// Resolution order:
//
//  1. PUBLIC_PROFILE_BASE_URL — explicit env var (e.g. "https://sypher.in/u/").
//     Set this in prod so the apex /u/ stays apex-relative even when the
//     FrontendLoginRedirect lives under a tool's basePath like /pegasus/auth/callback.
//  2. Derive from FRONTEND_LOGIN_REDIRECT_URL host. Convenient for local dev.
//  3. Last-ditch: scheme + r.Host + /u/. Only hits when both envs are blank,
//     which won't happen in any deployed env.
func (h *Handler) publicProfileBase(r *http.Request) string {
	if h.cfg != nil && h.cfg.PublicProfileBaseURL != "" {
		base := h.cfg.PublicProfileBaseURL
		if !strings.HasSuffix(base, "/") {
			base += "/"
		}
		return base
	}
	if h.cfg != nil && h.cfg.FrontendLoginRedirect != "" {
		if u, err := url.Parse(h.cfg.FrontendLoginRedirect); err == nil && u.Host != "" {
			return u.Scheme + "://" + u.Host + "/u/"
		}
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/u/"
}

// GetSlug returns the user's current slug + visibility + the full public URL.
// Matches live's GET /api/profile/slug → {slug, isPublic, profileUrl}.
func (h *Handler) GetSlug(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	p, err := h.store.GetProfile(r.Context(), uid)
	if err != nil {
		writeDBError(w, err)
		return
	}
	resp := map[string]any{
		"slug":     p.Slug,
		"isPublic": p.IsPublic,
	}
	if p.Slug != "" {
		resp["profileUrl"] = h.publicProfileBase(r) + p.Slug
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// CheckSlug GET /profile/slug/check?slug=foo → {slug, available}
func (h *Handler) CheckSlug(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	slug := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("slug")))
	if !slugRe.MatchString(slug) {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "slug must be 3-40 chars, lowercase a-z 0-9 -")
		return
	}
	avail, err := h.store.SlugAvailable(r.Context(), uid, slug)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"slug":      slug,
		"available": avail,
	})
}

type slugInput struct {
	Slug string `json:"slug"`
}

func (h *Handler) UpdateSlug(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	var in slugInput
	if !readJSON(w, r, &in) {
		return
	}
	slug := strings.ToLower(strings.TrimSpace(in.Slug))
	if !slugRe.MatchString(slug) {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "slug must be 3-40 chars, lowercase a-z 0-9 -")
		return
	}
	avail, err := h.store.SlugAvailable(r.Context(), uid, slug)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if !avail {
		httpx.WriteError(w, http.StatusConflict, "slug_taken", "slug already in use")
		return
	}
	if err := h.store.UpdateSlug(r.Context(), uid, slug); err != nil {
		writeDBError(w, err)
		return
	}
	h.GetProfile(w, r)
}

// visibilityInput accepts both {public} and {isPublic} keys for one round of
// transition. Live uses isPublic; our existing frontend wrote `public`.
type visibilityInput struct {
	Public   *bool `json:"public,omitempty"`
	IsPublic *bool `json:"isPublic,omitempty"`
}

func (h *Handler) UpdateVisibility(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	var in visibilityInput
	if !readJSON(w, r, &in) {
		return
	}
	var effective bool
	switch {
	case in.IsPublic != nil:
		effective = *in.IsPublic
	case in.Public != nil:
		effective = *in.Public
	default:
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "isPublic is required")
		return
	}
	if err := h.store.UpdateVisibility(r.Context(), uid, effective); err != nil {
		writeDBError(w, err)
		return
	}
	h.GetProfile(w, r)
}

// ----- Experiences -----

func (h *Handler) ListExperiences(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	out, err := h.store.ListExperiences(r.Context(), uid)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) CreateExperience(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	var in ExperienceInput
	if !readJSON(w, r, &in) {
		return
	}
	if in.Company == "" || in.Title == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "company and title required")
		return
	}
	e, err := h.store.CreateExperience(r.Context(), uid, in)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, e)
}

func (h *Handler) UpdateExperience(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in ExperienceInput
	if !readJSON(w, r, &in) {
		return
	}
	e, err := h.store.UpdateExperience(r.Context(), uid, id, in)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, e)
}

func (h *Handler) DeleteExperience(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if err := h.store.DeleteExperience(r.Context(), uid, id); err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ----- Educations -----

func (h *Handler) ListEducations(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	out, err := h.store.ListEducations(r.Context(), uid)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) CreateEducation(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	var in EducationInput
	if !readJSON(w, r, &in) {
		return
	}
	if in.School == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "school required")
		return
	}
	e, err := h.store.CreateEducation(r.Context(), uid, in)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, e)
}

func (h *Handler) UpdateEducation(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in EducationInput
	if !readJSON(w, r, &in) {
		return
	}
	e, err := h.store.UpdateEducation(r.Context(), uid, id, in)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, e)
}

func (h *Handler) DeleteEducation(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if err := h.store.DeleteEducation(r.Context(), uid, id); err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ----- Projects -----

func (h *Handler) ListProjects(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	out, err := h.store.ListProjects(r.Context(), uid)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) CreateProject(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	var in ProjectInput
	if !readJSON(w, r, &in) {
		return
	}
	if in.Name == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "name required")
		return
	}
	p, err := h.store.CreateProject(r.Context(), uid, in)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, p)
}

func (h *Handler) UpdateProject(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in ProjectInput
	if !readJSON(w, r, &in) {
		return
	}
	p, err := h.store.UpdateProject(r.Context(), uid, id, in)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

func (h *Handler) DeleteProject(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if err := h.store.DeleteProject(r.Context(), uid, id); err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ----- Skills -----

func (h *Handler) ListSkills(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	out, err := h.store.ListSkills(r.Context(), uid)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) CreateSkill(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	var in SkillInput
	if !readJSON(w, r, &in) {
		return
	}
	if in.Name == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "name required")
		return
	}
	sk, err := h.store.CreateSkill(r.Context(), uid, in)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, sk)
}

func (h *Handler) DeleteSkill(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if err := h.store.DeleteSkill(r.Context(), uid, id); err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
