#!/usr/bin/env bash
# setup-softhsm.sh – initialise a SoftHSM2 token for local development / CI.
set -euo pipefail

TOKEN_LABEL="${TOKEN_LABEL:-k8s-kms}"
SO_PIN="${SO_PIN:-12345678}"
USER_PIN="${USER_PIN:-1234}"
TOKEN_DIR="${TOKEN_DIR:-$(pwd)/tokens}"

echo "==> Creating token directory: $TOKEN_DIR"
mkdir -p "$TOKEN_DIR"

export SOFTHSM2_CONF="${SOFTHSM2_CONF:-$(pwd)/softhsm2.conf}"

# Write a local SoftHSM2 config if it doesn't exist.
if [ ! -f "$SOFTHSM2_CONF" ]; then
  cat > "$SOFTHSM2_CONF" <<EOF
directories.tokendir = $TOKEN_DIR
objectstore.backend = file
EOF
  echo "==> Wrote $SOFTHSM2_CONF"
fi

echo "==> Initialising token '$TOKEN_LABEL' (slot 0)"
softhsm2-util --init-token --slot 0 \
  --label "$TOKEN_LABEL" \
  --so-pin "$SO_PIN" \
  --pin "$USER_PIN"

echo "==> SoftHSM2 slots:"
softhsm2-util --show-slots

echo ""
echo "Done. To use this token set:"
echo "  export SOFTHSM2_CONF=$SOFTHSM2_CONF"
echo "  export PKCS11_PIN=$USER_PIN"
echo "  export TOKEN_LABEL=$TOKEN_LABEL"
