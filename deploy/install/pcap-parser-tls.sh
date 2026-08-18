#!/usr/bin/env bash
# Private PKI bootstrap for the pcap-parser Compose service. This file is
# sourced by install.sh and upgrade.sh; it intentionally never prints keys.

ongrid_prepare_pcap_parser_tls() (
    local data_dir="$1" root manager_dir parser_dir nginx_dir tmp_dir missing=0
    root="${data_dir}/pcap-parser"
    manager_dir="${root}/manager"
    parser_dir="${root}/parser"
    nginx_dir="${root}/nginx"

    for file in \
        "${manager_dir}/request.key" "${manager_dir}/client.crt" "${manager_dir}/client.key" "${manager_dir}/ca.crt" \
        "${parser_dir}/request.pub" "${parser_dir}/server.crt" "${parser_dir}/server.key" "${parser_dir}/ca.crt" \
        "${nginx_dir}/server.crt" "${nginx_dir}/server.key"; do
        [[ -f "$file" ]] || missing=1
    done
    if [[ "$missing" == 0 ]]; then
        ongrid_secure_pcap_parser_tls_permissions "$root"
        return 0
    fi

    command -v openssl >/dev/null 2>&1 || {
        printf '[ERROR] openssl is required to create private pcap-parser TLS material\n' >&2
        return 1
    }
    tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/ongrid-pcap-parser.XXXXXX") || return 1
    trap 'rm -rf "$tmp_dir"' EXIT

    rm -rf "$root"
    install -d -m 0750 "$manager_dir" "$parser_dir" "$nginx_dir"

    openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out "${tmp_dir}/ca.key" >/dev/null 2>&1 || return 1
    openssl req -x509 -new -key "${tmp_dir}/ca.key" -sha256 -days 3650 \
        -subj '/CN=ongrid-pcap-parser-internal-ca' -out "${tmp_dir}/ca.crt" >/dev/null 2>&1 || return 1

    openssl genpkey -algorithm ED25519 -out "${manager_dir}/request.key" >/dev/null 2>&1 || return 1
    openssl pkey -in "${manager_dir}/request.key" -pubout -out "${parser_dir}/request.pub" >/dev/null 2>&1 || return 1

    ongrid_issue_pcap_parser_certificate "$tmp_dir" "$manager_dir/client" "ongrid-manager" "clientAuth" || return 1
    ongrid_issue_pcap_parser_certificate "$tmp_dir" "$parser_dir/server" "pcap-parser" "serverAuth" "DNS:pcap-parser" || return 1
    ongrid_issue_pcap_parser_certificate "$tmp_dir" "$nginx_dir/server" "nginx" "serverAuth" "DNS:nginx" || return 1

    cp "${tmp_dir}/ca.crt" "${manager_dir}/ca.crt"
    cp "${tmp_dir}/ca.crt" "${parser_dir}/ca.crt"
    ongrid_secure_pcap_parser_tls_permissions "$root"
)

ongrid_issue_pcap_parser_certificate() {
    local tmp_dir="$1" destination="$2" common_name="$3" usage="$4" san="${5:-}" config
    config="${tmp_dir}/$(basename "$destination").cnf"
    openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "${destination}.key" >/dev/null 2>&1 || return 1
    openssl req -new -key "${destination}.key" -subj "/CN=${common_name}" -out "${tmp_dir}/$(basename "$destination").csr" >/dev/null 2>&1 || return 1
    {
        printf '%s\n' 'basicConstraints=critical,CA:FALSE'
        printf '%s\n' 'keyUsage=critical,digitalSignature,keyEncipherment'
        printf 'extendedKeyUsage=%s\n' "$usage"
        if [[ -n "$san" ]]; then
            printf 'subjectAltName=%s\n' "$san"
        fi
    } >"$config"
    openssl x509 -req -in "${tmp_dir}/$(basename "$destination").csr" \
        -CA "${tmp_dir}/ca.crt" -CAkey "${tmp_dir}/ca.key" -CAcreateserial \
        -days 825 -sha256 -extfile "$config" -out "${destination}.crt" >/dev/null 2>&1
}

ongrid_secure_pcap_parser_tls_permissions() {
    local root="$1" manager_dir="${root}/manager" parser_dir="${root}/parser" nginx_dir="${root}/nginx"
    chown -R 65532:65532 "$manager_dir"
    chown -R 10001:10001 "$parser_dir"
    chown -R root:root "$nginx_dir"
    chmod 0750 "$root" "$manager_dir" "$parser_dir" "$nginx_dir"
    chmod 0600 "$manager_dir/request.key" "$manager_dir/client.key" "$parser_dir/server.key" "$nginx_dir/server.key"
    chmod 0644 "$manager_dir/client.crt" "$manager_dir/ca.crt" "$parser_dir/request.pub" "$parser_dir/server.crt" "$parser_dir/ca.crt" "$nginx_dir/server.crt"
}
