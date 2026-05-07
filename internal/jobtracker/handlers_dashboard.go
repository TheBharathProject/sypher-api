package jobtracker

import (
	"net/http"

	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

// Dashboard returns aggregated metrics for /analytics/dashboard.
func (h *Handler) Dashboard(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	m, err := h.store.Dashboard(r.Context(), uid)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, m)
}
