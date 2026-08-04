#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
build_script="$repo_root/dist/build-edge-attachments.sh"
fetch_script="$repo_root/deploy/install/edge/fetch-edge-assets.sh"
tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT

fail() {
    printf 'edge assets test failed: %s\n' "$*" >&2
    exit 1
}

components=(
    node_exporter process_exporter mysqld_exporter postgres_exporter
    redis_exporter mongodb_exporter promtail otelcol-contrib
)
bin_root="$tmp_dir/bin"
mkdir -p "$bin_root/linux-amd64"
for component in "${components[@]}"; do
    printf '%s payload\n' "$component" > "$bin_root/linux-amd64/$component"
done
printf 'ongrid-edge payload\n' > "$bin_root/linux-amd64/ongrid-edge"

attachments="$tmp_dir/attachments"
EDGE_BIN_ROOT="$bin_root" \
PROMTAIL_VERSION=1 OTELCOL_VERSION=2 NODE_EXPORTER_VERSION=3 \
PROCESS_EXPORTER_VERSION=4 MYSQLD_EXPORTER_VERSION=5 \
POSTGRES_EXPORTER_VERSION=6 REDIS_EXPORTER_VERSION=7 \
MONGODB_EXPORTER_VERSION=8 \
    bash "$build_script" deps edge-deps-test "$attachments" linux-amd64
EDGE_BIN_ROOT="$bin_root" \
    bash "$build_script" edge vtest "$attachments" linux-amd64

# Immutable dependency archives must be byte-reproducible. Otherwise a rerun
# rebuilds a different sidecar and cannot reuse an already complete Release.
rebuilt_attachments="$tmp_dir/attachments-rebuilt"
sleep 2
EDGE_BIN_ROOT="$bin_root" \
PROMTAIL_VERSION=1 OTELCOL_VERSION=2 NODE_EXPORTER_VERSION=3 \
PROCESS_EXPORTER_VERSION=4 MYSQLD_EXPORTER_VERSION=5 \
POSTGRES_EXPORTER_VERSION=6 REDIS_EXPORTER_VERSION=7 \
MONGODB_EXPORTER_VERSION=8 \
    bash "$build_script" deps edge-deps-test "$rebuilt_attachments" linux-amd64
cmp -s \
    "$attachments/edge-deps-linux-amd64.tar.xz" \
    "$rebuilt_attachments/edge-deps-linux-amd64.tar.xz" \
    || fail "identical dependency inputs produced different immutable archives"

(cd "$attachments" && sha256sum -c edge-deps-linux-amd64.tar.xz.sha256)
(cd "$attachments" && sha256sum -c ongrid-edge-linux-amd64-vtest.sha256)

fixture_root="$tmp_dir/releases"
mkdir -p "$fixture_root/edge-deps-test" "$fixture_root/vtest"
cp "$attachments/edge-deps-linux-amd64.tar.xz"* "$fixture_root/edge-deps-test/"
cp "$attachments/ongrid-edge-linux-amd64-vtest"* "$fixture_root/vtest/"

fake_bin="$tmp_dir/fake-bin"
mkdir -p "$fake_bin"
cat > "$fake_bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
out=""
url=""
while (( $# > 0 )); do
    case "$1" in
        -o) out=$2; shift 2 ;;
        http://*|https://*) url=$1; shift ;;
        *) shift ;;
    esac
