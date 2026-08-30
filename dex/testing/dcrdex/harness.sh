#!/usr/bin/env bash
# Tmux script that configures and runs dcrdex.

set -e

source "$(dirname "$0")/harness-common.sh"

TEST_ROOT="${TEST_ROOT:-$HOME/dextest}"
DCRDEX_DATA_DIR="${TEST_ROOT}/dcrdex"
SESSION="dcrdex-harness"
FORWARDED_ARGS=$(dcrdex_shell_join_args "$@")

export SHELL
SHELL=$(which bash)

rm -rf "${DCRDEX_DATA_DIR}"
mkdir -p "${DCRDEX_DATA_DIR}"

dcrdex_write_build_scripts "${DCRDEX_DATA_DIR}" "${DCRDEX_DATA_DIR}/dcrdex"
dcrdex_build_binary "${DCRDEX_DATA_DIR}"

dcrdex_reset_database dcrdex_simnet_test

dcrdex_write_config \
  "${DCRDEX_DATA_DIR}" \
  dcrdex_simnet_test \
  127.0.0.1:17273 \
  127.0.0.1:16542 \
  127.0.0.1:17539 \
  "" \
  "" \
  ""
dcrdex_write_static_rpc_cert_pair "${DCRDEX_DATA_DIR}"
dcrdex_write_evm_protocol_overrides "${DCRDEX_DATA_DIR}"
dcrdex_write_dexadm_script \
  "${DCRDEX_DATA_DIR}/dexadm" \
  "${DCRDEX_DATA_DIR}/rpc.cert" \
  127.0.0.1:16542
dcrdex_write_run_script \
  "${DCRDEX_DATA_DIR}/run" \
  "${DCRDEX_DATA_DIR}" \
  "${DCRDEX_DATA_DIR}/dcrdex" \
  donedex

cat > "${DCRDEX_DATA_DIR}/quit" <<EOF
#!/usr/bin/env bash
tmux send-keys -t ${SESSION}:0 C-c
EOF

if [ -n "${NODERELAY}" ]; then
  cat >> "${DCRDEX_DATA_DIR}/quit" <<EOF
tmux send-keys -t ${SESSION}:1 C-c
tmux send-keys -t ${SESSION}:2 C-c
tmux wait-for donenoderelaybtc
tmux wait-for donenoderelaydcr
EOF
fi

cat >> "${DCRDEX_DATA_DIR}/quit" <<EOF
tmux wait-for donedex
tmux kill-session -t ${SESSION}
EOF
chmod +x "${DCRDEX_DATA_DIR}/quit"

RUN_CMD="${DCRDEX_DATA_DIR}/run"
if [ -n "${FORWARDED_ARGS}" ]; then
  RUN_CMD="${RUN_CMD} ${FORWARDED_ARGS}"
fi

echo "Starting dcrdex"
dcrdex_tmux_start_session "${SESSION}" dcrdex "${DCRDEX_DATA_DIR}"

if [ -n "${NODERELAY}" ]; then
  tmux send-keys -t "${SESSION}:0" "export NODERELAY=1" C-m

  BTC_NODERELAY_ID="btc_a21afba3"
  DCR_NODERELAY_ID="dcr_a21afba3"

  SOURCENODE_DIR=$(realpath "${DCRDEX_HARNESS_DIR}/../../../server/noderelay/cmd/sourcenode/")
  (
    cd "${SOURCENODE_DIR}"
    go build -o "${DCRDEX_DATA_DIR}/sourcenode"
  )

  RPC_PORT=20556
  RELAYFILE="${DCRDEX_DATA_DIR}/data/simnet/noderelay/relay-files/${BTC_NODERELAY_ID}.relayfile"

  dcrdex_tmux_init_window "${SESSION}" 1 sourcenode_btc "${DCRDEX_DATA_DIR}"
  tmux send-keys -t "${SESSION}:1" "sleep 4" C-m
  tmux send-keys -t "${SESSION}:1" "./sourcenode --port ${RPC_PORT} --relayfile ${RELAYFILE}; tmux wait-for -S donenoderelaybtc" C-m

  RPC_PORT=19561
  RELAYFILE="${DCRDEX_DATA_DIR}/data/simnet/noderelay/relay-files/${DCR_NODERELAY_ID}.relayfile"
  DCRD_CERT="${TEST_ROOT}/dcr/alpha/rpc.cert"

  dcrdex_tmux_init_window "${SESSION}" 2 sourcenode_dcr "${DCRDEX_DATA_DIR}"
  tmux send-keys -t "${SESSION}:2" "sleep 4" C-m
  tmux send-keys -t "${SESSION}:2" "./sourcenode --port ${RPC_PORT} --relayfile ${RELAYFILE} --localcert ${DCRD_CERT}; tmux wait-for -S donenoderelaydcr" C-m
fi

tmux send-keys -t "${SESSION}:0" "${RUN_CMD}" C-m
tmux select-window -t "${SESSION}:0"
tmux attach-session -t "${SESSION}"
