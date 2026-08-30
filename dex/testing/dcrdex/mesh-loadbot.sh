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

TEST_ROOT="${TEST_ROOT:-${HOME}/dextest}"
MESH_ROOT="${TEST_ROOT}/dcrdex-mesh"
CLIENT_ROOT="${TEST_ROOT}/mesh-clients"
CLIENT_CTL_DIR="${CLIENT_ROOT}/harness-ctl"
MESH_SESSION="${MESH_SESSION:-dcrdex-mesh-harness}"

APP_PASS="${APP_PASS:-abc}"
LOOPS="${MESH_LOADBOT_LOOPS:-4}"
SETUP="${MESH_LOADBOT_SETUP:-1}"
TRADE_SETTLE_SECS="${MESH_LOADBOT_TRADE_SETTLE_SECS:-40}"
PROMOTION_TIMEOUT_SECS="${MESH_LOADBOT_PROMOTION_TIMEOUT_SECS:-75}"
REJOIN_TIMEOUT_SECS="${MESH_LOADBOT_REJOIN_TIMEOUT_SECS:-75}"
ORDER_QTY="${MESH_LOADBOT_ORDER_QTY:-2000000000}"
HIGH_SELL_RATE="${MESH_LOADBOT_HIGH_SELL_RATE:-200000}"
TRADE_OPTIONS="${MESH_LOADBOT_TRADE_OPTIONS:-{\"swapsplit\":\"false\"}}"
BOOK_COMPARE_ATTEMPTS="${MESH_LOADBOT_BOOK_COMPARE_ATTEMPTS:-20}"
BOOK_COMPARE_SLEEP_SECS="${MESH_LOADBOT_BOOK_COMPARE_SLEEP_SECS:-3}"
LOG_FILE="${MESH_LOADBOT_LOG_FILE:-${TEST_ROOT}/mesh-loadbot.log}"

DCR_ASSET_ID=42
BTC_ASSET_ID=0

LOG_SCAN_FILES=(
  "${MESH_ROOT}/alpha/logs/simnet/dcrdex.log"
  "${MESH_ROOT}/beta/logs/simnet/dcrdex.log"
  "${CLIENT_ROOT}/client1/simnet/logs/dexc.log"
  "${CLIENT_ROOT}/client2/simnet/logs/dexc.log"
)
LOG_SCAN_START_LINES=()

log() {
  mkdir -p "$(dirname "${LOG_FILE}")"
  printf '[mesh-loadbot] %s\n' "$*" | tee -a "${LOG_FILE}"
}

die() {
  log "ERROR: $*"
  exit 1
}

usage() {
  cat <<EOF
Usage: $(basename "$0") [--setup|--no-setup] [--loops N]

Runs a small mesh trading/failover load test:
  1. Starts the mesh/client harness with mesh-client-setup.sh unless --no-setup is used.
  2. Places standing orders with one client connected to each node.
  3. Stops the current master and waits for the slave to promote.
  4. Places an order on the promoted node while the peer is down.
  5. Restarts the stopped node and waits for catch-up/equal mesh state.
  6. Repeats, so leadership alternates back and forth.

Useful environment variables:
  MESH_LOADBOT_LOOPS                  default ${LOOPS}
  MESH_LOADBOT_SETUP                  default ${SETUP}
  MESH_LOADBOT_TRADE_SETTLE_SECS      default ${TRADE_SETTLE_SECS}
  MESH_LOADBOT_PROMOTION_TIMEOUT_SECS default ${PROMOTION_TIMEOUT_SECS}
  MESH_LOADBOT_REJOIN_TIMEOUT_SECS    default ${REJOIN_TIMEOUT_SECS}
  MESH_LOADBOT_LOG_FILE               default ${LOG_FILE}
EOF
}

while (($#)); do
  case "$1" in
    --setup)
      SETUP=1
      shift
      ;;
    --no-setup)
      SETUP=0
      shift
      ;;
    --loops)
      [ "$#" -ge 2 ] || die "--loops requires a value"
      LOOPS="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown argument $1"
      ;;
  esac
