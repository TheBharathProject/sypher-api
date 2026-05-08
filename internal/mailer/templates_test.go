package mailer

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestRenderDigestEmpty(t *testing.T) {
	// Empty items should error rather than send a useless email. Cron
	// guards against this upstream (skips users with len(items) == 0)
	// but the template must also fail closed.
	_, _, _, err := RenderDigest(DigestData{Items: nil})
	if err == nil {
		t.Fatal("RenderDigest with no items should error")
	}
}

func TestRenderDigestBasics(t *testing.T) {
	d := DigestData{
		UserName: "Shubham",
		Items: []DigestItem{
			{
				Company: "Stripe",
				Role:    "SDE-2",
				Reason:  "stale",
				Detail:  "no movement for 14 days",
				URL:     "https://sypher.in/pegasus/applications#" + uuid.New().String(),
			},
			{
				Company: "Linear",
				Role:    "iOS Engineer",
				Reason:  "deadline_soon",
				Detail:  "deadline tomorrow",
				URL:     "https://sypher.in/pegasus/applications#" + uuid.New().String(),
			},
		},
		DashboardURL: "https://sypher.in/pegasus/dashboard",
	}

	subject, html, text, err := RenderDigest(d)
	if err != nil {
		t.Fatalf("RenderDigest err: %v", err)
	}

	// Subject reflects the count and pluralises correctly.
	if want := "Pegasus daily digest — 2 updates"; subject != want {
		t.Errorf("subject = %q, want %q", subject, want)
	}

	// HTML body has the expected anchors. Not asserting the entire
	// markup — that would be a maintenance trap. Just the things that
	// would silently break templating: company names, links, both
	// recipients of the kicker pill.
	for _, want := range []string{
		"Stripe",
		"SDE-2",
		"Linear",
		"iOS Engineer",
		"Stale",          // the eyebrow on the stale item
		"Deadline approaching", // the eyebrow on the deadline-soon item
		"Open Pegasus",   // the CTA button text
		d.DashboardURL,
		d.Items[0].URL,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("html body missing %q\n--- body ---\n%s", want, html)
		}
	}

	// Plain-text body — same expectations, lower bar.
	for _, want := range []string{
		"Stripe",
		"Linear",
		"daily digest",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text body missing %q\n--- body ---\n%s", want, text)
		}
	}
}

func TestRenderDigestSingularSubject(t *testing.T) {
	// One-item subject must NOT pluralise.
	subject, _, _, err := RenderDigest(DigestData{
		Items: []DigestItem{{Company: "x", Role: "y", Reason: "stale", Detail: "d"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "Pegasus daily digest — 1 update"; subject != want {
		t.Errorf("subject = %q, want %q", subject, want)
	}
}
