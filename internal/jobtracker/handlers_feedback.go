package jobtracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

func (h *Handler) PostFeedback(w http.ResponseWriter, r *http.Request) {
	var in FeedbackInput
	if !readJSON(w, r, &in) {
		return
	}
	if in.Message == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "message is required")
		return
	}
	var uid *uuid.UUID
	if v, ok := auth.UserID(r.Context()); ok {
		uid = &v
	}
	if err := h.store.InsertFeedback(r.Context(), uid, in); err != nil {
		writeDBError(w, err)
		return
	}

	// Fire a Slack notification on every feedback row. Fire-and-forget
	// so a flaky Slack endpoint can't make the user wait. Empty webhook
	// URL skips the call entirely — same defensive-degradation pattern
	// we use for R2/Deepseek/Razorpay.
	if h.cfg != nil && h.cfg.SlackFeedbackWebhookURL != "" {
		go h.postFeedbackToSlack(uid, in)
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"received": true})
}

// postFeedbackToSlack sends a one-line summary to the configured Slack
// incoming webhook. Logged-only on failure — feedback already landed in
// the DB so the user's submission isn't lost if Slack is down.
func (h *Handler) postFeedbackToSlack(uid *uuid.UUID, in FeedbackInput) {
	who := "anonymous"
	if uid != nil {
		who = uid.String()
	}
	body, err := json.Marshal(map[string]string{
		"text": fmt.Sprintf("📝 *Feedback from %s*\n> %s", who, in.Message),
	})
	if err != nil {
		h.logger.Warn("slack feedback marshal", "err", err)
		return
	}
	// Bound the request so a stuck Slack endpoint doesn't pile up goroutines.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", h.cfg.SlackFeedbackWebhookURL, bytes.NewReader(body))
	if err != nil {
		h.logger.Warn("slack feedback build req", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.logger.Warn("slack feedback post", "err", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		h.logger.Warn("slack feedback non-2xx", "status", resp.StatusCode)
	}
}
