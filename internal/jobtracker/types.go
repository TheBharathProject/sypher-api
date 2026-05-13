// Package jobtracker owns every /job-tracker/* endpoint:
//
//	types.go             request/response shapes shared across handlers
//	store.go             SQL — talks to Postgres
//	routes.go            RegisterRoutes wires every path onto the mux
//	handlers_me.go       /me, /me/name, /me/timezone, /me/api-token
//	handlers_dashboard.go /analytics/dashboard
//	handlers_applications.go /applications + import/export
//	handlers_notes.go    /notes + /notes/categories
//	handlers_profile.go  /profile + sub-resources + slug + visibility
//	handlers_public.go   /public/profile/{slug} + /public/analytics/{slug}
//	handlers_feedback.go /feedback
//
// All authenticated endpoints are gated by auth.RequireUser; handlers
// retrieve the user UUID via auth.MustUserID(ctx).
package jobtracker

import "github.com/google/uuid"

// Application is the row + JSON shape exposed at /job-tracker/applications.
type Application struct {
	ID              uuid.UUID `json:"id"`
	Company         string    `json:"company"`
	Role            string    `json:"role"`
	Source          string    `json:"source,omitempty"`
	Location        string    `json:"location,omitempty"`
	SalaryRange     string    `json:"salaryRange,omitempty"`
	Stage           string    `json:"stage"`
	AppliedAt       string    `json:"appliedAt,omitempty"`
	ApplyDeadline   string    `json:"applyDeadline,omitempty"`
	JobLink         string    `json:"jobLink,omitempty"`
	JobDescription  string    `json:"jobDescription,omitempty"`
	Notes           string    `json:"notes,omitempty"`
	Stale           bool      `json:"stale"`
	StageChangedAt  string    `json:"stageChangedAt,omitempty"`
	CreatedAt       string    `json:"createdAt"`
	UpdatedAt       string    `json:"updatedAt"`
}

// ApplicationInput is the writable surface (POST/PUT bodies).
type ApplicationInput struct {
	Company        string `json:"company"`
	Role           string `json:"role"`
	Source         string `json:"source"`
	Location       string `json:"location"`
	SalaryRange    string `json:"salaryRange"`
	Stage          string `json:"stage"`
	AppliedAt      string `json:"appliedAt"`
	ApplyDeadline  string `json:"applyDeadline"`
	JobLink        string `json:"jobLink"`
	JobDescription string `json:"jobDescription"`
	Notes          string `json:"notes"`
	Stale          bool   `json:"stale"`
}

// StageChange is one row from job_tracker.application_stage_history,
// shaped for /applications/{id}/timeline. From is empty on the initial
// "application created" row.
type StageChange struct {
	From      string `json:"from,omitempty"`
	To        string `json:"to"`
	ChangedAt string `json:"changedAt"`
}

// DashboardMetrics is the response body for /analytics/dashboard.
// Keep flat for now — funnel/weekly/topSources are computed client-side
// per the parity audit decision (see docs/LIVE_PARITY_AUDIT.md §2.2).
type DashboardMetrics struct {
	Total          int     `json:"total"`
	InPipeline     int     `json:"inPipeline"`
	Interviews     int     `json:"interviews"`
	Offers         int     `json:"offers"`
	AddedThisWeek  int     `json:"addedThisWeek"`
	ResponseRate   float64 `json:"responseRate"`
	ConversionRate float64 `json:"conversionRate"`
}

