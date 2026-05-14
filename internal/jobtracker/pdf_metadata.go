package jobtracker

import (
	"bytes"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/ledongthuc/pdf"
)

// PDFMetadata captures structural cues we can extract from an uploaded
// PDF resume without OCR or vision. The Resume AI flow passes a
// human-readable rendering of this (see (*PDFMetadata).String) to the
// AI prompt so the model can evaluate formatting-dependent checklist
// items (single_column, no_photo, font_consistency, bolding_policy,
// ats_safe_bullets, length_correct, short_link_labels) on uploaded
// PDFs that previously returned "na" because the AI only sees
// extracted text.
//
// Best-effort: when a field can't be determined confidently, we leave
// the zero value and rely on the prompt's "use what you have" framing.
type PDFMetadata struct {
	PageCount int

	// DistinctFonts is the set of font names used at least once.
	DistinctFonts []string

	// FontSizeCounts maps each rounded font size (points) to a count
	// of text runs at that size.
	FontSizeCounts map[float64]int

	// BoldRunCount is the number of text runs whose font name
	// contains a bold cue. The body-vs-header ratio is what the model
	// uses to decide bolding_policy.
	BoldRunCount int

	// BulletGlyphs collects distinct bullet-like leading characters
	// found at line starts.
	BulletGlyphs []string

	// SingleColumnLikely is a refined heuristic: groups text runs by
	// Y-coordinate (rows), takes the leftmost X of each row (line-
	// start), and checks if line-starts cluster on a single left
	// margin. Robust against LaTeX `\hfill` right-aligned tails.
	SingleColumnLikely bool

	// TextRuns is the total count of text spans on the document.
	TextRuns int

	// ImageCount is the number of image XObjects across all pages.
	// 0 → no_photo passes. ≥1 → no_photo fails (resume has a photo
	// or other visual that ATS scrapers may drop or mis-route).
	ImageCount int

	// HyperlinkURIs is the URIs of /Link annotations across all
	// pages — what the user actually clicks. Used for short_link_labels:
	// if any URI is >25 chars, the model checks whether it appears in
	// the visible resume text (verbatim) — if so, that's a raw long
	// URL on the page, which fails the check.
	HyperlinkURIs []string

	// LineStartHistogram is a debug field: rows-per-x-bucket (30pt
	// buckets). Used to inspect why the single-column heuristic
	// decided one way. NOT included in String() output.
	LineStartHistogram map[int]int
}

