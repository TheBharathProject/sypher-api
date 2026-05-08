package mailer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// resendEndpoint — Resend's sending endpoint. Hardcoded; if Resend ever
// changes it (they haven't since launch) we'll feel it via failing sends
// and update here.
const resendEndpoint = "https://api.resend.com/emails"

// resendMailer is the prod path. POST a single email; bail out on non-2xx
// with the response body in the error so logs make the failure obvious.
type resendMailer struct {
	apiKey   string
	fromAddr string
	fromName string
	logger   *slog.Logger
	// Optional override for tests. In production this is nil and we use
	// http.DefaultClient with a per-request context timeout instead.
	client *http.Client
}

type resendRequest struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Subject string `json:"subject"`
	HTML    string `json:"html,omitempty"`
	Text    string `json:"text,omitempty"`
}

func (m *resendMailer) Send(ctx context.Context, in Message) error {
	if in.To == "" {
		return fmt.Errorf("mailer: empty To")
	}
	if in.Subject == "" {
		return fmt.Errorf("mailer: empty Subject")
	}
	if in.HTML == "" && in.Text == "" {
		return fmt.Errorf("mailer: must set at least one of HTML/Text")
	}

	body, err := json.Marshal(resendRequest{
		From:    formatFrom(m.fromAddr, m.fromName),
		To:      in.To,
		Subject: in.Subject,
		HTML:    in.HTML,
		Text:    in.Text,
	})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	// Per-call timeout caps the goroutine the notifier spawns. Even if the
	// caller's context never cancels, the request can't run forever.
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, resendEndpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := m.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Resend returns {id: "..."} on success. We don't track the message
		// ID today — once we add deliverability instrumentation (D7 trigger)
		// this is where to capture it.
		return nil
	}

	// Surface the body so logs make a 4xx (DKIM not verified, bad from,
	// rate limited) or 5xx (Resend down) immediately diagnosable.
	bodyBytes, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("resend %d: %s", resp.StatusCode, string(bodyBytes))
}
