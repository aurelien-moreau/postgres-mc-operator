# Local Lab — 3 kind clusters on Mac

This guide sets up a complete test environment on your machine: one tech cluster
and two workload clusters, with working cross-cluster PostgreSQL replication
and bidirectional failover.

---

## What you will get

```
Docker (OrbStack / Docker Desktop)
│
├── kind: tech          localhost:6443   ← operator + PostgresMC CRD
├── kind: eu-west-1     localhost:6444   ← Zalando + PostgreSQL PRIMARY
└── kind: us-east-1     localhost:6445   ← Zalando + PostgreSQL STANDBY
        │                       │
        └───── WAL stream ──────┘
               via NodePort 32432 (created automatically by the operator)
               kind Docker network (172.18.0.x)
```

The operator creates and manages the replication service on each cluster —
no IPs to note manually, no NodePort to create by hand.
After a EU→US failover, the reverse US→EU failover works without any intervention.

**Duration**: ~15 minutes  
**Resources**: ~3 GB RAM, 4 CPU

---

## Prerequisites

```bash
brew install kind kubectl helm go
```

Docker Desktop or OrbStack must be running.

```bash
docker ps          # must respond without error
kind version       # v0.x.x
kubectl version --client
helm version
go version         # >= 1.22
```

---

## Step 1 — Create the 3 kind clusters

Workload clusters no longer need `extraPortMappings` — the operator manages
NodePort services directly via internal Docker IPs.

```bash
# Tech cluster
kind create cluster --name tech

# Workload clusters
kind create cluster --name eu-west-1
kind create cluster --name us-east-1
```

Verify:

```bash
kind get clusters
# eu-west-1
# tech
# us-east-1
```

Export kubeconfigs:

```bash
kind get kubeconfig --name tech      > /tmp/kube-tech.yaml
kind get kubeconfig --name eu-west-1 > /tmp/kube-eu.yaml
kind get kubeconfig --name us-east-1 > /tmp/kube-us.yaml
```

---

## Step 2 — Install Zalando on workload clusters

```bash
helm repo add zalando https://opensource.zalando.com/postgres-operator/charts/postgres-operator
helm repo update

helm install postgres-operator zalando/postgres-operator \
  --kubeconfig /tmp/kube-eu.yaml \
  --namespace postgres-operator \
  --create-namespace \
  --wait

helm install postgres-operator zalando/postgres-operator \
  --kubeconfig /tmp/kube-us.yaml \
  --namespace postgres-operator \
  --create-namespace \
  --wait
```

Verify:

```bash
KUBECONFIG=/tmp/kube-eu.yaml kubectl -n postgres-operator get pods
KUBECONFIG=/tmp/kube-us.yaml kubectl -n postgres-operator get pods
# NAME                                 READY   STATUS    RESTARTS   AGE
# postgres-operator-...                1/1     Running   0          1m
```

---

## Step 3 — Bootstrap workload clusters

The `hack/bootstrap-workload.sh` script creates the namespace, RBAC, and generates
the kubeconfig the operator will use to manage each workload cluster.

> **Network note**: the script automatically detects kind clusters and replaces
> the `127.0.0.1` address from the kind kubeconfig with the Docker-internal IP
> of the control-plane container (`172.18.0.x:6443`). This is required when the
> operator runs as a pod — pods cannot reach `127.0.0.1` of the host Mac, but all
> kind clusters share the same Docker `kind` network and can reach each other via
> their internal IPs.

```bash
bash hack/bootstrap-workload.sh /tmp/kube-eu.yaml eu-west-1 /tmp/kubeconfig-eu-for-tech.yaml
bash hack/bootstrap-workload.sh /tmp/kube-us.yaml us-east-1 /tmp/kubeconfig-us-for-tech.yaml
```

---

## Step 4 — Store kubeconfigs on the tech cluster

