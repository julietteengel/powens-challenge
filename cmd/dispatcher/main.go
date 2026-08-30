package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver used by sql.Open

	"powens-challenge/internal/config"
	"powens-challenge/internal/httpapi"
	"powens-challenge/internal/httpclient"
	"powens-challenge/internal/postgres"
	"powens-challenge/internal/worker"
)

func main() {
	cfg := config.Load()
	if cfg.DatabaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	if cfg.HMACSecret == "" {
		log.Fatal("HMAC_SECRET is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := openDB(ctx, cfg)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer func() { _ = db.Close() }()

	store := postgres.NewStore(db)
	deliverer := httpclient.NewDeliverer(cfg.DeliveryTimeout, []byte(cfg.HMACSecret))
	wg := startWorkers(ctx, cfg, store, deliverer)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpapi.NewMux(db),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	serverErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-serverErr:
		log.Printf("listen: %v", err)
	}
	stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGracePeriod)
	defer cancel()

	var shutdownWG sync.WaitGroup
	shutdownWG.Go(func() {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("http server shutdown: %v", err)
		}
	})
	shutdownWG.Go(wg.Wait)

	done := make(chan struct{})
	go func() { shutdownWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-shutdownCtx.Done():
	}
}

func openDB(ctx context.Context, cfg config.Config) (*sql.DB, error) {
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(cfg.Concurrency + 2)
	return db, nil
}

func startWorkers(ctx context.Context, cfg config.Config, store worker.Store, deliverer worker.Deliverer) *sync.WaitGroup {
	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Go(func() {
			worker.Run(ctx, store, deliverer, cfg.MaxAttempts, cfg.DeliveryTimeout)
		})
	}
	return &wg
}
