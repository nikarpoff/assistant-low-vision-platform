#!/usr/bin/env bash
#
# Runs the integration tests of the journal (build tag "integration") against
# a temporary PostgreSQL: a postgres:17-alpine container published on a free
# port of 127.0.0.1. The container is removed on exit, whatever the result.
#
# Requires Docker, Go and moq (scripts/setup-dev-env.sh). The image can be
# changed by ALV_TEST_POSTGRES_IMAGE.

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

image="${ALV_TEST_POSTGRES_IMAGE:-postgres:17-alpine}"
container="alv-test-postgres-$$-${RANDOM}"
password="alv-integration-test"

cleanup() {
  docker rm -f "${container}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Mocks are not committed: the test packages do not compile without them.
export PATH="${PATH}:$(go env GOPATH)/bin"
./scripts/regenerate-mocks.sh

echo "Starting ${image} as ${container}"
docker run -d --rm --name "${container}" \
  -e POSTGRES_PASSWORD="${password}" \
  -e POSTGRES_DB=alv_test \
  -p 127.0.0.1::5432 \
  "${image}" >/dev/null

port="$(docker port "${container}" 5432/tcp | head -n 1 | sed 's/.*://')"

# The image first runs a temporary server on the socket only, so wait for TCP.
for _ in $(seq 1 60); do
  if docker exec "${container}" pg_isready -q -h 127.0.0.1 -U postgres -d alv_test; then
    break
  fi
  sleep 1
done
docker exec "${container}" pg_isready -h 127.0.0.1 -U postgres -d alv_test

export APP_TEST_POSTGRES_DSN="postgres://postgres:${password}@127.0.0.1:${port}/alv_test?sslmode=disable"

go test -p 1 -count=1 -tags integration -v -run '^TestRepository$' ./...
