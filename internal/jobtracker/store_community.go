package jobtracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// =============================================================================
// Types
// =============================================================================

// CommunityPost is the wire shape for a single post on any surface. Per-
// surface fields live in Metadata; handlers validate the JSON shape per
// surface before write.
type CommunityPost struct {
	ID           uuid.UUID       `json:"id"`
	UserID       uuid.UUID       `json:"userId"`
	AuthorName   string          `json:"authorName"`
	AuthorSlug   string          `json:"authorSlug,omitempty"`
	Slug         string          `json:"slug"`
	Surface      string          `json:"surface"`
	Title        string          `json:"title"`
	Body         string          `json:"body,omitempty"`
	Metadata     json.RawMessage `json:"metadata"`
	IsPublic     bool            `json:"isPublic"`
	VoteCount    int             `json:"voteCount"`
	CommentCount int             `json:"commentCount"`
	Status       string          `json:"status"`
	// MyVote is the caller's vote on this post (-1, 0, or 1). 0 means
	// either "no vote" or "anonymous reader". Set to 0 on public read.
	MyVote    int       `json:"myVote"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type CommunityPostInput struct {
	Surface  string          `json:"-"` // set by the handler from path
	Title    string          `json:"title"`
	Body     string          `json:"body"`
	Metadata json.RawMessage `json:"metadata"`
	IsPublic bool            `json:"isPublic"`
}

// CommunityUpdateInput is the PATCH body shape. Surface is optional —
// when present alongside Metadata, the handler runs per-surface validation.
// Title-only edits (no Surface + no Metadata) skip validation entirely to
// avoid 400-rejecting existing posts that predate enum alignment.
type CommunityUpdateInput struct {
	Surface  string          `json:"surface,omitempty"`
	Title    string          `json:"title"`
	Body     string          `json:"body"`
	Metadata json.RawMessage `json:"metadata"`
	IsPublic bool            `json:"isPublic"`
}

type CommunityComment struct {
	ID         uuid.UUID  `json:"id"`
	PostID     uuid.UUID  `json:"postId"`
	UserID     uuid.UUID  `json:"userId"`
	AuthorName string     `json:"authorName"`
	AuthorSlug string     `json:"authorSlug,omitempty"`
	ParentID   *uuid.UUID `json:"parentId,omitempty"`
	Body       string     `json:"body"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"createdAt"`
}

type CommunityCommentInput struct {
	Body     string     `json:"body"`
	ParentID *uuid.UUID `json:"parentId,omitempty"`
}

// CommunityListOpts narrows the list query. Empty values fall back to
// safe defaults; cursor is the createdAt of the last row from the
// previous page.
type CommunityListOpts struct {
	Cursor      time.Time
	Limit       int
	PublicOnly  bool       // /public/community/* sets this true
	ViewerID    *uuid.UUID // for MyVote computation; nil on public read
	Sort        string     // "newest" | "votes" | "most-reviewed" — empty defaults to "newest"
}

// =============================================================================
// Posts
// =============================================================================

const communityPostCols = `p.id, p.user_id, COALESCE(u.name, ''), COALESCE(pf.slug, ''),
	p.slug, p.surface, p.title, COALESCE(p.body, ''),
	p.metadata, p.is_public, p.vote_count, p.comment_count, p.status,
	p.created_at, p.updated_at`

// scanCommunityPost handles both pgx.Row (single-row) and pgx.Rows (loop).
// MyVote is left at 0; callers that need it backfill via FillMyVotes below.
func scanCommunityPost(row pgxRowScanner, p *CommunityPost) error {
	return row.Scan(
		&p.ID, &p.UserID, &p.AuthorName, &p.AuthorSlug,
		&p.Slug, &p.Surface, &p.Title, &p.Body,
		&p.Metadata, &p.IsPublic, &p.VoteCount, &p.CommentCount, &p.Status,
		&p.CreatedAt, &p.UpdatedAt,
	)
}

// CreatePost inserts and returns the freshly-created post. The author's
// name + slug are joined back so the response shape matches list/get.
func (s *Store) CreatePost(ctx context.Context, userID uuid.UUID, in CommunityPostInput) (*CommunityPost, error) {
	if in.Metadata == nil {
		in.Metadata = json.RawMessage("{}")
	}
	slug, err := s.uniqueSlug(ctx, in.Title)
	if err != nil {
		return nil, fmt.Errorf("generate slug: %w", err)
	}
	const q = `
		WITH inserted AS (
			INSERT INTO job_tracker.community_posts
				(user_id, surface, title, body, metadata, is_public, slug)
			VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7)
			RETURNING *
		)
		SELECT ` + communityPostCols + `
		FROM inserted p
		LEFT JOIN auth.users u ON u.id = p.user_id
		LEFT JOIN job_tracker.profiles pf ON pf.user_id = p.user_id
	`
	var post CommunityPost
	if err := scanCommunityPost(s.pool.QueryRow(ctx, q, userID, in.Surface, in.Title, in.Body, in.Metadata, in.IsPublic, slug), &post); err != nil {
		return nil, err
	}
	return &post, nil
}

// ListPosts returns one page of posts on a surface, newest first.
// PublicOnly=true filters to is_public=true (used by the public route).
// ViewerID, when non-nil, fills MyVote on each post in a single roundtrip.
func (s *Store) ListPosts(ctx context.Context, surface string, opts CommunityListOpts) ([]CommunityPost, error) {
	limit := opts.Limit
	switch {
	case limit <= 0:
		limit = 20
	case limit > 50:
		limit = 50
	}

	args := []any{surface, limit}
	q := `
		SELECT ` + communityPostCols + `
		FROM job_tracker.community_posts p
		LEFT JOIN auth.users u ON u.id = p.user_id
		LEFT JOIN job_tracker.profiles pf ON pf.user_id = p.user_id
		WHERE p.surface = $1
		  AND p.status = 'active'
	`
	if opts.PublicOnly {
		q += " AND p.is_public = true"
	}
	if !opts.Cursor.IsZero() {
		args = append(args, opts.Cursor)
		q += fmt.Sprintf(" AND p.created_at < $%d", len(args))
	}

	// allowedSort maps the ?sort= query param to a safe ORDER BY clause.
	// Values come from a hard-coded Go map, never from raw user input, so
	// string interpolation here is safe. Unknown or absent sort values fall
	// back to "newest".
	// NOTE (v1 trade-off): cursor pagination always uses created_at regardless
	// of sort mode. Sort tabs should reset to page 1 on sort change since a
	// created_at cursor is meaningless for votes/comments ordering.
	allowedSort := map[string]string{
		"newest":        "p.created_at DESC, p.id DESC",
		"votes":         "p.vote_count DESC, p.created_at DESC",
		"most-reviewed": "p.comment_count DESC, p.created_at DESC",
	}
	clause, ok := allowedSort[opts.Sort]
	if !ok {
		clause = allowedSort["newest"]
	}
	q += " ORDER BY " + clause + " LIMIT $2"

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]CommunityPost, 0, limit)
	for rows.Next() {
		var p CommunityPost
		if err := scanCommunityPost(rows, &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if opts.ViewerID != nil && len(out) > 0 {
		if err := s.fillMyVotes(ctx, *opts.ViewerID, out); err != nil {
			// Non-fatal: log and continue with MyVote=0 across the board.
			// Caller may set its own logger if it cares.
			_ = err
		}
	}
	return out, nil
}

// GetPost fetches a single post + author info. ViewerID, when non-nil,
// fills MyVote.
func (s *Store) GetPost(ctx context.Context, postID uuid.UUID, viewerID *uuid.UUID) (*CommunityPost, error) {
	const q = `
		SELECT ` + communityPostCols + `
		FROM job_tracker.community_posts p
		LEFT JOIN auth.users u ON u.id = p.user_id
		LEFT JOIN job_tracker.profiles pf ON pf.user_id = p.user_id
		WHERE p.id = $1
	`
	var p CommunityPost
	if err := scanCommunityPost(s.pool.QueryRow(ctx, q, postID), &p); err != nil {
		return nil, err
	}
	if viewerID != nil {
		_ = s.fillMyVotes(ctx, *viewerID, []CommunityPost{p})
		// scanCommunityPost wrote to a copy; re-fetch MyVote separately.
		const vq = `SELECT value FROM job_tracker.community_votes WHERE post_id = $1 AND user_id = $2`
		var v int16
		err := s.pool.QueryRow(ctx, vq, postID, *viewerID).Scan(&v)
		if err == nil {
			p.MyVote = int(v)
		}
	}
	return &p, nil
}

// GetPostBySlug fetches a single post by its URL slug + author info.
// Mirrors GetPost but filters on p.slug = $1 instead of p.id = $1.
// ViewerID, when non-nil, fills MyVote.
func (s *Store) GetPostBySlug(ctx context.Context, slug string, viewerID *uuid.UUID) (*CommunityPost, error) {
	const q = `
		SELECT ` + communityPostCols + `
		FROM job_tracker.community_posts p
		LEFT JOIN auth.users u ON u.id = p.user_id
		LEFT JOIN job_tracker.profiles pf ON pf.user_id = p.user_id
		WHERE p.slug = $1
	`
	var p CommunityPost
	if err := scanCommunityPost(s.pool.QueryRow(ctx, q, slug), &p); err != nil {
		return nil, err
	}
	if viewerID != nil {
		const vq = `SELECT value FROM job_tracker.community_votes WHERE post_id = $1 AND user_id = $2`
		var v int16
		err := s.pool.QueryRow(ctx, vq, p.ID, *viewerID).Scan(&v)
		if err == nil {
			p.MyVote = int(v)
		}
	}
	return &p, nil
}

// fillMyVotes runs a single `WHERE post_id = ANY(ids)` query to backfill
// MyVote on every post in the list. O(1) DB round-trips regardless of
// how many posts are in the page.
func (s *Store) fillMyVotes(ctx context.Context, viewerID uuid.UUID, posts []CommunityPost) error {
	if len(posts) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(posts))
	for i, p := range posts {
		ids[i] = p.ID
	}
	const q = `SELECT post_id, value FROM job_tracker.community_votes WHERE user_id = $1 AND post_id = ANY($2)`
	rows, err := s.pool.Query(ctx, q, viewerID, ids)
	if err != nil {
		return err
	}
	defer rows.Close()

	votes := map[uuid.UUID]int{}
	for rows.Next() {
		var pid uuid.UUID
		var v int16
		if err := rows.Scan(&pid, &v); err != nil {
			return err
		}
		votes[pid] = int(v)
	}
	for i := range posts {
		if v, ok := votes[posts[i].ID]; ok {
			posts[i].MyVote = v
		}
	}
	return nil
}

// UpdatePost lets the author edit title/body/metadata/isPublic. Surface
// is immutable post-create. Returns pgx.ErrNoRows if not found OR not
// owned by userID — handler maps to 404.
func (s *Store) UpdatePost(ctx context.Context, userID, postID uuid.UUID, in CommunityPostInput) (*CommunityPost, error) {
	if in.Metadata == nil {
		in.Metadata = json.RawMessage("{}")
	}
	const q = `
		WITH updated AS (
			UPDATE job_tracker.community_posts
			SET title = $1,
			    body = NULLIF($2, ''),
			    metadata = $3,
			    is_public = $4,
			    updated_at = NOW()
			WHERE id = $5 AND user_id = $6 AND status = 'active'
			RETURNING *
		)
		SELECT ` + communityPostCols + `
		FROM updated p
		LEFT JOIN auth.users u ON u.id = p.user_id
		LEFT JOIN job_tracker.profiles pf ON pf.user_id = p.user_id
	`
	var p CommunityPost
	if err := scanCommunityPost(s.pool.QueryRow(ctx, q, in.Title, in.Body, in.Metadata, in.IsPublic, postID, userID), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// SoftDeletePost flips status to 'removed'. Indexes filter on status='active'
// so the row falls out of every list/detail query immediately. Original
// rows stay for moderation history.
func (s *Store) SoftDeletePost(ctx context.Context, userID, postID uuid.UUID) error {
	const q = `
		UPDATE job_tracker.community_posts
		SET status = 'removed', updated_at = NOW()
		WHERE id = $1 AND user_id = $2 AND status = 'active'
	`
	tag, err := s.pool.Exec(ctx, q, postID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// FlagPost moves a post to 'flagged' for admin review. No author check —
// any authed user can flag a post. The Phase 3.5+ admin queue triages.
func (s *Store) FlagPost(ctx context.Context, postID uuid.UUID) error {
	const q = `UPDATE job_tracker.community_posts SET status = 'flagged' WHERE id = $1 AND status = 'active'`
	_, err := s.pool.Exec(ctx, q, postID)
	return err
}

// =============================================================================
// Votes
// =============================================================================

// SetVote upserts a vote (value ±1) or clears it (value 0). Maintains
// vote_count atomically via a CTE. Returns the post's owner so the
// caller can decide whether to push a community_vote notification.
//
// Behaviour:
//   - value=1, no prior row → insert, vote_count += 1
//   - value=-1, no prior row → insert, vote_count -= 1
//   - value=1, prior was -1 → update, vote_count += 2
//   - value=0 → delete prior row (if any), vote_count -= prior_value
func (s *Store) SetVote(ctx context.Context, voterID, postID uuid.UUID, value int) (postOwnerID uuid.UUID, err error) {
	if value < -1 || value > 1 {
		return uuid.Nil, fmt.Errorf("vote value out of range: %d", value)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Read current state.
	var ownerID uuid.UUID
	const ownerQ = `SELECT user_id FROM job_tracker.community_posts WHERE id = $1 AND status = 'active'`
	if err := tx.QueryRow(ctx, ownerQ, postID).Scan(&ownerID); err != nil {
		return uuid.Nil, err
	}
	var prior int16
	const priorQ = `SELECT value FROM job_tracker.community_votes WHERE post_id = $1 AND user_id = $2`
	priorErr := tx.QueryRow(ctx, priorQ, postID, voterID).Scan(&prior)
	hasPrior := priorErr == nil
	if priorErr != nil && !errors.Is(priorErr, pgx.ErrNoRows) {
		return uuid.Nil, priorErr
	}

	switch {
	case value == 0 && hasPrior:
		if _, err := tx.Exec(ctx, `DELETE FROM job_tracker.community_votes WHERE post_id = $1 AND user_id = $2`, postID, voterID); err != nil {
			return uuid.Nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE job_tracker.community_posts SET vote_count = vote_count - $1 WHERE id = $2`, prior, postID); err != nil {
			return uuid.Nil, err
		}
	case value == 0 && !hasPrior:
		// no-op — clearing nothing.
	case hasPrior && int16(value) == prior:
		// idempotent re-vote, do nothing.
	case hasPrior:
		// flipping ±. Net delta is value - prior (e.g. -1 → +1 = +2).
		if _, err := tx.Exec(ctx, `UPDATE job_tracker.community_votes SET value = $1, created_at = NOW() WHERE post_id = $2 AND user_id = $3`, value, postID, voterID); err != nil {
			return uuid.Nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE job_tracker.community_posts SET vote_count = vote_count + $1 WHERE id = $2`, value-int(prior), postID); err != nil {
			return uuid.Nil, err
		}
	default:
		// First vote.
		if _, err := tx.Exec(ctx, `INSERT INTO job_tracker.community_votes (post_id, user_id, value) VALUES ($1, $2, $3)`, postID, voterID, value); err != nil {
			return uuid.Nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE job_tracker.community_posts SET vote_count = vote_count + $1 WHERE id = $2`, value, postID); err != nil {
			return uuid.Nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return ownerID, nil
}

