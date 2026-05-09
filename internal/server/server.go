package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TheBharathProject/sypher-api/internal/ai"
	"github.com/TheBharathProject/sypher-api/internal/auth"
	"github.com/TheBharathProject/sypher-api/internal/billing"
	"github.com/TheBharathProject/sypher-api/internal/config"
	"github.com/TheBharathProject/sypher-api/internal/cron"
	"github.com/TheBharathProject/sypher-api/internal/cron/jobs"
	"github.com/TheBharathProject/sypher-api/internal/health"
	"github.com/TheBharathProject/sypher-api/internal/httpx"
	"github.com/TheBharathProject/sypher-api/internal/jobtracker"
	"github.com/TheBharathProject/sypher-api/internal/mailer"
	"github.com/TheBharathProject/sypher-api/internal/storage"
	"github.com/TheBharathProject/sypher-api/internal/waitlist"
)

// Server bundles the things we need to serve HTTP. It's a plain struct;
// dependency injection in Go is just "pass things explicitly," not a
// framework feature.
type Server struct {
	cfg    *config.Config
	pool   *pgxpool.Pool
	logger *slog.Logger
	httpd  *http.Server
	// cron is set during routes() so Start() can spawn it under the
	// graceful-shutdown context. nil if no jobs are registered (kept
	// nullable so future single-tenant builds can opt out).
	cron *cron.Runner
}

