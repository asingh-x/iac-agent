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

	"github.com/nats-io/nats.go"
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

	// --- Queue driver resolution ---
	// Priority: QUEUE_DRIVER env > config > default (memory)
	// Resolved here, ahead of the "--- Queue ---" section further down, because
	// the stale-task cleanup immediately below needs to know which driver is in
	// play before it decides whether to run at all.
	queueDriver := cfg.Server.QueueDriver
	if v := os.Getenv("QUEUE_DRIVER"); v != "" {
		queueDriver = v
	}

	// Mark any tasks left in running/queued/waiting_for_input state as failed
	// (stale from a prior run) — but only when that's actually true. See
	// shouldMarkStaleTasksFailed's doc comment for why this must be skipped
	// under queue_driver=nats or queue_driver=postgres.
	if shouldMarkStaleTasksFailed(queueDriver) {
		if err := store.MarkStaleTasksFailed(context.Background()); err != nil {
			logger.Error("failed to cleanup stale tasks", "err", err)
		}
	} else {
		logger.Info("skipping stale-task cleanup: queue is durable across restarts, lease/redelivery handles recovery of any task an owning pod died mid-execution", "queue_driver", queueDriver)
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
	// queueDriver was already resolved above, ahead of the stale-task cleanup.

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

	var natsURL string
	var relayConn *nats.Conn // shared core NATS connection for cross-pod SSE/control relay; nil unless queue_driver=nats

	switch queueDriver {
	case "nats":
		natsURL = cfg.Server.NatsURL
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

		nc, err := nats.Connect(natsURL,
			nats.RetryOnFailedConnect(true),
			nats.MaxReconnects(-1),
			nats.ReconnectWait(2*time.Second),
		)
		if err != nil {
			logger.Error("failed to connect to nats for cross-pod relay", "err", err)
			os.Exit(1)
		}
		defer nc.Close()
		relayConn = nc
	case "postgres":
		for _, name := range queueNames {
			dsn := cfg.Server.PostgresQueueDSN
			if dsn == "" {
				dsn = os.Getenv("DB_URL") // falls back to the same DSN the main Store uses
			}
			if dsn == "" {
				// A TOML-only deployment (postgres_url set in config.toml,
				// no DB_URL env var) already resolved pgURL above for the
				// main Store — reuse that instead of failing the queue
				// while the store started up fine.
				dsn = pgURL
			}
			pq, err := queue.NewPostgresQueue(
				dsn, name,
				time.Duration(cfg.Server.PostgresQueueLeaseTTL)*time.Second,
				cfg.Server.PostgresQueueMaxAttempts,
			)
			if err != nil {
				logger.Error("failed to create postgres queue", "queue", name, "err", err)
				os.Exit(1)
			}
			defer pq.Close()
			queues[name] = pq
		}
		logger.Info("queue connected", "driver", "postgres", "queues", strings.Join(queueNames, ", "))
		logger.Warn("queue_driver=postgres's task queue itself is durable and shared safely across replicas, but has no cross-pod control plane relay wired (only queue_driver=nats sets that up): answer/permission/cancel requests AND live SSE streams for a task will fail or hang if load-balanced to a pod that doesn't own it. See docs/configuration.md's postgres queue driver section.",
			"queues", strings.Join(queueNames, ", "))
	default:
		bufSize := cfg.Server.QueueBuffer
		if bufSize <= 0 {
			bufSize = 500
		}
		for _, name := range queueNames {
			queues[name] = queue.NewMemoryQueue(bufSize)
		}
		logger.Warn("queue connected with driver=memory — safe for a single-replica deployment only: SSE streams, answer/permission/cancel requests, and the queue itself are all per-process and will NOT work correctly across multiple pod replicas. Set queue_driver=nats (or QUEUE_DRIVER=nats) for any multi-replica deployment.",
			"queues", strings.Join(queueNames, ", "))
	}

	// Ensure a "default" queue always exists.
	if _, ok := queues["default"]; !ok {
		queues["default"] = queues[queueNames[0]]
	}

	hub := server.NewHub()
	hub.SetEventStore(server.NewDBEventStore(store))
	hub.SetLogger(logger)
	runner := server.NewRunner(hub, store, queues["default"], provider, cfg, logger)

	if relayConn != nil {
		hub.SetRelay(server.NewNATSEventRelay(relayConn))
		// Backstop for a stream reading from the relay: NATS core pub/sub has
		// no delivery guarantee and the owning pod can die mid-task, so a
		// terminal event can simply never arrive. Re-reading the task's own
		// row lets such a stream end correctly instead of heartbeating
		// forever. Only wired when the relay is, since without it the local
		// channel is always authoritative.
		hub.SetStatusCheck(func(taskID string) *server.ServerEvent {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			task, err := store.GetTask(ctx, taskID)
			if err != nil || task == nil {
				// A transient DB error must not kill a live stream; the next
				// tick tries again.
				return nil
			}
			return server.InitialSnapshotEvent(task)
		})
		controlRelay, err := server.NewNATSControlRelay(relayConn, runner)
		if err != nil {
			logger.Error("failed to start cross-pod control relay", "err", err)
			os.Exit(1)
		}
		runner.SetControlRelay(controlRelay)
		logger.Info("cross-pod relay enabled", "transport", "nats")
	}

	// Start one worker goroutine per named queue.
	// All goroutines share the same runner (shared semaphore, answer channels, etc.).
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	for _, name := range queueNames {
		q := queues[name]
		go runner.StartQueue(ctx, q)
	}

	// Age-based reconciliation backstop, run unconditionally under every
	// queue_driver (unlike the startup MarkStaleTasksFailed sweep above, this
	// is safe to run regardless — see FailTasksOlderThan's doc comment in
	// internal/db/store.go for why an age predicate avoids the cross-pod
	// false-failure problem the old unconditional sweep had). It exists to
	// recover a task whose queue_driver=nats delivery is abandoned and
	// redelivered until it exhausts MaxDeliver (internal/queue/nats.go),
	// which otherwise has no automatic path back to a terminal DB status and
	// would sit as "running" forever.
	go runStaleTaskReconciler(ctx, store, cfg, logger)

	// Periodic cleanup for Runner.userSems (per-user LLM concurrency
	// semaphores), which otherwise only grows: a user who submits a task
	// once and never again would keep an entry forever. Same shutdown ctx /
	// goroutine-per-ticker pattern as runStaleTaskReconciler above.
	go runner.StartUserSemaphoreSweeper(ctx)

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

// shouldMarkStaleTasksFailed reports whether the process should run its
// startup stale-task cleanup (store.MarkStaleTasksFailed) for the given
// resolved queue driver value.
//
// It must be false for "nats" and "postgres": both queues are durable across
// process restarts, and each driver's own redelivery/lease-reclaim mechanism
// (NATS's AckWait/MaxDeliver in internal/queue/nats.go; Postgres's
// lease_ttl-based reclaim on the next Pop in internal/queue/postgres.go)
// plus the isTerminalStatus idempotency check in
// internal/server/task_runner.go already recover a task whose owning pod
// died mid-execution. Running the cleanup unconditionally there would mark
// OTHER pods' genuinely in-flight tasks as failed on any single pod's
// restart (a rolling restart or HPA scale-up, not just a crash) — and since
// isTerminalStatus treats "failed" as terminal, the queue's own later
// redelivery of that same task would then be silently skipped as
// "already done", losing the real work for good instead of retrying it.
//
// It is true for every other value, including "memory" and anything
// unrecognized (which the "--- Queue ---" section below also treats as
// memory): a restart there genuinely loses the in-memory queue's contents
// too, so any row still marked running/queued/waiting_for_input really is
// orphaned and safe — indeed necessary — to mark failed.
func shouldMarkStaleTasksFailed(queueDriver string) bool {
	return queueDriver != "nats" && queueDriver != "postgres"
}

// staleTaskReconcileInterval controls how often runStaleTaskReconciler calls
// store.FailTasksOlderThan. 15 minutes catches a genuinely stuck task
// reasonably promptly relative to the (much longer) age threshold itself,
// without hammering the database with an UPDATE scan more often than that.
//
// A var, not a const, purely so tests can shorten it — same reasoning as
// server.controlRelayTimeout; nothing mutates it at runtime.
var staleTaskReconcileInterval = 15 * time.Minute

// staleTaskReconcileErrMsg is recorded on a task's error_msg column when
// runStaleTaskReconciler reconciles it.
const staleTaskReconcileErrMsg = "Task abandoned: exceeded the stale-task reconciliation age threshold with no terminal outcome"

// runStaleTaskReconciler periodically marks failed any task that has sat
// non-terminal (running/queued/waiting_for_input) for longer than
// cfg.Server.StaleTaskMaxAge. Unlike the startup MarkStaleTasksFailed sweep
// (see shouldMarkStaleTasksFailed above), this age-based check is safe to run
// unconditionally under any queue_driver: it only ever touches rows that have
// been non-terminal for an unusually long time, not every non-terminal row
// right now, so it can never fail another pod's task that is simply,
// currently, legitimately still running. It exists to close the one gap that
// approach leaves open under queue_driver=nats — a task whose queue delivery
// is abandoned and redelivered until it exhausts the queue's max-delivery
// attempts (internal/queue/nats.go's MaxDeliver) has no other automatic path
// back to a terminal DB status and would otherwise sit as "running" forever.
//
// Runs until ctx is cancelled — call in a goroutine, tied to the same
// top-level ctx that governs the queue pop loops started in main(), so it
// stops cleanly alongside them on shutdown.
func runStaleTaskReconciler(ctx context.Context, store db.Store, cfg *config.Config, logger *slog.Logger) {
	maxAge := time.Duration(cfg.Server.StaleTaskMaxAge) * time.Second
	if maxAge <= 0 {
		maxAge = 2 * time.Hour
	}

	ticker := time.NewTicker(staleTaskReconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := store.FailTasksOlderThan(ctx, maxAge, staleTaskReconcileErrMsg)
			if err != nil {
				logger.Error("stale-task reconciliation failed", "err", err)
				continue
			}
			if n > 0 {
				logger.Warn("stale-task reconciliation marked tasks failed", "count", n, "max_age", maxAge)
			} else {
				logger.Info("stale-task reconciliation ran, nothing to reconcile", "max_age", maxAge)
			}
		}
	}
}