// =============================================================================
// Comments
// =============================================================================

const communityCommentCols = `c.id, c.post_id, c.user_id, COALESCE(u.name, ''), COALESCE(pf.slug, ''),
	c.parent_id, c.body, c.status, c.created_at`

func scanCommunityComment(row pgxRowScanner, c *CommunityComment) error {
	return row.Scan(
		&c.ID, &c.PostID, &c.UserID, &c.AuthorName, &c.AuthorSlug,
		&c.ParentID, &c.Body, &c.Status, &c.CreatedAt,
	)
}

// ListComments returns every comment on a post in chronological order.
// Includes soft-deleted rows so the UI can render a "[deleted]" placeholder
// — dropping them entirely would orphan their replies. Caller filters in
// the UI layer.
func (s *Store) ListComments(ctx context.Context, postID uuid.UUID) ([]CommunityComment, error) {
	const q = `
		SELECT ` + communityCommentCols + `
		FROM job_tracker.community_comments c
		LEFT JOIN auth.users u ON u.id = c.user_id
		LEFT JOIN job_tracker.profiles pf ON pf.user_id = c.user_id
		WHERE c.post_id = $1
		ORDER BY c.created_at ASC
	`
	rows, err := s.pool.Query(ctx, q, postID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CommunityComment{}
	for rows.Next() {
		var c CommunityComment
		if err := scanCommunityComment(rows, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateComment inserts a comment + atomically bumps the post's
// comment_count. Wraps in a tx so list queries never see a row before
// the count is updated.
func (s *Store) CreateComment(ctx context.Context, userID, postID uuid.UUID, in CommunityCommentInput) (*CommunityComment, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const ins = `
		INSERT INTO job_tracker.community_comments (post_id, user_id, parent_id, body)
		VALUES ($1, $2, $3, $4)
		RETURNING id, post_id, user_id, parent_id, body, status, created_at
	`
	var c CommunityComment
	if err := tx.QueryRow(ctx, ins, postID, userID, in.ParentID, in.Body).Scan(
		&c.ID, &c.PostID, &c.UserID, &c.ParentID, &c.Body, &c.Status, &c.CreatedAt,
	); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE job_tracker.community_posts SET comment_count = comment_count + 1 WHERE id = $1`, postID); err != nil {
		return nil, err
	}

	// Backfill author name + slug. Cheap second query inside the tx.
	const meta = `
		SELECT COALESCE(u.name, ''), COALESCE(pf.slug, '')
		FROM auth.users u
		LEFT JOIN job_tracker.profiles pf ON pf.user_id = u.id
		WHERE u.id = $1
	`
	if err := tx.QueryRow(ctx, meta, userID).Scan(&c.AuthorName, &c.AuthorSlug); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &c, nil
}

// SoftDeleteComment flips status to 'removed' for the author. Same shape
// as SoftDeletePost; preserves thread structure.
func (s *Store) SoftDeleteComment(ctx context.Context, userID, commentID uuid.UUID) error {
	const q = `
		UPDATE job_tracker.community_comments
		SET status = 'removed'
		WHERE id = $1 AND user_id = $2 AND status = 'active'
	`
	tag, err := s.pool.Exec(ctx, q, commentID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// =============================================================================
// Notification helpers
// =============================================================================

// NotifiableCommentParticipants returns DISTINCT user_id from active
// comments on a post, MINUS excludeUserID (the new commenter — don't
// notify them about their own reply). Caller adds the post author
// separately.
func (s *Store) NotifiableCommentParticipants(ctx context.Context, postID, excludeUserID uuid.UUID) ([]uuid.UUID, error) {
	const q = `
		SELECT DISTINCT user_id
		FROM job_tracker.community_comments
		WHERE post_id = $1 AND user_id != $2 AND status = 'active'
	`
	rows, err := s.pool.Query(ctx, q, postID, excludeUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []uuid.UUID{}
	for rows.Next() {
		var u uuid.UUID
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UserIDBySlug resolves a profile slug to its user_id. Returns
// pgx.ErrNoRows if no such slug exists. Used by the mention parser.
func (s *Store) UserIDBySlug(ctx context.Context, slug string) (uuid.UUID, error) {
	const q = `SELECT user_id FROM job_tracker.profiles WHERE slug = $1`
	var id uuid.UUID
	if err := s.pool.QueryRow(ctx, q, slug).Scan(&id); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// =============================================================================
// Rate-limit counters (D9)
// =============================================================================

// RecentPostCount returns how many posts this user has authored in the
// given surface within the lookback window. The community handlers use
// this to enforce per-day caps before allowing a write.
func (s *Store) RecentPostCount(ctx context.Context, userID uuid.UUID, surface string, since time.Time) (int, error) {
	const q = `
		SELECT COUNT(*) FROM job_tracker.community_posts
		WHERE user_id = $1 AND surface = $2 AND created_at >= $3
	`
	var n int
	err := s.pool.QueryRow(ctx, q, userID, surface, since).Scan(&n)
	return n, err
}

func (s *Store) RecentCommentCount(ctx context.Context, userID uuid.UUID, since time.Time) (int, error) {
	const q = `SELECT COUNT(*) FROM job_tracker.community_comments WHERE user_id = $1 AND created_at >= $2`
	var n int
	err := s.pool.QueryRow(ctx, q, userID, since).Scan(&n)
	return n, err
}

func (s *Store) RecentVoteCount(ctx context.Context, userID uuid.UUID, since time.Time) (int, error) {
	const q = `SELECT COUNT(*) FROM job_tracker.community_votes WHERE user_id = $1 AND created_at >= $2`
	var n int
	err := s.pool.QueryRow(ctx, q, userID, since).Scan(&n)
	return n, err
}
