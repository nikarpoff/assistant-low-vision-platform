package utils

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHealthcheck(t *testing.T) {
	var (
		status int
		port   int
	)

	setup := func(t *testing.T) {
		status = http.StatusOK
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/health" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(status)
		}))
		t.Cleanup(server.Close)
		port = server.Listener.Addr().(*net.TCPAddr).Port
	}

	t.Run("must_pass", func(t *testing.T) {
		setup(t)

		require.NoError(t, Healthcheck(port))
	})

	t.Run("must_fail_status", func(t *testing.T) {
		setup(t)
		status = http.StatusServiceUnavailable

		err := Healthcheck(port)

		require.EqualError(t, err, fmt.Sprintf(errHealthcheckStatusFormat, http.StatusServiceUnavailable))
	})

	t.Run("must_fail_not_listening", func(t *testing.T) {
		setup(t)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		port = listener.Addr().(*net.TCPAddr).Port
		require.NoError(t, listener.Close())

		err = Healthcheck(port)

		require.ErrorContains(t, err, "healthcheck failed: ")
	})
}
