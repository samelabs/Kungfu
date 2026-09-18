package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"kungfu.md/internal/config"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/server"
	"kungfu.md/internal/version"
)

// main is the process lifecycle owner: fail-closed startup, then the
// single ServeLifecycle shutdown orchestration on real OS signals.
func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Printf("[kungfu.md] Starting Kungfu %s", version.Get())

	// Startup step 1: config load — required config missing/malformed
	// fails closed here, before any resource is opened.
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("startup failed (config): %v", err)
	}
	log.Printf("[kungfu.md] Config loaded: listen=%s db=%s@%s:%d/%s",
		cfg.ListenAddr, cfg.DBUser, cfg.DBHost, cfg.DBPort, cfg.DBName)

	// Startup step 2: DB open + connectivity verification — an
	// invalid DSN or unreachable DB fails closed BEFORE serving.
	pool, err := pg.NewPool(cfg.DatabaseURL())
	if err != nil {
		log.Fatalf("startup failed (database): %v", err)
	}
	log.Println("[kungfu.md] Database connected")

	// Startup step 3: application + router construction.
	srv := server.New(cfg, pool)

	// Background loop (rate limiter GC) with an explicit stop signal —
	// ServeLifecycle stops it during shutdown; nothing leaks. R3.2:
	// a GC panic is caught by the runner's panic boundary, reported
	// on backgroundErrors, and routed through ServeLifecycle's single
	// shutdown path instead of crashing the process.
	gcStop := make(chan struct{})
	backgroundErrors := make(chan error, 1)
	go runRateLimiterGC(gcStop, backgroundErrors, 5*time.Minute, srv.RateLimiter.GC)

	httpServer := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      srv,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Shutdown orchestration: SIGINT/SIGTERM feed the SAME path.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	signals := make(chan shutdownTrigger, 2)
	go func() {
		for range sigCh {
			select {
			case signals <- shutdownTrigger{}:
			default: // orchestration already triggered
			}
		}
	}()

	err = ServeLifecycle(context.Background(), &lifecycle{
		httpServer:       httpServer,
		shutdownBudget:   10 * time.Second,
		signals:          signals,
		backgroundStops:  []chan struct{}{gcStop},
		closers:          []io.Closer{poolCloser{pool}},
		backgroundErrors: backgroundErrors,
	})
	if err != nil {
		log.Fatalf("runtime error: %v", err)
	}
	log.Println("[kungfu.md] Process exited cleanly")
}

// poolCloser adapts pg.Pool's parameterless Close to io.Closer so the
// lifecycle closes the DB exactly once, after HTTP shutdown.
type poolCloser struct{ pool *pg.Pool }

func (p poolCloser) Close() error { p.pool.Close(); return nil }
