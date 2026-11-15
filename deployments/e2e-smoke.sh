#!/usr/bin/env bash
# End-to-end check of the whole chain on stubs (DEP-8, AC-3).
#
#   ./deployments/e2e-smoke.sh                    # images from the registry
#   ALV_LOCAL_BUILD=1 ./deployments/e2e-smoke.sh  # images built from the sibling repositories
#
# 1. Brings up compose.fake.yml (project alv-e2e) and waits for /ready.
# 2. Runs cmd/loadgen --strict from the gateway image over the built-in mini dataset
#    (testdata/minidataset): scenarios describe, ask and passive in the describer modes
#    template and llm (the provider stub cmd/fakellm); the gateway is restarted between modes.
# 3. Checks that every request finished without an error, every answer has audio and
#    timings, every request is in the journal (psql in the postgres container) and
#    /v1/stats returns p95 of the stages.
# 4. Prints "e2e: ok". The stand is stopped and its volumes removed on any outcome.
#
# Any deviation - exit code 1. Needs Docker with compose, curl and jq; no provider keys,
# no GPU and no network access besides the registries.
set -euo pipefail

DEPLOY_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$DEPLOY_DIR/.." && pwd)"
PROJECT="alv-e2e"
NETWORK="alv-net"
GATEWAY_URL="http://127.0.0.1:8080"
SESSION_URL="ws://gateway:80/v1/session"
DATASET_DIR="$REPO_DIR/testdata/minidataset"
READY_TIMEOUT_S="${ALV_E2E_READY_TIMEOUT_S:-300}"
JOURNAL_TIMEOUT_S=30
MODES=(template llm)
# Stages every run of the mini dataset passes through (PLT-14)
STAGES=(detect_ms asr_ms llm_ttft_ms tts_first_chunk_ms server_total_ms)
IMAGE="ghcr.io/nikarpoff/assistant-low-vision-platform:${ALV_IMAGE_TAG:-latest}"

COMPOSE_FILES=(-f "$DEPLOY_DIR/compose.fake.yml")
if [[ "${ALV_LOCAL_BUILD:-0}" == "1" ]]; then
    COMPOSE_FILES+=(-f "$DEPLOY_DIR/compose.local-build.yml")
fi

# One-off tokens of this run: the gateway requires them unless authentication is disabled
APP_DEVICE_TOKENS="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
APP_ADMIN_TOKEN="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
LOADGEN_TOKEN="$APP_DEVICE_TOKENS"
export APP_DEVICE_TOKENS APP_ADMIN_TOKEN LOADGEN_TOKEN

WORK_DIR=""

fail() {
    echo "e2e: failed: $*" >&2
    exit 1
}

# compose ARGS... - docker compose of the stand; settings come only from the defaults of
# the compose files and this script, never from deployments/.env
compose() {
    docker compose --env-file /dev/null -p "$PROJECT" "${COMPOSE_FILES[@]}" "$@"
}

# cleanup - the stand, its volumes and the temporary files go away on any outcome
cleanup() {
    local code=$?
    if (( code != 0 )); then
        compose ps -a >&2 || true
        compose logs --no-color --tail 50 >&2 || true
    fi
    compose down -v --remove-orphans >/dev/null 2>&1 || true
    if [[ -n "$WORK_DIR" ]]; then
        rm -rf "$WORK_DIR"
    fi
    exit "$code"
}

# wait_ready - /ready returns 200 within READY_TIMEOUT_S
wait_ready() {
    local waited=0
    until curl -sf -o /dev/null --max-time 5 "$GATEWAY_URL/ready"; do
        if (( waited >= READY_TIMEOUT_S )); then
            curl -s --max-time 5 "$GATEWAY_URL/ready" >&2 || true
            echo >&2
            fail "/ready is not 200 after ${READY_TIMEOUT_S}s"
        fi
        sleep 2
        waited=$(( waited + 2 ))
    done
}

# psql_value SQL - a single value from the journal
psql_value() {
    compose exec -T postgres psql -U alv -d alv -tA -v ON_ERROR_STOP=1 -c "$1"
}

