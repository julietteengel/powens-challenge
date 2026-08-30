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

	_ "github.com/jackc/pgx/v5/stdlib"

	"powens-challenge/internal/config"
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

	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	db.SetMaxOpenConns(cfg.Concurrency + 2)

	store := postgres.NewStore(db)
	deliverer := httpclient.NewDeliverer(cfg.DeliveryTimeout, []byte(cfg.HMACSecret))

	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Go(func() {
			worker.Run(ctx, store, deliverer, cfg.MaxAttempts, cfg.DeliveryTimeout)
		})
	}

	srv := &http.Server{Addr: cfg.Addr, Handler: http.NewServeMux()}
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
