package jobtracker

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/TheBharathProject/sypher-api/internal/ai"
	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/billing"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

// Resume Builder — POST /job-tracker/resume-builder/parse
//
// Convert an uploaded resume (PDF via fileId, or pasted plain text)
// into the structured DraftContent shape the editor uses, so the user
// doesn't have to retype a resume they already have. The new draft
// itself is created by the FE via the existing POST …/drafts endpoint
// after this returns — keeping the parse step idempotent and the
// create step retriable on its own.

// maxResumeParseChars caps the text we feed the model. Deepseek's
// context is bigger than this, but a 12k cap (~3.5k tokens) covers any
// realistic resume while keeping latency / token bills predictable.
const maxResumeParseChars = 12000

type parseResumeInput struct {
	FileID string `json:"fileId,omitempty"`
	Text   string `json:"text,omitempty"`
}

type parseResumeResult struct {
	Content DraftContent `json:"content"`
	Title   string       `json:"title"`
}

func (h *Handler) ParseResumeForBuilder(w http.ResponseWriter, r *http.Request) {
	if !h.requireAI(w) {
		return
	}

	uid := auth.MustUserID(r.Context())
	var in parseResumeInput
	if !readJSON(w, r, &in) {
		return
	}

	// 1. Resolve resume text from one of the two input modes.
	var resumeText string
	var sourceTitleHint string // best-effort fallback if AI doesn't surface a name
	switch {
	case strings.TrimSpace(in.Text) != "":
		resumeText = in.Text
	case in.FileID != "":
		if !h.requireR2(w) {
			return
		}
		fid, err := uuid.Parse(in.FileID)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "bad_id", "invalid fileId")
			return
		}
		file, err := h.store.GetFile(r.Context(), uid, fid)
		if err != nil {
			writeDBError(w, err)
			return
		}
		body, err := h.r2.FetchAll(r.Context(), file.StorageKey)
		if err != nil {
			httpx.WriteError(w, http.StatusBadGateway, "fetch_failed", err.Error())
			return
		}
		resumeText = extractText(body, file.MimeType, file.FileName)
		sourceTitleHint = stripFileExtension(file.FileName)
	default:
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "fileId or text is required")
		return
	}

	resumeText = strings.TrimSpace(resumeText)
	if resumeText == "" {
		httpx.WriteError(w, http.StatusBadRequest, "empty_resume", "could not extract any text from the resume")
		return
	}
	if len(resumeText) > maxResumeParseChars {
		resumeText = resumeText[:maxResumeParseChars]
	}

	// 2. Gate billing — same two-tier model as ResumeReport / CoverLetter.
	if _, ok := h.gateAICredit(w, r, uid, billing.CostResumeParse, billing.ReasonResumeParse); !ok {
		return
	}

	// 3. AI parse → ParsedResume.
	parsed, usage, err := h.ai.ResumeParseToContent(r.Context(), resumeText)
	if err != nil {
		httpx.WriteError(w, http.StatusBadGateway, "ai_failed", err.Error())
		return
	}
	if usage != nil && h.aiUsage != nil {
		if rErr := h.aiUsage.Record(r.Context(), uid, "resume_parse", usage.TokensIn, usage.TokensOut); rErr != nil {
			// Non-fatal — log via the standard path and continue.
			fmt.Printf("resume_parse: token usage record failed: %v\n", rErr)
		}
	}

	// 4. Sanitize → DraftContent + title.
	content := sanitizeParsedResume(parsed)
	title := pickDraftTitle(content.Personal.Name, sourceTitleHint)

	httpx.WriteJSON(w, http.StatusOK, parseResumeResult{
		Content: content,
		Title:   title,
	})
}

