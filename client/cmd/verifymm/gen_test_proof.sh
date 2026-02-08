cd /Users/marci/git/dcrdex

# Dec 2025 (UTC): 2025-12-01T00:00:00Z .. 2026-01-01T00:00:00Z
START_MS=1764547200000
END_MS=1767225600000

PROOF_PATH=/tmp/mmproof-dec2025.json

PROOFGEN_START_MS=$START_MS \
PROOFGEN_END_MS=$END_MS \
PROOFGEN_UNCLOSED=5 \
PROOFGEN_SEED=1 \
PROOFGEN_OUT=$PROOF_PATH \
go test -tags proofgen ./client/cmd/verifymm -run TestGenerateProof -v

# Now run the verify + PDF commands that were generated:
cat "${PROOF_PATH%.json}.commands.txt"
