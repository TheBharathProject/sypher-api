package jobtracker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

// =============================================================================
// Surface validation
// =============================================================================

var validSurfaces = map[string]bool{
	"reviews":     true,
	"experiences": true,
	"referrals":   true,
	"ask":         true,
	"recruiters":  true,
}

func surfaceFromPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	s := r.PathValue("surface")
	if !validSurfaces[s] {
		httpx.WriteError(w, http.StatusNotFound, "unknown_surface", "no such community surface")
		return "", false
	}
	return s, true
}

// =============================================================================
// Rate-limit knobs (ADR-003 D9). Bumpable later via env if needed; v1
// numbers are deliberately conservative. 24-hour rolling windows.
// =============================================================================

const (
	maxPostsPerSurfacePerDay = 5
	maxCommentsPerDay        = 50
	maxVotesPerDay           = 100
)

// rateLimit returns true if the user is OVER the limit. Writes a 429
// with Retry-After (seconds) when so. Caller should `return` after a
// true response.
func (h *Handler) rateLimit(w http.ResponseWriter, r *http.Request, kind string, count, limit int) bool {
	if count < limit {
		return false
	}
	w.Header().Set("Retry-After", strconv.Itoa(int((24 * time.Hour).Seconds())))
	httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited",
		fmt.Sprintf("you've hit the daily %s limit (%d). try again tomorrow.", kind, limit))
	return true
}

// =============================================================================
// Posts
// =============================================================================

// ListCommunity GET /job-tracker/community/{surface}
func (h *Handler) ListCommunity(w http.ResponseWriter, r *http.Request) {
	surface, ok := surfaceFromPath(w, r)
	if !ok {
		return
	}
	uid := auth.MustUserID(r.Context())
	opts := parseListOpts(r)
	opts.ViewerID = &uid
	posts, err := h.store.ListPosts(r.Context(), surface, opts)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeListResponse(w, posts, opts.Limit)
}

// PublicListCommunity GET /job-tracker/public/community/{surface}
func (h *Handler) PublicListCommunity(w http.ResponseWriter, r *http.Request) {
	surface, ok := surfaceFromPath(w, r)
	if !ok {
		return
	}
	opts := parseListOpts(r)
	opts.PublicOnly = true
	posts, err := h.store.ListPosts(r.Context(), surface, opts)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeListResponse(w, posts, opts.Limit)
}

func parseListOpts(r *http.Request) CommunityListOpts {
	opts := CommunityListOpts{}
	if c := r.URL.Query().Get("cursor"); c != "" {
		if t, err := time.Parse(time.RFC3339, c); err == nil {
			opts.Cursor = t
		}
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			opts.Limit = n
		}
	}
	return opts
}

func writeListResponse(w http.ResponseWriter, posts []CommunityPost, limit int) {
	if limit <= 0 {
		limit = 20
	}
	var nextCursor any
	if len(posts) == limit {
		nextCursor = posts[len(posts)-1].CreatedAt.Format(time.RFC3339)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items":      posts,
		"nextCursor": nextCursor,
	})
}

// CreateCommunityPost POST /job-tracker/community/{surface}
func (h *Handler) CreateCommunityPost(w http.ResponseWriter, r *http.Request) {
	surface, ok := surfaceFromPath(w, r)
	if !ok {
		return
	}
	uid := auth.MustUserID(r.Context())

	// Rate limit (D9).
	since := time.Now().Add(-24 * time.Hour)
	count, err := h.store.RecentPostCount(r.Context(), uid, surface, since)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if h.rateLimit(w, r, "post", count, maxPostsPerSurfacePerDay) {
		return
	}

	var in CommunityPostInput
	if !readJSON(w, r, &in) {
		return
	}
	if in.Title == "" || len(in.Title) > 280 {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "title must be 1-280 chars")
		return
	}
	if len(in.Body) > 16384 {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "body too long (max 16k chars)")
		return
	}
	in.Surface = surface

	post, err := h.store.CreatePost(r.Context(), uid, in)
	if err != nil {
		writeDBError(w, err)
		return
	}

	// Synchronous mention-trigger (ADR-003 D6). Errors don't bubble —
	// a flaky notifier shouldn't fail an otherwise-good post create.
	h.fireMentionTriggers(r.Context(), post.ID, uid, in.Body)

	httpx.WriteJSON(w, http.StatusCreated, post)
}

