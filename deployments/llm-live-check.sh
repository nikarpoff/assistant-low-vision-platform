#!/usr/bin/env bash
# Live check of the real language model providers with real keys (DEP-11).
# Not a part of the end-to-end check on stubs (DEP-8).
#
#   ./llm-live-check.sh polza      # APP_LLM_API_KEY (Polza.ai key), APP_LLM_MODEL, APP_VLM_MODEL
#   ./llm-live-check.sh gigachat   # GIGACHAT_CREDENTIALS, GIGACHAT_MODEL; needs Docker
#
# Keys and model identifiers come only from the environment: a missing one is a
# failure, not a skip. Keys reach containers by variable name only and are never
# printed or written to files. cmd/llmbench runs from the gateway image
# (ghcr.io/nikarpoff/assistant-low-vision-platform:${ALV_IMAGE_TAG:-latest});
# with ALV_LOCAL_BUILD=1 the image is built from this repository.
set -euo pipefail

DEPLOY_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$DEPLOY_DIR/.." && pwd)"
POLZA_URL="https://polza.ai/api/v1"
GIGACHAT_PROXY_URL="http://127.0.0.1:8104/v1"
GIGACHAT_HEALTH_URL="http://127.0.0.1:8104/health"
HEALTH_WAIT_S=120
LIVE_PROJECT="alv-live"

usage() {
    echo "usage: $0 polza|gigachat" >&2
    exit 2
}

# require VAR... - every variable is set and not empty, otherwise "live: VAR is empty" and exit 1
require() {
    local name
    for name in "$@"; do
        if [[ -z "${!name:-}" ]]; then
            echo "live: $name is empty"
            exit 1
        fi
    done
}

# gateway_image - the image with cmd/llmbench: pulled from the registry or built locally
gateway_image() {
    if [[ "${ALV_LOCAL_BUILD:-0}" == "1" ]]; then
        docker build -q -f "$REPO_DIR/build/Dockerfile" -t alv-platform:live "$REPO_DIR" >/dev/null
        echo "alv-platform:live"
    else
        echo "ghcr.io/nikarpoff/assistant-low-vision-platform:${ALV_IMAGE_TAG:-latest}"
    fi
}

# llmbench ARGS... - runs cmd/llmbench with the key from APP_LLM_API_KEY and prints its JSON.
# The reference prompt and the test image are mounted from testdata/llmbench.
llmbench() {
    docker run --rm --network host \
        -e APP_LLM_API_KEY \
        -v "$REPO_DIR/testdata/llmbench:/testdata/llmbench:ro" \
        -w / \
        --entrypoint /llmbench \
        "$IMAGE" "$@"
}

# json_number KEY - a number field of the compact llmbench JSON on stdin
json_number() {
    sed -n "s/.*\"$1\":\([0-9.eE+-]*\).*/\1/p" | head -n 1
}

# json_first_model - the first non-empty value of response_models of the llmbench JSON on stdin
json_first_model() {
    sed -n 's/.*"response_models":\[\([^]]*\)\].*/\1/p' | head -n 1 \
        | tr ',' '\n' | sed -n 's/^"\(..*\)"$/\1/p' | head -n 1
}

# check_result KIND JSON - a normal stream with a non-empty model in every answer
check_result() {
    local kind="$1" result="$2" model p95
    result="$(tr -d '\n\r\t' <<< "$result" | sed 's/":  */":/g')"
    model="$(json_first_model <<< "$result")"
    p95="$(json_number p95_ms <<< "$result")"
    if [[ -z "$model" || -z "$p95" ]] || grep -q '"response_models":\[[^]]*""' <<< "$result"; then
        echo "live: $kind failed: no model in the provider answer" >&2
        exit 1
    fi
    MODEL="$model"
    P95="$p95"
}

check_polza() {
    require APP_LLM_API_KEY APP_LLM_MODEL APP_VLM_MODEL
    export APP_LLM_API_KEY
    IMAGE="$(gateway_image)"

    local result
    if ! result="$(llmbench --url "$POLZA_URL" --model "$APP_LLM_MODEL" --warmup 1 --n 5)"; then
        echo "live: polza llm failed" >&2
        exit 1
    fi
    check_result "polza llm" "$result"
    echo "live: polza llm ok model=$MODEL ttft_p95_ms=$P95"

    if ! result="$(llmbench --url "$POLZA_URL" --model "$APP_VLM_MODEL" --warmup 0 --n 3 \
        --image /testdata/llmbench/scene.jpg)"; then
        echo "live: polza vlm failed" >&2
        exit 1
    fi
    check_result "polza vlm" "$result"
    echo "live: polza vlm ok model=$MODEL"
}

# stop_proxy - the proxy is stopped on any outcome
stop_proxy() {
    docker compose --env-file /dev/null -p "$LIVE_PROJECT" -f "$DEPLOY_DIR/compose.vm.yml" \
        --profile gigachat down --remove-orphans >/dev/null 2>&1 || true
}

check_gigachat() {
    require GIGACHAT_CREDENTIALS GIGACHAT_MODEL
    # A one-off proxy key for this run only
    APP_LLM_API_KEY="$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')"
    export APP_LLM_API_KEY GIGACHAT_CREDENTIALS GIGACHAT_MODEL
    IMAGE="$(gateway_image)"

    trap stop_proxy EXIT
    docker compose --env-file /dev/null -p "$LIVE_PROJECT" -f "$DEPLOY_DIR/compose.vm.yml" \
        --profile gigachat up -d --no-deps gpt2giga >/dev/null

    local waited=0
    until curl -sf -o /dev/null --max-time 5 "$GIGACHAT_HEALTH_URL"; do
        if (( waited >= HEALTH_WAIT_S )); then
            echo "live: gigachat failed: gpt2giga is not healthy after ${HEALTH_WAIT_S}s" >&2
            exit 1
        fi
        sleep 2
        waited=$(( waited + 2 ))
    done

    local result
    if ! result="$(llmbench --url "$GIGACHAT_PROXY_URL" --model "$GIGACHAT_MODEL" --warmup 1 --n 5)"; then
        echo "live: gigachat failed" >&2
        exit 1
    fi
    check_result "gigachat" "$result"
    echo "live: gigachat ok model=$MODEL ttft_p95_ms=$P95"
}

[[ $# -eq 1 ]] || usage
case "$1" in
    polza) check_polza ;;
    gigachat) check_gigachat ;;
    *) usage ;;
esac