// ResumeTweak is the full row exposed at /ai/resume/tweaks/{id}. The
// `parent_id`/`application_id`/`source_file_id` references are flattened
// to plain string pointers in JSON so the FE doesn't have to know about
// pgx's null-uuid type. UserEdits is the optional post-AI manual edit
// the user persists via PATCH.
type ResumeTweak struct {
	ID            uuid.UUID `json:"id"`
	ParentID      *string   `json:"parentId,omitempty"`
	ApplicationID *string   `json:"applicationId,omitempty"`
	SourceFileID  *string   `json:"sourceFileId,omitempty"`
	Title         string    `json:"title"`
	SourceText    string    `json:"sourceText"`
	Prompt        string    `json:"prompt"`
	TweakedText   string    `json:"tweakedText"`
	UserEdits     string    `json:"userEdits,omitempty"`
	TokensIn      int       `json:"tokensIn"`
	TokensOut     int       `json:"tokensOut"`
	CreatedAt     string    `json:"createdAt"`
	UpdatedAt     string    `json:"updatedAt"`
}

// ResumeTweakSummary is the lightweight row returned by the list endpoint —
// drops the heavy text blobs so the history pane stays cheap to render.
type ResumeTweakSummary struct {
	ID            uuid.UUID `json:"id"`
	ParentID      *string   `json:"parentId,omitempty"`
	ApplicationID *string   `json:"applicationId,omitempty"`
	Title         string    `json:"title"`
	CreatedAt     string    `json:"createdAt"`
	UpdatedAt     string    `json:"updatedAt"`
}

// ResumeTweakInput is the writable surface (POST body). Either sourceFileId
// (a Vault resume) or sourceText (pasted) is required when ParentID is
// empty; if ParentID is set, the parent row's text supplies the source.
type ResumeTweakInput struct {
	ApplicationID string `json:"applicationId"`
	ParentID      string `json:"parentId"`
	SourceFileID  string `json:"sourceFileId"`
	SourceText    string `json:"sourceText"`
	Prompt        string `json:"prompt"`
	Title         string `json:"title"`
}

// ResumeTweakEdit is the PATCH body (subset of the full row that's
// user-editable post-creation). Empty fields mean "don't change".
type ResumeTweakEdit struct {
	Title     *string `json:"title,omitempty"`
	UserEdits *string `json:"userEdits,omitempty"`
}

// FunnelEntry is one stage's count in the public-analytics funnel.
type FunnelEntry struct {
	Stage string `json:"stage"`
	Count int    `json:"count"`
}

// WeeklyEntry is one calendar-week bucket of applied_at counts.
type WeeklyEntry struct {
	WeekStart string `json:"weekStart"` // YYYY-MM-DD (Monday of week, UTC)
	Count     int    `json:"count"`
}

// PublicAnalytics mirrors the live /api/public/analytics/{slug} shape.
type PublicAnalytics struct {
	Summary        DashboardMetrics `json:"summary"`
	Funnel         []FunnelEntry    `json:"funnel"`
	WeeklyActivity []WeeklyEntry    `json:"weeklyActivity"`
}

// Note is the full record returned from GET /notes/{id}.
type Note struct {
	ID         uuid.UUID  `json:"id"`
	CategoryID *uuid.UUID `json:"categoryId,omitempty"`
	Title      string     `json:"title"`
	Content    string     `json:"content"`
	Pinned     bool       `json:"pinned"`
	CreatedAt  string     `json:"createdAt"`
	UpdatedAt  string     `json:"updatedAt"`
}

// NoteListItem is the slim shape returned in GET /notes — body replaced by excerpt.
type NoteListItem struct {
	ID         uuid.UUID  `json:"id"`
	CategoryID *uuid.UUID `json:"categoryId,omitempty"`
	Title      string     `json:"title"`
	Excerpt    string     `json:"excerpt"`
	Pinned     bool       `json:"pinned"`
	CreatedAt  string     `json:"createdAt"`
	UpdatedAt  string     `json:"updatedAt"`
}

type NoteInput struct {
	CategoryID *string `json:"categoryId,omitempty"`
	Title      string  `json:"title"`
	Content    string  `json:"content"`
	Pinned     bool    `json:"pinned"`
}

type Category struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Color string    `json:"color,omitempty"`
}

