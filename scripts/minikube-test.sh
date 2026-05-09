#!/usr/bin/env bash
# minikube-test.sh – end-to-end test using minikube + SoftHSM2.
#
# Prerequisites:
#   - minikube, kubectl, docker installed
#   - Go toolchain available (for building)
#
# Bootstrap order (avoids the chicken-and-egg problem with KMS v2):
#   1. Build the Docker image
#   2. Start minikube WITHOUT encryption config
#   3. Load the image; create ConfigMap, Secret and directories
#   4. Deploy the KMS plugin as a static pod; wait for its Unix socket
#   5. Patch kube-apiserver manifest to add --encryption-provider-config
#      (and the required volume mounts) – kubelet restarts the apiserver
#   6. Create a secret and verify round-trip encryption
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
IMAGE_NAME="kms-hsm-plugin:latest"
MINIKUBE_PROFILE="k8s-hsm-kmsv2"
TOKEN_LABEL="k8s-kms"
SO_PIN="12345678"
USER_PIN="1234"
KMS_SOCKET="/run/kms-plugin/kms-plugin.sock"

cd "$PROJECT_DIR"

# ------------------------------------------------------------------ build
echo "==> Building Docker image"
docker build -t "$IMAGE_NAME" -f deploy/Dockerfile .

# ------------------------------------------------------------------ minikube
echo "==> Deleting any existing cluster to start from a clean state"
minikube delete --profile "$MINIKUBE_PROFILE" 2>/dev/null || true

echo "==> Starting fresh minikube cluster"
# Start WITHOUT --encryption-provider-config; the KMS plugin must be
# running and its Unix socket present before we enable encryption
# (see TEST-README.md for the bootstrap rationale).
minikube start --profile "$MINIKUBE_PROFILE"

echo "==> Loading image into minikube"
minikube image load --profile "$MINIKUBE_PROFILE" "$IMAGE_NAME"

# ------------------------------------------------------------------ setup
echo "==> Preparing directories on minikube node"
minikube ssh --profile "$MINIKUBE_PROFILE" -- \
  "sudo mkdir -p /var/lib/softhsm/tokens /run/kms-plugin /etc/kubernetes/enc"

echo "==> Writing softhsm2.conf to minikube node"
_SOFTHSM_CONF=$(mktemp /tmp/softhsm2-XXXXXX.conf)
cat > "$_SOFTHSM_CONF" << 'CONF'
directories.tokendir = /var/lib/softhsm/tokens/
objectstore.backend = file
log.level = INFO
CONF
minikube cp --profile "$MINIKUBE_PROFILE" "$_SOFTHSM_CONF" /etc/softhsm2.conf
rm "$_SOFTHSM_CONF"

# ------------------------------------------------------------------ KMS plugin
echo "==> Deploying KMS plugin static pod"
_MANIFEST=$(mktemp /tmp/kms-pod-XXXXXX.yaml)
sed "s/PKCS11_PIN_PLACEHOLDER/$USER_PIN/g" deploy/kms-plugin-pod.yaml > "$_MANIFEST"
minikube cp --profile "$MINIKUBE_PROFILE" \
  "$_MANIFEST" \
  /etc/kubernetes/manifests/kms-hsm-plugin.yaml
rm "$_MANIFEST"

echo "==> Waiting for KMS socket at ${KMS_SOCKET} (up to 120 s) ..."
for i in $(seq 1 24); do
  if minikube ssh --profile "$MINIKUBE_PROFILE" -- \
       "test -S ${KMS_SOCKET}" 2>/dev/null; then
    echo "    KMS socket is ready"
    break
  fi
  if [ "$i" -eq 24 ]; then
    echo "ERROR: KMS socket did not appear after 120 s"
    kubectl --context "$MINIKUBE_PROFILE" \
      describe pod kms-hsm-plugin -n kube-system 2>/dev/null || true
    exit 1
  fi
  echo "    waiting... ($i/24)"
  sleep 5
done

