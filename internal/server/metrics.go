package server

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	metricTasksSubmitted = promauto.NewCounter(prometheus.CounterOpts{
		Name: "tfagent_tasks_submitted_total",
		Help: "Total number of tasks submitted to the queue.",
	})

	metricTasksCompleted = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tfagent_tasks_completed_total",
		Help: "Total number of tasks completed, labelled by status (done|failed).",
	}, []string{"status"})

	metricTaskDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "tfagent_task_duration_seconds",
		Help:    "End-to-end task execution time in seconds.",
		Buckets: prometheus.DefBuckets,
	})

	metricLLMInputTokens = promauto.NewCounter(prometheus.CounterOpts{
		Name: "tfagent_llm_input_tokens_total",
		Help: "Total LLM input tokens consumed across all tasks.",
	})

	metricLLMOutputTokens = promauto.NewCounter(prometheus.CounterOpts{
		Name: "tfagent_llm_output_tokens_total",
		Help: "Total LLM output tokens generated across all tasks.",
	})

	metricLLMCacheReadTokens = promauto.NewCounter(prometheus.CounterOpts{
		Name: "tfagent_llm_cache_read_tokens_total",
		Help: "Total LLM prompt-cache tokens read (cache hits) across all tasks.",
	})

	metricLLMCacheCreatedTokens = promauto.NewCounter(prometheus.CounterOpts{
		Name: "tfagent_llm_cache_created_tokens_total",
		Help: "Total LLM prompt-cache tokens created (cache misses/writes) across all tasks.",
	})

	metricActiveSSEConns = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "tfagent_active_sse_connections",
		Help: "Number of currently open SSE streaming connections.",
	})

	metricQueueDepth = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tfagent_queue_depth",
		Help: "Current number of pending items in a named queue.",
	}, []string{"queue"})

	metricLLMConcurrencyUsed = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "tfagent_llm_concurrency_used",
		Help: "Current number of LLM concurrency semaphore slots in use.",
	})

	metricLLMConcurrencyCapacity = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "tfagent_llm_concurrency_capacity",
		Help: "Total configured LLM concurrency semaphore capacity.",
	})
)
