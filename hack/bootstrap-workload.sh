#!/usr/bin/env bash
# Bootstrap a workload cluster for postgres-mc-operator.
# Creates the namespace, ServiceAccount, RBAC, and a kubeconfig Secret.
#
# Usage:
#   bash hack/bootstrap-workload.sh <kubeconfig> <cluster-name> <output-kubeconfig>
#
# Example:
#   bash hack/bootstrap-workload.sh /tmp/kube-eu.yaml eu-west-1 /tmp/kubeconfig-eu-for-tech.yaml

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

# WorkloadClusterRole (doit être appliqué depuis la racine du repo)
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

echo "--> ClusterRole postgres-mc-operator-workload"
kubectl apply -f "$REPO_ROOT/config/rbac/workload-role.yaml"

# ClusterRoleBinding
echo "--> ClusterRoleBinding"
kubectl create clusterrolebinding postgres-mc-remote \
  --clusterrole=postgres-mc-operator-workload \
  --serviceaccount=postgres:postgres-mc-remote \
  --dry-run=client -o yaml | kubectl apply -f -

# Token (1 an pour le lab)
echo "--> Génération du token (8760h)"
TOKEN=$(kubectl -n postgres create token postgres-mc-remote --duration=8760h)

# Infos cluster
SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
CA_DATA=$(kubectl config view --minify --raw \
  -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')

# Écriture du kubeconfig de sortie
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
echo "==> OK — kubeconfig écrit dans $OUTPUT"
echo "    Server : $SERVER"
echo ""
echo "    Prochaine étape : stocker ce kubeconfig comme Secret sur le cluster tech"
echo "    kubectl -n postgres-system create secret generic kubeconfig-${CLUSTER_NAME} \\"
echo "      --from-file=kubeconfig=${OUTPUT}"
echo ""
