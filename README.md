# postgres-mc-operator

[![Docker Hub](https://img.shields.io/docker/v/aurelops/postgres-mc-operator?label=Docker%20Hub&logo=docker)](https://hub.docker.com/r/aurelops/postgres-mc-operator)
[![CI](https://github.com/aurelien-moreau/postgres-mc-operator/actions/workflows/ci.yaml/badge.svg)](https://github.com/aurelien-moreau/postgres-mc-operator/actions/workflows/ci.yaml)

Kubernetes operator for multi-cluster PostgreSQL, built on top of the [Zalando postgres-operator](https://github.com/zalando/postgres-operator).

A single `PostgresMC` resource on a **technical cluster** declares a PostgreSQL topology spanning multiple **workload clusters**. The operator provisions Zalando `postgresql` resources on each workload cluster, synchronises credentials, wires up cross-cluster streaming replication, and manages failover — all automatically.

---

## Architecture

```
┌─────────────────────────────────────────┐
│          Technical Cluster              │
│                                         │
│  ┌──────────────────┐                  │
│  │  postgres-mc-     │  reads kubeconfig│
│  │  operator         │──Secrets──────┐  │
│  └────────┬─────────┘               │  │
│           │ watches                  │  │
│  ┌────────▼─────────┐               │  │
│  │   PostgresMC CR   │               │  │
│  │  (spec + status)  │               │  │
│  └──────────────────┘               │  │
└─────────────────────────────────────┼──┘
                                      │
          ┌───────────────────────────┘
          │
     ┌────▼──────────────────────────────────┐
     │                                        │
┌────▼───────────┐          ┌────────────────▼─┐
│ Workload eu-west-1        │ Workload us-east-1 │
│                │          │                    │
│ postgresql     │ WAL      │ postgresql         │
│ (primary)      │─stream──►│ (standby)          │
│                │          │                    │
│ {pgmc}-replication        │ {pgmc}-replication  │
│ NodePort/LB    │          │ NodePort/LB         │
│ Zalando op     │          │ Zalando op         │
└────────────────┘          └────────────────────┘
```

**Technical cluster** — hosts the operator and the `PostgresMC` CRD. No PostgreSQL workloads run here.

**Workload clusters** — each runs the Zalando postgres-operator independently. The `postgres-mc-operator` drives them remotely via kubeconfig Secrets stored on the technical cluster. A `{pgmc}-replication` Service is created on **every** cluster so any cluster can serve as primary after a failover without manual reconfiguration.

---

## Prerequisites

| Component | Where | Version |
|-----------|-------|---------|
| Kubernetes | Technical cluster | ≥ 1.28 |
| Kubernetes | Each workload cluster | ≥ 1.28 |
| [Zalando postgres-operator](https://github.com/zalando/postgres-operator) | Each workload cluster | ≥ 1.11 |
| `kubectl` | Local / CI | any |
| `helm` (optional) | Local / CI | ≥ 3.12 |

Network requirement: workload cluster pods must be able to reach each other's replication endpoint. The operator creates and manages this service automatically (`NodePort` or `LoadBalancer`).

---

## Quick Start

### 1. Install the operator on the technical cluster

```bash
export KUBECONFIG=~/.kube/tech-cluster.yaml

kubectl create namespace postgres-system
kubectl apply -f config/crd/bases/pgmc.aurelops.com_postgresmcs.yaml
kubectl apply -f config/rbac/role.yaml

kubectl -n postgres-system create serviceaccount postgres-mc-operator
kubectl create clusterrolebinding postgres-mc-operator \
  --clusterrole=postgres-mc-operator-role \
  --serviceaccount=postgres-system:postgres-mc-operator

# Uses image: aurelops/postgres-mc-operator:latest from Docker Hub
kubectl apply -f config/manager/deployment.yaml
```

Verify:

```bash
kubectl -n postgres-system get pods -l app=postgres-mc-operator
# NAME                                    READY   STATUS    RESTARTS   AGE
# postgres-mc-operator-7d9f8b6c4-xq2kp   1/1     Running   0          30s
```

---

### 2. Bootstrap each workload cluster

Repeat for **every workload cluster**.

#### 2a. Install the Zalando postgres-operator

```bash
export KUBECONFIG=~/.kube/workload-eu-west-1.yaml

helm repo add zalando https://opensource.zalando.com/postgres-operator/charts/postgres-operator
helm repo update

helm install postgres-operator zalando/postgres-operator \
  --namespace postgres-operator \
  --create-namespace \
  --set configGeneral.workers=4
```

#### 2b. Create RBAC and generate kubeconfig

```bash
kubectl create namespace postgres
kubectl -n postgres create serviceaccount postgres-mc-remote
kubectl apply -f config/rbac/workload-role.yaml
kubectl create clusterrolebinding postgres-mc-remote \
  --clusterrole=postgres-mc-operator-workload \
  --serviceaccount=postgres:postgres-mc-remote

TOKEN=$(kubectl -n postgres create token postgres-mc-remote --duration=8760h)
SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
CA_DATA=$(kubectl config view --minify --raw \
  -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')

cat > /tmp/kubeconfig-eu-west-1.yaml << EOF
apiVersion: v1
kind: Config
clusters:
- name: eu-west-1
  cluster:
    server: ${SERVER}
    certificate-authority-data: ${CA_DATA}
contexts:
- name: eu-west-1
  context:
    cluster: eu-west-1
    user: postgres-mc-remote
    namespace: postgres
current-context: eu-west-1
users:
- name: postgres-mc-remote
  user:
    token: ${TOKEN}
EOF
```

Or use the helper script included in the repo:

```bash
bash hack/bootstrap-workload.sh ~/.kube/workload-eu-west-1.yaml eu-west-1 /tmp/kubeconfig-eu-west-1.yaml
```

#### 2c. Store the kubeconfig on the technical cluster

```bash
export KUBECONFIG=~/.kube/tech-cluster.yaml

kubectl -n postgres-system create secret generic kubeconfig-eu-west-1 \
  --from-file=kubeconfig=/tmp/kubeconfig-eu-west-1.yaml
```

Repeat steps **2a–2c** for every other workload cluster.

---

## Creating a PostgresMC cluster

```yaml
# my-postgres.yaml
apiVersion: pgmc.aurelops.com/v1alpha1
kind: PostgresMC
metadata:
  name: my-postgres
  namespace: postgres-system
spec:
  teamId: app

  # The operator creates a replication Service on every cluster automatically.
  # NodePort: for on-prem / kind environments.
  # LoadBalancer: for cloud environments (AWS, GCP, Azure).
  replicationService:
    type: LoadBalancer   # or NodePort with nodePort: 32432

  clusters:
    - name: eu-west-1
      clusterRef: kubeconfig-eu-west-1
      namespace: postgres
      role: primary

    - name: us-east-1
      clusterRef: kubeconfig-us-east-1
      namespace: postgres
      role: standby

  postgresqlSpec:
    volume:
      size: 10Gi
      storageClass: gp2
    numberOfInstances: 2
    users:
      appuser:
        - superuser
        - createdb
    databases:
      appdb: appuser
    postgresql:
      version: "16"
    resources:
      requests:
        cpu: 500m
        memory: 512Mi
      limits:
        cpu: "2"
        memory: 2Gi

  replication:
    mode: async
```

```bash
kubectl apply -f my-postgres.yaml

kubectl -n postgres-system get pgmc my-postgres -w
# NAME          PHASE          PRIMARY     READY   AGE
# my-postgres   Provisioning   eu-west-1   False   10s
# my-postgres   Ready          eu-west-1   True    90s
```

### What happens under the hood

1. Creates a `{name}-replication` Service (NodePort or LoadBalancer) on **every** cluster.
2. Discovers each cluster's external endpoint (node IP for NodePort, LB hostname for LoadBalancer) and stores it in `status.clusters[].endpoint`.
3. Applies a Zalando `postgresql` on `eu-west-1` (primary — no standby spec).
4. Waits for Zalando to create the credential Secrets (`postgres.*`, `standby.*`).
5. Copies those Secrets to `us-east-1` under the standby cluster name.
6. Creates `my-postgres-primary` alias Service on `us-east-1` pointing to `eu-west-1`'s discovered endpoint.
7. Applies a Zalando `postgresql` on `us-east-1` with:
   ```yaml
   standby:
     standby_host: my-postgres-primary.postgres.svc.cluster.local
     standby_port: "<replication-port>"
   ```
8. Zalando on `us-east-1` starts WAL streaming from the primary.

---

## Checking cluster status

```bash
kubectl -n postgres-system get pgmc

kubectl -n postgres-system describe pgmc my-postgres

# Discovered endpoints per cluster
kubectl -n postgres-system get pgmc my-postgres \
  -o jsonpath='{range .status.clusters[*]}{.name}: {.endpoint}{"\n"}{end}'
# eu-west-1: abc.eu-west-1.elb.amazonaws.com:5432
# us-east-1: abc.us-east-1.elb.amazonaws.com:5432
```

Example `describe` output:

```
Status:
  Phase:                Ready
  Current Primary:      eu-west-1
  Primary Failure Count: 0
  Clusters:
    Name: eu-west-1
      Role:     primary
      Endpoint: abc.eu-west-1.elb.amazonaws.com:5432
      Conditions:
        Reachable:            True
        PostgresqlReconciled: True
        Ready:                True
    Name: us-east-1
      Role:     standby
      Endpoint: abc.us-east-1.elb.amazonaws.com:5432
      Conditions:
        Reachable:            True
        PostgresqlReconciled: True
        Ready:                True
```

---

## Failover

### Manual failover (planned switchover)

Change the `role` field — the operator detects it and orchestrates the promotion:

```bash
kubectl -n postgres-system patch pgmc my-postgres --type=json -p='[
  {"op": "replace", "path": "/spec/clusters/0/role", "value": "standby"},
  {"op": "replace", "path": "/spec/clusters/1/role", "value": "primary"}
]'

kubectl -n postgres-system get pgmc my-postgres -w
# NAME          PHASE        PRIMARY     READY
# my-postgres   FailingOver  eu-west-1   True
# my-postgres   Ready        us-east-1   True
```

Because the operator created `my-postgres-replication` (NodePort or LB) on **both** clusters from the start, the reverse failover (US → EU) works immediately with no additional configuration.

### Failover sequence

1. Sets `status.phase = FailingOver`.
2. **Demotes** old primary: adds `standby` spec pointing to new primary's endpoint.
3. **Promotes** new primary: removes `standby` spec entirely.
4. **Updates** `my-postgres-primary` alias Service on all standbys to point to the new primary's discovered endpoint.
5. Sets `status.currentPrimary` to the new primary name.

### Automatic failover

Enable automatic promotion when the primary is unreachable:

```yaml
spec:
  autoFailover:
    enabled: true
    primaryUnreachableThreshold: 3  # reconcile cycles before promoting (~90s)
```

The operator increments `status.primaryFailureCount` on each cycle where the primary
is unreachable. When the count reaches the threshold, the first reachable standby is
promoted. The counter is reset to 0 when the primary becomes reachable again.

> **Default**: `enabled: false`. Disabled by default to avoid split-brain on transient
> network failures. Enable only when you have network-level fencing or accept the
> associated risk.

---

## Spec reference

```yaml
apiVersion: pgmc.aurelops.com/v1alpha1
kind: PostgresMC
spec:
  # Naming prefix for all Zalando clusters: {teamId}-{clusterName}
  teamId: <string>                          # required

  # Replication service — created on every cluster so any can serve as primary.
  replicationService:
    type: NodePort | LoadBalancer           # default: NodePort
    nodePort: <int>                         # default: 32432 (NodePort only, 30000–32767)

  clusters:
    - name: <string>                        # required — e.g. "eu-west-1"
                                            #   Pattern: ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$
      clusterRef: <string>                  # required — kubeconfig Secret name
      secretNamespace: <string>             # optional — defaults to PostgresMC namespace
      namespace: <string>                   # required — target namespace on workload cluster
      role: primary | standby               # required — exactly one must be primary
      externalHost: <string>                # optional — override auto-discovered endpoint
                                            #   (useful if node IP or LB address is fixed)
      externalPort: <int>                   # optional — override port (default: 5432 for LB,
                                            #   nodePort value for NodePort)

  # Passed verbatim to every Zalando postgresql.spec.
  # teamId and standby sections are injected automatically.
  # Full reference: https://github.com/zalando/postgres-operator/blob/master/docs/reference/cluster_manifest.md
  postgresqlSpec:
    volume:
      size: <string>          # e.g. "10Gi"
      storageClass: <string>  # e.g. "gp2", "standard"
    numberOfInstances: <int>
    users:
      <username>:
        - superuser | createdb | ...
    databases:
      <dbname>: <owner>
    postgresql:
      version: "<string>"     # e.g. "16"
    resources: {}             # standard k8s resource requests/limits

  replication:
    mode: async | sync        # default: async

  autoFailover:
    enabled: <bool>           # default: false
    primaryUnreachableThreshold: <int>  # default: 3 cycles (~90s)
```

### Kubeconfig Secret format

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: kubeconfig-<cluster-name>
  namespace: <same-namespace-as-PostgresMC>
type: Opaque
data:
  kubeconfig: <base64-encoded kubeconfig>   # key must be exactly "kubeconfig"
```

The kubeconfig's ServiceAccount must be bound to `postgres-mc-operator-workload`
ClusterRole on the workload cluster (see `config/rbac/workload-role.yaml`).

---

## Multi-cluster topology examples

### Two clusters (primary + standby)

```yaml
replicationService:
  type: LoadBalancer

clusters:
  - name: eu-west-1
    role: primary
  - name: us-east-1
    role: standby
```

```
eu-west-1 [primary] ──WAL──► us-east-1 [standby]
```

### Three clusters (primary + two standbys)

```yaml
replicationService:
  type: LoadBalancer

clusters:
  - name: eu-west-1
    role: primary
  - name: us-east-1
    role: standby
  - name: ap-southeast-1
    role: standby
```

```
eu-west-1 [primary] ──WAL──► us-east-1 [standby]
                    └──WAL──► ap-southeast-1 [standby]
```

After a failover to `us-east-1`, the operator automatically repoints `ap-southeast-1`
to stream from `us-east-1` using the discovered endpoint — no manual intervention.

---

## Troubleshooting

### PostgresMC stuck in `Provisioning`

```bash
kubectl -n postgres-system describe pgmc <name>
kubectl -n postgres-system get events --field-selector involvedObject.name=<name>
```

| Condition | False reason | Fix |
|-----------|-------------|-----|
| `Reachable` | `KubeconfigMissing` | Secret name/namespace doesn't match `clusterRef` |
| `Reachable` | `ClusterUnreachable` | Token expired; re-run bootstrap script |
| `PostgresqlReconciled` | `ZalandoApplyFailed` | Zalando operator not running on workload cluster |
| `Ready` | `CredentialsNotReady` | Zalando hasn't created credential Secrets yet — wait 30s |
| `Ready` | `ZalandoApplyFailed` | Check operator logs; replication service may still be provisioning |

### Replication service endpoint empty

```bash
kubectl -n postgres-system get pgmc <name> \
  -o jsonpath='{range .status.clusters[*]}{.name}: {.endpoint}{"\n"}{end}'
```

If empty for a cluster, the operator couldn't discover the endpoint. Possible causes:

- **NodePort**: no Ready node found — `kubectl get nodes` on the workload cluster.
- **LoadBalancer**: LB still provisioning (check `kubectl -n postgres get svc {name}-replication` — wait for `EXTERNAL-IP`).

### Verify the replication service selector

```bash
# The service must have Endpoints (not <none>)
kubectl -n postgres describe svc <pgmc-name>-replication

# Pods must have the spilo-role=master label
kubectl -n postgres get pods --show-labels | grep spilo-role
```

If `Endpoints: <none>`, the Zalando primary pod isn't Running yet or hasn't elected
a Patroni leader. Wait 30–60 seconds after the Zalando cluster reaches `Running`.

### Verify cross-cluster connectivity from a standby pod

```bash
# NodePort example
kubectl -n postgres exec -it <standby-pod> -- \
  pg_isready -h <pgmc-name>-primary.postgres.svc.cluster.local -p 32432

# LoadBalancer example (port 5432)
kubectl -n postgres exec -it <standby-pod> -- \
  pg_isready -h <pgmc-name>-primary.postgres.svc.cluster.local -p 5432
```

### Auto-failover triggered unexpectedly

Check the failure counter:

```bash
kubectl -n postgres-system get pgmc <name> \
  -o jsonpath='{.status.primaryFailureCount}'
```

If non-zero but primary looks healthy, the operator may be experiencing transient
connectivity issues to the tech cluster's API server. Consider increasing
`primaryUnreachableThreshold`.

### Rotate a workload cluster kubeconfig

```bash
export KUBECONFIG=~/.kube/workload-eu-west-1.yaml
kubectl -n postgres create token postgres-mc-remote --duration=8760h > /tmp/new-token

export KUBECONFIG=~/.kube/tech-cluster.yaml
kubectl -n postgres-system create secret generic kubeconfig-eu-west-1 \
  --from-file=kubeconfig=/tmp/kubeconfig-eu-west-1.yaml \
  --dry-run=client -o yaml | kubectl apply -f -
```

The operator detects the Secret's `resourceVersion` change and rebuilds the client
on the next reconcile.

---

## Development

```bash
make build          # build binary
make run            # run locally against current KUBECONFIG
make generate       # regenerate deepcopy functions
make manifests      # regenerate CRD + RBAC manifests
make test           # run tests with coverage
make docker-build IMG=aurelops/postgres-mc-operator:latest
make docker-push  IMG=aurelops/postgres-mc-operator:latest
```

The CI pipeline (`.github/workflows/ci.yaml`) builds and pushes to
[Docker Hub](https://hub.docker.com/r/aurelops/postgres-mc-operator) automatically
on every push to `main` and on version tags (`v*`).

### Project layout

```
api/v1alpha1/              CRD types (PostgresMC, conditions, deepcopy)
cmd/main.go                operator entrypoint and dependency wiring
internal/
  controller/              reconcile loop and sub-reconcilers
  clusters/                kubeconfig Secret → cached workload client
  zalando/                 Zalando postgresql Unstructured builder
  credentials/             credential Secret sync between clusters
  crosscluster/            primary alias Service (ExternalName / headless)
  replicationservice/      NodePort / LoadBalancer replication Service + endpoint discovery
  failover/                promotion / demotion orchestration
  status/                  condition helpers and phase aggregation
config/
  crd/bases/               generated CRD manifest
  rbac/                    ClusterRoles (tech cluster + workload clusters)
  samples/                 example PostgresMC manifests
docs/
  local-lab.md             3-cluster kind lab guide
hack/
  bootstrap-workload.sh    workload cluster RBAC + kubeconfig setup script
```

### Local lab

See [`docs/local-lab.md`](docs/local-lab.md) for a complete guide to running
three kind clusters on your Mac with full cross-cluster replication and failover.
