package utils

import (
	"fmt"
	"net/http"
	"time"
)

// HealthcheckTimeout limits the request of Healthcheck.
const HealthcheckTimeout = 3 * time.Second

// Texts of the healthcheck errors, shared by the code and the tests.
const (
	errHealthcheckFormat       = "healthcheck failed: %w"
	errHealthcheckStatusFormat = "healthcheck failed: status %d"
)

// healthURLFormat is the address the healthcheck asks: the local port of the command.
const healthURLFormat = "http://127.0.0.1:%d/health"

// Healthcheck asks GET http://127.0.0.1:<port>/health and returns nil on 200.
// The distroless images have no shell and curl, so the commands check themselves with
// the flag -healthcheck: the compose healthcheck runs ["CMD", "/gateway", "-healthcheck"] (DEP-7).
func Healthcheck(port int) error {
	client := &http.Client{Timeout: HealthcheckTimeout}

	response, err := client.Get(fmt.Sprintf(healthURLFormat, port))
	if err != nil {
		return fmt.Errorf(errHealthcheckFormat, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf(errHealthcheckStatusFormat, response.StatusCode)
	}
	return nil
}