```bash
KUBECONFIG=/tmp/kube-tech.yaml kubectl create namespace postgres-system

KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system \
  create secret generic kubeconfig-eu-west-1 \
  --from-file=kubeconfig=/tmp/kubeconfig-eu-for-tech.yaml

KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system \
  create secret generic kubeconfig-us-east-1 \
  --from-file=kubeconfig=/tmp/kubeconfig-us-for-tech.yaml
```

Verify:

```bash
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system get secrets
# NAME                   TYPE     DATA   AGE
# kubeconfig-eu-west-1   Opaque   1      5s
# kubeconfig-us-east-1   Opaque   1      3s
```

---

## Step 5 — Install the CRD and start the operator

```bash
KUBECONFIG=/tmp/kube-tech.yaml kubectl apply \
  -f config/crd/bases/pgmc.aurelops.com_postgresmcs.yaml
```

**Option A — run from source** (recommended for development), in a dedicated terminal:

```bash
export KUBECONFIG=/tmp/kube-tech.yaml
export PATH="/opt/homebrew/bin:$PATH"

go run ./cmd/main.go
```

**Option B — run from the Docker Hub image**:

```bash
KUBECONFIG=/tmp/kube-tech.yaml kubectl create namespace postgres-system

# ClusterRole (permissions for the operator)
KUBECONFIG=/tmp/kube-tech.yaml kubectl apply -f config/rbac/role.yaml

# ServiceAccount + binding
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system \
  create serviceaccount postgres-mc-operator

KUBECONFIG=/tmp/kube-tech.yaml kubectl create clusterrolebinding postgres-mc-operator \
  --clusterrole=postgres-mc-operator-role \
  --serviceaccount=postgres-system:postgres-mc-operator

KUBECONFIG=/tmp/kube-tech.yaml kubectl apply -f config/manager/deployment.yaml
```

> The deployment uses `aurelops/postgres-mc-operator:latest` from Docker Hub.

Either way, you should see:

```
{"level":"info","msg":"starting manager"}
{"level":"info","msg":"Starting workers","controller":"postgresmc","worker count":1}
```

---

## Step 6 — Create the test PostgresMC

No need to note IPs or create services manually — `replicationService`
delegates all of that to the operator.

```bash
cat > /tmp/test-pgmc.yaml << 'EOF'
apiVersion: pgmc.aurelops.com/v1alpha1
kind: PostgresMC
metadata:
  name: test-postgres
  namespace: postgres-system
spec:
  teamId: test

  # The operator automatically creates a NodePort 32432 on each cluster
  # and discovers the node's Docker IP — no manual configuration required.
  replicationService:
    type: NodePort
    nodePort: 32432

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
      size: 1Gi
      storageClass: standard     # default kind StorageClass
    numberOfInstances: 1         # 1 is enough locally
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
        cpu: 100m
        memory: 256Mi

  replication:
    mode: async
EOF

KUBECONFIG=/tmp/kube-tech.yaml kubectl apply -f /tmp/test-pgmc.yaml
```

---

## Step 7 — Monitor the deployment

### Global status

```bash
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system get pgmc -w
# NAME            PHASE          PRIMARY     READY   AGE
# test-postgres   Provisioning   eu-west-1   False   15s
# test-postgres   Ready          eu-west-1   True    ~2m
```

### Automatically discovered endpoint

```bash
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system \
  get pgmc test-postgres -o jsonpath='{.status.clusters[*].endpoint}'
# 172.18.0.2:32432 172.18.0.3:32432
```

The operator detected the Docker IPs of both nodes and exposes them in the status.

### Resources created on eu-west-1 (primary)

