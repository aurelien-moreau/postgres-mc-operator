#!/usr/bin/env bash
# Bootstrap a workload cluster for postgres-mc-operator.
# Creates the namespace, ServiceAccount, RBAC, and a kubeconfig Secret.
#
# Usage:
#   bash hack/bootstrap-workload.sh <kubeconfig> <cluster-name> <output-kubeconfig>
#
# Example:
#   bash hack/bootstrap-workload.sh /tmp/kube-eu.yaml eu-west-1 /tmp/kubeconfig-eu-for-tech.yaml
#
# Kind note: the generated kubeconfig uses the Docker-internal IP of the kind
# control-plane (172.18.0.x:6443) so that the operator pod on the tech cluster
# can reach the workload API server. The localhost-mapped port in the kind
# kubeconfig is only reachable from the host Mac, not from inside a pod.

set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "Usage: $0 <kubeconfig-path> <cluster-name> <output-kubeconfig-path>"
  exit 1
fi

KUBECONFIG_WORKLOAD=$1
CLUSTER_NAME=$2
OUTPUT=$3

export KUBECONFIG=$KUBECONFIG_WORKLOAD

echo ""
echo "==> Bootstrapping workload cluster: $CLUSTER_NAME"
echo "    Kubeconfig : $KUBECONFIG_WORKLOAD"
echo "    Output     : $OUTPUT"
echo ""

# Namespace
echo "--> Namespace postgres"
kubectl create namespace postgres --dry-run=client -o yaml | kubectl apply -f -

# ServiceAccount
echo "--> ServiceAccount postgres-mc-remote"
kubectl -n postgres create serviceaccount postgres-mc-remote \
  --dry-run=client -o yaml | kubectl apply -f -

# WorkloadClusterRole
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

echo "--> ClusterRole postgres-mc-operator-workload"
kubectl apply -f "$REPO_ROOT/config/rbac/workload-role.yaml"

# RoleBinding (namespaced — the Role is scoped to the postgres namespace)
echo "--> RoleBinding"
kubectl -n postgres create rolebinding postgres-mc-remote \
  --role=postgres-mc-operator-workload \
  --serviceaccount=postgres:postgres-mc-remote \
  --dry-run=client -o yaml | kubectl apply -f -

# Token (1 year for the lab)
echo "--> Generating token (8760h)"
TOKEN=$(kubectl -n postgres create token postgres-mc-remote --duration=8760h)

# Resolve the API server address.
# For kind clusters: use the Docker-internal IP of the control-plane container
# so the operator pod on the tech cluster can reach the workload API server.
# Fallback to whatever the kubeconfig says (works for real clusters with a
# reachable server address).
CONTAINER="${CLUSTER_NAME}-control-plane"
DOCKER_IP=$(docker inspect "$CONTAINER" \
  --format '{{.NetworkSettings.Networks.kind.IPAddress}}' 2>/dev/null || true)

if [[ -n "$DOCKER_IP" ]]; then
  SERVER="https://${DOCKER_IP}:6443"
  echo "--> kind cluster detected, using Docker-internal IP: $SERVER"
else
  SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
  echo "--> using server from kubeconfig: $SERVER"
fi

CA_DATA=$(kubectl config view --minify --raw \
  -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')

# Write output kubeconfig
cat > "$OUTPUT" << EOF
apiVersion: v1
kind: Config
clusters:
- name: ${CLUSTER_NAME}
  cluster:
    server: ${SERVER}
    certificate-authority-data: ${CA_DATA}
contexts:
- name: ${CLUSTER_NAME}
  context:
    cluster: ${CLUSTER_NAME}
    user: postgres-mc-remote
    namespace: postgres
current-context: ${CLUSTER_NAME}
users:
- name: postgres-mc-remote
  user:
    token: ${TOKEN}
EOF

echo ""
echo "==> Done — kubeconfig written to $OUTPUT"
echo "    Server : $SERVER"
echo ""
echo "    Next step: store this kubeconfig as a Secret on the tech cluster"
echo "    kubectl -n postgres-system create secret generic kubeconfig-${CLUSTER_NAME} \\"
echo "      --from-file=kubeconfig=${OUTPUT}"
echo ""