type CategoryInput struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// Profile is the user's public-facing identity record.
type Profile struct {
	Slug        string       `json:"slug,omitempty"`
	IsPublic    bool         `json:"isPublic"`
	Headline    string       `json:"headline,omitempty"`
	About       string       `json:"about,omitempty"`
	Location    string       `json:"location,omitempty"`
	LinkedinURL string       `json:"linkedinUrl,omitempty"`
	GithubURL   string       `json:"githubUrl,omitempty"`
	WebsiteURL  string       `json:"websiteUrl,omitempty"`
	Experiences []Experience `json:"experiences"`
	Educations  []Education  `json:"educations"`
	Projects    []Project    `json:"projects"`
	Skills      []Skill      `json:"skills"`
}

type ProfileInput struct {
	Headline    string `json:"headline"`
	About       string `json:"about"`
	Location    string `json:"location"`
	LinkedinURL string `json:"linkedinUrl"`
	GithubURL   string `json:"githubUrl"`
	WebsiteURL  string `json:"websiteUrl"`
}

type Experience struct {
	ID          uuid.UUID `json:"id"`
	Company     string    `json:"company"`
	Title       string    `json:"title"`
	Location    string    `json:"location,omitempty"`
	StartDate   string    `json:"startDate,omitempty"` // YYYY-MM-DD
	EndDate     string    `json:"endDate,omitempty"`
	Current     bool      `json:"current"`
	Description string    `json:"description,omitempty"`
	SortOrder   int       `json:"sortOrder"`
}

type ExperienceInput struct {
	Company     string `json:"company"`
	Title       string `json:"title"`
	Location    string `json:"location"`
	StartDate   string `json:"startDate"`
	EndDate     string `json:"endDate"`
	Current     bool   `json:"current"`
	Description string `json:"description"`
	SortOrder   int    `json:"sortOrder"`
}

type Education struct {
	ID          uuid.UUID `json:"id"`
	School      string    `json:"school"`
	Degree      string    `json:"degree,omitempty"`
	Field       string    `json:"field,omitempty"`
	StartDate   string    `json:"startDate,omitempty"`
	EndDate     string    `json:"endDate,omitempty"`
	GPA         string    `json:"gpa,omitempty"`
	Description string    `json:"description,omitempty"`
	SortOrder   int       `json:"sortOrder"`
}

type EducationInput struct {
	School      string `json:"school"`
	Degree      string `json:"degree"`
	Field       string `json:"field"`
	StartDate   string `json:"startDate"`
	EndDate     string `json:"endDate"`
	GPA         string `json:"gpa"`
	Description string `json:"description"`
	SortOrder   int    `json:"sortOrder"`
}

type Project struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	TechStack   string    `json:"techStack,omitempty"`
	Link        string    `json:"link,omitempty"`
	SortOrder   int       `json:"sortOrder"`
}

type ProjectInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	TechStack   string `json:"techStack"`
	Link        string `json:"link"`
	SortOrder   int    `json:"sortOrder"`
}

type Skill struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Category  string    `json:"category,omitempty"`
	SortOrder int       `json:"sortOrder"`
}

type SkillInput struct {
	Name      string `json:"name"`
	Category  string `json:"category"`
	SortOrder int    `json:"sortOrder"`
}

// PublicProfile is the unauthenticated /public/profile/{slug} response.
// Shape matches live: name + pictureUrl from auth.users; everything else
// from the user's profile + sub-resources. Skills are full objects, not strings.
type PublicProfile struct {
	Slug        string       `json:"slug"`
	Name        string       `json:"name"`
	PictureURL  string       `json:"pictureUrl,omitempty"`
	Headline    string       `json:"headline,omitempty"`
	About       string       `json:"about,omitempty"`
	Location    string       `json:"location,omitempty"`
	LinkedinURL string       `json:"linkedinUrl,omitempty"`
	GithubURL   string       `json:"githubUrl,omitempty"`
	WebsiteURL  string       `json:"websiteUrl,omitempty"`
	Experiences []Experience `json:"experiences"`
	Educations  []Education  `json:"educations"`
	Projects    []Project    `json:"projects"`
	Skills      []Skill      `json:"skills"`
}

