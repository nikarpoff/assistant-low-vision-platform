#!/usr/bin/env bash
#
# Prepares a development machine for the assistant-low-vision project: Go and
# moq for the gateway, JDK, Android SDK and Gradle for the mobile app, Python
# 3.12 for the AI services. The script is idempotent: whatever is already
# installed in the required version is left untouched, so a second run changes
# nothing.
#
#   Go 1.24.13         /usr/local/go, links /usr/local/bin/{go,gofmt}
#   moq v0.5.3         /usr/local/bin/moq
#   JDK 17 (or 21)     apt package openjdk-<n>-jdk-headless
#   Android SDK        /opt/android-sdk: cmdline-tools/latest, platform-tools,
#                      platforms;android-37.0, build-tools;37.0.0
#   Gradle 9.8.2       /opt/gradle/gradle-9.8.2, only to generate the wrapper
#   Python 3.12        uv python install 3.12
#
# It also writes sdk.dir to assistant-low-vision-mobile/local.properties and
# the Gradle daemon idle timeout to ~/.gradle/gradle.properties, and ends with
# a check that prints the versions of all tools and the free disk space.
#
# Android SDK licenses are accepted only on explicit request of the person who
# has read them: --accept-android-licenses or ALV_ACCEPT_ANDROID_LICENSES=1.
# Without it the script stops before `sdkmanager --licenses` with a hint.
#
# Requirements: Linux x86_64 with apt, curl, unzip, uv; Docker Engine with the
# compose plugin is checked but not installed. Run as a regular user with sudo
# rights (the script calls sudo itself) or as root.
#
# Usage: scripts/setup-dev-env.sh [--accept-android-licenses] [--mobile-dir DIR]
# Exit codes: 0 - environment is ready, 1 - a step or a final check failed or
# licenses are not accepted, 2 - wrong arguments.

set -euo pipefail

go_version="1.24.13"
go_sha256="1fc94b57134d51669c72173ad5d49fd62afb0f1db9bf3f798fd98ee423f8d730"
moq_version="v0.5.3"

android_sdk="/opt/android-sdk"
cmdline_tools_build="15859902"
cmdline_tools_sha256="4e4c464f145a7512b57d088ac6c278c03c9eea610886b35a5e0804e74eedf583"
android_packages=("platform-tools" "platforms;android-37.0" "build-tools;37.0.0")

gradle_version="9.8.2"
gradle_sha256="21bd5da2de49b642904077f38c0efdc7778e92b3ed0889392dca61b97e389607"
gradle_dir="/opt/gradle/gradle-${gradle_version}"
gradle_idle_timeout_ms="600000"

python_version="3.12"
min_free_gb=15

platform_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
root_dir="$(dirname "${platform_dir}")"

accept_licenses="${ALV_ACCEPT_ANDROID_LICENSES:-0}"
mobile_dir="${ALV_MOBILE_DIR:-${root_dir}/assistant-low-vision-mobile}"

usage() {
  cat <<EOF
Использование: $0 [--accept-android-licenses] [--mobile-dir DIR]

  --accept-android-licenses  принять лицензии Android SDK (sdkmanager --licenses);
                             то же, что ALV_ACCEPT_ANDROID_LICENSES=1
  --mobile-dir DIR           каталог репозитория приложения для local.properties;
                             то же, что ALV_MOBILE_DIR, по умолчанию
                             ${root_dir}/assistant-low-vision-mobile
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --accept-android-licenses)
      accept_licenses=1
      shift
      ;;
    --mobile-dir)
      if [[ $# -lt 2 ]]; then
        usage >&2
        exit 2
      fi
      mobile_dir="$2"
      shift 2
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      exit 2
      ;;
  esac
done

log() {
  echo "setup-dev-env: $*"
}

warn() {
  echo "setup-dev-env: ВНИМАНИЕ: $*" >&2
}

die() {
  echo "setup-dev-env: ОШИБКА: $*" >&2
  exit 1
}

step() {
  echo
  echo "==> $*"
}

# as_root runs a command as root: directly when the script already runs as
# root, through sudo otherwise.
as_root() {
  if [[ ${EUID} -eq 0 ]]; then
    "$@"
  else
    sudo "$@"
  fi
}

# download fetches URL to FILE and verifies its SHA-256 before anything is
# unpacked from it.
download() {
  local url="$1" sha256="$2" file="$3"
  log "скачивание ${url}"
  curl -fsSL --retry 3 -o "${file}" "${url}"
  if ! sha256sum --check --status <<<"${sha256}  ${file}"; then
    die "контрольная сумма SHA-256 не совпала: ${url}"
  fi
}

# install_archive unpacks ARCHIVE whose files lie under the top-level directory
# TOP into a staging directory next to DEST and then moves TOP to DEST, so an
# interrupted run never leaves a half-unpacked DEST that looks installed.
install_archive() {
  local archive="$1" top="$2" dest="$3" staging
  staging="$(dirname "${dest}")/.staging-$(basename "${dest}")"
  as_root rm -rf "${staging}"
  as_root mkdir -p "${staging}"
  case "${archive}" in
    *.zip) as_root unzip -q "${archive}" -d "${staging}" ;;
    *.tar.gz) as_root tar --no-same-owner -xzf "${archive}" -C "${staging}" ;;
    *) die "неизвестный формат архива: ${archive}" ;;
  esac
  as_root rm -rf "${dest}"
  as_root mv "${staging}/${top}" "${dest}"
  as_root rm -rf "${staging}"
}

