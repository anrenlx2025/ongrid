#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
permissions_lib="$repo_root/deploy/install/data-permissions.sh"
upgrade_script="$repo_root/deploy/install/upgrade.sh"
tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT

fail() {
    printf 'upgrade data-permissions test failed: %s\n' "$*" >&2
    exit 1
}

[[ -f "$permissions_lib" ]] || fail "missing data-permissions.sh"

# shellcheck source=../deploy/install/data-permissions.sh
source "$permissions_lib"

command_log="$tmp_dir/commands.log"
chown() {
    printf 'chown' >>"$command_log"
    printf ' %s' "$@" >>"$command_log"
    printf '\n' >>"$command_log"
}
chmod() {
    printf 'chmod' >>"$command_log"
    printf ' %s' "$@" >>"$command_log"
    printf '\n' >>"$command_log"
}

data_dir="$tmp_dir/data"
log_dir="$tmp_dir/log"
mkdir -p "$data_dir/loki/chunks"
printf 'existing data\n' >"$data_dir/loki/chunks/000001"

ongrid_prepare_data_directories "$data_dir" "$log_dir"

[[ -f "$data_dir/loki/chunks/000001" ]] || fail "normal preparation touched existing Loki data"
grep -Fqx "chown 10001:10001 $data_dir/loki" "$command_log" \
    || fail "normal preparation did not set the Loki root directory owner"
grep -Fqx "chown 65532:65532 $data_dir/skills" "$command_log" \
    || fail "normal preparation did not set the skills root directory owner"
if grep -Eq '^(chown|chmod) -R ' "$command_log"; then
    fail "normal preparation recursively traversed a data directory"
fi

: >"$command_log"
ongrid_repair_data_permissions "$data_dir"
grep -Fqx "chown -R 10001:10001 $data_dir/loki" "$command_log" \
    || fail "explicit repair did not recursively repair Loki"
grep -Fqx "chown -R 65532:65532 $data_dir/skills" "$command_log" \
    || fail "explicit repair did not recursively repair skills"

grep -Fq -- '--repair-permissions' "$upgrade_script" \
    || fail "upgrade.sh does not expose the explicit repair flag"
grep -Fq 'ongrid_prepare_data_directories "$ONGRID_DATA_DIR" "$ONGRID_LOG_DIR"' "$upgrade_script" \
    || fail "upgrade.sh does not use non-recursive directory preparation"
grep -Fq 'ongrid_repair_data_permissions "$ONGRID_DATA_DIR"' "$upgrade_script" \
    || fail "upgrade.sh does not gate recursive repair behind the explicit flag"
for persistent_dir in mysql prometheus loki tempo grafana skills pages workspace tools; do
    if grep -Eq "chown -R .*ONGRID_DATA_DIR/${persistent_dir}" "$upgrade_script"; then
        fail "upgrade.sh directly recurses through $persistent_dir outside the repair helper"
    fi
done

printf 'upgrade data-permissions tests passed\n'
