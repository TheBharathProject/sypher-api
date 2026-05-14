# sypher-tex — LaTeX compile sidecar

Operational runbook for the LaTeX compile service that backs the Resume
Builder's PDF export. Pairs with [ADR-0008](./adr/0008-resume-builder.md).

`sypher-tex` is a separate container (not part of the `sypher-api` image)
that exposes `POST /builds/sync` over the internal Docker network. The Go
backend calls it via `RenderResumeBuilderPDF`
(`internal/jobtracker/resume_builder_render.go`).

---

## What it is

A ~1.5 GB image built on `texlive/texlive:latest` (full TeX Live) wrapped
in a 50-line Flask HTTP server. The contract intentionally mirrors
YtoTech's `latex-on-http` so the Go side stays portable.

Source lives **outside** this repo on the developer's machine:

```
~/sypher-tex/
├── Dockerfile      # texlive/texlive + python3-flask
└── server.py       # Flask wrapper, /builds/sync + /health
```

The full file contents are reproduced at the bottom of this doc for
recovery if the local copy is lost.

### HTTP contract

```
POST /builds/sync
Content-Type: application/json

{
  "compiler": "pdflatex" | "xelatex" | "lualatex",
  "resources": [{ "main": true, "content": "<.tex source>" }]
}
```

Responses:

| Status | Body                                            | Meaning                              |
|--------|-------------------------------------------------|--------------------------------------|
| 200    | `application/pdf` (raw bytes)                   | Compile succeeded                    |
| 400    | `{ "logs": "<tail>", "error": "compile failed" }` | LaTeX compile error                  |
| 504    | `{ "error": "compile timed out after 90s" }`    | Subprocess exceeded `COMPILE_TIMEOUT_SEC` |

```
GET /health  -> 200 {"status":"ok"}
```

The Go client (`RenderResumeBuilderPDF`) bounds the HTTP call at 60s,
maps 4xx → `compile_failed` with log tail, and maps 5xx /
network-unreachable → `errLatexServiceUnavailable` (handlers turn that
into HTTP 503 `latex_service_unavailable`).

---

## Local development (macOS, Apple Silicon)

One-time setup:

```bash
mkdir -p ~/sypher-tex
cd ~/sypher-tex
# Create Dockerfile + server.py from the templates at the bottom of this doc.

docker build -t sypher-tex:local .
```

The build pulls `texlive/texlive:latest` (multi-arch — works native on
arm64, no Rosetta needed) and installs `python3-flask`. First build is
~5 min; subsequent rebuilds are seconds.

Run:

```bash
docker run -d \
  --name sypher-tex \
  -p 8090:8080 \
  --restart unless-stopped \
  sypher-tex:local
```

Set in the local `.env` for `sypher-api`:

```
LATEX_SERVICE_URL=http://localhost:8090
```

Then `go run ./cmd/api` — the server logs `latex service configured`
on startup if it picked up the env var.

Smoke test from the host:

```bash
curl -s -X POST http://localhost:8090/builds/sync \
  -H 'Content-Type: application/json' \
  -d '{"compiler":"pdflatex","resources":[{"main":true,"content":"\\documentclass{article}\\begin{document}Hello\\end{document}"}]}' \
  -o /tmp/hello.pdf
file /tmp/hello.pdf   # expect: PDF document, version 1.5
```

---

## Production (OCI VM)

The OCI VM already runs `sypher-api` + `sypher-postgres` on the
`sypher-net` Docker network. `sypher-tex` joins that network so
`sypher-api` reaches it at the alias `sypher-tex`.

One-time, from inside the VM (NOT through CI):

```bash
# 1. Ensure the network exists
docker network create sypher-net 2>/dev/null || true

# 2. Build the image on the VM (or push from your laptop, see below)
mkdir -p ~/sypher-tex && cd ~/sypher-tex
# scp Dockerfile + server.py up, or recreate from this doc
docker build -t sypher-tex:latest .

# 3. Run the container, attached to sypher-net
docker run -d \
  --name sypher-tex \
  --network sypher-net \
  --restart unless-stopped \
  sypher-tex:latest

# 4. Tell sypher-api where to find it
echo 'LATEX_SERVICE_URL=http://sypher-tex:8080' >> ~/.pg-secret

# 5. Roll sypher-api so it picks up the env var
bash ~/sypher-api/scripts/deploy.sh
```

No host port is published — only containers on `sypher-net` can reach
the service.

### Building on laptop, transferring to VM

If the VM doesn't have enough disk for the 1.5 GB build context, build
locally and ship the image:

```bash
# Laptop
docker build --platform linux/amd64 -t sypher-tex:latest ~/sypher-tex
docker save sypher-tex:latest | gzip > /tmp/sypher-tex.tar.gz
scp /tmp/sypher-tex.tar.gz oci:/tmp/

# VM
gunzip -c /tmp/sypher-tex.tar.gz | docker load
```