```bash
KUBECONFIG=/tmp/kube-eu.yaml kubectl -n postgres get all
# NAME                    READY   STATUS    RESTARTS   AGE
# pod/test-eu-west-1-0    1/1     Running   0          90s

# NAME                              TYPE        PORT(S)
# service/test-eu-west-1            ClusterIP   5432/TCP
# service/test-eu-west-1-repl       ClusterIP   5432/TCP
# service/test-postgres-replication NodePort    5432:32432/TCP  ← created by operator

KUBECONFIG=/tmp/kube-eu.yaml kubectl -n postgres get postgresql
# NAME             TEAM   VERSION   PODS   VOLUME   AGE   STATUS
# test-eu-west-1   test   16        1      1Gi      2m    Running
```

### Resources created on us-east-1 (standby)

```bash
KUBECONFIG=/tmp/kube-us.yaml kubectl -n postgres get svc
# NAME                              TYPE           PORT(S)
# test-postgres-primary             ExternalName   5432/TCP  ← alias pointing to primary
# test-postgres-replication         NodePort       5432:32432/TCP  ← ready if US becomes primary

KUBECONFIG=/tmp/kube-us.yaml kubectl -n postgres get secrets
# standby.test-us-east-1.credentials.postgresql...   Opaque
# postgres.test-us-east-1.credentials.postgresql...  Opaque
```

### Verify replication

```bash
KUBECONFIG=/tmp/kube-eu.yaml kubectl -n postgres exec -it test-eu-west-1-0 -- \
  psql -U postgres -c "SELECT client_addr, state, sent_lsn, replay_lsn FROM pg_stat_replication;"

#  client_addr  | state     | sent_lsn  | replay_lsn
# --------------+-----------+-----------+-----------
#  172.18.0.x   | streaming | 0/5000000 | 0/5000000
```

If `pg_stat_replication` is empty after 2 minutes, see the Common Issues section.

---

## Test manual failover (EU → US)

```bash
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system \
  patch pgmc test-postgres --type=json -p='[
    {"op": "replace", "path": "/spec/clusters/0/role", "value": "standby"},
    {"op": "replace", "path": "/spec/clusters/1/role", "value": "primary"}
  ]'

KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system get pgmc -w
# NAME            PHASE        PRIMARY     READY
# test-postgres   FailingOver  eu-west-1   True
# test-postgres   Ready        us-east-1   True
```

Verify that US is now primary and EU standby:

```bash
KUBECONFIG=/tmp/kube-us.yaml kubectl -n postgres exec -it test-us-east-1-0 -- \
  psql -U postgres -c "SELECT pg_is_in_recovery();"
# → f  (false = primary)

KUBECONFIG=/tmp/kube-eu.yaml kubectl -n postgres exec -it test-eu-west-1-0 -- \
  psql -U postgres -c "SELECT pg_is_in_recovery();"
# → t  (true = standby)
```

The `test-postgres-primary` service on EU now points to the US node IP,
discovered automatically by the operator — **no manual change needed**.

### Failback (US → EU)

The reverse failover works identically because the operator created
`test-postgres-replication` (NodePort 32432) on EU from the start:

```bash
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system \
  patch pgmc test-postgres --type=json -p='[
    {"op": "replace", "path": "/spec/clusters/0/role", "value": "primary"},
    {"op": "replace", "path": "/spec/clusters/1/role", "value": "standby"}
  ]'
```

---

## Test automatic failover

Enable auto-failover so the operator automatically promotes the standby
if the primary is unreachable for 3 consecutive reconciliations (~90s):

```bash
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system patch pgmc test-postgres \
  --type=merge -p='{"spec":{"autoFailover":{"enabled":true,"primaryUnreachableThreshold":3}}}'
```

Simulate a primary failure:

```bash
kind delete cluster --name eu-west-1
```

Watch the auto-promotion:

```bash
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system get pgmc -w
# NAME            PHASE       PRIMARY     READY
# test-postgres   Degraded    eu-west-1   False   ← failureCount rising
# test-postgres   FailingOver eu-west-1   False   ← threshold reached
# test-postgres   Ready       us-east-1   True    ← us-east-1 promoted
```

The counter is visible in the status:

```bash
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system \
  get pgmc test-postgres -o jsonpath='{.status.primaryFailureCount}'
```

