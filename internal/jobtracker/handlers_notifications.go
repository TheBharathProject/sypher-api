package jobtracker

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

// ListNotifications — GET /job-tracker/notifications
//
// Query params (all optional):
//
//	unreadOnly=1     filter to read_at IS NULL only
//	cursor=<rfc3339> page after this createdAt (exclusive)
//	limit=<n>        clamp to [1, 100], default 50
//
// Response: { items: [...], nextCursor: "..." | null }
func (h *Handler) ListNotifications(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())

	opts := ListNotificationsOpts{
		UnreadOnly: r.URL.Query().Get("unreadOnly") == "1",
	}
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		t, err := time.Parse(time.RFC3339, cursor)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "bad_cursor", "cursor must be RFC3339")
			return
		}
		opts.Cursor = t
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			opts.Limit = n
		}
	}

	items, err := h.store.ListNotifications(r.Context(), uid, opts)
	if err != nil {
		writeDBError(w, err)
		return
	}

	// Caller pages forward by passing the last item's createdAt. If we
	// returned fewer than the limit, there's no next page.
	var nextCursor string
	if opts.Limit == 0 {
		opts.Limit = 50
	}
	if len(items) == opts.Limit {
		nextCursor = items[len(items)-1].CreatedAt.Format(time.RFC3339)
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items":      items,
		"nextCursor": nilIfBlank(nextCursor),
	})
}

// UnreadCount — GET /job-tracker/notifications/count
//
// Returns { unread: int }. Cheap-ish — single COUNT(*) over the
// idx_notifications_user_unread index. Polled by the bell badge every
// ~60s while the user has Pegasus open.
func (h *Handler) UnreadCount(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	n, err := h.store.UnreadCount(r.Context(), uid)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"unread": n})
}

// MarkNotificationRead — PATCH /job-tracker/notifications/{id}/read
//
// 204 on success (already-read is treated as success — idempotent).
// 404 if the notification doesn't exist or isn't owned by the caller.
func (h *Handler) MarkNotificationRead(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "invalid notification id")
		return
	}
	if err := h.store.MarkRead(r.Context(), uid, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "notification not found")
			return
		}
		writeDBError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// MarkAllNotificationsRead — PATCH /job-tracker/notifications/read-all
//
// 200 with { marked: <count> } so the frontend can show "Marked 12 read".
func (h *Handler) MarkAllNotificationsRead(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	count, err := h.store.MarkAllRead(r.Context(), uid)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"marked": count})
}

func nilIfBlank(s string) any {
	if s == "" {
		return nil
	}
	return s
}
