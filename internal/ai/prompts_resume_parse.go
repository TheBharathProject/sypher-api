package ai

// Resume → DraftContent JSON extractor.
//
// The model is asked to turn a chunk of resume text (PDF-extracted or
// pasted) into JSON that matches the Resume Builder's DraftContent
// shape. The handler then sanity-checks + sanitises that JSON before
// persisting it as a new draft.

const resumeParseSystemPrompt = `You convert a candidate's resume into structured JSON for a resume builder.

Output STRICT JSON only — no preamble, no markdown fences, no comments.

The JSON MUST match this exact shape (omit keys with no data; do NOT invent fields):

{
  "personal": {
    "name": "string",
    "headline": "string",
    "email": "string",
    "phone": "string",
    "location": "string",
    "linkedinUrl": "string",
    "githubUrl": "string",
    "websiteUrl": "string"
  },
  "summary": "string",
  "experiences": [
    {
      "company": "string",
      "title": "string",
      "location": "string",
      "startDate": "string",
      "endDate": "string",
      "current": false,
      "description": ["bullet 1", "bullet 2"]
    }
  ],
  "educations": [
    {
      "school": "string",
      "degree": "string",
      "field": "string",
      "startDate": "string",
      "endDate": "string",
      "gpa": "string",
      "description": "string"
    }
  ],
  "projects": [
    {
      "name": "string",
      "description": "string",
      "techStack": "string",
      "link": "string"
    }
  ],
  "skills": [
    {
      "category": "string",
      "items": ["string"]
    }
  ]
}

Rules:
- Extract ONLY what the resume actually says. Never invent dates, employers, schools, GPAs, links, or metrics.
- "description" inside experiences is an array of bullets — split on bullet glyphs (•, -, *) or newlines. Never return it as a single string.
- "current" is true when the role's end date is "Present", "Current", "Now", or empty for an obviously-ongoing job; false otherwise.
- Dates stay as the candidate wrote them — "Jan 2024", "2019 – 2023", "Present" — do NOT convert to ISO.
- Skills must be grouped by category (Languages, Frameworks, Tools, Databases, Soft Skills, etc.). If the resume just lists a flat skills bag with no categories, emit ONE group with category "Skills".
- linkedinUrl / githubUrl / websiteUrl: include only if a URL is actually present. Don't fabricate from a username.
- headline = the one-line role tagline near the name (e.g. "Senior Backend Engineer"). If absent, omit.
- summary = the "Summary" / "Profile" / "About" paragraph, if present. If absent, omit.
- If a field is empty, omit it (don't emit empty strings or empty arrays for optional fields).
- Output JSON only. Nothing else.`

const resumeParseRetryPrompt = `Your previous response failed to parse as JSON. Error: %s

Re-emit ONLY the JSON body, with no surrounding text, no markdown fences, no comments. Same schema as before.`
