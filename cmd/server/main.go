package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tf-agent/tf-agent/internal/config"
	"github.com/tf-agent/tf-agent/internal/db"
	"github.com/tf-agent/tf-agent/internal/llm"
	"github.com/tf-agent/tf-agent/internal/queue"
	"github.com/tf-agent/tf-agent/internal/server"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		// cfg may be nil here; initialize defaults before logging
		cfg = config.Defaults()
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err != nil {
		logger.Warn("config load failed, using defaults", "err", err)
	}

	// --- Database (PostgreSQL only) ---
	pgURL := cfg.Server.PostgresURL
	if v := os.Getenv("DB_URL"); v != "" {
		pgURL = v
	}
	if pgURL == "" {
		logger.Error("DB_URL is required", "hint", "set DB_URL environment variable or postgres_url in config")
		os.Exit(1)
	}
	store, err := db.NewPostgres(pgURL)
	if err != nil {
		logger.Error("failed to connect to postgres", "err", err)
		os.Exit(1)
	}
	logger.Info("database connected", "driver", "postgres")
	defer store.Close()

	// Load (or generate) the AES-256 token encryption key.
	if err := server.LoadEncryptionKey(); err != nil {
		logger.Error("failed to load encryption key", "err", err)
		os.Exit(1)
	}

	// Mark any tasks left in running/queued/waiting state as failed (stale from prior run).
	if err := store.MarkStaleTasksFailed(context.Background()); err != nil {
		logger.Error("failed to cleanup stale tasks", "err", err)
	}

	// Bootstrap admin user from env var on first run.
	if adminToken := os.Getenv("TF_AGENT_ADMIN_TOKEN"); adminToken != "" {
		if err := store.EnsureAdmin(context.Background(), "admin", server.HashToken(adminToken)); err != nil {
			logger.Error("failed to bootstrap admin user", "err", err)
		} else {
			logger.Info("admin user ensured", "source", "TF_AGENT_ADMIN_TOKEN")
		}
	}

	provider, err := llm.NewProvider(cfg)
	if err != nil {
		logger.Error("failed to initialize llm provider", "err", err)
		os.Exit(1)
	}

	// --- Queue ---
	// Priority: QUEUE_DRIVER env > config > default (memory)
	queueDriver := cfg.Server.QueueDriver
	if v := os.Getenv("QUEUE_DRIVER"); v != "" {
		queueDriver = v
	}

	// QUEUE_NAMES: comma-separated list of named queues to process.
	// Each name gets its own worker goroutine. Defaults to "default".
	queueNamesRaw := os.Getenv("QUEUE_NAMES")
	if queueNamesRaw == "" {
		queueNamesRaw = "default"
	}
	var queueNames []string
	for _, n := range strings.Split(queueNamesRaw, ",") {
		n = strings.TrimSpace(n)
		if n != "" {
			queueNames = append(queueNames, n)
		}
	}
	if len(queueNames) == 0 {
		queueNames = []string{"default"}
	}

	queues := make(map[string]queue.Queue, len(queueNames))

	switch queueDriver {
	case "nats":
		natsURL := cfg.Server.NatsURL
		if v := os.Getenv("NATS_URL"); v != "" {
			natsURL = v
		}
		if natsURL == "" {
			natsURL = "nats://127.0.0.1:4222"
		}
		natsMaxMsgs := cfg.Server.NATSMaxMsgs
		if natsMaxMsgs <= 0 {
			natsMaxMsgs = queue.DefaultNATSMaxMsgs
		}
		for _, name := range queueNames {
			nq, err := queue.NewNATSQueue(natsURL, name, natsMaxMsgs)
			if err != nil {
				logger.Error("failed to connect to nats queue", "name", name, "err", err)
				os.Exit(1)
			}
			defer nq.Close()
			queues[name] = nq
		}
		logger.Info("queue connected", "driver", "nats", "url", natsURL, "queues", strings.Join(queueNames, ", "))
	default:
		bufSize := cfg.Server.QueueBuffer
		if bufSize <= 0 {
			bufSize = 500
		}
		for _, name := range queueNames {
			queues[name] = queue.NewMemoryQueue(bufSize)
		}
		logger.Info("queue connected", "driver", "memory", "queues", strings.Join(queueNames, ", "))
	}

	// Ensure a "default" queue always exists.
	if _, ok := queues["default"]; !ok {
		queues["default"] = queues[queueNames[0]]
	}

	hub := server.NewHub()
	runner := server.NewRunner(hub, store, queues["default"], provider, cfg, logger)

	// Start one worker goroutine per named queue.
	// All goroutines share the same runner (shared semaphore, answer channels, etc.).
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	for _, name := range queueNames {
		q := queues[name]
		go runner.StartQueue(ctx, q)
	}

	srv := server.NewServer(store, hub, queues, runner, cfg, webFS)

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	httpServer := &http.Server{
		Addr:         addr,
		Handler:      srv.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // SSE streams are long-lived
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		logger.Info("shutdown signal received, draining in-flight tasks")

		shutCtx, shutCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutCancel()
		_ = httpServer.Shutdown(shutCtx)

		gracePeriod := time.Duration(cfg.Server.ShutdownGracePeriod) * time.Second
		if gracePeriod <= 0 {
			gracePeriod = 60 * time.Second
		}
		drainCtx, drainCancel := context.WithTimeout(context.Background(), gracePeriod)
		defer drainCancel()
		runner.Shutdown(drainCtx)
		logger.Info("shutdown drain complete")
	}()

	logger.Info("tf-agent-server listening", "addr", addr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server exited with error", "err", err)
		os.Exit(1)
	}
}
