package kairos

import (
	"net/http"
	"strings"
	"time"

	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

// ─────────────────────────────────────────────────────────────────────────
// /kairos/announcements
//
// Response shape is dictated by kairos/lib/kairos-api.ts:
//   GET /kairos/announcements?window=24h|7d|30d&category=&symbol=
//     → {announcements:[ApiAnnouncement]}
//
// Routes are registered by the wiring task.
// ─────────────────────────────────────────────────────────────────────────

// announcementWindows maps the FE window vocabulary
// (ApiAnnouncementWindow) onto durations.
var announcementWindows = map[string]time.Duration{
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

// announcementsLimit is the feed page size (spec §3.4 — default 7d,
// limit 200).
const announcementsLimit = 200

// ListAnnouncements handles GET /kairos/announcements. window
// defaults to 7d; category matches case-insensitively; symbol is
// uppercased to match the ingested NSE symbols.
func (h *Handler) ListAnnouncements(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	window := q.Get("window")
	if window == "" {
		window = "7d"
	}
	dur, ok := announcementWindows[window]
	if !ok {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "window must be one of 24h|7d|30d")
		return
	}
	category := strings.TrimSpace(q.Get("category"))
	symbol := strings.ToUpper(strings.TrimSpace(q.Get("symbol")))

	since := time.Now().UTC().Add(-dur)
	out, err := h.store.ListAnnouncements(r.Context(), since, category, symbol, announcementsLimit)
	if err != nil {
		h.logger.Error("list announcements failed", "err", err, "request_id", httpx.RequestID(r.Context()))
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"announcements": out})
}