// GetCommunityPost GET /job-tracker/community/posts/{id}
// Accepts either a UUID or a slug in the {id} path parameter.
func (h *Handler) GetCommunityPost(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	idOrSlug := r.PathValue("id")

	var (
		post *CommunityPost
		err  error
	)
	if parsed, parseErr := uuid.Parse(idOrSlug); parseErr == nil {
		post, err = h.store.GetPost(r.Context(), parsed, &uid)
	} else {
		post, err = h.store.GetPostBySlug(r.Context(), idOrSlug, &uid)
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "post not found")
			return
		}
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, post)
}

// PublicGetCommunityPost GET /job-tracker/public/community/posts/{id}
// Accepts either a UUID or a slug in the {id} path parameter.
func (h *Handler) PublicGetCommunityPost(w http.ResponseWriter, r *http.Request) {
	idOrSlug := r.PathValue("id")

	var (
		post *CommunityPost
		err  error
	)
	if parsed, parseErr := uuid.Parse(idOrSlug); parseErr == nil {
		post, err = h.store.GetPost(r.Context(), parsed, nil)
	} else {
		post, err = h.store.GetPostBySlug(r.Context(), idOrSlug, nil)
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "post not found")
			return
		}
		writeDBError(w, err)
		return
	}
	if !post.IsPublic || post.Status != "active" {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "post not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, post)
}

// UpdateCommunityPost PATCH /job-tracker/community/posts/{id}
func (h *Handler) UpdateCommunityPost(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in CommunityPostInput
	if !readJSON(w, r, &in) {
		return
	}
	if in.Title == "" || len(in.Title) > 280 {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "title must be 1-280 chars")
		return
	}
	post, err := h.store.UpdatePost(r.Context(), uid, id, in)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "post not found or not owned")
			return
		}
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, post)
}

// DeleteCommunityPost DELETE /job-tracker/community/posts/{id} (soft-delete)
func (h *Handler) DeleteCommunityPost(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if err := h.store.SoftDeletePost(r.Context(), uid, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "post not found or not owned")
			return
		}
		writeDBError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// FlagCommunityPost POST /job-tracker/community/posts/{id}/flag
func (h *Handler) FlagCommunityPost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if err := h.store.FlagPost(r.Context(), id); err != nil {
		writeDBError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// =============================================================================
// Votes
// =============================================================================

type voteInput struct {
	Value int `json:"value"` // -1, 0, or 1
}

// VoteOnCommunityPost POST /job-tracker/community/posts/{id}/vote
func (h *Handler) VoteOnCommunityPost(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	postID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in voteInput
	if !readJSON(w, r, &in) {
		return
	}
	if in.Value < -1 || in.Value > 1 {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "value must be -1, 0, or 1")
		return
	}

	// Rate limit (D9).
	since := time.Now().Add(-24 * time.Hour)
	count, err := h.store.RecentVoteCount(r.Context(), uid, since)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if h.rateLimit(w, r, "vote", count, maxVotesPerDay) {
		return
	}

	ownerID, err := h.store.SetVote(r.Context(), uid, postID, in.Value)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "post not found")
			return
		}
		writeDBError(w, err)
		return
	}

	// Notification: only on upvotes, only when the voter isn't the owner.
	// PushIdempotent + the partial unique index on community_vote (ADR-003
	// D5) make this safe under double-fire.
	if in.Value == 1 && ownerID != uid && h.notifier != nil {
		refID := postID
		_, _ = h.notifier.PushIdempotent(r.Context(), NotificationInput{
			UserID:   ownerID,
			Kind:     "community_vote",
			RefType:  strPtrLocal("community_post"),
			RefID:    &refID,
			Title:    "Someone upvoted your post",
			Body:     "Open the post to see who's been reading.",
			LinkPath: strPtrLocal("/community/posts/" + postID.String()),
		}, time.Now().UTC())
	}

	w.WriteHeader(http.StatusNoContent)
}

// =============================================================================
// Comments
// =============================================================================

