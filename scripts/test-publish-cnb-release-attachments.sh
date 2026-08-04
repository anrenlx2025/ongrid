#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
publisher="$repo_root/scripts/publish-cnb-release-attachments.sh"
tmp_dir=$(mktemp -d "$repo_root/.tmp-test-cnb-attachments.XXXXXX")
trap 'rm -rf "$tmp_dir"' EXIT

mkdir -p "$tmp_dir/bin" "$tmp_dir/files"
printf 'one\n' > "$tmp_dir/files/one"
printf 'two\n' > "$tmp_dir/files/two"

cat > "$tmp_dir/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
url=${!#}
name=${url##*/}
printf '%s\n' "$url" >> "$FAKE_CURL_LOG"
if [[ -f "$FAKE_UPLOAD_STATE" || ",${FAKE_PRESENT:-}," == *",$name,"* ]]; then
    exit 0
fi
exit 22
EOF
cat > "$tmp_dir/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FAKE_DOCKER_LOG"
: > "$FAKE_UPLOAD_STATE"
EOF
chmod 0755 "$tmp_dir/bin/curl" "$tmp_dir/bin/docker"

run_publisher() {
    FAKE_CURL_LOG="$tmp_dir/curl.log" \
    FAKE_DOCKER_LOG="$tmp_dir/docker.log" \
    FAKE_UPLOAD_STATE="$tmp_dir/uploaded" \
    PATH="$tmp_dir/bin:$PATH" \
    CNB_TOKEN=test-token \
        bash "$publisher" vtest ongridio/ongrid-edge \
        https://cnb.test/ongridio/ongrid-edge/-/releases/download \
        cnbcool/attachments:latest "$tmp_dir/files/one" "$tmp_dir/files/two"
}

# A complete immutable release is reused without invoking the uploader.
: > "$tmp_dir/curl.log"
: > "$tmp_dir/docker.log"
FAKE_PRESENT=one,two run_publisher
[[ ! -s "$tmp_dir/docker.log" ]] || { echo "complete release was uploaded again" >&2; exit 1; }

# A partial immutable release must fail closed instead of overwriting it.
: > "$tmp_dir/curl.log"
: > "$tmp_dir/docker.log"
if FAKE_PRESENT=one run_publisher >/dev/null 2>&1; then
    echo "partial release was accepted" >&2
    exit 1
fi
[[ ! -s "$tmp_dir/docker.log" ]] || { echo "partial release invoked uploader" >&2; exit 1; }

# An empty release uploads once and verifies both resulting direct URLs.
: > "$tmp_dir/curl.log"
: > "$tmp_dir/docker.log"
rm -f "$tmp_dir/uploaded"
FAKE_PRESENT= run_publisher
grep -Fq 'PLUGIN_TAG=vtest' "$tmp_dir/docker.log"
grep -Fq 'cnbcool/attachments:latest' "$tmp_dir/docker.log"
[[ $(wc -l < "$tmp_dir/curl.log" | tr -d ' ') == 4 ]]

echo "CNB attachment publisher tests passed"