// sanitizeParsedResume converts the AI's free-form ParsedResume into
// DraftContent, enforcing caps + trimming so a hallucinated mega-resume
// can't blow up the editor.
func sanitizeParsedResume(p *ai.ParsedResume) DraftContent {
	if p == nil {
		return DraftContent{}
	}

	const (
		maxExperiences = 20
		maxEducations  = 10
		maxProjects    = 20
		maxSkillGroups = 20
		maxSkillItems  = 30
		maxBullets     = 30
		maxBulletChars = 600
		maxStringChars = 500
	)

	clip := func(s string) string {
		s = strings.TrimSpace(s)
		if len(s) > maxStringChars {
			s = s[:maxStringChars]
		}
		return s
	}

	out := DraftContent{
		Personal: DraftPersonal{
			Name:        clip(p.Personal.Name),
			Headline:    clip(p.Personal.Headline),
			Email:       clip(p.Personal.Email),
			Phone:       clip(p.Personal.Phone),
			Location:    clip(p.Personal.Location),
			LinkedinURL: clip(p.Personal.LinkedinURL),
			GithubURL:   clip(p.Personal.GithubURL),
			WebsiteURL:  clip(p.Personal.WebsiteURL),
		},
		Summary: clip(p.Summary),
	}

	for i, e := range p.Experiences {
		if i >= maxExperiences {
			break
		}
		exp := DraftExperience{
			Company:   clip(e.Company),
			Title:     clip(e.Title),
			Location:  clip(e.Location),
			StartDate: clip(e.StartDate),
			EndDate:   clip(e.EndDate),
			Current:   e.Current,
		}
		for j, b := range e.Description {
			if j >= maxBullets {
				break
			}
			b = strings.TrimSpace(b)
			if b == "" {
				continue
			}
			if len(b) > maxBulletChars {
				b = b[:maxBulletChars]
			}
			exp.Description = append(exp.Description, b)
		}
		// Drop fully-empty entries.
		if exp.Company == "" && exp.Title == "" && len(exp.Description) == 0 {
			continue
		}
		out.Experiences = append(out.Experiences, exp)
	}

	for i, e := range p.Educations {
		if i >= maxEducations {
			break
		}
		ed := DraftEducation{
			School:      clip(e.School),
			Degree:      clip(e.Degree),
			Field:       clip(e.Field),
			StartDate:   clip(e.StartDate),
			EndDate:     clip(e.EndDate),
			GPA:         clip(e.GPA),
			Description: clip(e.Description),
		}
		if ed.School == "" && ed.Degree == "" && ed.Field == "" {
			continue
		}
		out.Educations = append(out.Educations, ed)
	}

	for i, pr := range p.Projects {
		if i >= maxProjects {
			break
		}
		proj := DraftProject{
			Name:        clip(pr.Name),
			Description: clip(pr.Description),
			TechStack:   clip(pr.TechStack),
			Link:        clip(pr.Link),
		}
		if proj.Name == "" && proj.Description == "" {
			continue
		}
		out.Projects = append(out.Projects, proj)
	}

	for i, g := range p.Skills {
		if i >= maxSkillGroups {
			break
		}
		group := DraftSkillGroup{Category: clip(g.Category)}
		for j, item := range g.Items {
			if j >= maxSkillItems {
				break
			}
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			if len(item) > maxStringChars {
				item = item[:maxStringChars]
			}
			group.Items = append(group.Items, item)
		}
		if len(group.Items) == 0 {
			continue
		}
		if group.Category == "" {
			group.Category = "Skills"
		}
		out.Skills = append(out.Skills, group)
	}

	return out
}

// pickDraftTitle prefers the candidate's parsed name; if missing, falls
// back to the uploaded file's name (stripped of extension); if also
// missing, lands on a neutral default.
func pickDraftTitle(parsedName, fileNameHint string) string {
	name := strings.TrimSpace(parsedName)
	if name != "" {
		return fmt.Sprintf("%s's resume", name)
	}
	if fileNameHint != "" {
		return fileNameHint
	}
	return "Imported resume"
}

func stripFileExtension(s string) string {
	if idx := strings.LastIndex(s, "."); idx > 0 {
		return s[:idx]
	}
	return s
}