func New(cfg *config.Config, pool *pgxpool.Pool, logger *slog.Logger) *Server {
	s := &Server{cfg: cfg, pool: pool, logger: logger}
	s.httpd = &http.Server{
		Addr:              cfg.HTTPListenAddr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return s
}

// routes is where every endpoint gets registered. Adding a new tool's API
// is a single block here — keeps the surface visible at a glance.
//
// The mux is stdlib net/http (since Go 1.22 it supports method-and-pattern
// matching like "POST /waitlist", which is all we need at this scale).
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Meta — public, no auth
	mux.HandleFunc("GET /", s.handleRoot)
	mux.Handle("GET /health", health.NewHandler(s.pool))

	// Waitlist — auth gated by X-API-Key
	apiKey := requireAPIKey(s.cfg.WaitlistAPIKey)
	wlStore := waitlist.NewStore(s.pool)
	wlHandler := waitlist.NewHandler(wlStore, s.cfg.IPSalt, s.logger)
	mux.Handle("POST /waitlist", apiKey(wlHandler))

	// Auth — Google OAuth + JWT issuance. Public OAuth start/callback,
	// logout is also public (stateless JWT).
	authStore := auth.NewStore(s.pool)
	authHandler := auth.NewHandler(s.cfg, authStore, s.logger)

	// Job-tracker tool — every endpoint behind RequireUser, except the
	// public profile route which the tool registers itself.
	jtStore := jobtracker.NewStore(s.pool)
	// authStore is passed so RequireUser can resolve `pg_` browser-extension
	// tokens via auth.api_tokens in addition to the default JWT path.
	requireUser := auth.RequireUser(s.cfg.JWTSecret, s.cfg.JWTIssuer, s.cfg.JWTAudience, authStore)
	jtHandler := jobtracker.NewHandler(s.cfg, jtStore, authStore, s.logger)

	// Have the OAuth callback eagerly provision a job-tracker profile +
	// auto-pick a slug for new users. Visibility stays FALSE by default
	// (schema), so the public route returns 404 until the user opts in.
	authHandler.WithProvisioner(jtStore)

	mux.HandleFunc("GET /auth/google", authHandler.GoogleStart)
	mux.HandleFunc("GET /auth/google/callback", authHandler.GoogleCallback)
	mux.HandleFunc("POST /auth/logout", authHandler.Logout)

	// Phase 2: best-effort init of R2 + Deepseek. If creds aren't set, the
	// related endpoints respond 503 instead of refusing to boot.
	if r2, err := storage.New(context.Background(), storage.Settings{
		AccountID:       s.cfg.R2AccountID,
		AccessKeyID:     s.cfg.R2AccessKeyID,
		SecretAccessKey: s.cfg.R2SecretAccessKey,
		Bucket:          s.cfg.R2Bucket,
	}); err == nil {
		jtHandler.WithStorage(r2)
		s.logger.Info("r2 storage configured", "bucket", s.cfg.R2Bucket)
	} else {
		s.logger.Info("r2 storage skipped", "reason", err.Error())
	}
	if aiClient, err := ai.New(ai.Settings{
		APIKey:  s.cfg.DeepseekAPIKey,
		BaseURL: s.cfg.DeepseekBaseURL,
		Model:   s.cfg.DeepseekModel,
	}); err == nil {
		jtHandler.WithAI(aiClient, ai.NewUsageStore(s.pool, s.cfg.AIUsageMonthlyTokenLimit))
		s.logger.Info("deepseek configured", "model", s.cfg.DeepseekModel)
	} else {
		s.logger.Info("deepseek skipped", "reason", err.Error())
	}

	// Phase 2: mailer + notifier + cron. Mailer falls back to slog if
	// RESEND_API_KEY is empty so prod still ships in-app notifications
	// without DKIM verified yet. See ADR-001 D2/D6.
	mailerClient := mailer.New(s.cfg, s.logger)
	notifier := jobtracker.NewNotifier(jtStore, mailerClient, s.logger)
	jtHandler.WithNotifier(notifier)

	// Billing — Razorpay-backed subscriptions, one-time premium passes,
	// and credit packs. Client is nil when RAZORPAY_KEY_ID/SECRET are
	// blank; in that case the handler responds 503 to checkout calls
	// (mirrors how R2 / Deepseek degrade), but the rest of the app
	// still boots. Webhook is wired regardless — even without keys we
	// can echo back signature-rejection 401s without crashing.
	billingStore := billing.NewStore(s.pool)
	billingClient := billing.NewClient(s.cfg.RazorpayKeyID, s.cfg.RazorpayKeySecret)
	billingHandler := billing.NewHandler(billingStore, billingClient, s.cfg.RazorpayPlanID, s.cfg.RazorpayPlanIDPlus, s.logger)
	billingWebhook := billing.NewWebhookHandler(billingStore, s.cfg.RazorpayWebhookSecret, s.logger)
	if billingClient != nil {
		s.logger.Info("razorpay configured", "plan_id", s.cfg.RazorpayPlanID)
	} else {
		s.logger.Info("razorpay skipped", "reason", "RAZORPAY_KEY_ID or _SECRET unset")
	}

	urls := jobs.NewURLBuilder(s.cfg)
	s.cron = cron.New(s.logger,
		cron.Job{
			Name:     "stale-apps",
			NextFire: jobs.AtIST(3, 0),
			Run:      jobs.MarkStaleApplications(jtStore, notifier, urls, s.logger),
		},
		cron.Job{
			Name:     "daily-digest",
			NextFire: jobs.AtIST(9, 0),
			Run:      jobs.DailyApplicationDigest(jtStore, notifier, mailerClient, urls, s.logger),
		},
		cron.Job{
			Name:     "expire-one-time-premium",
			NextFire: jobs.AtIST(4, 0),
			Run:      jobs.ExpireOneTimePremium(billingStore, s.logger),
		},
	)

	jobtracker.RegisterRoutes(mux, jtHandler, requireUser)

	// Billing routes — auth-gated (premium is per-user) except the
	// webhook which is signed with HMAC.
	mux.Handle("POST /billing/checkout/subscription", requireUser(http.HandlerFunc(billingHandler.CheckoutSubscription)))
	mux.Handle("POST /billing/checkout/subscription-plus", requireUser(http.HandlerFunc(billingHandler.CheckoutSubscriptionPlus)))
	mux.Handle("POST /billing/checkout/premium-pass", requireUser(http.HandlerFunc(billingHandler.CheckoutPremiumPass)))
	mux.Handle("POST /billing/checkout/credits", requireUser(http.HandlerFunc(billingHandler.CheckoutCredits)))
	mux.Handle("POST /billing/subscriptions/{id}/cancel", requireUser(http.HandlerFunc(billingHandler.CancelSubscription)))
	mux.Handle("GET /billing/me", requireUser(http.HandlerFunc(billingHandler.GetMe)))
	mux.Handle("POST /webhooks/razorpay", billingWebhook) // public, signature-verified

	// Compose middleware. Outer wrappers run first.
	var h http.Handler = mux
	h = withCORS(h, s.cfg.CORSOrigins)
	h = withLogging(h, s.logger)
	h = withRecover(h, s.logger)
	return h
}

func (s *Server) handleRoot(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"service": "sypher-api",
		"version": "0.2.0",
		"status":  "ok",
	})
}

// Start blocks until the server stops, returning the cause.
// Cancellation of ctx triggers a graceful shutdown.
func (s *Server) Start(ctx context.Context) error {
	errCh := make(chan error, 1)

	// Spawn cron jobs under the same lifecycle as the http server. They
	// receive the parent ctx so SIGTERM cancels their loops; Wait() below
	// blocks the shutdown path until all in-flight Run() calls return.
	if s.cron != nil {
		s.cron.Start(ctx)
	}

	go func() {
		s.logger.Info("http server starting", "addr", s.cfg.HTTPListenAddr, "env", s.cfg.Env)
		if err := s.httpd.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		s.logger.Info("shutdown signal received, draining")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
		defer cancel()
		if err := s.httpd.Shutdown(shutdownCtx); err != nil {
			return err
		}
		// Block until every cron loop has exited. Loops park on
		// time.After most of the time, so this returns immediately under
		// normal load. If a job is mid-run, we wait it out.
		if s.cron != nil {
			s.cron.Wait()
		}
		return nil
	case err := <-errCh:
		return err
	}
}
