package jobtracker

import "net/http"

// RegisterRoutes wires every /job-tracker/* path onto the mux. The auth
// middleware is passed in so this package doesn't need to know about JWT
// secrets — server.go composes the dependency graph.
func RegisterRoutes(mux *http.ServeMux, h *Handler, requireUser func(http.Handler) http.Handler) {
	g := func(method, path string, fn http.HandlerFunc) {
		mux.Handle(method+" "+path, requireUser(fn))
	}

	// Me
	g("GET", "/job-tracker/me", h.GetMe)
	g("PATCH", "/job-tracker/me/name", h.UpdateName)
	g("PATCH", "/job-tracker/me/timezone", h.UpdateTimezone)
	g("POST", "/job-tracker/me/api-token", h.IssueAPIToken)
	g("GET", "/job-tracker/me/api-tokens", h.ListAPITokens)
	g("DELETE", "/job-tracker/me/api-tokens/{id}", h.RevokeAPIToken)
	g("POST", "/job-tracker/me/delete", h.DeleteAccount)
	g("PATCH", "/job-tracker/me/email-prefs", h.UpdateEmailPrefs)

	// Notifications (Phase 2)
	g("GET", "/job-tracker/notifications", h.ListNotifications)
	g("GET", "/job-tracker/notifications/count", h.UnreadCount)
	g("PATCH", "/job-tracker/notifications/{id}/read", h.MarkNotificationRead)
	g("PATCH", "/job-tracker/notifications/read-all", h.MarkAllNotificationsRead)

	// Dashboard analytics
	g("GET", "/job-tracker/analytics/dashboard", h.Dashboard)

	// Applications
	g("GET", "/job-tracker/applications", h.ListApplications)
	g("POST", "/job-tracker/applications", h.CreateApplication)
	g("GET", "/job-tracker/applications/export", h.ExportApplications)
	g("POST", "/job-tracker/applications/import", h.ImportApplications)
	g("POST", "/job-tracker/applications/import/preview", h.PreviewImportApplications)
	g("GET", "/job-tracker/applications/import/template", h.ApplicationsTemplate)
	g("GET", "/job-tracker/applications/check-link", h.CheckApplicationByLink)
	g("GET", "/job-tracker/applications/{id}", h.GetApplication)
	g("GET", "/job-tracker/applications/{id}/timeline", h.ApplicationTimeline)
	// PUT historically; PATCH is the REST-correct verb for partial updates.
	// Both are registered for one release window so deployed FE + extension
	// clients keep working. Prefer PATCH in new code.
	g("PUT", "/job-tracker/applications/{id}", h.UpdateApplication)
	g("PATCH", "/job-tracker/applications/{id}", h.UpdateApplication)
	g("DELETE", "/job-tracker/applications/{id}", h.DeleteApplication)

	// Notes
	g("GET", "/job-tracker/notes", h.ListNotes)
	g("POST", "/job-tracker/notes", h.CreateNote)
	g("GET", "/job-tracker/notes/categories", h.ListCategories)
	g("POST", "/job-tracker/notes/categories", h.CreateCategory)
	g("DELETE", "/job-tracker/notes/categories/{id}", h.DeleteCategory)
	g("GET", "/job-tracker/notes/{id}", h.GetNote)
	g("PUT", "/job-tracker/notes/{id}", h.UpdateNote)
	g("PATCH", "/job-tracker/notes/{id}", h.UpdateNote)
	g("DELETE", "/job-tracker/notes/{id}", h.DeleteNote)

	// Profile root
	g("GET", "/job-tracker/profile", h.GetProfile)
	g("PUT", "/job-tracker/profile", h.UpdateProfile)
	g("PATCH", "/job-tracker/profile", h.UpdateProfile)
	g("GET", "/job-tracker/profile/slug", h.GetSlug)
	g("GET", "/job-tracker/profile/slug/check", h.CheckSlug)
	g("PATCH", "/job-tracker/profile/slug", h.UpdateSlug)
	g("PATCH", "/job-tracker/profile/visibility", h.UpdateVisibility)

	// Profile sub-resources
	g("GET", "/job-tracker/profile/experiences", h.ListExperiences)
	g("POST", "/job-tracker/profile/experiences", h.CreateExperience)
	g("PUT", "/job-tracker/profile/experiences/{id}", h.UpdateExperience)
	g("PATCH", "/job-tracker/profile/experiences/{id}", h.UpdateExperience)
	g("DELETE", "/job-tracker/profile/experiences/{id}", h.DeleteExperience)

	g("GET", "/job-tracker/profile/educations", h.ListEducations)
	g("POST", "/job-tracker/profile/educations", h.CreateEducation)
	g("PUT", "/job-tracker/profile/educations/{id}", h.UpdateEducation)
	g("PATCH", "/job-tracker/profile/educations/{id}", h.UpdateEducation)
	g("DELETE", "/job-tracker/profile/educations/{id}", h.DeleteEducation)

	g("GET", "/job-tracker/profile/projects", h.ListProjects)
	g("POST", "/job-tracker/profile/projects", h.CreateProject)
	g("PUT", "/job-tracker/profile/projects/{id}", h.UpdateProject)
	g("PATCH", "/job-tracker/profile/projects/{id}", h.UpdateProject)
	g("DELETE", "/job-tracker/profile/projects/{id}", h.DeleteProject)

	g("GET", "/job-tracker/profile/skills", h.ListSkills)
	g("POST", "/job-tracker/profile/skills", h.CreateSkill)
	g("DELETE", "/job-tracker/profile/skills/{id}", h.DeleteSkill)

	// Feedback (auth-gated; user_id captured from token)
	g("POST", "/job-tracker/feedback", h.PostFeedback)

	// ----- Phase 2: Files (R2) -----
	g("GET", "/job-tracker/resumes", h.ListResumes)
	g("POST", "/job-tracker/resumes/upload-url", h.RequestResumeUploadURL)
	g("PATCH", "/job-tracker/resumes/{id}/finalize", h.FinalizeResume)
	g("PATCH", "/job-tracker/resumes/{id}", h.UpdateFileLabel)
	g("DELETE", "/job-tracker/resumes/{id}", h.DeleteResume)
	g("GET", "/job-tracker/resumes/{id}/usage", h.ResumeUsage)
	g("GET", "/job-tracker/resumes/{id}/view-url", h.ResumeViewURL)

	g("GET", "/job-tracker/cover-letters", h.ListCoverLetters)
	g("POST", "/job-tracker/cover-letters/upload-url", h.RequestCoverLetterUploadURL)
	g("PATCH", "/job-tracker/cover-letters/{id}/finalize", h.FinalizeCoverLetter)
	g("PATCH", "/job-tracker/cover-letters/{id}", h.UpdateFileLabel)
	g("DELETE", "/job-tracker/cover-letters/{id}", h.DeleteCoverLetter)
	g("GET", "/job-tracker/cover-letters/{id}/view-url", h.CoverLetterViewURL)

	// ----- Phase 2: AI (Deepseek) -----
	g("POST", "/job-tracker/resume/extract", h.ExtractResume)
	g("POST", "/job-tracker/ai/resume/report", h.GenerateResumeReport)
	g("GET", "/job-tracker/ai/resume/report/latest", h.LatestResumeReport)
	g("POST", "/job-tracker/ai/cover-letter", h.GenerateCoverLetter)
	g("GET", "/job-tracker/ai/usage", h.AIUsage)

	// Resume tweaks — versioned AI rewrites. POST charges 20 credits
	// once the 25k-token free monthly quota is exhausted. List/get/patch/
	// delete are free. Schema: migrations/0015_resume_tweaks.sql.
	g("POST", "/job-tracker/ai/resume/tweaks", h.CreateResumeTweak)
	g("GET", "/job-tracker/ai/resume/tweaks", h.ListResumeTweaks)
	g("GET", "/job-tracker/ai/resume/tweaks/{id}", h.GetResumeTweak)
	g("PATCH", "/job-tracker/ai/resume/tweaks/{id}", h.PatchResumeTweak)
	g("DELETE", "/job-tracker/ai/resume/tweaks/{id}", h.DeleteResumeTweak)

	// ----- Phase 3: community -----
	g("GET", "/job-tracker/community/{surface}", h.ListCommunity)
	g("POST", "/job-tracker/community/{surface}", h.CreateCommunityPost)
	g("GET", "/job-tracker/community/posts/{id}", h.GetCommunityPost)
	g("PATCH", "/job-tracker/community/posts/{id}", h.UpdateCommunityPost)
	g("DELETE", "/job-tracker/community/posts/{id}", h.DeleteCommunityPost)
	g("POST", "/job-tracker/community/posts/{id}/vote", h.VoteOnCommunityPost)
	g("POST", "/job-tracker/community/posts/{id}/flag", h.FlagCommunityPost)
	g("GET", "/job-tracker/community/posts/{id}/comments", h.ListCommunityComments)
	g("POST", "/job-tracker/community/posts/{id}/comments", h.CreateCommunityComment)
	g("DELETE", "/job-tracker/community/comments/{id}", h.DeleteCommunityComment)

	// Public profile + analytics — NO auth middleware
	mux.Handle("GET /job-tracker/public/profile/{slug}", http.HandlerFunc(h.PublicProfile))
	mux.Handle("GET /job-tracker/public/analytics/{slug}", http.HandlerFunc(h.PublicAnalytics))
	mux.Handle("GET /job-tracker/public/community/{surface}", http.HandlerFunc(h.PublicListCommunity))
	mux.Handle("GET /job-tracker/public/community/posts/{id}", http.HandlerFunc(h.PublicGetCommunityPost))
}