// String renders the metadata as the prompt-facing block. Stable
// format so the model can parse it predictably.
func (m *PDFMetadata) String() string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "PAGE COUNT: %d\n", m.PageCount)
	fmt.Fprintf(&b, "DISTINCT FONTS: %s\n", strings.Join(m.DistinctFonts, ", "))

	// Font sizes sorted descending so headers appear first.
	sizes := make([]float64, 0, len(m.FontSizeCounts))
	for sz := range m.FontSizeCounts {
		sizes = append(sizes, sz)
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(sizes)))
	var sizeParts []string
	for _, sz := range sizes {
		sizeParts = append(sizeParts, fmt.Sprintf("%.0fpt×%d", sz, m.FontSizeCounts[sz]))
	}
	fmt.Fprintf(&b, "FONT SIZES (size×count): %s\n", strings.Join(sizeParts, ", "))
	fmt.Fprintf(&b, "FONT VARIANTS: %d distinct font names\n", len(m.DistinctFonts))

	// Bolding policy hint: encode the decision directly so the model
	// doesn't have to interpret a ratio. Three regimes:
	//   - 0 bold runs              → passes (nothing is bold)
	//   - bold ratio < 10%          → likely headers/titles only, passes
	//   - bold ratio between 10-25% → mixed; check the text for bolded
	//                                 numbers/words inside bullets
	//   - bold ratio ≥ 25%          → fails; bolding is pervasive enough
	//                                 to suggest mid-sentence bolding
	ratio := 0.0
	if m.TextRuns > 0 {
		ratio = float64(m.BoldRunCount) / float64(m.TextRuns) * 100
	}
	hint := boldHint(m.BoldRunCount, ratio)
	fmt.Fprintf(&b, "BOLD RUNS: %d / %d total text runs (%.1f%% of runs are bold). %s\n",
		m.BoldRunCount, m.TextRuns, ratio, hint)

	fmt.Fprintf(&b, "BULLET GLYPHS: %s\n", strings.Join(m.BulletGlyphs, " "))

	// Column hint: encode the heuristic's decision in plain language.
	if m.SingleColumnLikely {
		b.WriteString("COLUMNS: single-column (line-start clustering confirms one left margin) — passes ATS-parsing check\n")
	} else {
		b.WriteString("COLUMNS: multi-column likely (line-starts split across two distinct left margins) — fails ATS-parsing check; verify by reading the resume text\n")
	}

	// Images: explicit decision text so the AI doesn't second-guess.
	if m.ImageCount == 0 {
		b.WriteString("IMAGES: 0 embedded images detected — passes no_photo check\n")
	} else {
		fmt.Fprintf(&b, "IMAGES: %d embedded image(s) detected — likely a photo, logo, or design element. Fails no_photo unless the resume text suggests a non-photo decorative use only.\n", m.ImageCount)
	}

	// Hyperlinks: list the URIs the user actually clicks. If any
	// long URI appears verbatim in the resume text, that's a raw URL
	// shown as text → fails short_link_labels. The model does the
	// substring check against the resume text body.
	if len(m.HyperlinkURIs) == 0 {
		b.WriteString("HYPERLINKS: 0 clickable links detected in the PDF annotations — short_link_labels not applicable; mark na if no links visible in text, otherwise judge from text\n")
	} else {
		b.WriteString("HYPERLINKS (URIs from PDF Link annotations — these are what the candidate's links point to):\n")
		for _, u := range m.HyperlinkURIs {
			fmt.Fprintf(&b, "  - %s\n", u)
		}
		b.WriteString("For short_link_labels: pass if every URI above is hidden behind a short label (LinkedIn / GitHub / domain.com); fail if any of those URIs appears VERBATIM in the resume text (= the long URL is shown as raw text instead of a label).\n")
	}

	return b.String()
}

// boldHint returns a one-liner the prompt embeds next to the bold ratio.
// Splitting this out so the decision is documented as code, not just
// in the formatted string.
func boldHint(count int, ratio float64) string {
	switch {
	case count == 0:
		return "Nothing is bold; passes bolding_policy (no mid-sentence bolding possible)."
	case ratio < 10:
		return "Low bold ratio — likely confined to titles, companies, dates, and section headers. Passes bolding_policy."
	case ratio < 25:
		return "Moderate bold ratio — check the resume text for bolded numbers or words inside bullets (e.g. \\textbf{80%}). Warn if mid-sentence bolding is present, otherwise pass."
	default:
		return "High bold ratio — pervasive bolding suggests mid-sentence emphasis inside body bullets. Fails bolding_policy."
	}
}

