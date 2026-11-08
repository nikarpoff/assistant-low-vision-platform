package httpserver

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
)

func TestNewServer(t *testing.T) {
	var (
		vars     *env.Vars
		server   *Server
		response *httptest.ResponseRecorder
		getLogs  func() string
		sessions http.Handler
		upgrades int
	)

	setup := func() {
		getLogs = logger.InitTestLogger()
		upgrades = 0
		sessions = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			upgrades++
			w.WriteHeader(http.StatusSwitchingProtocols)
		})
		vars = &env.Vars{AppServerPort: 80, AppServerReadTimeout: 15, AppServerWriteTimeout: 15, AppServerIdleTimeout: 60, AppServerShutdownTimeout: 5}
		server = NewServer(vars, sessions)
		response = httptest.NewRecorder()
	}

	t.Run("must_register_health_route", func(t *testing.T) {
		setup()

		server.server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))

		require.Equal(t, http.StatusOK, response.Code)
		require.Equal(t, "OK", response.Body.String())
	})

	t.Run("must_register_session_route", func(t *testing.T) {
		setup()

		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/session", nil))

		require.Equal(t, http.StatusSwitchingProtocols, response.Code)
		require.Equal(t, 1, upgrades)
	})

	t.Run("must_fail_session_wrong_method", func(t *testing.T) {
		setup()

		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/session", nil))

		require.Equal(t, http.StatusMethodNotAllowed, response.Code)
		require.Zero(t, upgrades)
	})

	t.Run("must_pass_server_settings", func(t *testing.T) {
		setup()

		require.Equal(t, ":80", server.server.Addr)
		require.Equal(t, "15s", server.server.ReadTimeout.String())
		require.Equal(t, "15s", server.server.WriteTimeout.String())
		require.Equal(t, "1m0s", server.server.IdleTimeout.String())
	})

	t.Run("must_fail_wrong_method", func(t *testing.T) {
		setup()

		server.server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/health", nil))

		require.Equal(t, http.StatusMethodNotAllowed, response.Code)
	})

	t.Run("must_fail_unknown_route", func(t *testing.T) {
		setup()

		server.server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/unknown", nil))

		require.Equal(t, http.StatusNotFound, response.Code)
	})

	t.Run("must_pass_run_and_stop", func(t *testing.T) {
		setup()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		vars.AppServerPort = listener.Addr().(*net.TCPAddr).Port
		require.NoError(t, listener.Close())
		server = NewServer(vars, sessions)
		done := make(chan error, 1)

		go func() {
			done <- server.Run()
		}()
		var healthResponse *http.Response
		require.Eventually(t, func() bool {
			healthResponse, err = http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", vars.AppServerPort))
			return err == nil
		}, 5*time.Second, 10*time.Millisecond)
		require.NoError(t, healthResponse.Body.Close())
		server.Stop()

		require.Equal(t, http.StatusOK, healthResponse.StatusCode)
		require.NoError(t, <-done)
		require.Contains(t, getLogs(), fmt.Sprintf("INFO: Server listening on port %d\n", vars.AppServerPort))
		require.Contains(t, getLogs(), "INFO: stopping HTTP server...\n")
		require.NotContains(t, getLogs(), "ERROR:")
	})

	t.Run("must_fail_port_in_use", func(t *testing.T) {
		setup()
		listener, err := net.Listen("tcp", ":0")
		require.NoError(t, err)
		defer listener.Close()
		vars.AppServerPort = listener.Addr().(*net.TCPAddr).Port
		server = NewServer(vars, sessions)

		err = server.Run()

		require.ErrorContains(t, err, "address already in use")
		require.Contains(t, getLogs(), "ERROR: failed to serve HTTP: ")
	})
}
