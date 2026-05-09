package billing

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a minimal Razorpay REST wrapper. Only the four operations
// Phase 6 needs (CreateOrder, CreateSubscription, CancelSubscription,
// VerifyWebhookSignature). We avoid the official Go SDK because (a) it
// pulls a meaningful dep tree and (b) the surface here is small enough
// that a thin client is more readable than an opaque indirection.
//
// Auth is HTTP basic with key_id : key_secret per Razorpay docs.
//
// All amounts the client sees are in PAISE (Razorpay's base unit). The
// caller is responsible for converting from rupees.
type Client struct {
	keyID      string
	keySecret  string
	httpClient *http.Client
	baseURL    string // tests can override; default is "https://api.razorpay.com/v1"
}

// NewClient returns nil if either credential is empty — handlers check
// for nil and respond 503 service_unavailable, mirroring how the rest
// of the codebase handles optional integrations (R2, Deepseek).
func NewClient(keyID, keySecret string) *Client {
	if keyID == "" || keySecret == "" {
		return nil
	}
	return &Client{
		keyID:      keyID,
		keySecret:  keySecret,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    "https://api.razorpay.com/v1",
	}
}

// KeyID is exposed for the frontend Checkout modal — Razorpay's JS
// requires the public key id (rzp_test_* or rzp_live_*) at modal init.
func (c *Client) KeyID() string {
	if c == nil {
		return ""
	}
	return c.keyID
}

// Order is the bare-essentials shape we read off Razorpay's create-order
// response. They return a lot more fields, but we only persist what we
// need to correlate the webhook later.
type Order struct {
	ID       string `json:"id"`       // order_xxx
	Amount   int    `json:"amount"`   // paise
	Currency string `json:"currency"` // "INR"
	Status   string `json:"status"`   // "created"
}

// CreateOrder makes a one-time order. notes are passed through to the
// payment.captured webhook so we can dispatch by kind without a DB
// lookup. amount is in paise.
func (c *Client) CreateOrder(amountPaise int, currency string, notes map[string]string) (*Order, error) {
	if c == nil {
		return nil, fmt.Errorf("razorpay client unavailable")
	}
	body := map[string]any{
		"amount":   amountPaise,
		"currency": currency,
		"notes":    notes,
		// Auto-capture: Razorpay captures the payment automatically when
		// it succeeds. Without this we'd need a separate capture call.
		"payment_capture": 1,
	}
	var out Order
	if err := c.do("POST", "/orders", body, &out); err != nil {
		return nil, fmt.Errorf("create order: %w", err)
	}
	return &out, nil
}

// Subscription is the subset of Razorpay's subscription resource we
// persist. `total_count: 12` means the eMandate is authorised for 12
// charges (a year), then auto-renews. We don't surface that to the user
// — they see "auto-renew, cancel anytime".
type Subscription struct {
	ID         string `json:"id"`          // sub_xxx
	PlanID     string `json:"plan_id"`     // plan_xxx
	Status     string `json:"status"`      // "created" → "authenticated" → "active"
	TotalCount int    `json:"total_count"` // billing cycles authorised
}

// CreateSubscription kicks off an autopay flow. The user has to
// authorise the mandate via the Razorpay Checkout modal; this call just
// reserves the subscription_id we'll receive webhook events against.
//
// notes are stamped on the subscription so subscription.activated /
// charged / halted webhooks can carry our user_id back without a DB
// lookup.
func (c *Client) CreateSubscription(planID string, totalCount int, notes map[string]string) (*Subscription, error) {
	if c == nil {
		return nil, fmt.Errorf("razorpay client unavailable")
	}
	body := map[string]any{
		"plan_id":     planID,
		"total_count": totalCount,
		"notes":       notes,
		// Show the customer 0 in their bank statement during the mandate
		// authorisation step (₹0 verification charge), then real billing
		// starts from period 1.
		"customer_notify": 1,
	}
	var out Subscription
	if err := c.do("POST", "/subscriptions", body, &out); err != nil {
		return nil, fmt.Errorf("create subscription: %w", err)
	}
	return &out, nil
}

// CancelSubscription tells Razorpay to NOT renew at the next billing
// date. The user keeps premium until current_period_end. Razorpay will
// fire subscription.cancelled when that period elapses.
func (c *Client) CancelSubscription(subscriptionID string) error {
	if c == nil {
		return fmt.Errorf("razorpay client unavailable")
	}
	body := map[string]any{
		"cancel_at_cycle_end": 1, // honor "cancel-at-period-end" semantics
	}
	if err := c.do("POST", "/subscriptions/"+subscriptionID+"/cancel", body, nil); err != nil {
		return fmt.Errorf("cancel subscription: %w", err)
	}
	return nil
}

// VerifyWebhookSignature is the trust boundary for the webhook handler.
// Razorpay signs the request body with HMAC-SHA256(webhook_secret) and
// puts the hex digest in `X-Razorpay-Signature`. We recompute and
// compare with crypto/subtle to avoid timing attacks.
//
// `body` is the raw request bytes (caller must read them BEFORE any
// json.Unmarshal — we re-parse in the handler).
func VerifyWebhookSignature(body []byte, signature, secret string) bool {
	if signature == "" || secret == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	// hmac.Equal is constant-time.
	return hmac.Equal([]byte(strings.ToLower(signature)), []byte(strings.ToLower(expected)))
}

// do is the shared transport helper: marshal body → POST/GET → parse out.
// Razorpay errors come back as { error: { code, description, ... } }
// with HTTP 4xx/5xx; we surface the description so logs are useful.
func (c *Client) do(method, path string, body any, out any) error {
	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		bodyReader = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.SetBasicAuth(c.keyID, c.keySecret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		// Try to parse the structured error; fall back to raw body.
		var errBody struct {
			Error struct {
				Code        string `json:"code"`
				Description string `json:"description"`
			} `json:"error"`
		}
		if jerr := json.Unmarshal(respBody, &errBody); jerr == nil && errBody.Error.Description != "" {
			return fmt.Errorf("razorpay %d %s: %s", resp.StatusCode, errBody.Error.Code, errBody.Error.Description)
		}
		return fmt.Errorf("razorpay %d: %s", resp.StatusCode, string(respBody))
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode: %w", err)
		}
	}
	return nil
}
