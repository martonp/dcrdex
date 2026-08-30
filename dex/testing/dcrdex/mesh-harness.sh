#!/usr/bin/env bash
# Tmux script that configures and runs two mesh-connected dcrdex instances.

set -e

source "$(dirname "$0")/harness-common.sh"

if [ -n "${NODERELAY}" ]; then
  echo "mesh-harness.sh does not yet support NODERELAY"
  exit 1
fi

TEST_ROOT="${TEST_ROOT:-$HOME/dextest}"
DCRDEX_MESH_ROOT="${TEST_ROOT}/dcrdex-mesh"
ALPHA_DIR="${DCRDEX_MESH_ROOT}/alpha"
BETA_DIR="${DCRDEX_MESH_ROOT}/beta"
SHARED_SIGKEY="${DCRDEX_MESH_ROOT}/sigkey"
SESSION="dcrdex-mesh-harness"
FORWARDED_ARGS=$(dcrdex_shell_join_args "$@")

export SHELL
SHELL=$(which bash)

rm -rf "${DCRDEX_MESH_ROOT}"
mkdir -p "${ALPHA_DIR}" "${BETA_DIR}"

dcrdex_write_build_scripts "${DCRDEX_MESH_ROOT}" "${DCRDEX_MESH_ROOT}/dcrdex"
dcrdex_build_binary "${DCRDEX_MESH_ROOT}"

dcrdex_reset_database dcrdex_simnet_alpha
dcrdex_reset_database dcrdex_simnet_beta

dcrdex_write_static_rpc_cert_pair "${ALPHA_DIR}"
dcrdex_write_static_rpc_cert_pair "${BETA_DIR}"
dcrdex_write_evm_protocol_overrides "${ALPHA_DIR}"
dcrdex_write_evm_protocol_overrides "${BETA_DIR}"

dcrdex_write_config \
  "${ALPHA_DIR}" \
  dcrdex_simnet_alpha \
  127.0.0.1:17273 \
  127.0.0.1:16542 \
  127.0.0.1:17539 \
  127.0.0.1:17573 \
  wss://127.0.0.1:17583 \
  "${BETA_DIR}/rpc.cert" \
  127.0.0.1:17273
dcrdex_write_config \
  "${BETA_DIR}" \
  dcrdex_simnet_beta \
  127.0.0.1:17283 \
  127.0.0.1:16552 \
  127.0.0.1:17539 \
  127.0.0.1:17583 \
  wss://127.0.0.1:17573 \
  "${ALPHA_DIR}/rpc.cert" \
  127.0.0.1:17283

echo "dexprivkeypath=${SHARED_SIGKEY}" >> "${ALPHA_DIR}/dcrdex.conf"
echo "dexprivkeypath=${SHARED_SIGKEY}" >> "${BETA_DIR}/dcrdex.conf"

dcrdex_write_run_script \
  "${DCRDEX_MESH_ROOT}/run-alpha" \
  "${ALPHA_DIR}" \
  "${DCRDEX_MESH_ROOT}/dcrdex" \
  donealpha
dcrdex_write_run_script \
  "${DCRDEX_MESH_ROOT}/run-beta" \
  "${BETA_DIR}" \
  "${DCRDEX_MESH_ROOT}/dcrdex" \
  donebeta
dcrdex_write_dexadm_script \
  "${DCRDEX_MESH_ROOT}/dexadm-alpha" \
  "${ALPHA_DIR}/rpc.cert" \
  127.0.0.1:16542
dcrdex_write_dexadm_script \
  "${DCRDEX_MESH_ROOT}/dexadm-beta" \
  "${BETA_DIR}/rpc.cert" \
  127.0.0.1:16552

cat > "${DCRDEX_MESH_ROOT}/quit" <<EOF
#!/usr/bin/env bash
tmux send-keys -t ${SESSION}:1 C-c
tmux send-keys -t ${SESSION}:2 C-c
tmux wait-for donealpha
tmux wait-for donebeta
tmux kill-session -t ${SESSION}
EOF
chmod +x "${DCRDEX_MESH_ROOT}/quit"

ALPHA_CMD="${DCRDEX_MESH_ROOT}/run-alpha"
BETA_CMD="${DCRDEX_MESH_ROOT}/run-beta"
if [ -n "${FORWARDED_ARGS}" ]; then
  ALPHA_CMD="${ALPHA_CMD} ${FORWARDED_ARGS}"
  BETA_CMD="${BETA_CMD} ${FORWARDED_ARGS}"
fi

echo "Starting mesh-connected dcrdex nodes"
dcrdex_tmux_start_session "${SESSION}" harness-ctl "${DCRDEX_MESH_ROOT}"
dcrdex_tmux_init_window "${SESSION}" 1 alpha "${ALPHA_DIR}"
dcrdex_tmux_init_window "${SESSION}" 2 beta "${BETA_DIR}"

tmux send-keys -t "${SESSION}:1" "${ALPHA_CMD}" C-m
tmux send-keys -t "${SESSION}:2" "sleep 2" C-m
tmux send-keys -t "${SESSION}:2" "${BETA_CMD}" C-m

tmux select-window -t "${SESSION}:0"
if [ -z "${NOATTACH:-}" ]; then
  tmux attach-session -t "${SESSION}"
fi
