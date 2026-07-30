#!/usr/bin/env bash
# Shared data-directory ownership and recovery helpers for Compose upgrades.
#
# Normal upgrades must stay O(number of top-level directories): files already
# written by a running service have the correct numeric owner, and recursively
# walking a large Loki/Tempo tree turns that invariant check into downtime.

ongrid_stat_path_owner() {
    local path="$1" owner

    if owner=$(stat -c '%u:%g' "$path" 2>/dev/null) \
        && [[ "$owner" =~ ^[0-9]+:[0-9]+$ ]]; then
        printf '%s\n' "$owner"
        return 0
    fi
    if owner=$(stat -f '%u:%g' "$path" 2>/dev/null) \
        && [[ "$owner" =~ ^[0-9]+:[0-9]+$ ]]; then
        printf '%s\n' "$owner"
        return 0
    fi
    return 1
}

ongrid_stat_path_mode() {
    local path="$1" mode

    if mode=$(stat -c '%a' "$path" 2>/dev/null) \
        && [[ "$mode" =~ ^[0-7]+$ ]]; then
        printf '%s\n' "$mode"
        return 0
    fi
    if mode=$(stat -f '%Lp' "$path" 2>/dev/null) \
        && [[ "$mode" =~ ^[0-7]+$ ]]; then
        printf '%s\n' "$mode"
        return 0
    fi
    return 1
}

ongrid_ensure_path_owner() {
    local expected="$1" path="$2" actual=""

    if chown "$expected" "$path" 2>/dev/null; then
        return 0
    fi
    if actual=$(ongrid_stat_path_owner "$path") && [[ "$actual" == "$expected" ]]; then
        return 0
    fi

    printf '[ERROR] could not set owner %s on %s (current owner: %s)\n' \
        "$expected" "$path" "${actual:-unknown}" >&2
    return 1
}

ongrid_ensure_path_mode() {
    local expected="$1" path="$2" actual=""
    local normalized_expected="${expected#0}"

    if chmod "$expected" "$path" 2>/dev/null; then
        return 0
    fi
    if actual=$(ongrid_stat_path_mode "$path") && [[ "$actual" == "$normalized_expected" ]]; then
        return 0
    fi

    printf '[ERROR] could not set mode %s on %s (current mode: %s)\n' \
        "$expected" "$path" "${actual:-unknown}" >&2
    return 1
}

ongrid_normalize_boolean() {
    local value
    value=$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')

    case "$value" in
        ''|0|false|no|off) printf '0\n' ;;
        1|true|yes|on) printf '1\n' ;;
        *) return 1 ;;
    esac
}

ongrid_chown_tree_required() {
    local owner="$1" path="$2"
    if ! chown -R "$owner" "$path" 2>/dev/null; then
        printf '[ERROR] could not recursively set owner %s on %s\n' "$owner" "$path" >&2
        return 1
    fi
}

ongrid_prepare_data_directories() {
    local data_dir="$1" log_dir="$2"
    local failed=0

    if ! mkdir -p \
        "$data_dir/mysql" \
        "$data_dir/prometheus" \
        "$data_dir/loki" \
        "$data_dir/tempo" \
        "$data_dir/qdrant" \
        "$data_dir/grafana" \
        "$data_dir/embeddings" \
        "$data_dir/skills" \
        "$data_dir/pages" \
        "$data_dir/workspace" \
        "$data_dir/tools" \
        "$log_dir"; then
        printf '[ERROR] could not create one or more data directories under %s\n' \
            "$data_dir" >&2
        return 1
    fi

    # Only the mount-point directories need ownership initialization. Existing
    # descendants were created by these same container UIDs and must not be
    # traversed during every upgrade.
    ongrid_ensure_path_owner 999:999 "$data_dir/mysql" || failed=1
    ongrid_ensure_path_owner 65534:65534 "$data_dir/prometheus" || failed=1
    ongrid_ensure_path_owner 10001:10001 "$data_dir/loki" || failed=1
    ongrid_ensure_path_owner 10001:10001 "$data_dir/tempo" || failed=1
    ongrid_ensure_path_owner 472:472 "$data_dir/grafana" || failed=1
    ongrid_ensure_path_owner 65532:65532 "$data_dir/embeddings" || failed=1
    ongrid_ensure_path_owner 65532:65532 "$data_dir/skills" || failed=1
    ongrid_ensure_path_owner 65532:65532 "$data_dir/pages" || failed=1
    ongrid_ensure_path_owner 65532:65532 "$data_dir/workspace" || failed=1
    ongrid_ensure_path_owner 65532:65532 "$data_dir/tools" || failed=1

    ongrid_ensure_path_mode 0755 "$data_dir" || failed=1
    ongrid_ensure_path_mode 0755 "$data_dir/embeddings" || failed=1
    ongrid_ensure_path_mode 0755 "$log_dir" || failed=1

    return "$failed"
}