done

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

require_cmd tmux
require_cmd jq
require_cmd grep
require_cmd sed

node_port() {
  case "$1" in
    alpha) printf '17273\n' ;;
    beta) printf '17283\n' ;;
    *) die "unknown node $1" ;;
  esac
}

node_window() {
  case "$1" in
    alpha) printf '1\n' ;;
    beta) printf '2\n' ;;
    *) die "unknown node $1" ;;
  esac
}

node_host() {
  printf '127.0.0.1:%s\n' "$(node_port "$1")"
}

node_run_script() {
  printf '%s/run-%s\n' "${MESH_ROOT}" "$1"
}

node_log() {
  printf '%s/%s/logs/simnet/dcrdex.log\n' "${MESH_ROOT}" "$1"
}

node_ctl() {
  case "$1" in
    alpha) printf '%s/bw1ctl\n' "${CLIENT_CTL_DIR}" ;;
    beta) printf '%s/bw2ctl\n' "${CLIENT_CTL_DIR}" ;;
    *) die "unknown node $1" ;;
  esac
}

peer_node() {
  case "$1" in
    alpha) printf 'beta\n' ;;
    beta) printf 'alpha\n' ;;
    *) die "unknown node $1" ;;
  esac
}

node_up() {
  local port
  port="$(node_port "$1")"
  (echo >"/dev/tcp/127.0.0.1/${port}") >/dev/null 2>&1
}

wait_port_down() {
  local node=$1
  local timeout=${2:-30}
  local i
  for ((i = 0; i < timeout; i++)); do
    if ! node_up "${node}"; then
      return 0
    fi
    sleep 1
  done
  return 1
}

wait_port_up() {
  local node=$1
  local timeout=${2:-60}
  local i
  for ((i = 0; i < timeout; i++)); do
    if node_up "${node}"; then
      return 0
    fi
    sleep 1
  done
  return 1
}

mesh_state() {
  local node=$1
  local log_file
  log_file="$(node_log "${node}")"
  [ -f "${log_file}" ] || return 1

  grep -E 'local_state=|Entered state ' "${log_file}" |
    sed -E \
      -e 's/.*local_state=([a-z_]+).*/\1/' \
      -e 's/.*Entered state ([a-z_]+).*/\1/' |
    tail -n 1
}

wait_node_state() {
  local node=$1
  local want=$2
  local timeout=$3
  local i state
  for ((i = 0; i < timeout; i++)); do
    if node_up "${node}"; then
      state="$(mesh_state "${node}" 2>/dev/null || true)"
      if [ "${state}" = "${want}" ]; then
        return 0
      fi
    fi
    sleep 1
  done
  return 1
}

current_master() {
  local node state
  for node in alpha beta; do
    if node_up "${node}"; then
      state="$(mesh_state "${node}" 2>/dev/null || true)"
      case "${state}" in
        established_master|established_master_peer_catching_up)
          printf '%s\n' "${node}"
          return 0
          ;;
      esac
    fi
  done
  return 1
}

wait_mesh_equal() {
  local timeout=$1
  local i a b
  for ((i = 0; i < timeout; i++)); do
    a="$(mesh_state alpha 2>/dev/null || true)"
    b="$(mesh_state beta 2>/dev/null || true)"
    if node_up alpha && node_up beta; then
      if { [ "${a}" = established_master ] && [ "${b}" = established_slave ]; } ||
        { [ "${a}" = established_slave ] && [ "${b}" = established_master ]; }; then
        log "Mesh equal: alpha=${a}, beta=${b}"
        return 0
      fi
    fi
    sleep 1
  done
  a="$(mesh_state alpha 2>/dev/null || true)"
  b="$(mesh_state beta 2>/dev/null || true)"
  die "timed out waiting for equal mesh state: alpha=${a:-unknown}, beta=${b:-unknown}"
}

stop_node() {
  local node=$1
  local window
  window="$(node_window "${node}")"
  log "Stopping ${node}"
  tmux send-keys -t "${MESH_SESSION}:${window}" C-c
  wait_port_down "${node}" 45 || die "${node} did not stop"
}