---

## Common issues

### `pg_stat_replication` empty after 2 minutes

The replication service has no endpoints yet. Check:

```bash
# The operator-created service must have Endpoints
KUBECONFIG=/tmp/kube-eu.yaml kubectl -n postgres \
  describe svc test-postgres-replication
# Endpoints must show an IP:5432, not "<none>"
```

If `Endpoints: <none>`, the Zalando pod is not yet `Running` or the
`spilo-role=master` selector does not match yet (pod still starting). Wait 30 seconds.

```bash
# Check pod labels
KUBECONFIG=/tmp/kube-eu.yaml kubectl -n postgres get pods --show-labels | grep spilo-role
```

Check connectivity from a standby pod:

```bash
KUBECONFIG=/tmp/kube-us.yaml kubectl -n postgres exec -it test-us-east-1-0 -- \
  pg_isready -h test-postgres-primary.postgres.svc.cluster.local -p 32432
```

### Endpoint in status is empty

```bash
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system \
  get pgmc test-postgres -o jsonpath='{.status.clusters[*].endpoint}'
```

If empty, the operator could not discover the node IP. Check the operator logs for
`"ensuring replication service"`. Likely cause: node not yet Ready.

```bash
KUBECONFIG=/tmp/kube-eu.yaml kubectl get nodes
```

### `storageClass: standard` not found

```bash
KUBECONFIG=/tmp/kube-eu.yaml kubectl get storageclass
```

Depending on the kind version, it may be `standard` or `local-path`. Update
`postgresqlSpec.volume.storageClass` accordingly.

### Error `acid.zalan.do/v1` unknown

```bash
KUBECONFIG=/tmp/kube-eu.yaml kubectl get crd postgresqls.acid.zalan.do
```

If absent, Zalando is not installed — go back to step 2.

---

## Full cleanup

```bash
kind delete cluster --name tech
kind delete cluster --name eu-west-1
kind delete cluster --name us-east-1

rm -f /tmp/kube-tech.yaml /tmp/kube-eu.yaml /tmp/kube-us.yaml \
      /tmp/kubeconfig-eu-for-tech.yaml /tmp/kubeconfig-us-for-tech.yaml \
      /tmp/test-pgmc.yaml
```

---

## Useful commands reference

```bash
# PostgresMC status + discovered endpoints
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system get pgmc
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system describe pgmc test-postgres
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system \
  get pgmc test-postgres -o jsonpath='{range .status.clusters[*]}{.name}: {.endpoint}{"\n"}{end}'

# Events
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system \
  get events --sort-by='.lastTimestamp' --field-selector involvedObject.name=test-postgres

# Operator services on each cluster
KUBECONFIG=/tmp/kube-eu.yaml kubectl -n postgres get svc
KUBECONFIG=/tmp/kube-us.yaml kubectl -n postgres get svc

# Zalando status
KUBECONFIG=/tmp/kube-eu.yaml kubectl -n postgres get postgresql
KUBECONFIG=/tmp/kube-us.yaml kubectl -n postgres get postgresql

# Check replication
KUBECONFIG=/tmp/kube-eu.yaml kubectl -n postgres exec -it test-eu-west-1-0 -- \
  psql -U postgres -c "SELECT client_addr, state FROM pg_stat_replication;"

# Direct psql connection
KUBECONFIG=/tmp/kube-eu.yaml kubectl -n postgres exec -it test-eu-west-1-0 -- psql -U postgres
KUBECONFIG=/tmp/kube-us.yaml kubectl -n postgres exec -it test-us-east-1-0 -- psql -U postgres

# Operator logs (in the go run terminal)
# PostgreSQL pod logs
KUBECONFIG=/tmp/kube-eu.yaml kubectl -n postgres logs -f test-eu-west-1-0
KUBECONFIG=/tmp/kube-us.yaml kubectl -n postgres logs -f test-us-east-1-0
```
