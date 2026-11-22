package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wojtek/dance-scheduler/internal/server"
	"github.com/wojtek/dance-scheduler/internal/store"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	var (
		port          = flag.String("port", envOr("PORT", "8095"), "HTTP listen port")
		dbPath        = flag.String("db", envOr("DB_PATH", "./dance.db"), "SQLite database path")
		baseURL       = flag.String("base-url", envOr("APP_BASE_URL", "http://localhost:8095"), "Public base URL for links")
		transportMode = flag.String("transport", envOr("TRANSPORT_MODE", "manual"), "Transport mode: manual, whatsapp, telegram")
		webhookURL    = flag.String("webhook-url", envOr("BOT_WEBHOOK_URL", ""), "Optional bot bridge webhook URL")
		adminPhone    = flag.String("admin-phone", envOr("ADMIN_BOOTSTRAP_PHONE", ""), "Bootstrap admin phone number")
		adminName     = flag.String("admin-name", envOr("ADMIN_BOOTSTRAP_NAME", "Admin"), "Bootstrap admin name")
		resolveOnce   = flag.Bool("resolve-once", false, "Run due resolutions + recruitment sweep once and exit")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(*dbPath, *baseURL, *transportMode, *webhookURL)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	now := time.Now().UTC()
	if err := st.EnsureUpcomingThursdays(ctx, now); err != nil {
		log.Fatalf("ensure upcoming Thursdays: %v", err)
	}
	if *adminPhone != "" && *adminName != "" {
		if _, err := st.CreateTeacher(ctx, *adminName, *adminPhone, "", true, now.Year()); err != nil {
			log.Printf("warn: bootstrap admin: %v", err)
		}
	}

	if *resolveOnce {
		res, err := st.RunResolver(ctx, now, 0)
		if err != nil {
			log.Fatalf("resolver error: %v", err)
		}
		rec, esc, err := st.RunRecruitment(ctx, now)
		if err != nil {
			log.Fatalf("recruitment error: %v", err)
		}
		log.Printf("sweep complete: resolved=%d recruited=%d escalated=%d", res, rec, esc)
		return
	}

	// Background ticker runs the deadline resolver and recruitment sweep every 15 minutes.
	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-ticker.C:
				_ = st.EnsureUpcomingThursdays(ctx, t.UTC())
				_, _ = st.RunResolver(ctx, t.UTC(), 0)
				_, _, _ = st.RunRecruitment(ctx, t.UTC())
			}
		}
	}()

	srv := &http.Server{
		Addr:              ":" + *port,
		Handler:           server.New(st),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	log.Printf("Thursday Latin Dancing Scheduler listening on http://localhost:%s (transport=%s, db=%s)", *port, *transportMode, *dbPath)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
}