start_node() {
  local node=$1
  local window run_script
  window="$(node_window "${node}")"
  run_script="$(node_run_script "${node}")"
  [ -x "${run_script}" ] || die "missing run script ${run_script}"

  if node_up "${node}"; then
    log "${node} is already running"
    return 0
  fi

  log "Starting ${node}"
  tmux send-keys -t "${MESH_SESSION}:${window}" "${run_script}" C-m
  wait_port_up "${node}" 60 || die "${node} did not start"
}

submit_trade() {
  local node=$1
  local sell=$2
  local rate=$3
  local label=$4
  local ctl host out
  ctl="$(node_ctl "${node}")"
  host="$(node_host "${node}")"

  [ -x "${ctl}" ] || die "missing client control helper ${ctl}"
  log "Submitting ${label}: node=${node}, sell=${sell}, rate=${rate}"
  if ! out="$("${ctl}" -p "${APP_PASS}" trade "${host}" true "${sell}" \
    "${DCR_ASSET_ID}" "${BTC_ASSET_ID}" "${ORDER_QTY}" "${rate}" false "${TRADE_OPTIONS}" 2>&1)"; then
    log "${label} failed: ${out}"
    return 1
  fi
  log "${label} accepted: $(jq -cr '.orderID // .id // .ID // .' <<<"${out}" 2>/dev/null || printf '%s' "${out}")"
}

standing_rate() {
  local cycle=$1
  local node=$2
  local phase=$3
  local offset=0

  [ "${node}" = beta ] && offset=$((offset + 1000))
  [ "${phase}" = after-rejoin ] && offset=$((offset + 2000))
  printf '%s\n' "$((HIGH_SELL_RATE + (cycle * 100) + offset))"
}

submit_standing_pair() {
  local cycle=$1
  local phase=$2

  submit_trade alpha true "$(standing_rate "${cycle}" alpha "${phase}")" "cycle ${cycle} ${phase} alpha sell" || return 1
  submit_trade beta true "$(standing_rate "${cycle}" beta "${phase}")" "cycle ${cycle} ${phase} beta sell" || return 1
}

submit_offline_trade() {
  local node=$1
  local cycle=$2
  submit_trade "${node}" true "$(standing_rate "${cycle}" "${node}" offline)" "cycle ${cycle} offline ${node} sell"
}

canonical_book() {
  jq -S '{
    sells: [.sells[]? | {qtyAtomic, msgRate, sell, token}],
    buys: [.buys[]? | {qtyAtomic, msgRate, sell, token}],
    epoch: [.epoch[]? | {qtyAtomic, msgRate, sell, token}]
  }'
}

verify_books_equal() {
  local attempt book_a book_b canon_a canon_b
  for ((attempt = 1; attempt <= BOOK_COMPARE_ATTEMPTS; attempt++)); do
    if book_a="$("$(node_ctl alpha)" orderbook "$(node_host alpha)" "${DCR_ASSET_ID}" "${BTC_ASSET_ID}" 50 2>/dev/null)" &&
      book_b="$("$(node_ctl beta)" orderbook "$(node_host beta)" "${DCR_ASSET_ID}" "${BTC_ASSET_ID}" 50 2>/dev/null)"; then
      canon_a="$(canonical_book <<<"${book_a}")"
      canon_b="$(canonical_book <<<"${book_b}")"
      if [ "${canon_a}" = "${canon_b}" ]; then
        log "Order books match"
        return 0
      fi
    fi
    sleep "${BOOK_COMPARE_SLEEP_SECS}"
  done

  log "alpha book: ${canon_a:-unavailable}"
  log "beta book: ${canon_b:-unavailable}"
  die "order books did not converge"
}

record_log_offsets() {
  local file lines
  LOG_SCAN_START_LINES=()
  for file in "${LOG_SCAN_FILES[@]}"; do
    lines=0
    if [ -f "${file}" ]; then
      lines="$(wc -l <"${file}" | tr -d ' ')"
    fi
    LOG_SCAN_START_LINES+=("${lines}")
  done
}

