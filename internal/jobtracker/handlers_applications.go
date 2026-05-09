package jobtracker

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
	"github.com/TheBharathProject/sypher-api/internal/importers"
)

func (h *Handler) ListApplications(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	q := r.URL.Query()
	apps, err := h.store.ListApplications(r.Context(), uid, ListAppsOpts{
		Stage:  q.Get("stage"),
		Source: q.Get("source"),
		Search: q.Get("search"),
	})
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, apps)
}

// CheckApplicationByLink answers "do I already have this job link in my
// tracker?". The browser extension hits this before showing its form,
// so a duplicate save flow shows "Already saved" with a deep link
// instead of opening a fresh capture form.
//
// Returns 200 always — the body's `exists` boolean carries the verdict.
// Empty/missing jobLink returns {exists:false} rather than 400 so the
// extension can call this unconditionally on any page.
func (h *Handler) CheckApplicationByLink(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	jobLink := r.URL.Query().Get("jobLink")
	res, err := h.store.CheckApplicationByJobLink(r.Context(), uid, jobLink)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (h *Handler) GetApplication(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	a, err := h.store.GetApplication(r.Context(), uid, id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, a)
}

func (h *Handler) validateAppInput(in *ApplicationInput) error {
	if strings.TrimSpace(in.Company) == "" {
		return fmt.Errorf("company is required")
	}
	if strings.TrimSpace(in.Role) == "" {
		return fmt.Errorf("role is required")
	}
	if in.Stage == "" {
		in.Stage = "INTERESTED"
	}
	if !isValidStage(in.Stage) {
		return fmt.Errorf("invalid stage: %s", in.Stage)
	}
	return nil
}

// CreateApplication handles POST /applications. It upserts on job_link
// — when the same user already has a row with the submitted job_link,
// the call is routed to UpdateApplication and the response is 200
// (with `X-Application-Status: updated`). New rows return 201.
//
// This shape lets the browser extension re-save a job (e.g. to bump the
// stage from INTERESTED → APPLIED) without leaving duplicate rows in
// the tracker. Empty job_link always inserts — manual entries without a
// URL aren't deduped.
func (h *Handler) CreateApplication(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	var in ApplicationInput
	if !readJSON(w, r, &in) {
		return
	}
	if err := h.validateAppInput(&in); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", err.Error())
		return
	}

	link := strings.TrimSpace(in.JobLink)
	if link != "" {
		existingID, err := h.store.FindApplicationIDByJobLink(r.Context(), uid, link)
		if err == nil {
			updated, uErr := h.store.UpdateApplication(r.Context(), uid, existingID, in)
			if uErr != nil {
				writeDBError(w, uErr)
				return
			}
			w.Header().Set("X-Application-Status", "updated")
			httpx.WriteJSON(w, http.StatusOK, updated)
			return
		}
		// Any error other than "no match" is a real DB failure — don't
		// silently fall through to insert and risk a constraint surprise.
		if !errors.Is(err, pgx.ErrNoRows) {
			writeDBError(w, err)
			return
		}
	}

	a, err := h.store.CreateApplication(r.Context(), uid, in)
	if err != nil {
		writeDBError(w, err)
		return
	}
	w.Header().Set("X-Application-Status", "created")
	httpx.WriteJSON(w, http.StatusCreated, a)
}

func (h *Handler) UpdateApplication(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in ApplicationInput
	if !readJSON(w, r, &in) {
		return
	}
	if err := h.validateAppInput(&in); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", err.Error())
		return
	}
	a, err := h.store.UpdateApplication(r.Context(), uid, id, in)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, a)
}

// ApplicationTimeline serves the stage-history rows for one application,
// oldest first. Used by the View modal's Timeline section.
func (h *Handler) ApplicationTimeline(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	timeline, err := h.store.ApplicationTimeline(r.Context(), uid, id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, timeline)
}

