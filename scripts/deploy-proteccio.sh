#!/usr/bin/env bash
# deploy-proteccio.sh – Deploy the KMS v2 HSM plugin backed by a Proteccio HSM
# on an existing Kubernetes cluster.
#
# Prerequisites (read PROD-README.md before running):
#   - kubectl configured and pointing at the target cluster
#   - docker (or buildah) available for building the image
#   - Proteccio client files staged in deploy/proteccio/
#       libnethsm.so, proteccio.rc, proteccio.crt
#   - SSH access to each control-plane node (for apiserver patching)
#
# Usage:
#   export KUBECONFIG=/path/to/kubeconfig        # optional, defaults to ~/.kube/config
#   export REGISTRY=registry.example.com/myorg   # where to push the image
#   export TOKEN_LABEL=HSM1-V1                   # PKCS#11 token label on the Proteccio
#   export USER_PIN=<proteccio-pin>              # PKCS#11 user PIN
#   export CONTROL_PLANE_SSH_USER=ubuntu          # SSH user for control-plane nodes
#   ./scripts/deploy-proteccio.sh
#
# All variables can also be set in the script below (less convenient in CI).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

# ── Configuration ──────────────────────────────────────────────────────────────
REGISTRY="${REGISTRY:-}"                      # e.g. registry.example.com/myorg
IMAGE_NAME="kms-hsm-plugin-proteccio"
IMAGE_TAG="${IMAGE_TAG:-latest}"
FULL_IMAGE="${REGISTRY:+${REGISTRY}/}${IMAGE_NAME}:${IMAGE_TAG}"

TOKEN_LABEL="${TOKEN_LABEL:?Set TOKEN_LABEL to the Proteccio PKCS#11 token label}"
USER_PIN="${USER_PIN:?Set USER_PIN to the Proteccio user PIN}"
KMS_SOCKET="/run/kms-plugin/kms-plugin.sock"
KMS_KEY_LABEL="${KMS_KEY_LABEL:-k8s-kms-kek}"
AUTO_CREATE_KEY="${AUTO_CREATE_KEY:-false}"     # set to "true" to auto-provision the KEK

# SSH access to control-plane nodes (needed for apiserver manifest patching)
CONTROL_PLANE_NODES="${CONTROL_PLANE_NODES:-}"         # space-separated IPs or hostnames
CONTROL_PLANE_SSH_USER="${CONTROL_PLANE_SSH_USER:-ubuntu}"
CONTROL_PLANE_SSH_KEY="${CONTROL_PLANE_SSH_KEY:-${HOME}/.ssh/id_rsa}"

cd "$PROJECT_DIR"

# ── Helpers ────────────────────────────────────────────────────────────────────
die() { echo "ERROR: $*" >&2; exit 1; }
info() { echo "==> $*"; }

node_ssh() {
  local node="$1"; shift
  ssh -i "$CONTROL_PLANE_SSH_KEY" -o StrictHostKeyChecking=no \
      "${CONTROL_PLANE_SSH_USER}@${node}" "$@"
}

node_scp() {
  local src="$1" dst="$2" node="$3"
  scp -i "$CONTROL_PLANE_SSH_KEY" -o StrictHostKeyChecking=no \
      "$src" "${CONTROL_PLANE_SSH_USER}@${node}:${dst}"
}

# ── 1. Validate Proteccio client files ─────────────────────────────────────────
info "Checking Proteccio client files in deploy/proteccio/"
for f in deploy/proteccio/libnethsm.so deploy/proteccio/proteccio.rc deploy/proteccio/proteccio.crt; do
  [ -f "$f" ] || die "Missing $f – see PROD-README.md for how to obtain these files"
done

# ── 2. Build and push image ────────────────────────────────────────────────────
info "Building Docker image: ${FULL_IMAGE}"
docker build -t "${FULL_IMAGE}" -f deploy/Dockerfile.proteccio .

if [ -n "$REGISTRY" ]; then
  info "Pushing ${FULL_IMAGE}"
  docker push "${FULL_IMAGE}"
else
  info "REGISTRY not set – skipping push (assume image is already on the nodes)"
fi

# ── 3. Deploy KMS plugin static pod on each control-plane node ────────────────
if [ -z "$CONTROL_PLANE_NODES" ]; then
  die "Set CONTROL_PLANE_NODES to a space-separated list of control-plane node IPs/hostnames"
fi