# link points LINK at TARGET unless it already does.
link() {
  local target="$1" name="$2"
  if [[ "$(readlink "${name}" || true)" == "${target}" ]]; then
    log "ссылка ${name} -> ${target} уже есть"
    return
  fi
  as_root ln -sfn "${target}" "${name}"
  log "создана ссылка ${name} -> ${target}"
}

# set_property writes KEY=VALUE to a Java properties FILE owned by the current
# user: replaces an existing KEY, appends a missing one, leaves the rest as is.
set_property() {
  local file="$1" key="$2" value="$3" line
  line="${key}=${value}"
  mkdir -p "$(dirname "${file}")"
  if [[ -f "${file}" ]] && grep -qxF "${line}" "${file}"; then
    log "${file}: ${line} уже записано"
    return
  fi
  if [[ -f "${file}" ]] && grep -q "^${key//./\\.}=" "${file}"; then
    sed -i "s|^${key//./\\.}=.*|${line}|" "${file}"
  else
    if [[ -s "${file}" && -n "$(tail -c 1 "${file}")" ]]; then
      echo >>"${file}"
    fi
    echo "${line}" >>"${file}"
  fi
  log "${file}: записано ${line}"
}

check_prerequisites() {
  local cmd
  step "Предварительные условия"
  if [[ "$(uname -s)" != "Linux" || "$(uname -m)" != "x86_64" ]]; then
    die "нужен Linux x86_64: build-tools и platform-tools Android для Linux есть только под эту архитектуру"
  fi
  for cmd in apt-get apt-cache dpkg dpkg-query curl unzip tar sha256sum; do
    if ! command -v "${cmd}" >/dev/null; then
      die "не найдена команда ${cmd}: нужен Debian или Ubuntu, curl и unzip ставятся командой sudo apt-get install curl unzip"
    fi
  done
  if [[ ${EUID} -ne 0 ]] && ! command -v sudo >/dev/null; then
    die "запустите скрипт от root или установите sudo"
  fi
  if [[ ${EUID} -eq 0 && -n "${SUDO_USER:-}" ]]; then
    warn "скрипт запущен через sudo: ~/.gradle и Python ${python_version} будут настроены для root, а не для ${SUDO_USER}; запускайте его от своего пользователя"
  fi
  uv_bin="$(command -v uv || true)"
  if [[ -z "${uv_bin}" && -x "${HOME}/.local/bin/uv" ]]; then
    uv_bin="${HOME}/.local/bin/uv"
  fi
  if [[ -z "${uv_bin}" ]]; then
    die "не найден uv: установите его по https://docs.astral.sh/uv/getting-started/installation/"
  fi
  log "система $(uname -sm), пользователь $(id -un), uv ${uv_bin}"
}

install_go() {
  local current=""
  step "Go ${go_version}"
  if [[ -f /usr/local/go/VERSION ]]; then
    current="$(head -n 1 /usr/local/go/VERSION)"
  fi
  if [[ "${current}" == "go${go_version}" ]]; then
    log "Go ${go_version} уже установлен в /usr/local/go"
  else
    if [[ -n "${current}" ]]; then
      log "в /usr/local/go найден ${current}, он будет заменён на go${go_version}"
    fi
    download "https://go.dev/dl/go${go_version}.linux-amd64.tar.gz" "${go_sha256}" "${tmp_dir}/go.tar.gz"
    install_archive "${tmp_dir}/go.tar.gz" go /usr/local/go
    log "Go ${go_version} установлен в /usr/local/go"
  fi
  link /usr/local/go/bin/go /usr/local/bin/go
  link /usr/local/go/bin/gofmt /usr/local/bin/gofmt
}

