package jobtracker

import (
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
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
	a, err := h.store.CreateApplication(r.Context(), uid, in)
	if err != nil {
		writeDBError(w, err)
		return
	}
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

// CSV columns for import/export. Order is the contract.
var appCSVHeader = []string{
	"company", "role", "source", "location", "salaryRange", "stage",
	"appliedAt", "applyDeadline", "jobLink", "description", "notes", "stale",
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
	_ = cw.Write(appCSVHeader)
	cw.Flush()
}

type importPreviewResult struct {
	Preview []ApplicationInput `json:"preview"`
	Errors  []string           `json:"errors"`
}

// PreviewImportApplications parses uploaded CSV and returns the rows it
// would insert without writing them.
func (h *Handler) PreviewImportApplications(w http.ResponseWriter, r *http.Request) {
	rows, errs, ok := h.parseImportCSV(w, r)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, importPreviewResult{Preview: rows, Errors: errs})
}

// ImportApplications parses uploaded CSV and inserts rows.
func (h *Handler) ImportApplications(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	rows, errs, ok := h.parseImportCSV(w, r)
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

// parseImportCSV reads CSV from either multipart form (field "file") or raw body.
// Returns (rows, errs, ok=true) on success. On HTTP-level failure it writes
// an error response and returns ok=false.
func (h *Handler) parseImportCSV(w http.ResponseWriter, r *http.Request) ([]ApplicationInput, []string, bool) {
	var src io.Reader
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "bad_request", "form parse: "+err.Error())
			return nil, nil, false
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "bad_request", "missing 'file' field")
			return nil, nil, false
		}
		defer f.Close()
		src = f
	} else {
		src = io.LimitReader(r.Body, 8<<20)
	}

	cr := csv.NewReader(src)
	header, err := cr.Read()
	if err != nil {
		return nil, []string{"empty or unreadable CSV"}, true
	}
	idx := map[string]int{}
	for i, h := range header {
		idx[strings.TrimSpace(h)] = i
	}
	required := []string{"company", "role"}
	for _, k := range required {
		if _, ok := idx[k]; !ok {
			return nil, []string{"missing required column: " + k}, true
		}
	}

	rows := make([]ApplicationInput, 0)
	errs := make([]string, 0)
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("line %d: %s", line, err.Error()))
			continue
		}
		get := func(k string) string {
			i, ok := idx[k]
			if !ok || i >= len(rec) {
				return ""
			}
			return strings.TrimSpace(rec[i])
		}
		in := ApplicationInput{
			Company:       get("company"),
			Role:          get("role"),
			Source:        get("source"),
			Location:      get("location"),
			SalaryRange:   get("salaryRange"),
			Stage:         orDefault(get("stage"), "INTERESTED"),
			AppliedAt:     get("appliedAt"),
			ApplyDeadline: get("applyDeadline"),
			JobLink:       get("jobLink"),
			JobDescription: get("description"),
			Notes:         get("notes"),
			Stale:         strings.EqualFold(get("stale"), "true"),
		}
		rows = append(rows, in)
	}
	return rows, errs, true
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
