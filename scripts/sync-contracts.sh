#!/usr/bin/env bash
#
# Copies the shared contracts from this repository (the source of truth) to the
# sibling repositories of the common project, or checks the copies with --check.
#
#   testdata/contracts/<service>/   -> <service>/tests/contract/
#   testdata/protocol/vectors.json  -> assistant-low-vision-mobile/app/src/test/resources/protocol/vectors.json
#
# The common project layout is expected: this repository and the service and
# mobile repositories lie side by side in one directory. Copies are mirrors:
# files that are absent in the source are removed from the copy. Copies are
# never edited by hand: change the source, run this script, commit everywhere.
#
# Usage: scripts/sync-contracts.sh [--check]
# Exit codes: 0 - copies match, 1 - copies differ or a repository is missing,
# 2 - wrong arguments.

set -euo pipefail

usage() {
  echo "usage: $0 [--check]"
}

mode="sync"
if [[ $# -gt 1 ]]; then
  usage >&2
  exit 2
fi
case "${1:-}" in
  "") ;;
  --check) mode="check" ;;
  -h | --help)
    usage
    exit 0
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac

platform_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
root_dir="$(dirname "${platform_dir}")"

services=(objects-detector asr-service tts-service)
mobile_repo="assistant-low-vision-mobile"
vectors_src="${platform_dir}/testdata/protocol/vectors.json"
vectors_dst="${root_dir}/${mobile_repo}/app/src/test/resources/protocol/vectors.json"

contract_src() {
  echo "${platform_dir}/testdata/contracts/$1"
}

contract_dst() {
  echo "${root_dir}/$1/tests/contract"
}

require_dir() {
  if [[ ! -d "$1" ]]; then
    echo "contracts: directory not found: $1" >&2
    exit 1
  fi
}

check() {
  local failed=0 service
  for service in "${services[@]}"; do
    if ! diff -r "$(contract_src "${service}")" "$(contract_dst "${service}")"; then
      echo "contracts: ${service}/tests/contract differs from testdata/contracts/${service}" >&2
      failed=1
    fi
  done
  if ! cmp "${vectors_src}" "${vectors_dst}"; then
    echo "contracts: ${mobile_repo} protocol vectors differ from testdata/protocol/vectors.json" >&2
    failed=1
  fi
  if [[ ${failed} -ne 0 ]]; then
    echo "contracts: copies are out of sync, run scripts/sync-contracts.sh" >&2
    return 1
  fi
  echo "contracts: copies are in sync"
}

sync() {
  local service src dst
  for service in "${services[@]}"; do
    src="$(contract_src "${service}")"
    dst="$(contract_dst "${service}")"
    require_dir "${src}"
    require_dir "${root_dir}/${service}"
    rm -rf "${dst}"
    mkdir -p "${dst}"
    cp -R "${src}/." "${dst}/"
    echo "contracts: copied testdata/contracts/${service} to ${service}/tests/contract"
  done
  require_dir "${root_dir}/${mobile_repo}"
  mkdir -p "$(dirname "${vectors_dst}")"
  cp "${vectors_src}" "${vectors_dst}"
  echo "contracts: copied testdata/protocol/vectors.json to ${mobile_repo}"
}

if [[ "${mode}" == "sync" ]]; then
  sync
fi
check