// ExtractPDFMetadata reads an uploaded PDF's bytes and pulls
// structural cues. Returns nil + nil on any error so the caller can
// fall through to the no-metadata prompt path (we never block a score
// request on metadata extraction).
func ExtractPDFMetadata(data []byte) *PDFMetadata {
	if len(data) == 0 {
		return nil
	}
	defer func() {
		// pdf lib panics on some malformed inputs — swallow.
		_ = recover()
	}()
	r := bytes.NewReader(data)
	doc, err := pdf.NewReader(r, int64(len(data)))
	if err != nil {
		return nil
	}

	m := &PDFMetadata{
		PageCount:      doc.NumPage(),
		FontSizeCounts: map[float64]int{},
	}
	fontSet := map[string]struct{}{}
	bulletSet := map[string]struct{}{}
	uriSet := map[string]struct{}{}
	// Group runs by Y-bucket so we can find line-starts later.
	// Use the unexported struct type defined at package scope (pdfRunXY)
	// so the helper function's signature stays simple.
	allRuns := []pdfRunXY{}

	for i := 1; i <= m.PageCount; i++ {
		page := doc.Page(i)
		if page.V.IsNull() {
			continue
		}

		// --- text runs ---
		texts := page.Content().Text
		for _, t := range texts {
			m.TextRuns++
			if t.Font != "" {
				fontSet[t.Font] = struct{}{}
				if looksBold(t.Font) {
					m.BoldRunCount++
				}
			}
			if t.FontSize > 0 {
				rounded := math.Round(t.FontSize*2) / 2
				m.FontSizeCounts[rounded]++
			}
			if t.X > 0 || t.Y > 0 {
				allRuns = append(allRuns, pdfRunXY{X: t.X, Y: t.Y})
			}
			s := strings.TrimSpace(t.S)
			if len(s) > 0 && len(s) <= 2 && isBulletLike(s) {
				bulletSet[s] = struct{}{}
			}
		}

		// --- images via /Resources/XObject ---
		// Each named XObject is a dict; Subtype /Image is an image.
		// Some PDFs embed shading or form XObjects under the same
		// XObject dict, so we filter explicitly.
		xobj := page.Resources().Key("XObject")
		for _, name := range xobj.Keys() {
			obj := xobj.Key(name)
			if obj.Key("Subtype").Name() == "Image" {
				m.ImageCount++
			}
		}

		// --- hyperlinks via /Annots[].Subtype = "Link" ---
		// A /Link annotation either has /A (action dict with /URI for
		// external URLs) or /Dest (internal jumps — skip those).
		annots := page.V.Key("Annots")
		for j := 0; j < annots.Len(); j++ {
			a := annots.Index(j)
			if a.Key("Subtype").Name() != "Link" {
				continue
			}
			uri := strings.TrimSpace(a.Key("A").Key("URI").Text())
			if uri == "" {
				continue
			}
			uriSet[uri] = struct{}{}
		}
	}

	for f := range fontSet {
		m.DistinctFonts = append(m.DistinctFonts, f)
	}
	sort.Strings(m.DistinctFonts)
	for g := range bulletSet {
		m.BulletGlyphs = append(m.BulletGlyphs, g)
	}
	sort.Strings(m.BulletGlyphs)
	for u := range uriSet {
		m.HyperlinkURIs = append(m.HyperlinkURIs, u)
	}
	sort.Strings(m.HyperlinkURIs)

	m.SingleColumnLikely = inferSingleColumnFromLineStarts(allRuns)
	m.LineStartHistogram = lineStartBins(allRuns)

	return m
}

// looksBold heuristically tags a font name as bold-ish. PDF font
// names typically encode weight in the name. We cover three naming
// conventions:
//
//   1. English-word weights: "Bold", "Black", "Heavy".
//   2. PostScript suffixes:  "Times-Bold", "Helvetica-BoldOblique",
//                            "ArialMT-Bold", anything with a "-Bold"
//                            segment.
//   3. LaTeX font-family codes (the tricky one):
//      - CMBX*    Computer Modern Bold eXtended (the most common LaTeX bold)
//      - CMBSY*   Computer Modern Bold Symbol
//      - CMBXTI*  Computer Modern Bold eXtended Text Italic
//      - CMBCSC*  Computer Modern Bold Caps and Small Caps
//      - CMSSBX*  Computer Modern Sans Serif Bold eXtended
//      - LMBX*    Latin Modern Bold eXtended
//      - LMSANS*BX / LMSSBX*  Latin Modern Sans Bold
//      - ptmb*    Times Bold (Berry naming scheme)
//      - phvb*    Helvetica Bold
//      - pbk*     Bookman (no bold marker — skip)
//
// The user's resume showed CMBX10 + CMBX12 not being detected,
// which is the case that broke the bolding_policy check.
func looksBold(font string) bool {
	f := strings.ToLower(font)

	// English weight markers — works for most non-LaTeX PDFs.
	for _, marker := range []string{
		"bold", "black", "heavy",
		"-b,", "-b ", // some PS encodings
	} {
		if strings.Contains(f, marker) {
			return true
		}
	}
	if strings.HasSuffix(f, "-b") {
		return true
	}

	// LaTeX / Computer Modern / Latin Modern family codes.
	// All of these encode "bold" inside the family name without
	// using the word.
	latexBoldPrefixes := []string{
		"cmbx",   // CM Bold eXtended
		"cmbsy",  // CM Bold Symbol
		"cmbxti", // CM Bold eXtended Text Italic
		"cmbcsc", // CM Bold Caps & Small Caps
		"cmssbx", // CM Sans Serif Bold eXtended
		"lmbx",   // Latin Modern Bold eXtended
		"lmssbx", // LM Sans Bold eXtended
		"ptmb",   // Times Bold (Berry)
		"phvb",   // Helvetica Bold
	}
	for _, p := range latexBoldPrefixes {
		if strings.HasPrefix(f, p) {
			return true
		}
	}
	return false
}