# run_loadgen MODE - one pass of the mini dataset with --strict from the gateway image
run_loadgen() {
    local mode="$1"
    if ! docker run --rm --network "$NETWORK" \
        -e LOADGEN_TOKEN \
        -v "$DATASET_DIR:/testdata/minidataset:ro" \
        -v "$WORK_DIR:/out" \
        -w / \
        --entrypoint /loadgen \
        "$IMAGE" \
        --url "$SESSION_URL" \
        --dataset /testdata/minidataset \
        --scenarios describe,ask,passive \
        --sessions 1 --pause-ms 0 --runs 1 --warmup 1 \
        --describer-mode "$mode" \
        --strict \
        --out "/out/$mode.jsonl" \
        --summary "/out/$mode.summary.json" >/dev/null; then
        fail "loadgen --strict in mode $mode"
    fi
}

# check_requests MODE - every request finished without an error, every answer has audio and timings
check_requests() {
    local mode="$1" file="$WORK_DIR/$mode.jsonl" kind count bad
    [[ -s "$file" ]] || fail "loadgen wrote no requests in mode $mode"
    for kind in describe ask passive; do
        count="$(jq -s --arg kind "$kind" '[.[] | select(.kind == $kind)] | length' "$file")"
        (( count > 0 )) || fail "no $kind requests in mode $mode"
    done
    bad="$(jq -c 'select(.status == "error" or .error_code != null and .error_code != "")' "$file")"
    [[ -z "$bad" ]] || fail "requests with errors in mode $mode: $bad"
    bad="$(jq -c 'select((.status == "ok" or .status == "degraded")
        and ((.audio_bytes // 0) <= 0 or .timings == null or (.timings | length) == 0))' "$file")"
    [[ -z "$bad" ]] || fail "answers without audio or timings in mode $mode: $bad"
}

# check_journal MODE - every request of the run is in the journal with the mode, none is an error
check_journal() {
    local mode="$1" file="$WORK_DIR/$mode.jsonl" ids expected found errors waited=0
    ids="$(jq -r '.request_id' "$file" | sed "s/.*/'&'/" | paste -sd, -)"
    expected="$(jq -s 'length' "$file")"
    # The journal writer is asynchronous: wait for the buffer to be flushed
    while :; do
        found="$(psql_value "SELECT count(*) FROM request WHERE describer_mode = '$mode' AND request_id IN ($ids)")"
        (( found == expected )) && break
        if (( waited >= JOURNAL_TIMEOUT_S )); then
            fail "journal has $found of $expected requests of mode $mode"
        fi
        sleep 1
        waited=$(( waited + 1 ))
    done
    errors="$(psql_value "SELECT count(*) FROM request WHERE request_id IN ($ids) AND status = 'error'")"
    (( errors == 0 )) || fail "journal has $errors requests with status error in mode $mode"
    if [[ "$mode" == "llm" ]]; then
        found="$(psql_value "SELECT count(*) FROM request WHERE request_id IN ($ids) AND llm_model = 'fake/fake-llm'")"
        (( found > 0 )) || fail "journal has no answers of the provider stub in mode llm"
    fi
}

# check_stats - /v1/stats returns p95 of every stage of the chain
check_stats() {
    local stats stage p95
    stats="$(curl -sf --max-time 10 -H "Authorization: Bearer $APP_ADMIN_TOKEN" "$GATEWAY_URL/v1/stats")" \
        || fail "/v1/stats is not 200"
    for stage in "${STAGES[@]}"; do
        p95="$(jq -r --arg stage "$stage" '.stages[$stage].p95 // empty | numbers' <<< "$stats")"
        [[ -n "$p95" ]] || fail "/v1/stats has no p95 of $stage: $stats"
    done
}

for tool in docker curl jq; do
    command -v "$tool" >/dev/null || fail "$tool is not installed"
done
[[ -f "$DATASET_DIR/annotations.json" ]] || fail "no mini dataset in $DATASET_DIR"

trap cleanup EXIT
WORK_DIR="$(mktemp -d)"
# loadgen runs as the nonroot user of the distroless image
chmod 777 "$WORK_DIR"

# A stand left over by an interrupted run must not affect this one
compose down -v --remove-orphans >/dev/null 2>&1 || true
if [[ "${ALV_LOCAL_BUILD:-0}" == "1" ]]; then
    compose build || fail "compose build"
else
    compose pull --quiet || fail "compose pull"
fi

for mode in "${MODES[@]}"; do
    echo "e2e: mode $mode"
    # The first mode brings the stand up, the next one recreates only the gateway
    APP_DESCRIBER_MODE="$mode" compose up -d >/dev/null || fail "compose up in mode $mode"
    wait_ready
    run_loadgen "$mode"
    check_requests "$mode"
    check_journal "$mode"
done
check_stats

echo "e2e: ok"
