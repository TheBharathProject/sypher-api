package jobtracker

import (
	"strings"
	"testing"
)

// Smoke test: render jakes-resume with a representative DraftContent and
// assert the macros the FE parser anchors on are present. This is the
// minimum check that protects the parser contract — see jakes-resume.ts.
func TestRenderResumeBuilderTeX_JakesResume_EmitsParserAnchors(t *testing.T) {
	content := DraftContent{
		Personal: DraftPersonal{
			Name:        "Ada Lovelace",
			Headline:    "Computer Scientist",
			Email:       "ada@example.com",
			Phone:       "+1 555 0100",
			Location:    "London, UK",
			LinkedinURL: "https://linkedin.com/in/ada",
			GithubURL:   "https://github.com/ada",
		},
		Summary: "Pioneer of computing & analytical engines.",
		Experiences: []DraftExperience{
			{
				Company:     "Analytical Engine Co.",
				Title:       "Lead Programmer",
				Location:    "London",
				StartDate:   "Jan 2024",
				EndDate:     "",
				Current:     true,
				Description: []string{"Wrote the first algorithm.", "Mentored Babbage."},
			},
		},
		Educations: []DraftEducation{
			{
				School:    "Self-taught",
				Degree:    "BSc",
				Field:     "Mathematics",
				StartDate: "1830",
				EndDate:   "1835",
				GPA:       "3.9",
			},
		},
		Projects: []DraftProject{
			{
				Name:        "Note G",
				TechStack:   "Punch cards, Bernoulli numbers",
				Description: "First published algorithm.",
				Link:        "https://example.com/note-g",
			},
		},
		Skills: []DraftSkillGroup{
			{Category: "Languages", Items: []string{"Mathematics", "Symbolic logic"}},
		},
		Style: DraftStyle{
			AccentColor:     "1a1a1a",
			SectionDivider:  "solid",
			HeaderAlignment: "center",
		},
	}

	tex, err := RenderResumeBuilderTeX(content, "jakes-resume")
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}

	wantContains := []string{
		`{\Huge \scshape Ada Lovelace}`,
		`\href{mailto:ada@example.com}`,
		`\section*{Summary}`,
		`\section*{Education}`,
		`\section*{Experience}`,
		`\section*{Projects}`,
		`\section*{Skills}`,
		`\resumeSubHeadingListStart`,
		`\resumeSubHeadingListEnd`,
		`\resumeSubheading`,
		`\resumeProjectHeading`,
		`\resumeItem{Wrote the first algorithm.}`,
		`\resumeItemListStart`,
		`\resumeItemListEnd`,
		`GPA: 3.9`,
		`\textbf{Languages}`,
		`Mathematics, Symbolic logic`,
		`-- Present`,
	}
	for _, sub := range wantContains {
		if !strings.Contains(tex, sub) {
			t.Errorf("rendered tex missing expected substring %q\n--- tex ---\n%s\n--- end ---", sub, tex)
		}
	}
}
