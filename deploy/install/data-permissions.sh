#!/usr/bin/env bash
# Shared data-directory ownership helpers for the Compose installer.
#
# Normal upgrades must stay O(number of top-level directories): files already
# written by a running service have the correct numeric owner, and recursively
# walking a large Loki/Tempo tree turns that invariant check into downtime.

ongrid_chown_path_best_effort() {
    local owner="$1" path="$2"
    if ! chown "$owner" "$path" 2>/dev/null; then
        printf '[WARN] could not set owner %s on %s; verify storage permissions before startup\n' \
            "$owner" "$path" >&2
    fi
}

ongrid_chown_tree_best_effort() {
    local owner="$1" path="$2"
    if ! chown -R "$owner" "$path" 2>/dev/null; then
        printf '[WARN] could not recursively set owner %s on %s\n' "$owner" "$path" >&2
    fi
}

ongrid_prepare_data_directories() {
    local data_dir="$1" log_dir="$2"

    mkdir -p \
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
        "$log_dir"

    # Only the mount-point directories need ownership initialization. Existing
    # descendants were created by these same container UIDs and must not be
    # traversed during every upgrade.
    ongrid_chown_path_best_effort 999:999 "$data_dir/mysql"
    ongrid_chown_path_best_effort 65534:65534 "$data_dir/prometheus"
    ongrid_chown_path_best_effort 10001:10001 "$data_dir/loki"
    ongrid_chown_path_best_effort 10001:10001 "$data_dir/tempo"
    ongrid_chown_path_best_effort 472:472 "$data_dir/grafana"
    ongrid_chown_path_best_effort 65532:65532 "$data_dir/embeddings"
    ongrid_chown_path_best_effort 65532:65532 "$data_dir/skills"
    ongrid_chown_path_best_effort 65532:65532 "$data_dir/pages"
    ongrid_chown_path_best_effort 65532:65532 "$data_dir/workspace"
    ongrid_chown_path_best_effort 65532:65532 "$data_dir/tools"

    chmod 0755 "$data_dir" "$data_dir/embeddings" "$log_dir" 2>/dev/null || true
}

ongrid_repair_data_permissions() {
    local data_dir="$1"

    # Explicit recovery path only. This can walk every inode and therefore must
    # never be called by a normal upgrade.
    ongrid_chown_tree_best_effort 999:999 "$data_dir/mysql"
    ongrid_chown_tree_best_effort 65534:65534 "$data_dir/prometheus"
    ongrid_chown_tree_best_effort 10001:10001 "$data_dir/loki"
    ongrid_chown_tree_best_effort 10001:10001 "$data_dir/tempo"
    ongrid_chown_tree_best_effort 472:472 "$data_dir/grafana"
    ongrid_chown_tree_best_effort 65532:65532 "$data_dir/embeddings"
    ongrid_chown_tree_best_effort 65532:65532 "$data_dir/skills"
    ongrid_chown_tree_best_effort 65532:65532 "$data_dir/pages"
    ongrid_chown_tree_best_effort 65532:65532 "$data_dir/workspace"
    ongrid_chown_tree_best_effort 65532:65532 "$data_dir/tools"
    chmod -R 0755 "$data_dir/embeddings" 2>/dev/null || true
}