ongrid_repair_data_permissions() {
    local data_dir="$1"
    local failed=0

    # Explicit recovery path only. This can walk every inode and therefore must
    # never be called by a normal upgrade.
    ongrid_chown_tree_required 999:999 "$data_dir/mysql" || failed=1
    ongrid_chown_tree_required 65534:65534 "$data_dir/prometheus" || failed=1
    ongrid_chown_tree_required 10001:10001 "$data_dir/loki" || failed=1
    ongrid_chown_tree_required 10001:10001 "$data_dir/tempo" || failed=1
    ongrid_chown_tree_required 472:472 "$data_dir/grafana" || failed=1
    ongrid_chown_tree_required 65532:65532 "$data_dir/embeddings" || failed=1
    ongrid_chown_tree_required 65532:65532 "$data_dir/skills" || failed=1
    ongrid_chown_tree_required 65532:65532 "$data_dir/pages" || failed=1
    ongrid_chown_tree_required 65532:65532 "$data_dir/workspace" || failed=1
    ongrid_chown_tree_required 65532:65532 "$data_dir/tools" || failed=1
    if ! chmod -R 0755 "$data_dir/embeddings" 2>/dev/null; then
        printf '[ERROR] could not recursively set mode 0755 on %s\n' \
            "$data_dir/embeddings" >&2
        failed=1
    fi

    return "$failed"
}

ongrid_repair_data_permissions_if_enabled() {
    local enabled="$1" data_dir="$2"

    case "$enabled" in
        0) return 0 ;;
        1) ongrid_repair_data_permissions "$data_dir" ;;
        *)
            printf '[ERROR] invalid normalized permission-repair flag: %s\n' "$enabled" >&2
            return 2
            ;;
    esac
}

ongrid_restore_existing_stack() {
    local install_dir="$1"

    printf '[WARN] attempting to restore the existing stack\n' >&2
    if (
        cd "$install_dir" && docker compose --env-file .env up -d
    ); then
        printf '[INFO] existing stack restore requested successfully\n'
        return 0
    fi

    printf '[ERROR] failed to restore the existing stack; inspect docker compose status\n' >&2
    return 1
}

ongrid_prepare_data_directories_or_restore() {
    local data_dir="$1" log_dir="$2" install_dir="$3"

    if ongrid_prepare_data_directories "$data_dir" "$log_dir"; then
        return 0
    fi

    printf '[ERROR] data directory preparation failed after stopping the stack\n' >&2
    if ! ongrid_restore_existing_stack "$install_dir"; then
        printf '[ERROR] automatic recovery failed\n' >&2
    fi
    return 1
}

ongrid_repair_data_permissions_or_restore() {
    local enabled="$1" data_dir="$2" install_dir="$3"

    if ongrid_repair_data_permissions_if_enabled "$enabled" "$data_dir"; then
        return 0
    fi

    printf '[ERROR] permission repair failed; the new release was not installed\n' >&2
    if ! ongrid_restore_existing_stack "$install_dir"; then
        printf '[ERROR] automatic recovery failed\n' >&2
    fi
    return 1
}
