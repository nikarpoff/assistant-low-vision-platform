// Package metrics declares the Prometheus metrics of the gateway (PLT-13).
//
// Every metric lives in the registry of a Metrics value rather than in the
// global default registry, so several gateways can run in one process (tests).
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metric names, as they appear on /metrics.
const (
	StageDurationName  = "alv_stage_duration_seconds"
	E2EDurationName    = "alv_e2e_duration_seconds"
	RequestsName       = "alv_requests_total"
	UpstreamErrorsName = "alv_upstream_errors_total"
	SessionsActiveName = "alv_sessions_active"
	JournalDroppedName = "alv_journal_dropped_total"
)

// Label names.
const (
	labelStage    = "stage"
	labelKind     = "kind"
	labelNetwork  = "network"
	labelStatus   = "status"
	labelUpstream = "upstream"
	labelReason   = "reason"
)

var (
	// stageBuckets cover the gateway stages from a few milliseconds up to the request deadline.
	stageBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.3, 0.5, 0.75, 1, 1.5, 2, 3, 5, 8}
	// e2eBuckets are dense around the 2 s target of NFR-1.
	e2eBuckets = []float64{0.25, 0.5, 0.75, 1, 1.25, 1.5, 1.75, 2, 2.25, 2.5, 3, 4, 5, 8, 15}
)

// Metrics holds every metric of the gateway and the registry they are registered in.
type Metrics struct {
	// StageDuration is alv_stage_duration_seconds{stage, kind}.
	StageDuration *prometheus.HistogramVec
	// E2EDuration is alv_e2e_duration_seconds{kind, network}.
	E2EDuration *prometheus.HistogramVec
	// Requests is alv_requests_total{kind, status}.
	Requests *prometheus.CounterVec
	// UpstreamErrors is alv_upstream_errors_total{upstream, reason}; for upstream="llm" the reasons are those of PLT-15.
	UpstreamErrors *prometheus.CounterVec
	// SessionsActive is alv_sessions_active.
	SessionsActive prometheus.Gauge
	// JournalDropped is alv_journal_dropped_total.
	JournalDropped prometheus.Counter

	registry *prometheus.Registry
}

// NewMetrics creates the metrics of PLT-13 in a new registry.
func NewMetrics() *Metrics {
	m := &Metrics{
		StageDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    StageDurationName,
			Help:    "Duration of a request stage (PLT-14) measured by the gateway.",
			Buckets: stageBuckets,
		}, []string{labelStage, labelKind}),
		E2EDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    E2EDurationName,
			Help:    "Time from the trigger to the first audio reported by the application in client.metrics.",
			Buckets: e2eBuckets,
		}, []string{labelKind, labelNetwork}),
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: RequestsName,
			Help: "Requests by kind and final status.",
		}, []string{labelKind, labelStatus}),
		UpstreamErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: UpstreamErrorsName,
			Help: "Failed calls to the model services and the language model provider by reason.",
		}, []string{labelUpstream, labelReason}),
		SessionsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: SessionsActiveName,
			Help: "WebSocket sessions open now.",
		}),
		JournalDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: JournalDroppedName,
			Help: "Journal records dropped because the buffer was full or the database was unavailable.",
		}),
		registry: prometheus.NewRegistry(),
	}

	// The registry is new and the names are distinct, so the registration cannot fail.
	m.registry.MustRegister(
		m.StageDuration,
		m.E2EDuration,
		m.Requests,
		m.UpstreamErrors,
		m.SessionsActive,
		m.JournalDropped,
	)

	return m
}

// Registry returns the registry with all metrics of the gateway.
func (m *Metrics) Registry() *prometheus.Registry {
	return m.registry
}

// Handler serves the metrics of the registry in the Prometheus text format (GET /metrics).
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