func (h *Handler) DeleteApplication(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if err := h.store.DeleteApplication(r.Context(), uid, id); err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// CSV columns for export. Order is the contract; round-tripping through
// import works because the importer ignores unknown headers.
var appCSVHeader = []string{
	"company", "role", "source", "location", "salaryRange", "stage",
	"appliedAt", "applyDeadline", "jobLink", "description", "notes", "stale",
}

// Template header is the export header minus `stale`. `stale` is a
// system-managed flag (set by the daily cron when stage_changed_at is
// older than 14d) — users filling in a fresh template shouldn't think
// they need to set it.
var appImportTemplateHeader = []string{
	"company", "role", "source", "location", "salaryRange", "stage",
	"appliedAt", "applyDeadline", "jobLink", "description", "notes",
}

// ExportApplications streams the user's applications as CSV.
func (h *Handler) ExportApplications(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	apps, err := h.store.ListApplications(r.Context(), uid, ListAppsOpts{})
	if err != nil {
		writeDBError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="applications.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write(appCSVHeader)
	for _, a := range apps {
		_ = cw.Write([]string{
			a.Company, a.Role, a.Source, a.Location, a.SalaryRange, a.Stage,
			a.AppliedAt, a.ApplyDeadline, a.JobLink, a.JobDescription, a.Notes,
			boolStr(a.Stale),
		})
	}
	cw.Flush()
}

// ApplicationsTemplate returns an empty CSV with just the header — used by
// the import flow as a starting template.
func (h *Handler) ApplicationsTemplate(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="applications-template.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write(appImportTemplateHeader)
	cw.Flush()
}

type importPreviewResult struct {
	Preview []ApplicationInput `json:"preview"`
	Errors  []string           `json:"errors"`
	Source  string             `json:"source"`
}

// PreviewImportApplications parses uploaded payload and returns the
// rows it would insert without writing them. Source is selected via
// `?source=csv|linkedin|naukri` (default csv) per ADR-004 D2.
func (h *Handler) PreviewImportApplications(w http.ResponseWriter, r *http.Request) {
	rows, errs, source, ok := h.parseImport(w, r)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, importPreviewResult{
		Preview: rows,
		Errors:  errs,
		Source:  source,
	})
}

// ImportApplications parses + inserts. Same source dispatch as preview.
func (h *Handler) ImportApplications(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	rows, errs, _, ok := h.parseImport(w, r)
	if !ok {
		return
	}
	imported := 0
	for _, in := range rows {
		if err := h.validateAppInput(&in); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if _, err := h.store.CreateApplication(r.Context(), uid, in); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		imported++
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"imported": imported,
		"errors":   errs,
	})
}

// parseImport reads the uploaded file (multipart or raw body), looks up
// the right importer for `?source=`, and runs Parse. Returns
// (rows, errs, source, ok=true) on parse success — even when there were
// per-row errors. Returns ok=false on transport-level failure (already
// wrote an HTTP error response).
//
// Per-importer rows come back as importers.ApplicationInput; we map
// those to the local jobtracker.ApplicationInput which is the same
// shape but lives in this package to avoid an import cycle.
func (h *Handler) parseImport(w http.ResponseWriter, r *http.Request) ([]ApplicationInput, []string, string, bool) {
	source := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("source")))
	imp, err := importers.For(source)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_source", err.Error())
		return nil, nil, source, false
	}

	src, ok := readImportBody(w, r)
	if !ok {
		return nil, nil, source, false
	}

	rawRows, errs, ferr := imp.Parse(src)
	if ferr != nil {
		// Fatal parse error (missing required column, empty CSV, etc.).
		// Surface as 400 with the importer's message — frontend renders
		// it in the preview pane so the user can pick a different file.
		httpx.WriteError(w, http.StatusBadRequest, "bad_csv", ferr.Error())
		return nil, nil, source, false
	}

	rows := make([]ApplicationInput, 0, len(rawRows))
	for _, in := range rawRows {
		rows = append(rows, ApplicationInput{
			Company:        in.Company,
			Role:           in.Role,
			Source:         in.Source,
			Location:       in.Location,
			SalaryRange:    in.SalaryRange,
			Stage:          in.Stage,
			AppliedAt:      in.AppliedAt,
			ApplyDeadline:  in.ApplyDeadline,
			JobLink:        in.JobLink,
			JobDescription: in.JobDescription,
			Notes:          in.Notes,
			Stale:          in.Stale,
		})
	}
	return rows, errs, imp.Name(), true
}

// readImportBody reads from either multipart form ("file" field) or
// raw body. Same behaviour as before — preserved here so all importers
// share the same upload affordance on the frontend.
func readImportBody(w http.ResponseWriter, r *http.Request) (io.Reader, bool) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "bad_request", "form parse: "+err.Error())
			return nil, false
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "bad_request", "missing 'file' field")
			return nil, false
		}
		// File handle leaks if the request is cancelled mid-parse — but
		// the importer reads everything synchronously so by the time we
		// return, f is already consumed. Closing here would race the
		// caller's Read.
		return f, true
	}
	return io.LimitReader(r.Body, 8<<20), true
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