// ListCommunityComments GET /job-tracker/community/posts/{id}/comments
func (h *Handler) ListCommunityComments(w http.ResponseWriter, r *http.Request) {
	postID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	comments, err := h.store.ListComments(r.Context(), postID)
	if err != nil {
		writeDBError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": comments})
}

// CreateCommunityComment POST /job-tracker/community/posts/{id}/comments
func (h *Handler) CreateCommunityComment(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	postID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}

	// Rate limit (D9).
	since := time.Now().Add(-24 * time.Hour)
	count, err := h.store.RecentCommentCount(r.Context(), uid, since)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if h.rateLimit(w, r, "comment", count, maxCommentsPerDay) {
		return
	}

	var in CommunityCommentInput
	if !readJSON(w, r, &in) {
		return
	}
	if in.Body == "" || len(in.Body) > 8192 {
		httpx.WriteError(w, http.StatusBadRequest, "bad_input", "body must be 1-8192 chars")
		return
	}

	// Confirm post exists + we have its owner for the reply notification.
	post, err := h.store.GetPost(r.Context(), postID, nil)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "post not found")
			return
		}
		writeDBError(w, err)
		return
	}

	comment, err := h.store.CreateComment(r.Context(), uid, postID, in)
	if err != nil {
		writeDBError(w, err)
		return
	}

	// Synchronous reply triggers (ADR-003 D3, D4). Errors are logged
	// but don't fail the comment.
	h.fireReplyTriggers(r.Context(), post, comment, uid)
	h.fireMentionTriggers(r.Context(), postID, uid, in.Body)

	httpx.WriteJSON(w, http.StatusCreated, comment)
}

// DeleteCommunityComment DELETE /job-tracker/community/comments/{id}
func (h *Handler) DeleteCommunityComment(w http.ResponseWriter, r *http.Request) {
	uid := auth.MustUserID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if err := h.store.SoftDeleteComment(r.Context(), uid, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "comment not found or not owned")
			return
		}
		writeDBError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// =============================================================================
// Notification trigger helpers
// =============================================================================

// fireReplyTriggers pushes community_reply to:
//   - the post author
//   - every distinct prior commenter on the post (D4)
//
// Excluding the new commenter themselves. Push errors are logged and
// swallowed so a flaky notifier doesn't fail the comment.
func (h *Handler) fireReplyTriggers(ctx context.Context, post *CommunityPost, comment *CommunityComment, commenterID uuid.UUID) {
	if h.notifier == nil {
		return
	}
	recipients, err := h.store.NotifiableCommentParticipants(ctx, post.ID, commenterID)
	if err != nil {
		h.logger.Error("community: load participants", "err", err)
		recipients = nil
	}
	// Add the post author if they aren't already in the participant
	// list and they aren't the new commenter.
	if post.UserID != commenterID && !containsUUID(recipients, post.UserID) {
		recipients = append(recipients, post.UserID)
	}

	preview := comment.Body
	if len(preview) > 140 {
		preview = preview[:140] + "…"
	}
	link := "/community/posts/" + post.ID.String() + "#comment-" + comment.ID.String()

	for _, uid := range recipients {
		refID := post.ID
		_, err := h.notifier.Push(ctx, NotificationInput{
			UserID:   uid,
			Kind:     "community_reply",
			RefType:  strPtrLocal("community_post"),
			RefID:    &refID,
			Title:    fmt.Sprintf("New reply: %s", post.Title),
			Body:     preview,
			LinkPath: &link,
		})
		if err != nil {
			h.logger.Error("community: push reply", "err", err, "user_id", uid)
		}
	}
}

// fireMentionTriggers parses the body for @slug and pushes
// community_mention notifications. Idempotent per user/post/day via the
// dedup index on kind='community_mention'.
func (h *Handler) fireMentionTriggers(ctx context.Context, postID, authorID uuid.UUID, body string) {
	if h.notifier == nil || body == "" {
		return
	}
	slugs := ExtractMentions(body)
	if len(slugs) == 0 {
		return
	}
	dayKey := time.Now().UTC()
	link := "/community/posts/" + postID.String()

	for _, slug := range slugs {
		uid, err := h.store.UserIDBySlug(ctx, slug)
		if err != nil {
			// pgx.ErrNoRows is normal — slug isn't a real user. Skip.
			continue
		}
		if uid == authorID {
			continue // don't notify yourself
		}
		refID := postID
		_, err = h.notifier.PushIdempotent(ctx, NotificationInput{
			UserID:   uid,
			Kind:     "community_mention",
			RefType:  strPtrLocal("community_post"),
			RefID:    &refID,
			Title:    "You were mentioned",
			Body:     "",
			LinkPath: &link,
		}, dayKey)
		if err != nil {
			h.logger.Error("community: push mention", "err", err, "user_id", uid, "slug", slug)
		}
	}
}

func containsUUID(haystack []uuid.UUID, needle uuid.UUID) bool {
	for _, u := range haystack {
		if u == needle {
			return true
		}
	}
	return false
}

// strPtrLocal is the file-scoped *string helper. Lives here (vs the
// shared one in cron/jobs/applications.go::strPtr) because Go forbids
// importing a non-exported helper across packages and adding it here
// keeps handlers_community.go self-contained.
func strPtrLocal(s string) *string { return &s }