// isBulletLike returns true for common bullet markers or decorative
// glyphs ATS may drop.
func isBulletLike(s string) bool {
	if len(s) == 1 {
		switch s {
		case "•", "·", "▪", "■", "◦", "▶", "▸", "❯", "➔", "✦", "★", "-", "*":
			return true
		}
	}
	return false
}

// pdfRunXY is just the (X, Y) of a text run — used to find line-starts
// for the single-column heuristic. Defined at package scope so the
// helper signature stays clean.
type pdfRunXY struct {
	X float64
	Y float64
}

// inferSingleColumnFromLineStarts groups text runs into rows by
// Y-coordinate, takes the leftmost X per row (the line-start), and
// checks if line-starts cluster on a single left margin.
//
// Real-world LaTeX resumes have:
//   - The bulk of rows at one left margin (body text)
//   - A handful of centered rows for header (name/headline/contact)
//   - Bullet rows slightly indented (small offset)
//
// True multi-column resumes have ~half the rows starting at a left
// margin and the other half starting at a clearly different left
// margin (>100pt offset).
//
// Heuristic: collapse the rows into x-bins, find the bin with the
// most rows, and call it single-column if the dominant bin contains
// ≥55% of rows. The threshold was lowered from 70% → 55% because
// LaTeX resumes have legitimate non-body rows (centered headers,
// indented bullets) that pull the dominant share below 70%.
func inferSingleColumnFromLineStarts(runs []pdfRunXY) bool {
	if len(runs) < 20 {
		return true
	}
	const yBin = 8.0
	const leftBin = 30.0
	rowMinX := map[int]float64{}
	for _, r := range runs {
		k := int(r.Y / yBin)
		if cur, ok := rowMinX[k]; !ok || r.X < cur {
			rowMinX[k] = r.X
		}
	}
	if len(rowMinX) < 5 {
		return true
	}
	bins := map[int]int{}
	for _, x := range rowMinX {
		bins[int(x/leftBin)]++
	}
	var max int
	for _, c := range bins {
		if c > max {
			max = c
		}
	}
	share := float64(max) / float64(len(rowMinX))
	return share >= 0.55
}

// lineStartBins is a debug helper that returns the row count per
// 30pt-x bucket. Used to log the distribution for tuning the
// single-column heuristic on real-world PDFs.
func lineStartBins(runs []pdfRunXY) map[int]int {
	const yBin = 8.0
	const leftBin = 30.0
	rowMinX := map[int]float64{}
	for _, r := range runs {
		k := int(r.Y / yBin)
		if cur, ok := rowMinX[k]; !ok || r.X < cur {
			rowMinX[k] = r.X
		}
	}
	bins := map[int]int{}
	for _, x := range rowMinX {
		bins[int(x/leftBin)]++
	}
	return bins
}