// FeedbackInput is the body for POST /feedback.
type FeedbackInput struct {
	Message string         `json:"message"`
	Context map[string]any `json:"context"`
}

// validStages enforces the stage enum on the server. Source of truth lives
// here so the frontend can't write a stage we don't support.
var validStages = map[string]struct{}{
	"INTERESTED":   {},
	"APPLIED":      {},
	"PHONE_SCREEN": {},
	"TECHNICAL":    {},
	"ONSITE":       {},
	"OFFER":        {},
	"REJECTED":     {},
}

func isValidStage(s string) bool {
	_, ok := validStages[s]
	return ok
}

// validSources enforces the source enum. Empty string is accepted —
// users routinely save applications without remembering exactly where
// they found the JD. Otherwise must match one of these literal values;
// the FE select uses the same set.
var validSources = map[string]struct{}{
	"":             {},
	"LINKEDIN":     {},
	"NAUKRI":       {},
	"REFERRAL":     {},
	"COMPANY_SITE": {},
	"OTHER":        {},
}

func isValidSource(s string) bool {
	_, ok := validSources[s]
	return ok
}

// Reminder is the full row exposed via the reminders endpoints.
type Reminder struct {
	ID            string  `json:"id"`
	ApplicationID string  `json:"applicationId"`
	TriggersAt    string  `json:"triggersAt"`
	Note          *string `json:"note,omitempty"`
	FiredAt       *string `json:"firedAt,omitempty"`
	CreatedAt     string  `json:"createdAt"`
	UpdatedAt     string  `json:"updatedAt"`
}

// ReminderInput is the POST body for creating a reminder.
type ReminderInput struct {
	TriggersAt string  `json:"triggersAt"`
	Note       *string `json:"note,omitempty"`
}

// ReminderEdit is the PATCH body.
type ReminderEdit struct {
	TriggersAt *string `json:"triggersAt,omitempty"`
	Note       *string `json:"note,omitempty"`
}

// DueReminder is a minimal row used by the reminders cron job.
type DueReminder struct {
	ID            uuid.UUID
	UserID        uuid.UUID
	ApplicationID uuid.UUID
	Note          *string
}

// Recruiter is a personal (private) recruiter contact. Phone is intentionally
// omitted here — it is served by the dedicated GET …/{id}/phone endpoint which
// enforces its own rate limit to protect sensitive contact data.
type Recruiter struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Email       string  `json:"email"`
	Company     *string `json:"company,omitempty"`
	LinkedinURL *string `json:"linkedinUrl,omitempty"`
	Notes       *string `json:"notes,omitempty"`
	CreatedAt   string  `json:"createdAt"`
	UpdatedAt   string  `json:"updatedAt"`
}

// RecruiterPhone is the response body for GET /job-tracker/recruiters/{id}/phone.
type RecruiterPhone struct {
	Phone *string `json:"phone"`
}

// RecruiterInput is the POST body.
type RecruiterInput struct {
	Name        string  `json:"name"`
	Email       string  `json:"email"`
	Company     *string `json:"company,omitempty"`
	LinkedinURL *string `json:"linkedinUrl,omitempty"`
	Phone       *string `json:"phone,omitempty"`
	Notes       *string `json:"notes,omitempty"`
}

// RecruiterEdit is the PATCH body (all optional).
type RecruiterEdit struct {
	Name        *string `json:"name,omitempty"`
	Email       *string `json:"email,omitempty"`
	Company     *string `json:"company,omitempty"`
	LinkedinURL *string `json:"linkedinUrl,omitempty"`
	Phone       *string `json:"phone,omitempty"`
	Notes       *string `json:"notes,omitempty"`
}
