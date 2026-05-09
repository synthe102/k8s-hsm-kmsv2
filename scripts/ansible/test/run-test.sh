#!/usr/bin/env bash
# run-test.sh – Build the Docker image, start a fresh minikube cluster, then
# run the Ansible playbook to deploy and verify KMS encryption-at-rest using
# SoftHSM2.
#
# Prerequisites:
#   minikube, kubectl, docker, ansible (pip install ansible)
#
# Usage:
#   ./scripts/ansible/test/run-test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

MINIKUBE_PROFILE="k8s-hsm-kmsv2"
IMAGE_NAME="kms-hsm-plugin:latest"

cd "$PROJECT_DIR"

# ── 1. Build image ─────────────────────────────────────────────────────────────
echo "==> Building Docker image"
docker build -t "$IMAGE_NAME" -f deploy/Dockerfile .

# ── 2. Clean-start minikube ────────────────────────────────────────────────────
echo "==> Deleting any existing cluster to start from a clean state"
minikube delete --profile "$MINIKUBE_PROFILE" 2>/dev/null || true

echo "==> Starting fresh minikube cluster"
minikube start --profile "$MINIKUBE_PROFILE"

echo "==> Loading image into minikube"
minikube image load --profile "$MINIKUBE_PROFILE" "$IMAGE_NAME"

# ── 3. Generate Ansible inventory from minikube node details ──────────────────
echo "==> Generating Ansible inventory from minikube node details"
# minikube's Docker driver maps SSH (port 22 inside the container) to a random
# host port on 127.0.0.1.  We read that port via docker inspect.
_HOST="127.0.0.1"
_PORT=$(docker inspect "$MINIKUBE_PROFILE" \
  | python3 -c "import json,sys; c=json.load(sys.stdin)[0]; print(c['NetworkSettings']['Ports']['22/tcp'][0]['HostPort'])")
_USER="docker"
_KEY=$(minikube ssh-key --profile "$MINIKUBE_PROFILE")

INVENTORY=$(mktemp /tmp/minikube-inventory-XXXXXX.ini)
trap "rm -f $INVENTORY" EXIT

# NOTE: Ansible INI inventory does not support backslash line continuation;
# the entire entry must be on a single line.
cat > "$INVENTORY" << EOF
[minikube]
minikube-node ansible_host=${_HOST} ansible_port=${_PORT} ansible_user=${_USER} ansible_ssh_private_key_file=${_KEY} ansible_ssh_common_args='-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null'
EOF

# ── 4. Run playbook ────────────────────────────────────────────────────────────
echo "==> Running Ansible playbook"
cd "$SCRIPT_DIR"
ansible-playbook -i "$INVENTORY" deploy-kms-test.yml

# ── 5. Verify round-trip encryption ───────────────────────────────────────────
echo "==> Verifying secret round-trip encryption"
cd "$PROJECT_DIR"
kubectl --context "$MINIKUBE_PROFILE" create secret generic kms-test-secret \
  --from-literal=mykey=myvalue --dry-run=client -o yaml \
  | kubectl --context "$MINIKUBE_PROFILE" apply -f -

VALUE=$(kubectl --context "$MINIKUBE_PROFILE" get secret kms-test-secret \
  -o jsonpath='{.data.mykey}' | base64 -d)
if [ "$VALUE" = "myvalue" ]; then
  echo "SUCCESS – secret round-trip verified"
else
  echo "FAILURE – expected 'myvalue', got '${VALUE}'"
  exit 1
fi

echo ""
echo "==> Ansible test deployment complete."
