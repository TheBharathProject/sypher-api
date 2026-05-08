package mailer

import (
	"bytes"
	"fmt"
	"text/template"
)

// DigestItem is one line in the daily digest email — either a stale
// application or one with an approaching deadline. Caller (the cron
// job) decides which kind each item is and fills the matching fields.
type DigestItem struct {
	Company string
	Role    string
	// One of: "stale" | "deadline_soon"
	Reason string
	// Human-readable detail e.g. "no movement for 14 days" or
	// "deadline tomorrow". The template trusts this — sanitise upstream.
	Detail string
	// Path under sypher.in/pegasus that opens the application. Frontend
	// joins this with the apex prefix; emails ship absolute URLs.
	URL string
}

type DigestData struct {
	UserName string
	Items    []DigestItem
	// Absolute URL for the "Open Pegasus" button in the email footer.
	DashboardURL string
}

// RenderDigest builds the subject + html + text bodies for the daily digest.
// Exposed so the cron job can build a Message and hand it to mailer.Send.
//
// The template has zero JS — every email reader (Outlook, Gmail, Apple Mail,
// Spark, k9) needs to render it. Inline-styled HTML; one column; no images;
// no tracking pixels (intentional — see ADR-001 D7).
func RenderDigest(d DigestData) (subject, html, text string, err error) {
	if len(d.Items) == 0 {
		return "", "", "", fmt.Errorf("RenderDigest: empty items")
	}

	subject = fmt.Sprintf("Pegasus daily digest — %d update%s", len(d.Items), plural(len(d.Items)))

	htmlBuf := &bytes.Buffer{}
	if err = digestHTMLTmpl.Execute(htmlBuf, d); err != nil {
		return "", "", "", fmt.Errorf("html template: %w", err)
	}
	textBuf := &bytes.Buffer{}
	if err = digestTextTmpl.Execute(textBuf, d); err != nil {
		return "", "", "", fmt.Errorf("text template: %w", err)
	}
	return subject, htmlBuf.String(), textBuf.String(), nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// HTML template — single column, system fonts, neutral palette. Renders
// cleanly in dark-mode-aware clients via prefers-color-scheme without us
// having to maintain two palettes.
var digestHTMLTmpl = template.Must(template.New("digest_html").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8" />
<meta name="color-scheme" content="light dark" />
<meta name="supported-color-schemes" content="light dark" />
<title>Pegasus daily digest</title>
</head>
<body style="margin:0;padding:0;background:#f5f1eb;font-family:Georgia,'Times New Roman',serif;color:#1a1a1a;">
  <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background:#f5f1eb;padding:32px 16px;">
    <tr>
      <td align="center">
        <table role="presentation" width="540" cellpadding="0" cellspacing="0" border="0" style="max-width:540px;width:100%;background:#fdfcf9;border:1px solid #e0dbd1;border-radius:14px;padding:32px;">
          <tr><td>
            <p style="margin:0 0 6px 0;font-size:11px;letter-spacing:0.22em;text-transform:uppercase;color:#8a8a8a;">Pegasus</p>
            <h1 style="margin:0 0 18px 0;font-size:28px;font-weight:400;letter-spacing:-0.02em;line-height:1.05;">
              Today's nudge{{if .UserName}}, {{.UserName}}{{end}}.
            </h1>
            <p style="margin:0 0 24px 0;font-size:15px;line-height:1.6;color:#5a5a5a;font-style:italic;">
              {{len .Items}} application{{if ne (len .Items) 1}}s{{end}} could use a look.
            </p>

            {{range .Items}}
            <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin-bottom:14px;">
              <tr>
                <td style="padding:14px 0;border-top:1px solid #e0dbd1;">
                  <p style="margin:0 0 4px 0;font-size:11px;letter-spacing:0.18em;text-transform:uppercase;color:#8a8a8a;">
                    {{if eq .Reason "stale"}}Stale{{else}}Deadline approaching{{end}}
                  </p>
                  <p style="margin:0 0 4px 0;font-size:16px;font-weight:400;color:#1a1a1a;">
                    <strong>{{.Company}}</strong> · {{.Role}}
                  </p>
                  <p style="margin:0 0 8px 0;font-size:13.5px;color:#5a5a5a;font-style:italic;">{{.Detail}}</p>
                  {{if .URL}}<a href="{{.URL}}" style="font-size:12.5px;color:#1a1a1a;text-decoration:underline;">Open in Pegasus →</a>{{end}}
                </td>
              </tr>
            </table>
            {{end}}

            <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin-top:28px;border-top:1px solid #e0dbd1;padding-top:24px;">
              <tr><td>
                {{if .DashboardURL}}<a href="{{.DashboardURL}}" style="display:inline-block;padding:11px 22px;background:#1a1a1a;color:#fdfcf9;text-decoration:none;border-radius:8px;font-size:14px;font-family:-apple-system,'Helvetica Neue',Arial,sans-serif;">Open Pegasus</a>{{end}}
                <p style="margin:18px 0 0 0;font-size:11px;letter-spacing:0.06em;color:#8a8a8a;">
                  You're getting this because you have stale or deadline-approaching applications. Mute these in Settings (coming soon).
                </p>
              </td></tr>
            </table>
          </td></tr>
        </table>
      </td>
    </tr>
  </table>
</body>
</html>`))

// Plain-text fallback. Some readers (notably internal corporate gateways)
// strip HTML; this is what they show. Also indexed by spam filters.
var digestTextTmpl = template.Must(template.New("digest_text").Parse(`Pegasus — daily digest

{{if .UserName}}Hi {{.UserName}},
{{end}}{{len .Items}} application(s) could use a look today:

{{range .Items}}
[{{if eq .Reason "stale"}}STALE{{else}}DEADLINE{{end}}] {{.Company}} · {{.Role}}
  {{.Detail}}
  {{if .URL}}{{.URL}}{{end}}
{{end}}

{{if .DashboardURL}}Open Pegasus: {{.DashboardURL}}
{{end}}
You're getting this because you have stale or deadline-approaching
applications. Mute these in Settings (coming soon).
`))
