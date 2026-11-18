#!/usr/bin/env bash
#
# Removes the moq mocks (*_moq_test.go) and generates them again by the
# //go:generate directives. Mocks are not committed, so run this script on a
# clean clone before go vet and go test.

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

find . -name "*_moq_test.go" -type f -exec rm -f {} +
go generate ./...
