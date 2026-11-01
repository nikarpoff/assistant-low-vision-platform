// Package adminauth guards the administrative routes of the gateway (/v1/stats, /v1/config)
// with the admin token APP_ADMIN_TOKEN (PLT-13, NFR-7).
package adminauth

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/env"
	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
)

const (
	headerAuthorization   = "Authorization"
	headerWWWAuthenticate = "WWW-Authenticate"
	schemeBearer          = "Bearer"
	msgUnauthorized       = "unauthorized"
)

// errorResponse is the body of the 401 answer.
type errorResponse struct {
	Error string `json:"error"`
}

// Middleware lets a request through only with "Authorization: Bearer <APP_ADMIN_TOKEN>".
// The token is compared in constant time. With APP_AUTH_DISABLED=true every request goes through.
type Middleware struct {
	token    []byte
	disabled bool
}

// NewMiddleware creates a new Middleware with the admin token of the configuration.
func NewMiddleware(vars *env.Vars) *Middleware {
	return &Middleware{
		token:    []byte(vars.AppAdminToken),
		disabled: vars.AppAuthDisabled,
	}
}

// Handle wraps the handler: a request without the admin token gets 401 {"error": "unauthorized"}.
func (m *Middleware) Handle(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.disabled && !m.authorized(r) {
			logger.Warning("unauthorized admin request: method=%s path=%s", r.Method, r.URL.Path)
			w.Header().Set(headerWWWAuthenticate, schemeBearer)
			respondWithJSON(w, http.StatusUnauthorized, &errorResponse{Error: msgUnauthorized})
			return
		}

		next.ServeHTTP(w, r)
	})
}

// authorized reports whether the request carries the admin token. An empty admin token matches nothing.
func (m *Middleware) authorized(r *http.Request) bool {
	scheme, token, ok := strings.Cut(r.Header.Get(headerAuthorization), " ")
	if !ok || !strings.EqualFold(scheme, schemeBearer) || len(m.token) == 0 {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), m.token) == 1
}

func respondWithJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		logger.Error(err)
	}
}