install_moq() {
  local build_info=""
  step "moq ${moq_version}"
  if [[ -x /usr/local/bin/moq ]]; then
    build_info="$(/usr/local/go/bin/go version -m /usr/local/bin/moq 2>/dev/null || true)"
  fi
  if grep -Eq "^[[:space:]]+mod[[:space:]]+github.com/matryer/moq[[:space:]]+${moq_version}([[:space:]]|$)" <<<"${build_info}"; then
    log "moq ${moq_version} уже установлен в /usr/local/bin"
    return
  fi
  as_root env GOBIN=/usr/local/bin GOTOOLCHAIN=local GOFLAGS= /usr/local/go/bin/go install "github.com/matryer/moq@${moq_version}"
  log "moq ${moq_version} установлен в /usr/local/bin"
}

jdk_installed() {
  local status
  status="$(dpkg-query -W -f='${Status}' "openjdk-$1-jdk-headless" 2>/dev/null || true)"
  [[ "${status}" == "install ok installed" ]]
}

jdk_available() {
  local policy
  policy="$(LC_ALL=C apt-cache policy "openjdk-$1-jdk-headless" 2>/dev/null || true)"
  grep -Eq '^[[:space:]]+Candidate: [0-9]' <<<"${policy}"
}

install_jdk() {
  step "JDK"
  if jdk_installed 17; then
    jdk_version=17
    log "openjdk-17-jdk-headless уже установлен"
  elif jdk_installed 21 && ! jdk_available 17; then
    jdk_version=21
    log "openjdk-21-jdk-headless уже установлен, openjdk-17-jdk-headless в репозиториях нет"
  else
    as_root apt-get update -q
    if jdk_available 17; then
      jdk_version=17
    elif jdk_available 21; then
      jdk_version=21
    else
      die "в репозиториях нет ни openjdk-17-jdk-headless, ни openjdk-21-jdk-headless"
    fi
    as_root env DEBIAN_FRONTEND=noninteractive apt-get install -y -q --no-install-recommends "openjdk-${jdk_version}-jdk-headless"
    log "openjdk-${jdk_version}-jdk-headless установлен"
  fi
  java_home="/usr/lib/jvm/java-${jdk_version}-openjdk-$(dpkg --print-architecture)"
  if [[ ! -x "${java_home}/bin/java" ]]; then
    die "не найден ${java_home}/bin/java из пакета openjdk-${jdk_version}-jdk-headless"
  fi
  export JAVA_HOME="${java_home}"
  log "JAVA_HOME=${java_home}"
}

run_sdkmanager() {
  as_root env JAVA_HOME="${java_home}" "${android_sdk}/cmdline-tools/latest/bin/sdkmanager" --sdk_root="${android_sdk}" "$@"
}

install_cmdline_tools() {
  local dest="${android_sdk}/cmdline-tools/latest"
  step "Android SDK: cmdline-tools"
  if [[ -x "${dest}/bin/sdkmanager" ]]; then
    log "cmdline-tools уже установлены в ${dest}"
    return
  fi
  download "https://dl.google.com/android/repository/commandlinetools-linux-${cmdline_tools_build}_latest.zip" \
    "${cmdline_tools_sha256}" "${tmp_dir}/cmdline-tools.zip"
  as_root mkdir -p "${android_sdk}/cmdline-tools"
  install_archive "${tmp_dir}/cmdline-tools.zip" cmdline-tools "${dest}"
  log "cmdline-tools установлены в ${dest}"
}

accept_android_licenses() {
  local license="${android_sdk}/licenses/android-sdk-license"
  step "Android SDK: лицензии"
  if [[ -s "${license}" ]]; then
    log "лицензии Android SDK уже приняты: ${android_sdk}/licenses"
    return
  fi
  if [[ "${accept_licenses}" != "1" ]]; then
    die "лицензии Android SDK не приняты, установка остановлена перед sdkmanager --licenses.
  Прочитайте и примите их сами:
    sudo env JAVA_HOME=${java_home} ${android_sdk}/cmdline-tools/latest/bin/sdkmanager --licenses
  или, прочитав их, запустите скрипт заново с --accept-android-licenses
  (или ALV_ACCEPT_ANDROID_LICENSES=1): тогда он примет их за вас."
  fi
  log "принятие лицензий Android SDK (--accept-android-licenses)"
  if ! { yes || true; } | run_sdkmanager --licenses >/dev/null; then
    die "sdkmanager --licenses завершился с ошибкой"
  fi
  if [[ ! -s "${license}" ]]; then
    die "sdkmanager --licenses не записал ${license}"
  fi
  log "лицензии Android SDK приняты: ${android_sdk}/licenses"
}

