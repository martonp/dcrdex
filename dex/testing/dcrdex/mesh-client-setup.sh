#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR=$(
  cd "$(dirname "${BASH_SOURCE[0]}")"
  pwd
)
REPO_ROOT=$(
  cd "${SCRIPT_DIR}/../../.."
  pwd
)

export PATH="${HOME}/go/bin:${PATH}"
export SHELL="${SHELL:-$(which bash)}"

TEST_ROOT="${TEST_ROOT:-${HOME}/dextest}"
CLIENT_ROOT="${TEST_ROOT}/mesh-clients"
CLIENT_BIN_DIR="${CLIENT_ROOT}/bin"
CLIENT_CTL_DIR="${CLIENT_ROOT}/harness-ctl"
CLIENT1_DIR="${CLIENT_ROOT}/client1"
CLIENT2_DIR="${CLIENT_ROOT}/client2"

CLIENT_SESSION="mesh-clients"
DCR_SESSION="dcr-harness"
BTC_SESSION="btc-harness"
MESH_SESSION="dcrdex-mesh-harness"

BISONW_SRC="${REPO_ROOT}/client/cmd/bisonw"
BWCTL_SRC="${REPO_ROOT}/client/cmd/bwctl"
WEBSITE_SRC="${REPO_ROOT}/client/webserver/site"
WEBSITE_DIST="${WEBSITE_SRC}/dist"
BISONW_BIN="${CLIENT_BIN_DIR}/bisonw"
BWCTL_BIN="${CLIENT_BIN_DIR}/bwctl"

APP_PASS="${APP_PASS:-abc}"
DCR_WALLET_PASS="${DCR_WALLET_PASS:-abc}"
BTC_WALLET_PASS="${BTC_WALLET_PASS:-}"

CLIENT1_SEED="${CLIENT1_SEED:-2295f6cf5d5983ed8ea558f3fe76cefbe13ca4a4bf1f2fa1f8e72b0f29a548c16cb4c3e3a3ebdb68d7db083f6f5c13d8e5f4dbe5f0a3857e268a6c9f6a264f3f}"
CLIENT2_SEED="${CLIENT2_SEED:-8aa6c552511a5d95bc3868d31caa411db8fe28f77d9a97b96c7e50439abca86ca14c92866d61c924cf6ef5d5d5d8048c36697de57b2f44b90f4f22f2fca45e0c}"

CLIENT1_WEB_ADDR="${CLIENT1_WEB_ADDR:-127.0.0.1:5760}"
CLIENT1_RPC_ADDR="${CLIENT1_RPC_ADDR:-127.0.0.1:5761}"
CLIENT2_WEB_ADDR="${CLIENT2_WEB_ADDR:-127.0.0.1:5762}"
CLIENT2_RPC_ADDR="${CLIENT2_RPC_ADDR:-127.0.0.1:5763}"

CLIENT1_DEX_HOST="${CLIENT1_DEX_HOST:-127.0.0.1:17273}"
CLIENT2_DEX_HOST="${CLIENT2_DEX_HOST:-127.0.0.1:17283}"
CLIENT1_DEX_CERT="${CLIENT1_DEX_CERT:-${TEST_ROOT}/dcrdex-mesh/alpha/rpc.cert}"
CLIENT2_DEX_CERT="${CLIENT2_DEX_CERT:-${TEST_ROOT}/dcrdex-mesh/beta/rpc.cert}"

DCR_TRADING1_CONF="${DCR_TRADING1_CONF:-${TEST_ROOT}/dcr/trading1/trading1.conf}"
DCR_TRADING2_CONF="${DCR_TRADING2_CONF:-${TEST_ROOT}/dcr/trading2/trading2.conf}"
BTC_ALPHA_CONF="${BTC_ALPHA_CONF:-${TEST_ROOT}/btc/alpha/alpha.conf}"
BTC_BETA_CONF="${BTC_BETA_CONF:-${TEST_ROOT}/btc/beta/beta.conf}"