done
[[ -n "$out" && -n "$url" ]]
printf '%s\n' "$url" >> "$FAKE_CURL_LOG"
relative=${url#*releases/download/}
cp "$FAKE_RELEASE_ROOT/$relative" "$out"
EOF
chmod 0755 "$fake_bin/curl"

dest="$tmp_dir/dest"
cache="$tmp_dir/cache"
FAKE_CURL_LOG="$tmp_dir/curl.log" \
FAKE_RELEASE_ROOT="$fixture_root" \
PATH="$fake_bin:$PATH" \
ONGRID_EDGE_DEPS_TAG=edge-deps-test \
ONGRID_EDGE_ARTIFACT_CACHE_DIR="$cache" \
ONGRID_EDGE_ARTIFACT_BASE_URL=https://cnb.test/repo/-/releases/download \
    bash "$fetch_script" "$dest" vtest linux-amd64

grep -Fqx 'https://cnb.test/repo/-/releases/download/edge-deps-test/edge-deps-linux-amd64.tar.xz' "$tmp_dir/curl.log" \
    || fail "public dependency archive was not downloaded from its immutable release"
grep -Fqx 'https://cnb.test/repo/-/releases/download/vtest/ongrid-edge-linux-amd64-vtest' "$tmp_dir/curl.log" \
    || fail "versioned ongrid-edge binary was not downloaded directly"
for component in "${components[@]}"; do
    [[ -x "$dest/${component}-linux-amd64" ]] || fail "missing staged $component"
done
[[ -x "$dest/ongrid-edge-linux-amd64" ]] || fail "missing staged ongrid-edge"
grep -Fq 'edge-deps-test/edge-deps-linux-amd64.tar.xz' "$dest/edge-assets-linux-amd64.ref" \
    || fail "dependency source was not recorded"
grep -Fq 'vtest/ongrid-edge-linux-amd64-vtest' "$dest/edge-assets-linux-amd64.ref" \
    || fail "edge source was not recorded"

# Dependency archives keep stable filenames across release tags. Reusing one
# cache directory after a dependency version bump must fetch and stage the new
# tag instead of silently accepting the old archive and recording false source
# provenance.
for component in "${components[@]}"; do
    printf '%s next payload\n' "$component" > "$bin_root/linux-amd64/$component"
done
next_attachments="$tmp_dir/attachments-next"
EDGE_BIN_ROOT="$bin_root" \
PROMTAIL_VERSION=11 OTELCOL_VERSION=12 NODE_EXPORTER_VERSION=13 \
PROCESS_EXPORTER_VERSION=14 MYSQLD_EXPORTER_VERSION=15 \
POSTGRES_EXPORTER_VERSION=16 REDIS_EXPORTER_VERSION=17 \
MONGODB_EXPORTER_VERSION=18 \
    bash "$build_script" deps edge-deps-next "$next_attachments" linux-amd64
mkdir -p "$fixture_root/edge-deps-next"
cp "$next_attachments/edge-deps-linux-amd64.tar.xz"* "$fixture_root/edge-deps-next/"
: > "$tmp_dir/next-curl.log"
FAKE_CURL_LOG="$tmp_dir/next-curl.log" \
FAKE_RELEASE_ROOT="$fixture_root" \
PATH="$fake_bin:$PATH" \
ONGRID_EDGE_DEPS_TAG=edge-deps-next \
ONGRID_EDGE_ARTIFACT_CACHE_DIR="$cache" \
ONGRID_EDGE_ARTIFACT_BASE_URL=https://cnb.test/repo/-/releases/download \
    bash "$fetch_script" "$tmp_dir/next-dest" vtest linux-amd64
grep -Fqx 'https://cnb.test/repo/-/releases/download/edge-deps-next/edge-deps-linux-amd64.tar.xz' "$tmp_dir/next-curl.log" \
    || fail "a new dependency tag reused the previous tag's cached archive"
if grep -Fq '/vtest/ongrid-edge-linux-amd64-vtest' "$tmp_dir/next-curl.log"; then
    fail "an unchanged versioned Edge binary was not reused from its tag-scoped cache"
fi
for component in "${components[@]}"; do
    cmp -s "$bin_root/linux-amd64/$component" "$tmp_dir/next-dest/${component}-linux-amd64" \
        || fail "dependency tag change did not stage the new $component payload"
done
grep -Fq 'edge-deps-next/edge-deps-linux-amd64.tar.xz' "$tmp_dir/next-dest/edge-assets-linux-amd64.ref" \
    || fail "dependency tag change recorded the wrong source"

# A valid local cache must make a repeated installation independent of CNB.
rm -rf "$fixture_root"
: > "$tmp_dir/cache-curl.log"
FAKE_CURL_LOG="$tmp_dir/cache-curl.log" \
FAKE_RELEASE_ROOT="$fixture_root" \
PATH="$fake_bin:$PATH" \
ONGRID_EDGE_DEPS_TAG=edge-deps-test \
ONGRID_EDGE_ARTIFACT_CACHE_DIR="$cache" \
ONGRID_EDGE_ARTIFACT_BASE_URL=https://cnb.test/repo/-/releases/download \
    bash "$fetch_script" "$tmp_dir/cache-dest" vtest linux-amd64
[[ ! -s "$tmp_dir/cache-curl.log" ]] || fail "verified cache unexpectedly hit the network"

# The archive's checksum-protected dependency metadata must agree with the
# release tag requested by the installer. A valid archive copied under a
# different tag is not an interchangeable dependency release.
mkdir -p "$fixture_root/edge-deps-forged" "$fixture_root/vtest"
cp "$next_attachments/edge-deps-linux-amd64.tar.xz"* "$fixture_root/edge-deps-forged/"
cp "$attachments/ongrid-edge-linux-amd64-vtest"* "$fixture_root/vtest/"
if FAKE_CURL_LOG="$tmp_dir/forged-tag-curl.log" \
    FAKE_RELEASE_ROOT="$fixture_root" \
    PATH="$fake_bin:$PATH" \
    ONGRID_EDGE_DEPS_TAG=edge-deps-forged \
    ONGRID_EDGE_ARTIFACT_CACHE_DIR="$tmp_dir/forged-tag-cache" \
    ONGRID_EDGE_ARTIFACT_BASE_URL=https://cnb.test/repo/-/releases/download \
    bash "$fetch_script" "$tmp_dir/forged-tag-dest" vtest linux-amd64 \
        >"$tmp_dir/forged-tag.out" 2>"$tmp_dir/forged-tag.log"; then
    fail "dependency archive metadata from another release tag was accepted"
fi
grep -Fq 'dependency archive release tag does not match edge-deps-forged' "$tmp_dir/forged-tag.log" \
    || fail "dependency tag mismatch did not produce an actionable error"

# Required archive entries must be rejected as non-regular before manifest
# hashing. This guard wrapper records if sha256sum is ever asked to follow the
# injected symlink; the installer must fail without touching it.
real_sha256sum=$(command -v sha256sum)
nonregular_stage="$tmp_dir/nonregular-stage"
nonregular_release="$fixture_root/edge-deps-nonregular"
mkdir -p "$nonregular_stage" "$nonregular_release"
tar -xJf "$attachments/edge-deps-linux-amd64.tar.xz" -C "$nonregular_stage"
rm -f "$nonregular_stage/node_exporter"
ln -s process_exporter "$nonregular_stage/node_exporter"
awk -F= '
    $1 == "release_tag" { print "release_tag=edge-deps-nonregular"; next }
    { print }
' "$nonregular_stage/DEPENDENCIES" > "$nonregular_stage/DEPENDENCIES.new"
mv "$nonregular_stage/DEPENDENCIES.new" "$nonregular_stage/DEPENDENCIES"
(cd "$nonregular_stage" && "$real_sha256sum" TARGET DEPENDENCIES "${components[@]}" > MANIFEST.sha256)
tar -cJf "$nonregular_release/edge-deps-linux-amd64.tar.xz" \
    -C "$nonregular_stage" TARGET DEPENDENCIES MANIFEST.sha256 "${components[@]}"
(cd "$nonregular_release" && "$real_sha256sum" edge-deps-linux-amd64.tar.xz > edge-deps-linux-amd64.tar.xz.sha256)

guard_bin="$tmp_dir/guard-bin"
mkdir -p "$guard_bin"
cp "$fake_bin/curl" "$guard_bin/curl"
cat > "$guard_bin/sha256sum" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
for path in "$@"; do
    if [[ -L "$path" ]]; then
        printf '%s\n' "$path" >> "$FAKE_SHA_SYMLINK_LOG"
        exit 97
    fi
done
exec "$REAL_SHA256SUM" "$@"
EOF
chmod 0755 "$guard_bin/curl" "$guard_bin/sha256sum"
: > "$tmp_dir/sha-symlink.log"
if FAKE_CURL_LOG="$tmp_dir/nonregular-curl.log" \
    FAKE_RELEASE_ROOT="$fixture_root" \
    FAKE_SHA_SYMLINK_LOG="$tmp_dir/sha-symlink.log" \
    REAL_SHA256SUM="$real_sha256sum" \
    PATH="$guard_bin:$PATH" \
    ONGRID_EDGE_DEPS_TAG=edge-deps-nonregular \
    ONGRID_EDGE_ARTIFACT_CACHE_DIR="$tmp_dir/nonregular-cache" \
    ONGRID_EDGE_ARTIFACT_BASE_URL=https://cnb.test/repo/-/releases/download \
    bash "$fetch_script" "$tmp_dir/nonregular-dest" vtest linux-amd64 \
        >"$tmp_dir/nonregular.out" 2>"$tmp_dir/nonregular.log"; then
    fail "dependency archive with a symlink component was accepted"
fi
[[ ! -s "$tmp_dir/sha-symlink.log" ]] \
    || fail "manifest verification hashed a non-regular archive entry before rejecting it"

# A mismatched direct binary and sidecar must fail before staging anything.
mkdir -p "$fixture_root/edge-deps-test" "$fixture_root/vtest"
cp "$attachments/edge-deps-linux-amd64.tar.xz"* "$fixture_root/edge-deps-test/"
cp "$attachments/ongrid-edge-linux-amd64-vtest"* "$fixture_root/vtest/"
printf 'tampered\n' >> "$fixture_root/vtest/ongrid-edge-linux-amd64-vtest"
if FAKE_CURL_LOG="$tmp_dir/bad-curl.log" \
    FAKE_RELEASE_ROOT="$fixture_root" \
    PATH="$fake_bin:$PATH" \
    ONGRID_EDGE_DEPS_TAG=edge-deps-test \
    ONGRID_EDGE_ARTIFACT_CACHE_DIR="$tmp_dir/bad-cache" \
    ONGRID_EDGE_ARTIFACT_BASE_URL=https://cnb.test/repo/-/releases/download \
    bash "$fetch_script" "$tmp_dir/bad-dest" vtest linux-amd64 >/dev/null 2>&1; then
    fail "tampered direct binary passed checksum verification"
fi

# The sidecar must describe the file currently being verified. Pointing the
# Edge sidecar at the already downloaded dependency archive must not allow a
# modified Edge binary to pass.
cp "$attachments/edge-deps-linux-amd64.tar.xz.sha256" \
    "$fixture_root/vtest/ongrid-edge-linux-amd64-vtest.sha256"
if FAKE_CURL_LOG="$tmp_dir/wrong-name-curl.log" \
    FAKE_RELEASE_ROOT="$fixture_root" \
    PATH="$fake_bin:$PATH" \
    ONGRID_EDGE_DEPS_TAG=edge-deps-test \
    ONGRID_EDGE_ARTIFACT_CACHE_DIR="$tmp_dir/wrong-name-cache" \
    ONGRID_EDGE_ARTIFACT_BASE_URL=https://cnb.test/repo/-/releases/download \
    bash "$fetch_script" "$tmp_dir/wrong-name-dest" vtest linux-amd64 >/dev/null 2>&1; then
    fail "checksum sidecar for another attachment verified a tampered Edge binary"
fi
[[ ! -e "$tmp_dir/wrong-name-dest/ongrid-edge-linux-amd64" ]] \
    || fail "tampered Edge binary was staged after sidecar filename mismatch"

printf 'edge attachment tests passed\n'
