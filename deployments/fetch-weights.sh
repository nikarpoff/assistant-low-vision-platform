#!/usr/bin/env bash
# Downloads the weights of the models set in the settings file into the named volumes
# of the detector, ASR and TTS ahead of time, for an offline start through ARTIFACTS_PATH (DEP-6).
#
#   ./fetch-weights.sh [--dry-run] [--env-file <file>]...
#
# --env-file defaults to deployments/.env and may be repeated (.env, then reference.env):
# a later file wins, a variable of the shell wins over the files - as in docker compose.
# For every service it prints "fetch: <service> <backend>/<model> -> <volume>:<dir>";
# --dry-run only prints the plan, without Docker and the network.
# Without --dry-run it runs `python -m scripts.fetch_weights` in the service image
# (compose.models.yml), so neither Python nor the models are needed on the host.
set -euo pipefail

DEPLOY_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE_FILE="$DEPLOY_DIR/compose.models.yml"
WEIGHTS_DIR="/opt/model"

usage() {
    echo "usage: $0 [--dry-run] [--env-file <file>]..." >&2
    exit 2
}

dry_run=0
env_files=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        --dry-run) dry_run=1; shift ;;
        --env-file)
            [[ $# -ge 2 ]] || usage
            env_files+=("$2"); shift 2 ;;
        -h|--help) usage ;;
        *) echo "fetch: unknown argument $1" >&2; usage ;;
    esac
done
[[ ${#env_files[@]} -gt 0 ]] || env_files=("$DEPLOY_DIR/.env")

# Settings are read as data, never sourced: KEY=VALUE lines, comments and blanks skipped
declare -A settings=()
for file in "${env_files[@]}"; do
    if [[ ! -f "$file" ]]; then
        echo "fetch: env file $file not found" >&2
        exit 1
    fi
    while IFS= read -r line || [[ -n "$line" ]]; do
        line="${line%$'\r'}"
        [[ "$line" =~ ^[[:space:]]*(#|$) ]] && continue
        [[ "$line" =~ ^[[:space:]]*(export[[:space:]]+)?([A-Za-z_][A-Za-z0-9_]*)=(.*)$ ]] || continue
        key="${BASH_REMATCH[2]}"
        value="${BASH_REMATCH[3]}"
        if [[ "$value" =~ ^\"(.*)\"[[:space:]]*$ || "$value" =~ ^\'(.*)\'[[:space:]]*$ ]]; then
            value="${BASH_REMATCH[1]}"
        else
            value="${value%%[[:space:]]#*}"
            value="${value%"${value##*[![:space:]]}"}"
        fi
        settings["$key"]="$value"
    done < "$file"
done

# setting KEY DEFAULT - the shell, then the files, then the default of the compose file
setting() {
    local key="$1" fallback="$2" value
    if [[ -n "${!key+x}" ]]; then
        value="${!key}"
    else
        value="${settings[$key]-}"
    fi
    printf '%s' "${value:-$fallback}"
}

compose_args=()
for file in "${env_files[@]}"; do
    compose_args+=(--env-file "$file")
done
compose_args+=(-f "$COMPOSE_FILE")

status=0
for service in detector asr tts; do
    prefix="$(tr '[:lower:]' '[:upper:]' <<< "$service")"
    backend="$(setting "${prefix}_MODEL_BACKEND" fake)"
    model="$(setting "${prefix}_MODEL_NAME" default)"
    echo "fetch: $service $backend/$model -> alv-$service-weights:$WEIGHTS_DIR"
    [[ $dry_run -eq 1 ]] && continue

    # The service of compose.models.yml brings the image, the settings and the volume
    if ! docker compose "${compose_args[@]}" run --rm --no-deps -T \
        --entrypoint python "$service" -m scripts.fetch_weights; then
        echo "fetch: $service failed" >&2
        status=1
    fi
done
exit $status