DCR_ASSET_ID=42
BTC_ASSET_ID=0

DEFAULT_DCR_BOND_AMOUNT="${DEFAULT_DCR_BOND_AMOUNT:-1000000000}"
DEFAULT_DCR_BOND_CONFS="${DEFAULT_DCR_BOND_CONFS:-2}"

WAIT_ATTEMPTS="${WAIT_ATTEMPTS:-180}"
WAIT_SLEEP_SECS="${WAIT_SLEEP_SECS:-1}"

CLIENT1_CONF="${CLIENT1_DIR}/dexc.conf"
CLIENT1_CTL_CONF="${CLIENT1_DIR}/dexcctl.conf"
CLIENT2_CONF="${CLIENT2_DIR}/dexc.conf"
CLIENT2_CTL_CONF="${CLIENT2_DIR}/dexcctl.conf"

log() {
  echo "[mesh-client-setup] $*"
}

read_ini_value() {
  local file=$1
  local key=$2

  awk -F= -v want="${key}" '
    $1 == want {
      sub(/^[^=]*=/, "", $0)
      print $0
      exit
    }
  ' "${file}"
}

require_cmd() {
  local cmd=$1
  if ! command -v "${cmd}" >/dev/null 2>&1; then
    echo "missing required command: ${cmd}" >&2
    exit 1
  fi
}

stop_tmux_session() {
  local session=$1
  if tmux has-session -t "${session}" 2>/dev/null; then
    log "Stopping tmux session ${session}"
    tmux kill-session -t "${session}"
    sleep 1
  fi
}

wait_for_output() {
  local desc=$1
  shift

  local attempt output
  for ((attempt = 1; attempt <= WAIT_ATTEMPTS; attempt++)); do
    if output="$("$@" 2>/dev/null)"; then
      printf '%s' "${output}"
      return 0
    fi
    sleep "${WAIT_SLEEP_SECS}"
  done

  echo "timed out waiting for ${desc}" >&2
  return 1
}

wait_for_port() {
  local desc=$1
  local host=$2
  local port=$3
  local attempt

  for ((attempt = 1; attempt <= WAIT_ATTEMPTS; attempt++)); do
    if (echo >"/dev/tcp/${host}/${port}") >/dev/null 2>&1; then
      return 0
    fi
    sleep "${WAIT_SLEEP_SECS}"
  done

  echo "timed out waiting for ${desc} on ${host}:${port}" >&2
  return 1
}

wait_for_wallet_ready() {
  local ctl_conf=$1
  local asset_id=$2
  local label=$3
  local attempt json

  for ((attempt = 1; attempt <= WAIT_ATTEMPTS; attempt++)); do
    json="$("${BWCTL_BIN}" -C "${ctl_conf}" walletstate "${asset_id}" 2>/dev/null || true)"
    if [ -n "${json}" ] && jq -e '.open == true and .running == true and .synced == true and (.balance.available // 0) > 0' >/dev/null <<<"${json}"; then
      return 0
    fi
    sleep "${WAIT_SLEEP_SECS}"
  done

  echo "timed out waiting for ${label} wallet ${asset_id} to become ready" >&2
  return 1
}

wait_for_effective_tier() {
  local ctl_conf=$1
  local dex_host=$2
  local label=$3
  local attempt json

  for ((attempt = 1; attempt <= WAIT_ATTEMPTS; attempt++)); do
    json="$("${BWCTL_BIN}" -C "${ctl_conf}" exchanges 2>/dev/null || true)"
    if [ -n "${json}" ] && jq -e --arg host "${dex_host}" '.[$host].auth.effectiveTier >= 1' >/dev/null <<<"${json}"; then
      return 0
    fi
    sleep "${WAIT_SLEEP_SECS}"
  done

  echo "timed out waiting for ${label} to bond to ${dex_host}" >&2
  return 1
}

