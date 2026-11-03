package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"listingboard/internal/worker"
)

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required")
	}
	dataDir := env("DATA_DIR", "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatal(err)
	}

	stepPause := 2 * time.Second
	if pauseStr := os.Getenv("STEP_PAUSE"); pauseStr != "" {
		var err error
		stepPause, err = time.ParseDuration(pauseStr)
		if err != nil {
			log.Fatalf("invalid STEP_PAUSE: %v", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := worker.WaitStore(ctx, dsn, 30*time.Second, 500*time.Millisecond)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	w := worker.New(worker.Config{
		Store:     st,
		DataDir:   dataDir,
		StepPause: stepPause,
	})

	log.Printf("worker started: dataDir=%s stepPause=%s", dataDir, stepPause)
	if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
	log.Println("worker stopped cleanly")
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
