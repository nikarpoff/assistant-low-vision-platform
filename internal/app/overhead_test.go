//go:build !race

package app

import (
	"testing"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/pipeline"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/protocol"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/stats"
)

// TestOverheadBudget checks PLT-18 by wall time. The race detector slows the code
// down several times, so the file is excluded from race builds.
func TestOverheadBudget(t *testing.T) {
	const (
		warmup   = 20
		requests = 200
		budgetMS = 15.0
	)

	var (
		g    *gateway
		conn *websocket.Conn
	)

	setup := func() {
		logger.InitTestLogger()
		g = newGateway(t, map[string]string{"APP_LOG_LEVEL": "INFO"})
		conn = g.open(t)
	}

	t.Run("must_pass", func(t *testing.T) {
		setup()
		for i := 0; i < warmup; i++ {
			describe(t, conn, uuid.New())
		}

		totals := make([]float64, 0, requests)
		for i := 0; i < requests; i++ {
			all := describe(t, conn, uuid.New())
			end, ok := all[len(all)-1].message.(*protocol.AnswerEnd)
			require.True(t, ok)
			require.False(t, end.Degraded)
			totals = append(totals, float64(end.Timings[pipeline.StageServerTotal]))
		}
		p50 := stats.Percentile(totals, 0.5)
		p95 := stats.Percentile(totals, 0.95)
		t.Logf("server_total_ms with stubs without delay: requests=%d p50=%.1f p95=%.1f budget_ms=%.0f", requests, p50, p95, budgetMS)

		require.LessOrEqual(t, p95, budgetMS)
	})
}
