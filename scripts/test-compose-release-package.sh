#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT

install_help=$(bash "$repo_root/deploy/install/install.sh" --help)
uninstall_help=$(bash "$repo_root/deploy/install/uninstall.sh" --help)

if grep -Eqi -- 'systemd|--mode|--with-deps' <<<"$install_help"; then
  echo "install help still advertises removed Manager systemd support" >&2
  exit 1
fi
if grep -Eqi -- 'systemd|--mode' <<<"$uninstall_help"; then
  echo "uninstall help still advertises removed Manager systemd support" >&2
  exit 1
fi
if bash "$repo_root/deploy/install/install.sh" --mode=systemd >"$tmp_dir/install-mode.log" 2>&1; then
  echo "install unexpectedly accepted --mode=systemd" >&2
  exit 1
fi
if bash "$repo_root/deploy/install/uninstall.sh" --mode=systemd >"$tmp_dir/uninstall-mode.log" 2>&1; then
  echo "uninstall unexpectedly accepted --mode=systemd" >&2
  exit 1
fi
if [[ -d "$repo_root/deploy/install/systemd" ]] \
    && find "$repo_root/deploy/install/systemd" -type f -print -quit | grep -q .; then
  echo "Manager systemd install files still exist" >&2
  exit 1
fi

stage="$tmp_dir/stage/ongrid-vtest-linux"
out="$tmp_dir/out"
edge_bin_root="$tmp_dir/edge-bin"
mkdir -p "$edge_bin_root/linux-amd64"
for binary in \
  ongrid-edge promtail otelcol-contrib node_exporter process_exporter \
  mysqld_exporter postgres_exporter redis_exporter mongodb_exporter; do
  printf '%s test payload\n' "$binary" >"$edge_bin_root/linux-amd64/$binary"
done
PACKAGE_TARGET=linux \
EDGE_TARGETS=linux-amd64 \
EDGE_BIN_ROOT="$edge_bin_root" \
ONGRID_EDGE_DEPS_TAG=edge-deps-test \
ONGRID_BUNDLE_EMBEDDING_MODEL=0 \
  bash "$repo_root/dist/package.sh" vtest "$stage" "$out" \
    >"$tmp_dir/package.log" 2>&1 || {
      cat "$tmp_dir/package.log" >&2
      exit 1
    }

archive="$out/ongrid-vtest-linux.tar.xz"
test -s "$archive"
tar -tf "$archive" >"$tmp_dir/archive.list"

for required in \
  ongrid-vtest-linux/install.sh \
  ongrid-vtest-linux/uninstall.sh \
  ongrid-vtest-linux/upgrade.sh \
  ongrid-vtest-linux/public-url.sh \
  ongrid-vtest-linux/data-permissions.sh \
  ongrid-vtest-linux/docker-compose.yml \
  ongrid-vtest-linux/prometheus.yml \
  ongrid-vtest-linux/edge/fetch-edge-assets.sh \
  ongrid-vtest-linux/edge/verify-edge-deps-archive.sh \
  ongrid-vtest-linux/edge/edge-assets-lib.sh \
  ongrid-vtest-linux/edge/build-edge-bundle.sh \
  ongrid-vtest-linux/edge/edge-artifacts.env; do
  grep -Fxq "$required" "$tmp_dir/archive.list"
done

mkdir -p "$tmp_dir/extracted"
tar -xf "$archive" -C "$tmp_dir/extracted"
grep -Fxq 'ONGRID_EDGE_DEPS_TAG=edge-deps-test' \
  "$tmp_dir/extracted/ongrid-vtest-linux/edge/edge-artifacts.env"
if grep -q '^ONGRID_EDGE_TARGETS=' \
  "$tmp_dir/extracted/ongrid-vtest-linux/edge/edge-artifacts.env"; then
  echo "thin universal package pins an Edge architecture" >&2
  exit 1
fi
bash "$tmp_dir/extracted/ongrid-vtest-linux/install.sh" --help >/dev/null
bash "$tmp_dir/extracted/ongrid-vtest-linux/upgrade.sh" --help >/dev/null

for forbidden in \
  ongrid-vtest-linux/bin/ \
  ongrid-vtest-linux/systemd/ \
  ongrid-vtest-linux/prometheus/prometheus.yml \
  ongrid-vtest-linux/edge/ongrid-edge-linux-amd64 \
  ongrid-vtest-linux/edge/promtail-linux-amd64 \
  ongrid-vtest-linux/edge/otelcol-contrib-linux-amd64 \
  ongrid-vtest-linux/edge/node_exporter-linux-amd64 \
  ongrid-vtest-linux/edge/process_exporter-linux-amd64 \
  ongrid-vtest-linux/edge/mysqld_exporter-linux-amd64 \
  ongrid-vtest-linux/edge/postgres_exporter-linux-amd64 \
  ongrid-vtest-linux/edge/redis_exporter-linux-amd64 \
  ongrid-vtest-linux/edge/mongodb_exporter-linux-amd64; do
  if grep -Fq "$forbidden" "$tmp_dir/archive.list"; then
    echo "release package contains removed path: $forbidden" >&2
    exit 1
  fi
done

# Opting into an offline package is a completeness promise. Missing binaries
# must fail the package build instead of producing an archive that cannot be
# installed.
mkdir -p "$tmp_dir/empty-edge-bin/linux-amd64"
if PACKAGE_TARGET=linux \
  EDGE_TARGETS=linux-amd64 \
  EDGE_BIN_ROOT="$tmp_dir/empty-edge-bin" \
  ONGRID_BUNDLE_EDGE_ASSETS=1 \
  ONGRID_BUNDLE_EMBEDDING_MODEL=0 \
    bash "$repo_root/dist/package.sh" vtest \
      "$tmp_dir/offline-stage/ongrid-vtest-linux" "$tmp_dir/offline-out" \
      >"$tmp_dir/offline-package.log" 2>&1; then
  echo "offline package unexpectedly accepted an empty Edge binary root" >&2
  exit 1
fi

offline_stage="$tmp_dir/offline-complete-stage/ongrid-vtest-linux"
offline_out="$tmp_dir/offline-complete-out"
PACKAGE_TARGET=linux \
EDGE_TARGETS=linux-amd64 \
EDGE_BIN_ROOT="$edge_bin_root" \
ONGRID_BUNDLE_EDGE_ASSETS=1 \
ONGRID_BUNDLE_EMBEDDING_MODEL=0 \
  bash "$repo_root/dist/package.sh" vtest "$offline_stage" "$offline_out" \
    >"$tmp_dir/offline-complete.log" 2>&1 || {
      cat "$tmp_dir/offline-complete.log" >&2
      exit 1
    }
offline_archive="$offline_out/ongrid-vtest-linux.tar.xz"
tar -tf "$offline_archive" > "$tmp_dir/offline-archive.list"
mkdir -p "$tmp_dir/offline-extracted"
tar -xf "$offline_archive" -C "$tmp_dir/offline-extracted"
grep -Fxq 'ONGRID_EDGE_TARGETS=linux-amd64' \
  "$tmp_dir/offline-extracted/ongrid-vtest-linux/edge/edge-artifacts.env"
for binary in \
  ongrid-edge promtail otelcol-contrib node_exporter process_exporter \
  mysqld_exporter postgres_exporter redis_exporter mongodb_exporter; do
  grep -Fxq "ongrid-vtest-linux/edge/${binary}-linux-amd64" \
    "$tmp_dir/offline-archive.list" \
    || { echo "complete offline package omitted $binary" >&2; exit 1; }
done
