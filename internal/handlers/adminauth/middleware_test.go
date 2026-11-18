package adminauth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
)

func TestMiddleware(t *testing.T) {
	var (
		vars     *env.Vars
		calls    int
		handler  http.Handler
		request  *http.Request
		response *httptest.ResponseRecorder
		getLogs  func() string
	)

	setup := func(token string, disabled bool) {
		getLogs = logger.InitTestLogger()
		vars = &env.Vars{AppAdminToken: token, AppAuthDisabled: disabled}
		calls = 0
		handler = NewMiddleware(vars).Handle(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.WriteHeader(http.StatusOK)
		}))
		request = httptest.NewRequest(http.MethodGet, "/v1/stats?kind=describe", nil)
		response = httptest.NewRecorder()
	}

	requireUnauthorized := func(t *testing.T) {
		require.Equal(t, http.StatusUnauthorized, response.Code)
		require.JSONEq(t, `{"error": "unauthorized"}`, response.Body.String())
		require.Equal(t, "application/json", response.Header().Get("Content-Type"))
		require.Equal(t, "Bearer", response.Header().Get("WWW-Authenticate"))
		require.Equal(t, 0, calls)
		require.Contains(t, getLogs(), "WARNING: unauthorized admin request: method=GET path=/v1/stats")
	}

	t.Run("must_pass", func(t *testing.T) {
		setup("admin-token", false)
		request.Header.Set("Authorization", "Bearer admin-token")

		handler.ServeHTTP(response, request)

		require.Equal(t, http.StatusOK, response.Code)
		require.Equal(t, 1, calls)
	})

	t.Run("must_pass_scheme_in_any_case", func(t *testing.T) {
		setup("admin-token", false)
		request.Header.Set("Authorization", "bearer admin-token")

		handler.ServeHTTP(response, request)

		require.Equal(t, http.StatusOK, response.Code)
		require.Equal(t, 1, calls)
	})

	t.Run("must_pass_auth_disabled", func(t *testing.T) {
		setup("", true)

		handler.ServeHTTP(response, request)

		require.Equal(t, http.StatusOK, response.Code)
		require.Equal(t, 1, calls)
	})

	t.Run("must_fail_unauthorized", func(t *testing.T) {
		setup("admin-token", false)

		handler.ServeHTTP(response, request)

		requireUnauthorized(t)
	})

	t.Run("must_fail_wrong_token", func(t *testing.T) {
		setup("admin-token", false)
		request.Header.Set("Authorization", "Bearer admin-token-2")

		handler.ServeHTTP(response, request)

		requireUnauthorized(t)
		require.NotContains(t, getLogs(), "admin-token")
	})

	t.Run("must_fail_wrong_scheme", func(t *testing.T) {
		setup("admin-token", false)
		request.Header.Set("Authorization", "Basic admin-token")

		handler.ServeHTTP(response, request)

		requireUnauthorized(t)
	})

	t.Run("must_fail_empty_admin_token", func(t *testing.T) {
		setup("", false)
		request.Header.Set("Authorization", "Bearer ")

		handler.ServeHTTP(response, request)

		requireUnauthorized(t)
	})
}
