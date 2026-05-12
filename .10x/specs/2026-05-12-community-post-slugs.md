# Spec: Community Post Human-Readable Slugs

**Date:** 2026-05-12  
**Status:** Ready for implementation  
**Priority:** P1 — ships alongside Phase 12 community polish  
**Feature slug:** `community-post-slugs`

---

## Problem

Community post URLs currently look like:

```
/pegasus/community/posts/a3f2b1c0-5d6e-7f8a-9b0c-1d2e3f4a5b6c
```

This is non-shareable, ugly, and unfriendly to bookmarks, copy-paste in WhatsApp/Discord, and SEO (if we ever open community to Google). Users can't glance at a URL and know what post it is.

---

## Goal

Community post URLs should look like:

```
/pegasus/community/posts/how-my-google-l5-interview-went
/pegasus/community/posts/is-naukri-or-linkedin-better-for-sde
/pegasus/community/posts/need-referral-at-atlassian-2026
```

If the exact slug is already taken (two users post with the same title), automatically append a short random suffix:

```
/pegasus/community/posts/how-my-google-l5-interview-went-a3f2
```

---

## Scope

### In scope
- Backend: slug generation on post creation, slug column on `community_posts`, lookup by slug or UUID (backward compat)
- Backend: backfill existing rows with UUID-based slugs (safe, idempotent)
- Frontend: switch post card links from `post.id` to `post.slug`
- Frontend: `ApiCommunityPost` type gains `slug: string`

### Out of scope
- Custom slug editing by user (v2 feature)
- Slug-based OG meta tags (v2)
- Redirect from old UUID URLs (not needed — no external links exist yet)

---

## Technical Design

### Database

**Migration `0019_community_slugs.sql`:**

```sql
ALTER TABLE job_tracker.community_posts
  ADD COLUMN IF NOT EXISTS slug TEXT;

-- Backfill existing rows with a safe UUID-derived slug
-- (not human-readable but unique and non-breaking)
UPDATE job_tracker.community_posts
  SET slug = 'post-' || REPLACE(id::text, '-', '')
  WHERE slug IS NULL;

ALTER TABLE job_tracker.community_posts
  ALTER COLUMN slug SET NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS community_posts_slug_idx
  ON job_tracker.community_posts (slug);
```

Why backfill before `NOT NULL`: avoids constraint failure on existing rows. UUID-derived slugs are ugly but stable and unique — no production 404s.

### Slug generation (backend — `internal/jobtracker/`)

New file: `slug.go`

```go
// slugify converts a title to a URL-safe slug:
//   "How my Google L5 interview went!" → "how-my-google-l5-interview-went"
// Rules:
//   - lowercase
//   - spaces and underscores → hyphens
//   - strip all chars that are not [a-z0-9-]
//   - collapse consecutive hyphens
//   - trim leading/trailing hyphens
//   - truncate at 80 chars, back up to last hyphen boundary
//   - empty result → "post"
func slugify(title string) string { ... }

// uniqueSlug generates a slug from title and checks it against existing slugs.
// If taken, appends a 4-char lowercase hex suffix and tries once more.
// Falls back to "post-{8-char-hex}" if title is empty.
// Caller: store_community.go:CreatePost
func (s *Store) uniqueSlug(ctx context.Context, title string) (string, error) {
    base := slugify(title)
    if base == "" {
        base = "post"
    }
    // Try base slug
    var exists bool
    err := s.pool.QueryRow(ctx,
        `SELECT EXISTS(SELECT 1 FROM job_tracker.community_posts WHERE slug = $1)`,
        base,
    ).Scan(&exists)
    if err != nil { return "", err }
    if !exists { return base, nil }
    // Append 4-char hex suffix
    b := make([]byte, 2)
    _, _ = rand.Read(b)
    candidate := fmt.Sprintf("%s-%x", base, b)
    // Truncate base if candidate > 85 chars
    return candidate, nil
}
```

**`store_community.go:CreatePost`** — call `uniqueSlug` and set the `slug` column in the INSERT.

### Lookup by slug or UUID

**`handlers_community.go:GetCommunityPost`** (and `PublicGetCommunityPost`):

```go
idOrSlug := r.PathValue("id")
// Try UUID first (backward compat with any existing bookmarks/extension)
if _, err := uuid.Parse(idOrSlug); err == nil {
    post, err = h.store.GetCommunityPostByID(ctx, uid, parsedUUID)
} else {
    post, err = h.store.GetCommunityPostBySlug(ctx, uid, idOrSlug)
}
```

Add `GetCommunityPostBySlug(ctx, userID, slug)` to `store_community.go`.

### API response

`ApiCommunityPost` in both backend (`types.go`) and frontend (`lib/api-client.ts`) gains:

```go
// Go
Slug string `json:"slug"`
```

```typescript
// TypeScript
slug: string;
```

### Frontend

**`app/community/[section]/page.tsx`** — post card `<Link href>`:

```tsx
// Before
<Link href={withBase(`/community/posts/${post.id}`)}>

// After
<Link href={withBase(`/community/posts/${post.slug}`)}>
```

**`app/community/posts/[id]/page.tsx`** — `[id]` in the filename already accepts any string; no filename rename needed. The fetch call passes whatever is in the URL path — if it's a UUID the backend handles it, if it's a slug it handles it too.

---

## Acceptance Criteria

1. `POST /job-tracker/community/reviews` with title "My Google L5 Experience" → response contains `slug: "my-google-l5-experience"`
2. `GET /job-tracker/community/posts/my-google-l5-experience` returns the same post as `GET /job-tracker/community/posts/{uuid}`
3. Two users post with identical titles → second post gets slug `my-google-l5-experience-{4hex}` (not a duplicate error)
4. Title with only special chars (e.g. "!!!") → slug `post-{4hex}` (no empty slug in DB)
5. Existing posts in DB are backfilled with `post-{32hex}` slugs (UUID-derived) — no 404s
6. Frontend community feed: clicking a post card navigates to `/pegasus/community/posts/{slug}` URL
7. Old UUID-style URL still resolves (backward compat via UUID-first lookup)

---

## Migration order

This migration is `0019_community_slugs.sql`. It must run before Phase 12's sort indexes (which can be `0020_`), since the sort index work may touch the same table.

Wait — re-check: Phase 12 also needs a migration for community sort indexes. If slugs are `0019`, community sort indexes become `0020`.

---

## Effort estimate

- Backend: ~2 hours (migration + slugify fn + store change + lookup dual-path)
- Frontend: ~30 min (type update + 1 link change in community list)
- Total: half a day
