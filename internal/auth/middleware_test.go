package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TheBharathProject/sypher-api/internal/httpx"
)

// testHeaderUser is the header stubRequireUser reads in place of a real
// bearer token. Keeps the test focused on RequireAdmin's own logic.
const testHeaderUser = "X-Test-User"

// stubRequireUser mimics RequireUser for tests: it reads a user UUID
// from a header and stamps it on the context via WithUserID — the same
// contract production RequireUser fulfils — or writes 401 when absent.
func stubRequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.Header.Get(testHeaderUser)
		if raw == "" {
			httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "invalid token")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithUserID(r.Context(), id)))
	})
}

// TestRequireAdmin exercises the middleware against a real database —
// RequireAdmin reads is_admin via *Store, which is raw pgx by design
// (no interface seam to fake). Set TEST_DATABASE_URL (with migrations
// applied, ≥ 0026) to run it; otherwise it skips.
func TestRequireAdmin(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; RequireAdmin needs a real database")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	store := NewStore(pool)

	newUser := func(label string, admin bool) *User {
		t.Helper()
		suffix := uuid.NewString()
		u, err := store.UpsertUser(ctx,
			"test-"+label+"-"+suffix,
			label+"-"+suffix+"@example.test",
			"Test "+label, "")
		if err != nil {
			t.Fatalf("upsert %s: %v", label, err)
		}
		t.Cleanup(func() {
			if err := store.DeleteUser(ctx, u.ID); err != nil {
				t.Errorf("cleanup %s: %v", label, err)
			}
		})
		if admin {
			if err := store.SetUserFlags(ctx, u.ID, nil, boolPtr(true)); err != nil {
				t.Fatalf("flag %s admin: %v", label, err)
			}
		}
		return u
	}
	admin := newUser("admin", true)
	regular := newUser("regular", false)

	handler := RequireAdmin(stubRequireUser, store)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "ok")
		}))

	tests := []struct {
		name       string
		userHeader string
		wantStatus int
		wantCode   string // error envelope code; "" for success
	}{
		{"admin passes", admin.ID.String(), http.StatusOK, ""},
		{"non-admin forbidden", regular.ID.String(), http.StatusForbidden, "admin_required"},
		{"anonymous unauthorized", "", http.StatusUnauthorized, "unauthorized"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/kairos/admin/snapshot", nil)
			if tc.userHeader != "" {
				req.Header.Set(testHeaderUser, tc.userHeader)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status: got %d want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantCode == "" {
				return
			}
			var body httpx.ErrorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode error body: %v (body %s)", err, rec.Body.String())
			}
			if body.Error != tc.wantCode {
				t.Errorf("error code: got %q want %q", body.Error, tc.wantCode)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }
