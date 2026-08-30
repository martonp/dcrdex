#!/usr/bin/env bash

DCRDEX_HARNESS_DIR=$(
  cd "$(dirname "${BASH_SOURCE[0]}")"
  pwd
)

dcrdex_write_build_scripts() {
  local harness_root=$1
  local binary_path=$2

  cat > "${harness_root}/build" <<EOF
#!/usr/bin/env bash
cd "${DCRDEX_HARNESS_DIR}/../../../server/cmd/dcrdex/"
go build -o "${binary_path}" -ldflags \\
    "-X 'decred.org/dcrdex/dex.testLockTimeTaker=3m' \\
    -X 'decred.org/dcrdex/dex.testLockTimeMaker=6m'"
EOF
  chmod +x "${harness_root}/build"

  cat > "${harness_root}/build-lock" <<EOF
#!/usr/bin/env bash
cd "${DCRDEX_HARNESS_DIR}/../../../server/cmd/dcrdex/"
go build -o "${binary_path}" -ldflags \\
    "-X 'decred.org/dcrdex/dex.testLockTimeTaker=\${1:-1m}' \\
    -X 'decred.org/dcrdex/dex.testLockTimeMaker=\${2:-2m}'"
EOF
  chmod +x "${harness_root}/build-lock"
}

dcrdex_build_binary() {
  local harness_root=$1
  "${harness_root}/build"
}

dcrdex_reset_database() {
  local db_name=$1

  dcrdex_admin_psql -v ON_ERROR_STOP=1 -c 'DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = '"'"'dcrdex'"'"') THEN CREATE ROLE dcrdex LOGIN; END IF; END $$;'
  dcrdex_admin_psql -v ON_ERROR_STOP=1 -c "DROP DATABASE IF EXISTS ${db_name}" -c "CREATE DATABASE ${db_name} OWNER dcrdex"
}

dcrdex_admin_psql() {
  if sudo -u postgres -H psql -d postgres -Atqc "select 1" >/dev/null 2>&1; then
    sudo -u postgres -H psql -d postgres "$@"
    return $?
  fi

  if psql -h 127.0.0.1 -d postgres -Atqc "select 1" >/dev/null 2>&1; then
    psql -h 127.0.0.1 -d postgres "$@"
    return $?
  fi

  if psql -d postgres -Atqc "select 1" >/dev/null 2>&1; then
    psql -d postgres "$@"
    return $?
  fi

  echo "unable to connect to postgres as an admin user" >&2
  return 1
}

dcrdex_write_config() {
  local app_dir=$1
  local db_name=$2
  local rpc_listen=$3
  local admin_addr=$4
  local node_relay_addr=$5
  local mesh_listen=$6
  local mesh_peer=$7
  local mesh_peer_cert=$8
  local client_addr=$9

  cat > "${app_dir}/dcrdex.conf" <<EOF
pgdbname=${db_name}
simnet=1
rpclisten=${rpc_listen}
debuglevel=trace
loglocal=true
signingkeypass=keypass
adminsrvon=1
adminsrvpass=adminpass
adminsrvaddr=${admin_addr}
bcasttimeout=1m
# freecancels=1
maxepochcancels=128
httpprof=1
noderelayaddr=${node_relay_addr}
EOF

  if [ -n "${PG_PASS}" ]; then
    echo "pgpass=\"${PG_PASS}\"" >> "${app_dir}/dcrdex.conf"
  fi
  if [ -n "${mesh_listen}" ]; then
    echo "meshlisten=${mesh_listen}" >> "${app_dir}/dcrdex.conf"
  fi
  if [ -n "${mesh_peer}" ]; then
    echo "meshpeer=${mesh_peer}" >> "${app_dir}/dcrdex.conf"
  fi
  if [ -n "${mesh_peer_cert}" ]; then
    echo "meshpeercert=${mesh_peer_cert}" >> "${app_dir}/dcrdex.conf"
  fi
  if [ -n "${client_addr}" ]; then
    echo "clientaddr=${client_addr}" >> "${app_dir}/dcrdex.conf"
  fi
}

