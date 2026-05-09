// Package billing — Razorpay-backed payments for Pegasus premium tier
// and AI credits. See docs/adr/0006-razorpay-billing.md.
//
// Layout:
//   packs.go      Pricing tables (premium pass + credit packs).
//   client.go     Thin Razorpay HTTP client (orders + subscriptions).
//   store.go      Postgres data access for billing.* + auth.users.
//   handlers.go   POST /billing/checkout/* + GET /billing/me + cancel.
//   webhook.go    POST /webhooks/razorpay (signature-verified, public).
package billing

import "fmt"

// Premium pricing — INR, in paise. Two recurring tiers + one-time pass.
//
// Razorpay requires a server-side Plan resource per tier (created once
// in the dashboard, referenced via env). The handler picks the right
// plan_id based on which checkout endpoint was hit.
const (
	PremiumMonthlyPaise   = 9900  // ₹99 — standard recurring tier (premium only)
	PremiumPlusPaise      = 29900 // ₹299 — bundle tier (premium + bundle credits)
	PremiumPassDays       = 30    // one-time pass duration
	PremiumPlusBundleCred = 200   // credits granted to plus tier on every charge
)

// CreditPack defines a server-authoritative top-up bundle. Pricing is
// hard-coded so a malicious client can't request custom amounts. If we
// need promotional pricing later, this can move to a DB table.
//
// The mapping is deliberately tier'd — each step up gives slightly more
// credits per rupee, nudging users toward larger packs without making
// the small one feel punitive.
// JSON tags are camelCase to match the frontend's ApiBillingMe type.
// Without these, Go's encoding/json default to PascalCase field names
// (AmountPaise, Credits, …) and the frontend reads `pack.amountPaise`
// as undefined → renders "₹NaN" + sends an empty packId on checkout.
type CreditPack struct {
	ID           string `json:"id"`           // stable handle the frontend sends in checkout body
	Label        string `json:"label"`        // human-friendly name shown on /upgrade
	AmountPaise  int    `json:"amountPaise"`  // total charge in paise (1 INR = 100 paise)
	Credits      int    `json:"credits"`      // credits added on successful payment
}

var creditPacks = []CreditPack{
	{ID: "starter", Label: "Starter", AmountPaise: 9900, Credits: 100},
	{ID: "plus", Label: "Plus", AmountPaise: 24900, Credits: 350},
	{ID: "pro", Label: "Pro", AmountPaise: 49900, Credits: 700},
}

// CreditPacks returns a copy of the pack list — exported for the handler
// that surfaces packs to the frontend (so the UI doesn't need to
// duplicate prices).
func CreditPacks() []CreditPack {
	out := make([]CreditPack, len(creditPacks))
	copy(out, creditPacks)
	return out
}

// PackByID resolves a pack handle from the request body. Returns an
// error if the id is unknown — handlers map that to 400 bad_input.
func PackByID(id string) (CreditPack, error) {
	for _, p := range creditPacks {
		if p.ID == id {
			return p, nil
		}
	}
	return CreditPack{}, fmt.Errorf("unknown credit pack: %q", id)
}