discover_bond_setting() {
  local json=$1
  local jq_expr=$2
  local fallback=$3
  local value

  value="$(jq -r "${jq_expr} // empty" <<<"${json}")"
  if [ -z "${value}" ] || [ "${value}" = "null" ]; then
    printf '%s\n' "${fallback}"
    return 0
  fi

  printf '%s\n' "${value}"
}

json_wallet_config() {
  local conf_file=$1
  shift

  local jq_args=()
  local jq_expr='{'
  local first=1
  local key ini_key value
  for key in "$@"; do
    ini_key=${key#*=}
    if [ "${ini_key}" = "${key}" ]; then
      ini_key=${key}
    else
      key=${key%%=*}
    fi

    value="$(read_ini_value "${conf_file}" "${ini_key}")"
    if [ -z "${value}" ]; then
      echo "missing ${ini_key} in ${conf_file}" >&2
      return 1
    fi

    jq_args+=(--arg "${key}" "${value}")
    if [ ${first} -eq 0 ]; then
      jq_expr+=", "
    fi
    jq_expr+="\"${key}\": \$${key}"
    first=0
  done
  jq_expr+='}'

  jq -n "${jq_args[@]}" "${jq_expr}"
}

build_client_binaries() {
  require_cmd npm
  log "Building client web assets"
  (
    cd "${WEBSITE_SRC}"
    if [ ! -d node_modules ]; then
      npm clean-install
    fi
    npm run build
  )

  log "Building bisonw"
  mkdir -p "${CLIENT_BIN_DIR}"
  (
    cd "${BISONW_SRC}"
    go build -o "${BISONW_BIN}" -ldflags \
      "-X decred.org/dcrdex/dex.testLockTimeTaker=3m -X decred.org/dcrdex/dex.testLockTimeMaker=6m"
  )

  log "Building bwctl"
  (
    cd "${BWCTL_SRC}"
    go build -o "${BWCTL_BIN}"
  )
}

write_client_config() {
  local dir=$1
  local conf_path=$2
  local ctl_conf_path=$3
  local web_addr=$4
  local rpc_addr=$5

  local ctl_key="${dir}/ctl.key"
  local ctl_cert="${dir}/ctl.cert"

  mkdir -p "${dir}"

  cat > "${conf_path}" <<EOF
simnet=1
webaddr=${web_addr}
rpc=1
rpckey=${ctl_key}
rpccert=${ctl_cert}
rpcuser=user
rpcpass=pass
rpcaddr=${rpc_addr}
loglocal=true
EOF

  cat > "${ctl_conf_path}" <<EOF
rpcuser=user
rpcpass=pass
rpccert=${ctl_cert}
rpcaddr=${rpc_addr}
simnet=1
EOF
}

write_client_helpers() {
  mkdir -p "${CLIENT_CTL_DIR}"

  cat > "${CLIENT_CTL_DIR}/bw1ctl" <<EOF
#!/usr/bin/env bash
"${BWCTL_BIN}" -C "${CLIENT1_CTL_CONF}" "\$@"
EOF
  chmod +x "${CLIENT_CTL_DIR}/bw1ctl"

  cat > "${CLIENT_CTL_DIR}/bw2ctl" <<EOF
#!/usr/bin/env bash
"${BWCTL_BIN}" -C "${CLIENT2_CTL_CONF}" "\$@"
EOF
  chmod +x "${CLIENT_CTL_DIR}/bw2ctl"

  cat > "${CLIENT_CTL_DIR}/open1" <<EOF
#!/usr/bin/env bash
if command -v xdg-open >/dev/null 2>&1; then
  xdg-open "http://${CLIENT1_WEB_ADDR}" >/dev/null 2>&1
elif command -v open >/dev/null 2>&1; then
  open "http://${CLIENT1_WEB_ADDR}" >/dev/null 2>&1
else
  echo "http://${CLIENT1_WEB_ADDR}"
fi
EOF
  chmod +x "${CLIENT_CTL_DIR}/open1"

  cat > "${CLIENT_CTL_DIR}/open2" <<EOF
#!/usr/bin/env bash
if command -v xdg-open >/dev/null 2>&1; then
  xdg-open "http://${CLIENT2_WEB_ADDR}" >/dev/null 2>&1
elif command -v open >/dev/null 2>&1; then
  open "http://${CLIENT2_WEB_ADDR}" >/dev/null 2>&1
else
  echo "http://${CLIENT2_WEB_ADDR}"
fi
EOF
  chmod +x "${CLIENT_CTL_DIR}/open2"

  cat > "${CLIENT_CTL_DIR}/attach" <<EOF
#!/usr/bin/env bash
tmux attach-session -t "${CLIENT_SESSION}"
EOF
  chmod +x "${CLIENT_CTL_DIR}/attach"

  cat > "${CLIENT_CTL_DIR}/quit-clients" <<EOF
#!/usr/bin/env bash
if tmux has-session -t "${CLIENT_SESSION}" 2>/dev/null; then
  tmux send-keys -t "${CLIENT_SESSION}:1" C-c
  tmux send-keys -t "${CLIENT_SESSION}:2" C-c
  sleep 1
  tmux kill-session -t "${CLIENT_SESSION}"
fi
EOF
  chmod +x "${CLIENT_CTL_DIR}/quit-clients"

  cat > "${CLIENT_CTL_DIR}/quit" <<EOF
#!/usr/bin/env bash
set +e
"${CLIENT_CTL_DIR}/quit-clients"
for script in \
  "${TEST_ROOT}/dcrdex-mesh/quit" \
  "${TEST_ROOT}/btc/harness-ctl/quit" \
  "${TEST_ROOT}/dcr/harness-ctl/quit"; do
  if [ -x "\${script}" ]; then
    "\${script}"
  fi
done
EOF
  chmod +x "${CLIENT_CTL_DIR}/quit"
}

start_client_session() {
  log "Starting fresh client tmux session ${CLIENT_SESSION}"
  tmux new-session -d -s "${CLIENT_SESSION}" "${SHELL}"
  tmux rename-window -t "${CLIENT_SESSION}:0" "harness-ctl"
  tmux send-keys -t "${CLIENT_SESSION}:0" "cd ${CLIENT_CTL_DIR}" C-m

  tmux new-window -t "${CLIENT_SESSION}:1" -n "client1" "${SHELL}"
  tmux send-keys -t "${CLIENT_SESSION}:1" "cd ${CLIENT1_DIR}" C-m
  tmux send-keys -t "${CLIENT_SESSION}:1" "\"${BISONW_BIN}\" --appdata=\"${CLIENT1_DIR}\" --simnet" C-m

  tmux new-window -t "${CLIENT_SESSION}:2" -n "client2" "${SHELL}"
  tmux send-keys -t "${CLIENT_SESSION}:2" "cd ${CLIENT2_DIR}" C-m
  tmux send-keys -t "${CLIENT_SESSION}:2" "\"${BISONW_BIN}\" --appdata=\"${CLIENT2_DIR}\" --simnet" C-m
}

setup_client() {
  local label=$1
  local ctl_conf=$2
  local seed=$3
  local dcr_wallet_conf=$4
  local btc_wallet_conf=$5
  local btc_wallet_name=$6
  local dcr_config btc_config btc_rpcbind

  log "Initializing ${label}"
  "${BWCTL_BIN}" -C "${ctl_conf}" -p "${APP_PASS}" init "${seed}"

  dcr_config="$(json_wallet_config "${dcr_wallet_conf}" \
    username \
    password \
    rpclisten \
    rpccert)"
  dcr_config="$(jq '. + {account: "default"}' <<<"${dcr_config}")"

  log "Configuring ${label} DCR wallet"
  "${BWCTL_BIN}" -C "${ctl_conf}" -p "${APP_PASS}" -p "${DCR_WALLET_PASS}" \
    newwallet "${DCR_ASSET_ID}" dcrwalletRPC "${dcr_config}"

  btc_config="$(json_wallet_config "${btc_wallet_conf}" \
    rpcuser \
    rpcpassword \
    rpcport)"
  btc_rpcbind="$(read_ini_value "${btc_wallet_conf}" rpcbind)"
  if [ -z "${btc_rpcbind}" ]; then
    btc_rpcbind="127.0.0.1"
  fi
  btc_config="$(jq --arg walletname "${btc_wallet_name}" --arg rpcbind "${btc_rpcbind}" \
    '. + {walletname: $walletname, rpcbind: $rpcbind}' <<<"${btc_config}")"

  log "Configuring ${label} BTC wallet"
  "${BWCTL_BIN}" -C "${ctl_conf}" -p "${APP_PASS}" -p "${BTC_WALLET_PASS}" \
    newwallet "${BTC_ASSET_ID}" bitcoindRPC "${btc_config}"

  log "Waiting for ${label} wallets to sync and show funded balances"
  wait_for_wallet_ready "${ctl_conf}" "${DCR_ASSET_ID}" "${label}"
  wait_for_wallet_ready "${ctl_conf}" "${BTC_ASSET_ID}" "${label}"

  log "Unlocking ${label}"
  "${BWCTL_BIN}" -C "${ctl_conf}" -p "${APP_PASS}" login
}

post_bond() {
  local label=$1
  local ctl_conf=$2
  local dex_host=$3
  local dex_cert=$4
  local bond_amount=$5

  log "Posting ${label} DCR bond to ${dex_host}"
  "${BWCTL_BIN}" -C "${ctl_conf}" -p "${APP_PASS}" \
    postbond "${dex_host}" "${bond_amount}" "${DCR_ASSET_ID}" 0 true "${dex_cert}"
}

cleanup_existing() {
  stop_tmux_session "${CLIENT_SESSION}"
  stop_tmux_session "${MESH_SESSION}"
  stop_tmux_session "${BTC_SESSION}"
  stop_tmux_session "${DCR_SESSION}"
  rm -rf "${CLIENT_ROOT}"
}

main() {
  require_cmd tmux
  require_cmd jq
  require_cmd go
  require_cmd dcrd
  require_cmd dcrwallet
  require_cmd dcrctl
  require_cmd bitcoind
  require_cmd bitcoin-cli

  cleanup_existing

  log "Starting DCR harness"
  (
    cd "${REPO_ROOT}/dex/testing/dcr"
    NOATTACH=1 ./harness.sh
  )

  log "Starting BTC harness"
  (
    cd "${REPO_ROOT}/dex/testing/btc"
    NOATTACH=1 ./harness.sh
  )

  log "Starting mesh dcrdex harness"
  (
    cd "${REPO_ROOT}/dex/testing/dcrdex"
    NOATTACH=1 ./mesh-harness.sh
  )

  log "Waiting for DCR harness control wallets"
  wait_for_output "DCR trading1 wallet" "${TEST_ROOT}/dcr/harness-ctl/trading1" getbalance >/dev/null
  wait_for_output "DCR trading2 wallet" "${TEST_ROOT}/dcr/harness-ctl/trading2" getbalance >/dev/null

  log "Waiting for BTC harness control wallets"
  wait_for_output "BTC gamma wallet" "${TEST_ROOT}/btc/harness-ctl/gamma" getbalance >/dev/null
  wait_for_output "BTC delta wallet" "${TEST_ROOT}/btc/harness-ctl/delta" getbalance >/dev/null

  log "Waiting for mesh DEX ports"
  wait_for_port "mesh alpha" 127.0.0.1 17273
  wait_for_port "mesh beta" 127.0.0.1 17283

  build_client_binaries

  mkdir -p "${CLIENT1_DIR}" "${CLIENT2_DIR}"
  write_client_config "${CLIENT1_DIR}" "${CLIENT1_CONF}" "${CLIENT1_CTL_CONF}" "${CLIENT1_WEB_ADDR}" "${CLIENT1_RPC_ADDR}"
  write_client_config "${CLIENT2_DIR}" "${CLIENT2_CONF}" "${CLIENT2_CTL_CONF}" "${CLIENT2_WEB_ADDR}" "${CLIENT2_RPC_ADDR}"
  write_client_helpers

  start_client_session

  log "Waiting for client RPC servers"
  wait_for_output "client1 RPC" "${BWCTL_BIN}" -C "${CLIENT1_CTL_CONF}" version >/dev/null
  wait_for_output "client2 RPC" "${BWCTL_BIN}" -C "${CLIENT2_CTL_CONF}" version >/dev/null

  setup_client "client1" "${CLIENT1_CTL_CONF}" "${CLIENT1_SEED}" "${DCR_TRADING1_CONF}" "${BTC_ALPHA_CONF}" gamma
  setup_client "client2" "${CLIENT2_CTL_CONF}" "${CLIENT2_SEED}" "${DCR_TRADING2_CONF}" "${BTC_BETA_CONF}" delta

  log "Querying bond asset config"
  client1_bond_assets="$("${BWCTL_BIN}" -C "${CLIENT1_CTL_CONF}" bondassets "${CLIENT1_DEX_HOST}" "${CLIENT1_DEX_CERT}")"
  client2_bond_assets="$("${BWCTL_BIN}" -C "${CLIENT2_CTL_CONF}" bondassets "${CLIENT2_DEX_HOST}" "${CLIENT2_DEX_CERT}")"

  client1_bond_amount="$(discover_bond_setting "${client1_bond_assets}" '.assets.dcr.amount // .assets.DCR.amount // .assets["42"].amount' "${DEFAULT_DCR_BOND_AMOUNT}")"
  client2_bond_amount="$(discover_bond_setting "${client2_bond_assets}" '.assets.dcr.amount // .assets.DCR.amount // .assets["42"].amount' "${DEFAULT_DCR_BOND_AMOUNT}")"
  client1_bond_confs="$(discover_bond_setting "${client1_bond_assets}" '.assets.dcr.confs // .assets.DCR.confs // .assets["42"].confs' "${DEFAULT_DCR_BOND_CONFS}")"
  client2_bond_confs="$(discover_bond_setting "${client2_bond_assets}" '.assets.dcr.confs // .assets.DCR.confs // .assets["42"].confs' "${DEFAULT_DCR_BOND_CONFS}")"

  post_bond "client1" "${CLIENT1_CTL_CONF}" "${CLIENT1_DEX_HOST}" "${CLIENT1_DEX_CERT}" "${client1_bond_amount}"
  post_bond "client2" "${CLIENT2_CTL_CONF}" "${CLIENT2_DEX_HOST}" "${CLIENT2_DEX_CERT}" "${client2_bond_amount}"

  confirm_blocks="${client1_bond_confs}"
  if [ "${client2_bond_confs}" -gt "${confirm_blocks}" ]; then
    confirm_blocks="${client2_bond_confs}"
  fi
  if [ "${confirm_blocks}" -lt 2 ]; then
    confirm_blocks=2
  fi

  log "Mining ${confirm_blocks} DCR confirmation block(s) for the posted bonds"
  "${TEST_ROOT}/dcr/harness-ctl/mine-alpha" "${confirm_blocks}" >/dev/null

  log "Waiting for both clients to reach effective tier 1"
  wait_for_effective_tier "${CLIENT1_CTL_CONF}" "${CLIENT1_DEX_HOST}" client1
  wait_for_effective_tier "${CLIENT2_CTL_CONF}" "${CLIENT2_DEX_HOST}" client2

  log "Setup complete"
  log "Client appdata directories:"
  log "  client1: ${CLIENT1_DIR}"
  log "  client2: ${CLIENT2_DIR}"
  log "Client control helpers: ${CLIENT_CTL_DIR}"
  log "tmux sessions:"
  log "  ${DCR_SESSION}"
  log "  ${BTC_SESSION}"
  log "  ${MESH_SESSION}"
  log "  ${CLIENT_SESSION}"
}

main "$@"
