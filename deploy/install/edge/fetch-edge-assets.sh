#!/usr/bin/env bash
# Download checksum-verified Edge binaries directly from CNB Release assets.

set -euo pipefail

DEST_DIR=${1:?usage: fetch-edge-assets.sh <destination> <version> [linux-amd64|linux-arm64 ...]}
VERSION=${2:?version}
shift 2

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
CONFIG_FILE=${ONGRID_EDGE_ARTIFACT_CONFIG:-$SCRIPT_DIR/edge-artifacts.env}
CONFIG_DEPS_TAG=""
if [[ -f "$CONFIG_FILE" ]]; then
    CONFIG_DEPS_TAG=$(sed -n 's/^ONGRID_EDGE_DEPS_TAG=//p' "$CONFIG_FILE" | tail -n 1)
fi

BASE_URL=${ONGRID_EDGE_ARTIFACT_BASE_URL:-https://cnb.cool/ongridio/ongrid-edge/-/releases/download}
DEPS_TAG=${ONGRID_EDGE_DEPS_TAG:-$CONFIG_DEPS_TAG}
[[ -n "$DEPS_TAG" ]] || { echo "ONGRID_EDGE_DEPS_TAG is required (normally provided by edge-artifacts.env)" >&2; exit 1; }
CACHE_DIR=${ONGRID_EDGE_ARTIFACT_CACHE_DIR:-/var/cache/ongrid/edge-artifacts}
CURL_FLAGS=(--fail --location --show-error --retry 3 --retry-all-errors --retry-delay 3 --connect-timeout 15)

if (( $# > 0 )); then
    TARGETS=("$@")
else
    read -r -a TARGETS <<<"${ONGRID_EDGE_TARGETS:-linux-amd64}"
fi

log() { printf '[edge-assets] %s\n' "$*" >&2; }
die() { printf '[edge-assets] error: %s\n' "$*" >&2; exit 1; }

[[ "$VERSION" =~ ^v?[0-9A-Za-z][0-9A-Za-z._-]*$ ]] || die "invalid Edge version: $VERSION"
[[ "$DEPS_TAG" =~ ^[0-9A-Za-z][0-9A-Za-z._-]*$ ]] || die "invalid dependency release tag: $DEPS_TAG"

command -v curl >/dev/null 2>&1 || die "curl is required"
command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required"
command -v tar >/dev/null 2>&1 || die "tar is required"
mkdir -p "$DEST_DIR" "$CACHE_DIR"

work=$(mktemp -d "${TMPDIR:-/tmp}/ongrid-edge-assets.XXXXXX")
trap 'rm -rf "$work"' EXIT

download_verified() {
    local tag=$1 filename=$2
    local cached="$CACHE_DIR/$filename" sidecar="$CACHE_DIR/$filename.sha256"
    if [[ -s "$cached" && -s "$sidecar" ]] && (cd "$CACHE_DIR" && sha256sum -c "$(basename "$sidecar")" >/dev/null 2>&1); then
        log "using verified cache $filename"
        printf '%s\n' "$cached"
        return 0
    fi

    local incoming="$work/incoming/$filename"
    mkdir -p "$(dirname "$incoming")"
    log "downloading $BASE_URL/$tag/$filename"
    curl "${CURL_FLAGS[@]}" -o "$incoming" "$BASE_URL/$tag/$filename"
    curl "${CURL_FLAGS[@]}" -o "$incoming.sha256" "$BASE_URL/$tag/$filename.sha256"
    (cd "$(dirname "$incoming")" && sha256sum -c "$(basename "$incoming").sha256") >&2 \
        || die "checksum verification failed for $filename"
    install -m 0644 "$incoming" "$cached"
    install -m 0644 "$incoming.sha256" "$sidecar"
    printf '%s\n' "$cached"
}

deps=(
    node_exporter process_exporter mysqld_exporter postgres_exporter
    redis_exporter mongodb_exporter promtail otelcol-contrib
)

for target in "${TARGETS[@]}"; do
    case "$target" in
        linux-amd64|linux-arm64) ;;
        *) die "unsupported Edge target: $target" ;;
    esac

    deps_name="edge-deps-${target}.tar.xz"
    edge_name="ongrid-edge-${target}-${VERSION}"
    deps_archive=$(download_verified "$DEPS_TAG" "$deps_name")
    edge_binary=$(download_verified "$VERSION" "$edge_name")

    extract_dir="$work/extract-$target"
    mkdir -p "$extract_dir"
    while IFS= read -r entry; do
        entry=${entry#./}
        case "$entry" in
            ""|TARGET|DEPENDENCIES|MANIFEST.sha256|node_exporter|process_exporter|mysqld_exporter|postgres_exporter|redis_exporter|mongodb_exporter|promtail|otelcol-contrib) ;;
            *) die "$deps_name contains unexpected path: $entry" ;;
        esac
    done < <(tar -tJf "$deps_archive")
    tar -xJf "$deps_archive" -C "$extract_dir"
    [[ -f "$extract_dir/MANIFEST.sha256" && ! -L "$extract_dir/MANIFEST.sha256" && -s "$extract_dir/MANIFEST.sha256" ]] \
        || die "$deps_name is missing a regular MANIFEST.sha256"
    (cd "$extract_dir" && sha256sum -c MANIFEST.sha256) || die "dependency manifest failed for $target"
    [[ "$(tr -d '[:space:]' < "$extract_dir/TARGET")" == "$target" ]] \
        || die "dependency archive target does not match $target"

    for component in "${deps[@]}"; do
        [[ -f "$extract_dir/$component" && ! -L "$extract_dir/$component" && -s "$extract_dir/$component" ]] \
            || die "$deps_name is missing regular file $component"
        install -m 0755 "$extract_dir/$component" "$DEST_DIR/${component}-${target}"
    done
    install -m 0755 "$edge_binary" "$DEST_DIR/ongrid-edge-${target}"

    deps_sha=$(awk 'NR == 1 {print $1}' "$CACHE_DIR/$deps_name.sha256")
    edge_sha=$(awk 'NR == 1 {print $1}' "$CACHE_DIR/$edge_name.sha256")
    printf 'deps=%s/%s/%s sha256:%s\nedge=%s/%s/%s sha256:%s\n' \
        "$BASE_URL" "$DEPS_TAG" "$deps_name" "$deps_sha" \
        "$BASE_URL" "$VERSION" "$edge_name" "$edge_sha" \
        > "$DEST_DIR/edge-assets-${target}.ref"
    log "verified and staged $target"
done