# ------------------------------------------------------------------ enable encryption
echo "==> Copying encryption config to minikube node"
minikube cp --profile "$MINIKUBE_PROFILE" \
  deploy/encryption-config.yaml \
  /etc/kubernetes/enc/encryption-config.yaml

echo "==> Patching kube-apiserver manifest to enable encryption-at-rest"
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

# 1. Add encryption flag after --tls-private-key-file
if "--encryption-provider-config" not in txt:
    txt = re.sub(r'(    - --tls-private-key-file=[^\n]+)',
                 r'\1\n' + ENC_FLAG, txt)

# 2. Add enc volumeMount after last existing volumeMount
if "name: enc" not in txt:
    txt = re.sub(
        r'(    - mountPath: /usr/share/ca-certificates\n      name: usr-share-ca-certificates\n      readOnly: true)',
        r'\1\n' + ENC_MOUNT, txt)

# 3. Add kms-socket volumeMount after enc mount
if "name: kms-socket" not in txt:
    txt = txt.replace(ENC_MOUNT, ENC_MOUNT + "\n" + KMS_MOUNT)

# 4. Add enc volume after last existing volume
if "\n    name: enc\n" not in txt:
    txt = re.sub(
        r'(  - hostPath:\n      path: /usr/share/ca-certificates\n      type: DirectoryOrCreate\n    name: usr-share-ca-certificates)',
        r'\1\n' + ENC_VOL, txt)

# 5. Add kms-socket volume after enc volume
if "\n    name: kms-socket\n" not in txt:
    txt = txt.replace(ENC_VOL, ENC_VOL + "\n" + KMS_VOL)

with open(MANIFEST, "w") as f:
    f.write(txt)

print("kube-apiserver manifest patched with encryption-provider-config and KMS socket mount")
PYEOF

minikube cp --profile "$MINIKUBE_PROFILE" "$_PATCH" /tmp/patch-apiserver.py
minikube ssh --profile "$MINIKUBE_PROFILE" -- sudo python3 /tmp/patch-apiserver.py
rm "$_PATCH"

echo "==> Waiting for kube-apiserver to restart (up to 3 min) ..."
sleep 10  # give kubelet a moment to detect the manifest change
for i in $(seq 1 36); do
  if kubectl --context "$MINIKUBE_PROFILE" get --raw /healthz &>/dev/null; then
    echo "    kube-apiserver is healthy"
    break
  fi
  if [ "$i" -eq 36 ]; then
    echo "ERROR: kube-apiserver did not recover in time"
    exit 1
  fi
  echo "    waiting... ($i/36)"
  sleep 5
done

echo "==> Waiting for KMS plugin pod to be Ready (up to 90 s) ..."
for i in $(seq 1 18); do
  # Static pods get the node name appended; match with a prefix filter
  POD_NAME=$(kubectl --context "$MINIKUBE_PROFILE" get pods -n kube-system \
    --no-headers -o custom-columns=NAME:.metadata.name 2>/dev/null \
    | grep '^kms-hsm-plugin' | head -1)
  if [ -n "$POD_NAME" ]; then
    kubectl --context "$MINIKUBE_PROFILE" wait --for=condition=Ready \
      pod/"$POD_NAME" -n kube-system --timeout=30s && break
  fi
  sleep 5
done

# ------------------------------------------------------------------ verify
echo "==> Creating test secret"
kubectl --context "$MINIKUBE_PROFILE" create secret generic kms-test-secret \
  --from-literal=mykey=myvalue --dry-run=client -o yaml \
  | kubectl --context "$MINIKUBE_PROFILE" apply -f -

echo "==> Reading secret back"
VALUE=$(kubectl --context "$MINIKUBE_PROFILE" get secret kms-test-secret \
  -o jsonpath='{.data.mykey}' | base64 -d)
if [ "$VALUE" = "myvalue" ]; then
  echo "SUCCESS – secret round-trip verified"
else
  echo "FAILURE – expected 'myvalue', got '$VALUE'"
  exit 1
fi

echo ""
echo "==> Minikube integration test passed."