install_android_packages() {
  local pkg missing=()
  step "Android SDK: пакеты"
  for pkg in "${android_packages[@]}"; do
    if [[ ! -f "${android_sdk}/${pkg//;//}/package.xml" ]]; then
      missing+=("${pkg}")
    fi
  done
  if [[ ${#missing[@]} -eq 0 ]]; then
    log "пакеты уже установлены: ${android_packages[*]}"
    return
  fi
  log "установка пакетов: ${missing[*]}"
  run_sdkmanager --install "${missing[@]}" </dev/null
  for pkg in "${missing[@]}"; do
    if [[ ! -f "${android_sdk}/${pkg//;//}/package.xml" ]]; then
      die "sdkmanager не установил ${pkg}"
    fi
  done
  log "пакеты установлены: ${missing[*]}"
}

write_local_properties() {
  step "local.properties приложения"
  if [[ ! -d "${mobile_dir}" ]]; then
    warn "не найден каталог приложения ${mobile_dir}: sdk.dir не записан, укажите его через --mobile-dir"
    return
  fi
  set_property "${mobile_dir}/local.properties" sdk.dir "${android_sdk}"
}

install_gradle() {
  step "Gradle ${gradle_version}"
  if [[ -x "${gradle_dir}/bin/gradle" ]]; then
    log "Gradle ${gradle_version} уже установлен в ${gradle_dir}"
  else
    download "https://services.gradle.org/distributions/gradle-${gradle_version}-bin.zip" \
      "${gradle_sha256}" "${tmp_dir}/gradle.zip"
    as_root mkdir -p "$(dirname "${gradle_dir}")"
    install_archive "${tmp_dir}/gradle.zip" "gradle-${gradle_version}" "${gradle_dir}"
    log "Gradle ${gradle_version} установлен в ${gradle_dir}"
  fi
  set_property "${HOME}/.gradle/gradle.properties" org.gradle.daemon.idletimeout "${gradle_idle_timeout_ms}"
}

install_python() {
  local found
  step "Python ${python_version}"
  # `uv python install` rewrites the files of an installed Python even when it
  # reports "already installed", so an installed one is looked up first.
  if found="$("${uv_bin}" python find --managed-python "${python_version}" 2>/dev/null)"; then
    log "Python ${python_version} уже установлен через uv: ${found}"
    return
  fi
  "${uv_bin}" python install "${python_version}"
}

# check_free_space prints the free space on every file system the project uses
# (SDKs in /opt, caches in the home directory, Docker images) and warns when
# it is below min_free_gb: by the end of the setup some of it is already used.
check_free_space() {
  local path avail target gb docker_root seen=" "
  docker_root="$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || true)"
  echo "\$ df /opt ${HOME} ${docker_root:-/var/lib/docker}"
  for path in /opt "${HOME}" "${docker_root:-/var/lib/docker}"; do
    if [[ ! -d "${path}" ]]; then
      continue
    fi
    read -r avail target < <(df -P -k "${path}" | awk 'NR == 2 { print $4, $6 }')
    if [[ "${seen}" == *" ${target} "* ]]; then
      continue
    fi
    seen+="${target} "
    gb="$(awk -v kb="${avail}" 'BEGIN { printf "%.1f", kb / 1024 / 1024 }')"
    if ((avail < min_free_gb * 1024 * 1024)); then
      warn "свободно ${gb} ГБ на ${target} (${path}), для работы над проектом нужно не меньше ${min_free_gb} ГБ"
    else
      log "свободно ${gb} ГБ на ${target} (${path})"
    fi
  done
}

# run_check prints a command and its output and remembers the command when it
# fails, so all checks run and the summary lists every failure.
run_check() {
  echo "\$ $*"
  if ! "$@" 2>&1; then
    failed_checks+=("$*")
  fi
}

# moq_module_version prints the moq module version from the build info of the
# binary: `moq -version` of a binary built by `go install` always says "dev".
moq_module_version() {
  local build_info
  build_info="$(go version -m "$(command -v moq)")"
  grep -E "^[[:space:]]+mod[[:space:]]+github.com/matryer/moq[[:space:]]+${moq_version}([[:space:]]|$)" <<<"${build_info}"
}

final_check() {
  failed_checks=()
  step "Проверка окружения"
  cd /
  run_check go version
  run_check moq -version
  run_check moq_module_version
  run_check java -version
  run_check "${android_sdk}/cmdline-tools/latest/bin/sdkmanager" --list_installed
  run_check "${gradle_dir}/bin/gradle" --version
  run_check "${uv_bin}" python find "${python_version}"
  run_check docker compose version
  check_free_space
  echo
  if [[ ${#failed_checks[@]} -gt 0 ]]; then
    printf 'setup-dev-env: ОШИБКА: не прошла проверка: %s\n' "${failed_checks[@]}" >&2
    exit 1
  fi
  log "окружение готово"
}

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

check_prerequisites
install_go
install_moq
install_jdk
install_cmdline_tools
accept_android_licenses
install_android_packages
write_local_properties
install_gradle
install_python
final_check
