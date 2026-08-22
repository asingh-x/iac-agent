package agent

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Tool-level metrics, exposed at /metrics (internal/server/handler.go wires
// promhttp.Handler onto the shared, process-wide Prometheus registry — these
// register there too, without internal/agent needing to import
// internal/server, which would introduce an import cycle since
// internal/server already imports internal/agent).
var (
	metricToolDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "tfagent_tool_duration_seconds",
		Help:    "Tool execution duration in seconds, labelled by tool name.",
		Buckets: prometheus.DefBuckets,
	}, []string{"tool"})

	metricToolCalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tfagent_tool_calls_total",
		Help: "Total number of tool executions, labelled by tool name and result (success|error).",
	}, []string{"tool", "result"})
)