scan_logs() {
  local hits="" file start idx file_hits
  for idx in "${!LOG_SCAN_FILES[@]}"; do
    file="${LOG_SCAN_FILES[${idx}]}"
    start="${LOG_SCAN_START_LINES[${idx}]:-0}"
    [ -f "${file}" ] || continue

    file_hits="$(
      tail -n "+$((start + 1))" "${file}" |
        grep -En '\[(ERR|CRT)\]|panic|event seq mismatch|failed to apply|mesh event publish failed|market closed' |
        grep -Ev 'ListenAndServe failed for http/pprof|Server disconnect|connection refused|connect: connection refused' || true
    )"
    if [ -n "${file_hits}" ]; then
      hits+="${file}"$'\n'"${file_hits}"$'\n'
    fi
  done

  if [ -n "${hits}" ]; then
    printf '%s\n' "${hits}" >&2
    die "unexpected errors found in mesh/client logs"
  fi
}

setup_harness() {
  if [ "${SETUP}" = "1" ]; then
    log "Running mesh-client-setup.sh"
    WAIT_ATTEMPTS="${WAIT_ATTEMPTS:-240}" WAIT_SLEEP_SECS="${WAIT_SLEEP_SECS:-1}" \
      "${SCRIPT_DIR}/mesh-client-setup.sh"
  else
    log "Using existing mesh/client harness"
  fi

  [ -x "${CLIENT_CTL_DIR}/bw1ctl" ] || die "missing ${CLIENT_CTL_DIR}/bw1ctl; run with --setup"
  [ -x "${CLIENT_CTL_DIR}/bw2ctl" ] || die "missing ${CLIENT_CTL_DIR}/bw2ctl; run with --setup"
  tmux has-session -t "${MESH_SESSION}" 2>/dev/null || die "missing tmux session ${MESH_SESSION}"
  wait_mesh_equal "${REJOIN_TIMEOUT_SECS}"
}

run_cycle() {
  local cycle=$1
  local master promoted stopped

  log "Starting cycle ${cycle}/${LOOPS}"
  wait_mesh_equal "${REJOIN_TIMEOUT_SECS}"
  verify_books_equal

  submit_standing_pair "${cycle}" before-failover
  log "Waiting ${TRADE_SETTLE_SECS}s for the online trade round"
  sleep "${TRADE_SETTLE_SECS}"
  verify_books_equal

  master="$(current_master)" || die "could not determine current master"
  stopped="${master}"
  promoted="$(peer_node "${master}")"

  stop_node "${stopped}"
  wait_node_state "${promoted}" established_master "${PROMOTION_TIMEOUT_SECS}" ||
    die "${promoted} did not promote after ${stopped} stopped"
  log "${promoted} promoted after ${stopped} stopped"

  submit_offline_trade "${promoted}" "${cycle}" ||
    die "promoted node ${promoted} did not accept a trade"
  log "Waiting ${TRADE_SETTLE_SECS}s with ${stopped} offline"
  sleep "${TRADE_SETTLE_SECS}"

  start_node "${stopped}"
  wait_mesh_equal "${REJOIN_TIMEOUT_SECS}"
  verify_books_equal

  submit_standing_pair "${cycle}" after-rejoin
  log "Waiting ${TRADE_SETTLE_SECS}s after catch-up/rejoin"
  sleep "${TRADE_SETTLE_SECS}"
  verify_books_equal
  scan_logs
  log "Completed cycle ${cycle}/${LOOPS}"
}

main() {
  setup_harness
  record_log_offsets
  mkdir -p "$(dirname "${LOG_FILE}")"
  : >"${LOG_FILE}"
  log "Mesh loadbot starting: loops=${LOOPS}, setup=${SETUP}"

  local cycle
  for ((cycle = 1; cycle <= LOOPS; cycle++)); do
    run_cycle "${cycle}"
  done

  scan_logs
  log "Mesh loadbot completed successfully"
}

main "$@"