dcrdex_write_static_rpc_cert_pair() {
  local app_dir=$1

  cat > "${app_dir}/rpc.cert" <<'EOF'
-----BEGIN CERTIFICATE-----
MIICpTCCAgagAwIBAgIQZMfxMkSi24xMr4CClCODrzAKBggqhkjOPQQDBDBJMSIw
IAYDVQQKExlkY3JkZXggYXV0b2dlbmVyYXRlZCBjZXJ0MSMwIQYDVQQDExp1YnVu
dHUtcy0xdmNwdS0yZ2ItbG9uMS0wMTAeFw0yMDA2MDgxMjM4MjNaFw0zMDA2MDcx
MjM4MjNaMEkxIjAgBgNVBAoTGWRjcmRleCBhdXRvZ2VuZXJhdGVkIGNlcnQxIzAh
BgNVBAMTGnVidW50dS1zLTF2Y3B1LTJnYi1sb24xLTAxMIGbMBAGByqGSM49AgEG
BSuBBAAjA4GGAAQApXJpVD7si8yxoITESq+xaXWtEpsCWU7X+8isRDj1cFfH53K6
/XNvn3G+Yq0L22Q8pMozGukA7KuCQAAL0xnuo10AecWBN0Zo2BLHvpwKkmAs71C+
5BITJksqFxvjwyMKbo3L/5x8S/JmAWrZoepBLfQ7HcoPqLAcg0XoIgJjOyFZgc+j
gYwwgYkwDgYDVR0PAQH/BAQDAgKkMA8GA1UdEwEB/wQFMAMBAf8wZgYDVR0RBF8w
XYIadWJ1bnR1LXMtMXZjcHUtMmdiLWxvbjEtMDGCCWxvY2FsaG9zdIcEfwAAAYcQ
AAAAAAAAAAAAAAAAAAAAAYcEsj5QQYcEChAABYcQ/oAAAAAAAAAYPqf//vUPXDAK
BggqhkjOPQQDBAOBjAAwgYgCQgFMEhyTXnT8phDJAnzLbYRktg7rTAbTuQRDp1PE
jf6b2Df4DkSX7JPXvVi3NeBru+mnrOkHBUMqZd0m036aC4q/ZAJCASa+olu4Isx7
8JE3XB6kGr+s48eIFPtmq1D0gOvRr3yMHrhJe3XDNqvppcHihG0qNb0gyaiX18Cv
vF8Ti1x2vTkD
-----END CERTIFICATE-----
EOF

  cat > "${app_dir}/rpc.key" <<'EOF'
-----BEGIN EC PRIVATE KEY-----
MIHcAgEBBEIADTDRCsp8om9OhJa+m46FZ5IhgLAno1Rp6B0i2lqESL5x9vV/upiV
TbNzCeFqEY5/Ujra9f8ZovqMlrIQmNOaZFmgBwYFK4EEACOhgYkDgYYABAClcmlU
PuyLzLGghMRKr7Fpda0SmwJZTtf7yKxEOPVwV8fncrr9c2+fcb5irQvbZDykyjMa
6QDsq4JAAAvTGe6jXQB5xYE3RmjYEse+nAqSYCzvUL7kEhMmSyoXG+PDIwpujcv/
nHxL8mYBatmh6kEt9Dsdyg+osByDRegiAmM7IVmBzw==
-----END EC PRIVATE KEY-----
EOF
}

dcrdex_write_evm_protocol_overrides() {
  local app_dir=$1

  cat > "${app_dir}/evm-protocol-overrides.json" <<'EOF'
{}
EOF
}

dcrdex_write_dexadm_script() {
  local script_path=$1
  local cert_path=$2
  local admin_addr=$3

  cat > "${script_path}" <<EOF
#!/usr/bin/env bash
if [[ "\$#" -eq "2" ]]; then
    curl --cacert "${cert_path}" --basic -u u:adminpass --header "Content-Type: text/plain" --data-binary "\$2" "https://${admin_addr}/api/\$1"
else
    curl --cacert "${cert_path}" --basic -u u:adminpass "https://${admin_addr}/api/\$1"
fi
EOF
  chmod +x "${script_path}"
}

dcrdex_write_run_script() {
  local script_path=$1
  local app_dir=$2
  local binary_path=$3
  local done_signal=$4

  cat > "${script_path}" <<EOF
#!/usr/bin/env bash
cd "${app_dir}"
DCRDEX_MARKETS_PATH="${app_dir}/markets.json" "${DCRDEX_HARNESS_DIR}/genmarkets.sh"
"${binary_path}" --appdata="\$(pwd)" "\$@"; tmux wait-for -S ${done_signal}
EOF
  chmod +x "${script_path}"
}

dcrdex_shell_join_args() {
  if [ "$#" -eq 0 ]; then
    return 0
  fi
  printf '%q ' "$@"
}

dcrdex_tmux_init_window() {
  local session=$1
  local index=$2
  local name=$3
  local cwd=$4

  if [ "${index}" = "0" ]; then
    tmux rename-window -t "${session}:0" "${name}"
  else
    tmux new-window -t "${session}:${index}" -n "${name}" "${SHELL}"
  fi
  tmux send-keys -t "${session}:${index}" "cd ${cwd}" C-m
}

dcrdex_tmux_start_session() {
  local session=$1
  local initial_name=$2
  local initial_cwd=$3

  tmux new-session -d -s "${session}" "${SHELL}"
  dcrdex_tmux_init_window "${session}" 0 "${initial_name}" "${initial_cwd}"
}