Note the `--platform linux/amd64` flag — the OCI VM is x86, your laptop
is probably arm64. Without that flag the image won't run on the VM.

---

## Debugging compile failures

The user-facing flow on a 4xx already surfaces the last ~60 lines of
the LaTeX log via the Resume Builder UI (`rb-tex-preview-log` block).
For deeper debugging:

```bash
# Tail the container output for the failing request
docker logs -f sypher-tex

# Exec in and inspect a failing compile by hand
docker exec -it sypher-tex bash
cd /tmp
echo '\documentclass{article}\begin{document}Hi\end{document}' > t.tex
pdflatex -interaction=nonstopmode -halt-on-error t.tex
cat t.log
```

Common failure shapes:

| Symptom                                        | Cause / Fix                                                                |
|------------------------------------------------|----------------------------------------------------------------------------|
| `! LaTeX Error: File X.sty not found`          | Package missing from the image (rare with full TeX Live). Confirm spelling. |
| `\pdfglyphtounicode` undefined                 | Document is using xelatex without the `pdftex` primitive. Switch to pdflatex. |
| `\begin{itemize} on input line N ended by \end{document}` | Document is missing `\end{itemize}` (or analogous `\resumeSubHeadingListEnd`). |
| HTTP 504 from sidecar                          | Compile exceeded 90s. Almost always an infinite-loop macro — check the doc. |
| HTTP 503 from sypher-api                       | Sidecar container is down or `LATEX_SERVICE_URL` wasn't picked up. `docker ps`, then bounce sypher-api. |

The Go client splits "compile failed" vs "service unavailable" — the
former is the user's LaTeX problem (4xx), the latter is our infra
problem (5xx / network).

---

## Bumping TeX Live

The base image `texlive/texlive:latest` updates a few times a year. To
pull a newer TeX Live:

```bash
cd ~/sypher-tex
docker pull texlive/texlive:latest
docker build --no-cache -t sypher-tex:latest .
docker stop sypher-tex && docker rm sypher-tex
docker run -d --name sypher-tex --network sypher-net --restart unless-stopped sypher-tex:latest
```

No `sypher-api` redeploy needed — the HTTP contract is stable.

If you want to pin a specific TeX Live release for reproducibility,
change the base image to e.g. `texlive/texlive:TL2024-historic` and
rebuild.

---

## Resource footprint

| Item                | Value                                |
|---------------------|--------------------------------------|
| Image size          | ~1.5 GB on disk                      |
| Idle RAM            | ~40 MB (Flask only; TeX Live disk-resident) |
| Per-compile RAM     | ~150–300 MB (pdflatex, typical resume) |
| Per-compile CPU     | One core, 1–4 s for a real resume    |
| Per-compile disk    | TemporaryDirectory, cleaned on success/failure |
| Concurrency model   | Flask threaded=True, one subprocess per request |

For the current user base (single user, occasional compiles) this is
ample. If concurrency grows, the easiest scale-up is multiple replicas
behind a round-robin alias — no shared state.

---

## When to retire this

Triggers that would warrant rethinking:

- Multi-tenant Resume Builder with concurrent compiles → swap Flask for
  gunicorn workers OR run multiple replicas; still cheap.
- Cold-start latency becomes a complaint (>5 s on first compile per
  container restart) → pre-warm the font cache by compiling a small
  doc in the Dockerfile.
- Need to support Overleaf-style packages that aren't in CTAN → unlikely
  for resumes; tlmgr install during build if it ever happens.
- Replace with a managed LaTeX-as-a-service → only if SLA matters; we
  pay no hosting overhead today since the VM has spare capacity.

---

## File reference

### `~/sypher-tex/Dockerfile`

```dockerfile
FROM texlive/texlive:latest
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3-flask \
    && rm -rf /var/lib/apt/lists/*
COPY server.py /app/server.py
WORKDIR /tmp
EXPOSE 8080
ENV PYTHONUNBUFFERED=1
CMD ["python3", "/app/server.py"]
```

### `~/sypher-tex/server.py`

See the actual file on disk. The contract is documented at the top of
this doc; the implementation is intentionally minimal (one endpoint,
one `subprocess.run`, no queue, no cache).

Key constants:

- `COMPILE_TIMEOUT_SEC = 90` — subprocess wall-clock bound
- `ALLOWED_COMPILERS = {"xelatex", "pdflatex", "lualatex"}` — only
  these three are accepted; arbitrary binaries are rejected
- Port `8080` inside container, published as host `8090` in local dev,
  reached as `http://sypher-tex:8080` on `sypher-net` in prod

---

## Related

- [ADR-0008 — Resume Builder](./adr/0008-resume-builder.md) — design rationale
- `internal/jobtracker/resume_builder_render.go` — Go client
- `internal/jobtracker/handlers_resume_builder.go` — HTTP handlers
- `scripts/deploy.sh` — `LATEX_SERVICE_URL` is in `OPTIONAL_VARS`
- `.env.example` — variable documentation for new contributors
