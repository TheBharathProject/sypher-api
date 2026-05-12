package jobtracker

import (
	"encoding/json"
	"testing"
)

func TestValidateCommunityMetadata(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		surface string
		raw     json.RawMessage
		wantErr bool
		field   string // non-empty: assert err.Field == field
	}{
		// ----------------------------------------------------------------
		// nil / empty metadata — always passes regardless of surface
		// ----------------------------------------------------------------
		{
			name:    "nil metadata passes for any surface",
			surface: "experiences",
			raw:     nil,
			wantErr: false,
		},
		{
			name:    "zero-length metadata passes",
			surface: "experiences",
			raw:     json.RawMessage(""),
			wantErr: false,
		},

		// ----------------------------------------------------------------
		// experiences — outcome
		// ----------------------------------------------------------------
		{
			name:    "experiences valid outcome Offer passes",
			surface: "experiences",
			raw:     json.RawMessage(`{"outcome":"Offer"}`),
			wantErr: false,
		},
		{
			name:    "experiences valid outcome Reject passes",
			surface: "experiences",
			raw:     json.RawMessage(`{"outcome":"Reject"}`),
			wantErr: false,
		},
		{
			name:    "experiences valid outcome Ghosted passes",
			surface: "experiences",
			raw:     json.RawMessage(`{"outcome":"Ghosted"}`),
			wantErr: false,
		},
		{
			name:    "experiences valid outcome InProgress passes",
			surface: "experiences",
			raw:     json.RawMessage(`{"outcome":"InProgress"}`),
			wantErr: false,
		},
		{
			name:    "experiences valid outcome Withdrew passes",
			surface: "experiences",
			raw:     json.RawMessage(`{"outcome":"Withdrew"}`),
			wantErr: false,
		},
		{
			name:    "experiences old value Rejected fails",
			surface: "experiences",
			raw:     json.RawMessage(`{"outcome":"Rejected"}`),
			wantErr: true,
			field:   "outcome",
		},
		{
			name:    "experiences old value In Progress fails",
			surface: "experiences",
			raw:     json.RawMessage(`{"outcome":"In Progress"}`),
			wantErr: true,
			field:   "outcome",
		},
		{
			name:    "experiences absent outcome passes",
			surface: "experiences",
			raw:     json.RawMessage(`{"company":"Acme"}`),
			wantErr: false,
		},
		{
			name:    "experiences empty string outcome passes",
			surface: "experiences",
			raw:     json.RawMessage(`{"outcome":""}`),
			wantErr: false,
		},

		// ----------------------------------------------------------------
		// experiences — difficulty
		// ----------------------------------------------------------------
		{
			name:    "experiences valid difficulty Easy passes",
			surface: "experiences",
			raw:     json.RawMessage(`{"difficulty":"Easy"}`),
			wantErr: false,
		},
		{
			name:    "experiences invalid difficulty fails",
			surface: "experiences",
			raw:     json.RawMessage(`{"difficulty":"Very Hard"}`),
			wantErr: true,
			field:   "difficulty",
		},

		// ----------------------------------------------------------------
		// ask — tags
		// ----------------------------------------------------------------
		{
			name:    "ask 3 valid tags passes",
			surface: "ask",
			raw:     json.RawMessage(`{"tags":["Career","Interview","Compensation"]}`),
			wantErr: false,
		},
		{
			name:    "ask 1 valid tag passes",
			surface: "ask",
			raw:     json.RawMessage(`{"tags":["Layoffs"]}`),
			wantErr: false,
		},
		{
			name:    "ask 0 tags passes",
			surface: "ask",
			raw:     json.RawMessage(`{"tags":[]}`),
			wantErr: false,
		},
		{
			name:    "ask 4 tags fails",
			surface: "ask",
			raw:     json.RawMessage(`{"tags":["Career","Interview","Compensation","Remote"]}`),
			wantErr: true,
			field:   "tags",
		},
		{
			name:    "ask invalid tag name fails",
			surface: "ask",
			raw:     json.RawMessage(`{"tags":["FAANG"]}`),
			wantErr: true,
			field:   "tags",
		},
		{
			name:    "ask all valid tags from allowlist pass",
			surface: "ask",
			raw:     json.RawMessage(`{"tags":["Visa","Fresher","Tools"]}`),
			wantErr: false,
		},

		// ----------------------------------------------------------------
		// recruiters — specializations
		// ----------------------------------------------------------------
		{
			name:    "recruiters valid specialization Tech passes",
			surface: "recruiters",
			raw:     json.RawMessage(`{"specializations":["Tech"]}`),
			wantErr: false,
		},
		{
			name:    "recruiters valid specialization Campus passes",
			surface: "recruiters",
			raw:     json.RawMessage(`{"specializations":["Campus","Contract"]}`),
			wantErr: false,
		},
		{
			name:    "recruiters invalid specialization fails",
			surface: "recruiters",
			raw:     json.RawMessage(`{"specializations":["Backend"]}`),
			wantErr: true,
			field:   "specializations",
		},
		{
			name:    "recruiters empty specializations passes",
			surface: "recruiters",
			raw:     json.RawMessage(`{"specializations":[]}`),
			wantErr: false,
		},

		// ----------------------------------------------------------------
		// recruiters — hiringLevels
		// ----------------------------------------------------------------
		{
			name:    "recruiters valid hiringLevel Senior passes",
			surface: "recruiters",
			raw:     json.RawMessage(`{"hiringLevels":["Senior"]}`),
			wantErr: false,
		},
		{
			name:    "recruiters invalid hiringLevel fails",
			surface: "recruiters",
			raw:     json.RawMessage(`{"hiringLevels":["Entry Level"]}`),
			wantErr: true,
			field:   "hiringLevels",
		},

		// ----------------------------------------------------------------
		// reviews — targetRole
		// ----------------------------------------------------------------
		{
			name:    "reviews valid targetRole SDE passes",
			surface: "reviews",
			raw:     json.RawMessage(`{"targetRole":"SDE"}`),
			wantErr: false,
		},
		{
			name:    "reviews valid targetRole PM passes",
			surface: "reviews",
			raw:     json.RawMessage(`{"targetRole":"PM"}`),
			wantErr: false,
		},
		{
			name:    "reviews invalid targetRole Consultant fails",
			surface: "reviews",
			raw:     json.RawMessage(`{"targetRole":"Consultant"}`),
			wantErr: true,
			field:   "targetRole",
		},
		{
			name:    "reviews absent targetRole passes",
			surface: "reviews",
			raw:     json.RawMessage(`{"company":"Google"}`),
			wantErr: false,
		},

		// ----------------------------------------------------------------
		// reviews — experienceLevel
		// ----------------------------------------------------------------
		{
			name:    "reviews valid experienceLevel Fresher passes",
			surface: "reviews",
			raw:     json.RawMessage(`{"experienceLevel":"Fresher"}`),
			wantErr: false,
		},
		{
			name:    "reviews valid experienceLevel Mid (2-5) passes",
			surface: "reviews",
			raw:     json.RawMessage(`{"experienceLevel":"Mid (2-5)"}`),
			wantErr: false,
		},
		{
			name:    "reviews invalid experienceLevel fails",
			surface: "reviews",
			raw:     json.RawMessage(`{"experienceLevel":"Senior Engineer"}`),
			wantErr: true,
			field:   "experienceLevel",
		},

		// ----------------------------------------------------------------
		// referrals — always passes
		// ----------------------------------------------------------------
		{
			name:    "referrals with any metadata passes",
			surface: "referrals",
			raw:     json.RawMessage(`{"anything":"goes"}`),
			wantErr: false,
		},
		{
			name:    "referrals nil metadata passes",
			surface: "referrals",
			raw:     nil,
			wantErr: false,
		},

		// ----------------------------------------------------------------
		// unknown surface — passes (don't block unknown surfaces)
		// ----------------------------------------------------------------
		{
			name:    "unknown surface passes",
			surface: "unknown-surface",
			raw:     json.RawMessage(`{"field":"value"}`),
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateCommunityMetadata(tc.surface, tc.raw)
			if tc.wantErr && err == nil {
				t.Fatalf("expected validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
			if tc.wantErr && err != nil && tc.field != "" {
				if err.Field != tc.field {
					t.Fatalf("expected error field %q, got %q (message: %s)", tc.field, err.Field, err.Message)
				}
			}
		})
	}
}