for NODE in $CONTROL_PLANE_NODES; do
  info "Deploying KMS plugin on ${NODE}"

  # Create socket directory
  node_ssh "$NODE" "sudo mkdir -p /run/kms-plugin"

  # Inject PIN and token label into the pod manifest
  _MANIFEST=$(mktemp /tmp/kms-pod-XXXXXX.yaml)
  sed \
    -e "s/PKCS11_PIN_PLACEHOLDER/${USER_PIN}/g" \
    -e "s/PROTECCIO_TOKEN_PLACEHOLDER/${TOKEN_LABEL}/g" \
    -e "s|kms-hsm-plugin-proteccio:latest|${FULL_IMAGE}|g" \
    deploy/kms-plugin-pod-proteccio.yaml > "$_MANIFEST"

  # Optionally inject --auto-create-key
  if [ "$AUTO_CREATE_KEY" = "true" ]; then
    sed -i "s|# - --auto-create-key|- --auto-create-key|" "$_MANIFEST"
  fi

  node_scp "$_MANIFEST" /tmp/kms-hsm-plugin.yaml "$NODE"
  rm "$_MANIFEST"
  node_ssh "$NODE" "sudo mv /tmp/kms-hsm-plugin.yaml /etc/kubernetes/manifests/kms-hsm-plugin.yaml"

  # ── 4. Wait for KMS socket ───────────────────────────────────────────────────
  info "Waiting for KMS socket at ${KMS_SOCKET} on ${NODE} (up to 120 s)..."
  for i in $(seq 1 24); do
    if node_ssh "$NODE" "test -S ${KMS_SOCKET}" 2>/dev/null; then
      echo "    KMS socket is ready on ${NODE}"
      break
    fi
    if [ "$i" -eq 24 ]; then
      die "KMS socket did not appear on ${NODE} after 120 s"
    fi
    echo "    waiting... ($i/24)"
    sleep 5
  done

  # ── 5. Copy encryption config ────────────────────────────────────────────────
  info "Copying encryption config to ${NODE}"
  node_ssh "$NODE" "sudo mkdir -p /etc/kubernetes/enc"
  node_scp deploy/encryption-config.yaml "$NODE:/tmp/encryption-config.yaml"
  node_ssh "$NODE" "sudo mv /tmp/encryption-config.yaml /etc/kubernetes/enc/encryption-config.yaml"

  # ── 6. Patch kube-apiserver manifest ────────────────────────────────────────
  info "Patching kube-apiserver manifest on ${NODE}"
  _PATCH=$(mktemp /tmp/patch-apiserver-XXXXXX.py)
  cat > "$_PATCH" << 'PYEOF'
import re, sys

MANIFEST = "/etc/kubernetes/manifests/kube-apiserver.yaml"

ENC_FLAG  = "    - --encryption-provider-config=/etc/kubernetes/enc/encryption-config.yaml"
ENC_MOUNT = "    - mountPath: /etc/kubernetes/enc\n      name: enc\n      readOnly: true"
KMS_MOUNT = "    - mountPath: /run/kms-plugin\n      name: kms-socket"
ENC_VOL   = "  - hostPath:\n      path: /etc/kubernetes/enc\n      type: DirectoryOrCreate\n    name: enc"
KMS_VOL   = "  - hostPath:\n      path: /run/kms-plugin\n      type: DirectoryOrCreate\n    name: kms-socket"

with open(MANIFEST) as f:
    txt = f.read()

if "--encryption-provider-config" not in txt:
    txt = re.sub(r'(    - --tls-private-key-file=[^\n]+)',
                 r'\1\n' + ENC_FLAG, txt)

if "name: enc" not in txt:
    txt = re.sub(
        r'(    - mountPath: /usr/share/ca-certificates\n      name: usr-share-ca-certificates\n      readOnly: true)',
        r'\1\n' + ENC_MOUNT, txt)

if "name: kms-socket" not in txt:
    txt = txt.replace(ENC_MOUNT, ENC_MOUNT + "\n" + KMS_MOUNT)

if "\n    name: enc\n" not in txt:
    txt = re.sub(
        r'(  - hostPath:\n      path: /usr/share/ca-certificates\n      type: DirectoryOrCreate\n    name: usr-share-ca-certificates)',
        r'\1\n' + ENC_VOL, txt)

if "\n    name: kms-socket\n" not in txt:
    txt = txt.replace(ENC_VOL, ENC_VOL + "\n" + KMS_VOL)

with open(MANIFEST, "w") as f:
    f.write(txt)

print("kube-apiserver manifest patched")
PYEOF

  node_scp "$_PATCH" "$NODE:/tmp/patch-apiserver.py"
  rm "$_PATCH"
  node_ssh "$NODE" "sudo python3 /tmp/patch-apiserver.py"

  # ── 7. Wait for apiserver recovery ──────────────────────────────────────────
  info "Waiting for kube-apiserver to recover on ${NODE} (up to 3 min)..."
  sleep 10
  for i in $(seq 1 36); do
    if kubectl get --raw /healthz &>/dev/null; then
      echo "    kube-apiserver is healthy"
      break
    fi
    if [ "$i" -eq 36 ]; then
      die "kube-apiserver did not recover on ${NODE}"
    fi
    echo "    waiting... ($i/36)"
    sleep 5
  done
done

# ── 8. Verify round-trip encryption ────────────────────────────────────────────
info "Verifying secret round-trip encryption"
kubectl create secret generic kms-test-secret \
  --from-literal=mykey=myvalue --dry-run=client -o yaml \
  | kubectl apply -f -

VALUE=$(kubectl get secret kms-test-secret -o jsonpath='{.data.mykey}' | base64 -d)
if [ "$VALUE" = "myvalue" ]; then
  echo "SUCCESS – secret round-trip verified"
else
  echo "FAILURE – expected 'myvalue', got '${VALUE}'"
  exit 1
fi

echo ""
echo "==> Proteccio KMS plugin deployed successfully."
echo ""
echo "IMPORTANT: See PROD-README.md for key-rotation and etcd backup procedures."
