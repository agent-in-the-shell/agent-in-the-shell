#!/usr/bin/env bash
set -euo pipefail

MODULE="github.com/agent-in-the-shell/agent-in-the-shell"
REPO="agent-in-the-shell/agent-in-the-shell"
SERVICES=(
  "agent-model"
  "agent-shell"
)

usage() {
  cat >&2 <<'USAGE'
usage: release-smoke.sh <archives|go-install|homebrew|docker> --tag <tag> [--platform <os/arch>]
USAGE
  exit 2
}

retry() {
  local attempts="${RETRY_ATTEMPTS:-8}"
  local delay="${RETRY_DELAY:-10}"
  local n=1
  local status=0
  until "$@"; do
    status=$?
    if [ "$n" -ge "$attempts" ]; then
      return "$status"
    fi
    echo "retry $n/$attempts failed: $*" >&2
    sleep "$delay"
    n=$((n + 1))
  done
}

default_platform() {
  local os arch
  case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) echo "unsupported OS: $(uname -s)" >&2; exit 2 ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) echo "unsupported arch: $(uname -m)" >&2; exit 2 ;;
  esac
  printf '%s/%s\n' "$os" "$arch"
}

# sha256_file prints the hex digest of a file. Linux runners have sha256sum,
# macOS runners only have shasum; this script runs on both.
sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# verify_checksum fails unless the downloaded asset matches the digest goreleaser
# published for it. Without this the smoke test downloads an arbitrary binary
# over the network and EXECUTES it, which makes it an amplifier for a tampered
# release rather than a check on one. A missing entry is a failure too — the
# absence of a digest must not read as "nothing to verify".
verify_checksum() {
  local checksums="$1" asset="$2" path="$3"
  local want got
  want="$(awk -v name="$asset" '$2 == name || $2 == "*" name {print $1; exit}' "$checksums")"
  if [ -z "$want" ]; then
    echo "checksums.txt has no entry for $asset" >&2
    exit 1
  fi
  got="$(sha256_file "$path")"
  if [ "$got" != "$want" ]; then
    echo "checksum mismatch for $asset: got $got, want $want" >&2
    exit 1
  fi
}

split_platform() {
  local platform="$1"
  GOOS="${platform%/*}"
  GOARCH="${platform#*/}"
  if [ "$GOOS" = "$GOARCH" ] || [ -z "$GOOS" ] || [ -z "$GOARCH" ]; then
    echo "invalid platform: $platform" >&2
    exit 2
  fi
}

# smoke_args_for picks the argument that proves a binary runs. --help is the one
# every service has, needs no config, no network and no credentials, and exits 0
# — which is the whole bar here.
#
# Functional gateway and process-spawning checks also run before tagging.
smoke_args_for() {
  SMOKE_ARGS=("--help")
}

run_binary_smoke() {
  local bin="$1"
  local service="$2"
  local work="$3"
  mkdir -p "$work"
  smoke_args_for
  "$bin" "${SMOKE_ARGS[@]}"
}

run_archives() {
  split_platform "$PLATFORM"
  local root
  root="$(mktemp -d)"
  local checksums="$root/checksums.txt"
  retry curl -fsSL -o "$checksums" "https://github.com/$REPO/releases/download/$TAG/checksums.txt"
  for service in "${SERVICES[@]}"; do
    local asset archive extract_dir bin
    asset="${service}_${GOOS}_${GOARCH}.tar.gz"
    archive="$root/$asset"
    extract_dir="$root/$service"
    mkdir -p "$extract_dir"
    retry curl -fsSL -o "$archive" "https://github.com/$REPO/releases/download/$TAG/$asset"
    verify_checksum "$checksums" "$asset" "$archive"
    tar -xzf "$archive" -C "$extract_dir"
    bin="$extract_dir/$service"
    if [ ! -x "$bin" ]; then
      chmod +x "$bin"
    fi
    run_binary_smoke "$bin" "$service" "$root/smoke-$service"
  done
}

run_go_install() {
  local root
  root="$(mktemp -d)"
  export GOBIN="$root/bin"
  export GOMODCACHE="$root/mod"
  export GOCACHE="$root/cache"
  mkdir -p "$GOBIN" "$GOMODCACHE" "$GOCACHE"
  retry go list -m "$MODULE@$TAG" >/dev/null
  for service in "${SERVICES[@]}"; do
    retry go install "$MODULE/cmd/$service@$TAG"
    run_binary_smoke "$GOBIN/$service" "$service" "$root/smoke-$service"
  done
}

run_homebrew() {
  if ! command -v brew >/dev/null 2>&1; then
    echo "Homebrew is not installed on this runner" >&2
    exit 1
  fi
  local root
  root="$(mktemp -d)"
  retry brew update
  for service in "${SERVICES[@]}"; do
    retry brew install "agent-in-the-shell/tap/$service"
    local want got
    want="${TAG#v}"
    got="$(brew list --versions "$service" | awk 'NR == 1 {print $2}')"
    if [ "$got" != "$want" ] && [ "$got" != "$TAG" ]; then
      echo "Homebrew installed $service version $got, want $TAG" >&2
      exit 1
    fi
    local bin
    bin="$(command -v "$service")"
    run_binary_smoke "$bin" "$service" "$root/smoke-$service"
  done
}

# DOCKER_SERVICES is a subset of SERVICES: only services with a dockers_v2 entry
# in .goreleaser.yaml are published as images. agent-shell is deliberately not —
# see the comment there. Iterating SERVICES here would fail the smoke test on an
# image that was never built.
DOCKER_SERVICES=(
  "agent-model"
)

run_docker() {
  split_platform "$PLATFORM"
  local image_tag="${TAG#v}"
  for service in "${DOCKER_SERVICES[@]}"; do
    local image inspect compact
    image="ghcr.io/agent-in-the-shell/$service:$image_tag"
    inspect="$(retry docker buildx imagetools inspect --raw "$image")"
    compact="$(printf '%s' "$inspect" | tr -d '[:space:]')"
    echo "$compact" | grep -q "\"os\":\"$GOOS\""
    echo "$compact" | grep -q "\"architecture\":\"$GOARCH\""
    retry docker pull --platform "$PLATFORM" "$image"
    smoke_args_for
    retry docker run --rm --platform "$PLATFORM" "$image" "${SMOKE_ARGS[@]}"
  done
}

main() {
  if [ "${#SERVICES[@]}" -eq 0 ]; then
    echo "no released services were generated into this smoke script" >&2
    exit 1
  fi
  if [ $# -lt 1 ]; then
    usage
  fi
  local cmd="$1"
  shift
  TAG=""
  PLATFORM="$(default_platform)"
  while [ $# -gt 0 ]; do
    case "$1" in
      --tag)
        [ $# -ge 2 ] || usage
        TAG="$2"
        shift 2
        ;;
      --platform)
        [ $# -ge 2 ] || usage
        PLATFORM="$2"
        shift 2
        ;;
      *)
        usage
        ;;
    esac
  done
  if [ -z "$TAG" ]; then
    usage
  fi
  case "$cmd" in
    archives) run_archives ;;
    go-install) run_go_install ;;
    homebrew) run_homebrew ;;
    docker) run_docker ;;
    *) usage ;;
  esac
}

main "$@"
