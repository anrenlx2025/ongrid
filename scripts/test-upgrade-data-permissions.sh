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
chown_should_fail=0
chmod_should_fail=0
chown() {
    printf 'chown' >>"$command_log"
    printf ' %s' "$@" >>"$command_log"
    printf '\n' >>"$command_log"
    [[ "$chown_should_fail" == 0 ]]
}
chmod() {
    printf 'chmod' >>"$command_log"
    printf ' %s' "$@" >>"$command_log"
    printf '\n' >>"$command_log"
    [[ "$chmod_should_fail" == 0 ]]
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
ongrid_repair_data_permissions_if_enabled 0 "$data_dir"
[[ ! -s "$command_log" ]] \
    || fail "disabled permission repair still traversed a data directory"

: >"$command_log"
ongrid_repair_data_permissions_if_enabled 1 "$data_dir"
grep -Fqx "chown -R 10001:10001 $data_dir/loki" "$command_log" \
    || fail "explicit repair did not recursively repair Loki"
grep -Fqx "chown -R 65532:65532 $data_dir/skills" "$command_log" \
    || fail "explicit repair did not recursively repair skills"

[[ "$(ongrid_normalize_boolean '')" == 0 ]] \
    || fail "empty boolean did not disable permission repair"
[[ "$(ongrid_normalize_boolean 0)" == 0 ]] \
    || fail "boolean 0 did not disable permission repair"
[[ "$(ongrid_normalize_boolean false)" == 0 ]] \
    || fail "boolean false did not disable permission repair"
[[ "$(ongrid_normalize_boolean OFF)" == 0 ]] \
    || fail "boolean OFF did not disable permission repair"
[[ "$(ongrid_normalize_boolean 1)" == 1 ]] \
    || fail "boolean 1 did not enable permission repair"
[[ "$(ongrid_normalize_boolean TRUE)" == 1 ]] \
    || fail "boolean TRUE did not enable permission repair"
if ongrid_normalize_boolean invalid >/dev/null; then
    fail "invalid boolean value was accepted"
fi

chown_should_fail=1
if ongrid_repair_data_permissions_if_enabled 1 "$data_dir" 2>"$tmp_dir/chown-error.log"; then
    fail "explicit repair ignored recursive chown failures"
fi
grep -Fq '[ERROR] could not recursively set owner' "$tmp_dir/chown-error.log" \
    || fail "recursive chown failure did not produce an error"
chown_should_fail=0

chmod_should_fail=1
if ongrid_repair_data_permissions_if_enabled 1 "$data_dir" 2>"$tmp_dir/chmod-error.log"; then
    fail "explicit repair ignored recursive chmod failures"
fi
grep -Fq '[ERROR] could not recursively set mode 0755' "$tmp_dir/chmod-error.log" \
    || fail "recursive chmod failure did not produce an error"
chmod_should_fail=0

grep -Fq -- '--repair-permissions' "$upgrade_script" \
    || fail "upgrade.sh does not expose the explicit repair flag"
grep -Fq 'REPAIR_PERMISSIONS=$(ongrid_normalize_boolean "$REPAIR_PERMISSIONS_RAW")' "$upgrade_script" \
    || fail "upgrade.sh does not normalize the permission-repair setting"
if grep -Fq '[[ -n "$REPAIR_PERMISSIONS" ]]' "$upgrade_script"; then
    fail "upgrade.sh still treats every non-empty permission-repair value as enabled"
fi
grep -Fq 'ongrid_prepare_data_directories "$ONGRID_DATA_DIR" "$ONGRID_LOG_DIR"' "$upgrade_script" \
    || fail "upgrade.sh does not use non-recursive directory preparation"
grep -Fq 'ongrid_repair_data_permissions_if_enabled "$REPAIR_PERMISSIONS" "$ONGRID_DATA_DIR"' "$upgrade_script" \
    || fail "upgrade.sh does not use the tested permission-repair gate"
grep -Fq 'restore_existing_stack' "$upgrade_script" \
    || fail "upgrade.sh does not expose a recovery path after repair failure"
for persistent_dir in mysql prometheus loki tempo grafana skills pages workspace tools; do
    if grep -Eq "chown -R .*ONGRID_DATA_DIR/${persistent_dir}" "$upgrade_script"; then
        fail "upgrade.sh directly recurses through $persistent_dir outside the repair helper"
    fi
done

printf 'upgrade data-permissions tests passed\n'
