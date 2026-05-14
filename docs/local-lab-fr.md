# Lab local — 3 clusters kind sur Mac

Ce guide crée un environnement de test complet sur ta machine : un cluster technique
et deux clusters workload, avec la réplication PostgreSQL cross-cluster fonctionnelle
et le failover bidirectionnel.

---

## Ce que tu vas obtenir

```
Docker (OrbStack / Docker Desktop)
│
├── kind: tech          localhost:6443   ← opérateur + CRD PostgresMC
├── kind: eu-west-1     localhost:6444   ← Zalando + PostgreSQL PRIMARY
└── kind: us-east-1     localhost:6445   ← Zalando + PostgreSQL STANDBY
        │                       │
        └───── WAL stream ──────┘
               via NodePort 32432 (créé automatiquement par l'opérateur)
               réseau Docker kind (172.18.0.x)
```

L'opérateur crée et gère lui-même le service de réplication sur chaque cluster —
aucune IP à noter manuellement, aucun NodePort à créer à la main.
Après un failover EU→US, le retour US→EU fonctionne sans intervention.

**Durée** : ~15 minutes  
**Ressources** : ~3 Go RAM, 4 CPU

---

## Prérequis

```bash
brew install kind kubectl helm go
```

Docker Desktop ou OrbStack doit être lancé.

```bash
docker ps          # doit répondre sans erreur
kind version       # v0.x.x
kubectl version --client
helm version
go version         # >= 1.22
```

---

## Étape 1 — Créer les 3 clusters kind

Les clusters workload n'ont plus besoin d'`extraPortMappings` — l'opérateur gère
les services NodePort directement via les IPs Docker internes.

```bash
# Cluster technique
kind create cluster --name tech

# Clusters workload
kind create cluster --name eu-west-1
kind create cluster --name us-east-1
```

Vérifie :

```bash
kind get clusters
# eu-west-1
# tech
# us-east-1
```

Exporte les kubeconfigs :

```bash
kind get kubeconfig --name tech      > /tmp/kube-tech.yaml
kind get kubeconfig --name eu-west-1 > /tmp/kube-eu.yaml
kind get kubeconfig --name us-east-1 > /tmp/kube-us.yaml
```

---

## Étape 2 — Installer Zalando sur les clusters workload

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

Vérifie :

```bash
KUBECONFIG=/tmp/kube-eu.yaml kubectl -n postgres-operator get pods
KUBECONFIG=/tmp/kube-us.yaml kubectl -n postgres-operator get pods
# NAME                                 READY   STATUS    RESTARTS   AGE
# postgres-operator-...                1/1     Running   0          1m
```

---

## Étape 3 — Bootstrapper les clusters workload

Le script `hack/bootstrap-workload.sh` crée le namespace, le RBAC et génère
le kubeconfig que l'opérateur utilisera pour piloter chaque cluster workload.

> **Note réseau** : le script détecte automatiquement les clusters kind et remplace
> l'adresse `127.0.0.1` du kubeconfig kind par l'IP Docker interne du control-plane
> (`172.18.0.x:6443`). C'est indispensable quand l'opérateur tourne en pod — les pods
> ne peuvent pas joindre `127.0.0.1` du Mac hôte, mais tous les clusters kind partagent
> le même réseau Docker `kind` et peuvent se joindre via leurs IPs internes.

```bash
bash hack/bootstrap-workload.sh /tmp/kube-eu.yaml eu-west-1 /tmp/kubeconfig-eu-for-tech.yaml
bash hack/bootstrap-workload.sh /tmp/kube-us.yaml us-east-1 /tmp/kubeconfig-us-for-tech.yaml
```

---

## Étape 4 — Stocker les kubeconfigs sur le cluster tech

```bash
KUBECONFIG=/tmp/kube-tech.yaml kubectl create namespace postgres-system

KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system \
  create secret generic kubeconfig-eu-west-1 \
  --from-file=kubeconfig=/tmp/kubeconfig-eu-for-tech.yaml

KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system \
  create secret generic kubeconfig-us-east-1 \
  --from-file=kubeconfig=/tmp/kubeconfig-us-for-tech.yaml
```

Vérifie :

```bash
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system get secrets
# NAME                   TYPE     DATA   AGE
# kubeconfig-eu-west-1   Opaque   1      5s
# kubeconfig-us-east-1   Opaque   1      3s
```

---

## Étape 5 — Installer le CRD et lancer l'opérateur

```bash
KUBECONFIG=/tmp/kube-tech.yaml kubectl apply \
  -f config/crd/bases/pgmc.aurelops.com_postgresmcs.yaml
```

**Option A — depuis les sources** (recommandé pour le développement), dans un terminal dédié :

```bash
export KUBECONFIG=/tmp/kube-tech.yaml
export PATH="/opt/homebrew/bin:$PATH"

go run ./cmd/main.go
```

**Option B — depuis l'image Docker Hub** :

```bash
KUBECONFIG=/tmp/kube-tech.yaml kubectl create namespace postgres-system

# ClusterRole (permissions de l'opérateur)
KUBECONFIG=/tmp/kube-tech.yaml kubectl apply -f config/rbac/role.yaml

# ServiceAccount + binding
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system \
  create serviceaccount postgres-mc-operator

KUBECONFIG=/tmp/kube-tech.yaml kubectl create clusterrolebinding postgres-mc-operator \
  --clusterrole=postgres-mc-operator-role \
  --serviceaccount=postgres-system:postgres-mc-operator

KUBECONFIG=/tmp/kube-tech.yaml kubectl apply -f config/manager/deployment.yaml
```

> Le déploiement utilise `aurelops/postgres-mc-operator:latest` depuis Docker Hub.

Dans les deux cas, tu dois voir :

```
{"level":"info","msg":"starting manager"}
{"level":"info","msg":"Starting workers","controller":"postgresmc","worker count":1}
```

---

## Étape 6 — Créer le PostgresMC de test

Plus besoin de noter d'IP ni de créer de service à la main — `replicationService`
délègue tout ça à l'opérateur.

```bash
cat > /tmp/test-pgmc.yaml << 'EOF'
apiVersion: pgmc.aurelops.com/v1alpha1
kind: PostgresMC
metadata:
  name: test-postgres
  namespace: postgres-system
spec:
  teamId: test

  # L'opérateur crée automatiquement un NodePort 32432 sur chaque cluster
  # et découvre l'IP Docker du node — aucune config manuelle requise.
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
      storageClass: standard     # StorageClass par défaut de kind
    numberOfInstances: 1         # 1 suffit en local
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

## Étape 7 — Suivre le déploiement

### Statut global

```bash
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system get pgmc -w
# NAME            PHASE          PRIMARY     READY   AGE
# test-postgres   Provisioning   eu-west-1   False   15s
# test-postgres   Ready          eu-west-1   True    ~2m
```

### Endpoint découvert automatiquement

```bash
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system \
  get pgmc test-postgres -o jsonpath='{.status.clusters[*].endpoint}'
# 172.18.0.2:32432 172.18.0.3:32432
```

L'opérateur a détecté les IPs Docker des deux nodes et les expose dans le status.

### Ressources créées sur eu-west-1 (primaire)

```bash
KUBECONFIG=/tmp/kube-eu.yaml kubectl -n postgres get all
# NAME                    READY   STATUS    RESTARTS   AGE
# pod/test-eu-west-1-0    1/1     Running   0          90s

# NAME                              TYPE        PORT(S)
# service/test-eu-west-1            ClusterIP   5432/TCP
# service/test-eu-west-1-repl       ClusterIP   5432/TCP
# service/test-postgres-replication NodePort    5432:32432/TCP  ← créé par l'opérateur

KUBECONFIG=/tmp/kube-eu.yaml kubectl -n postgres get postgresql
# NAME             TEAM   VERSION   PODS   VOLUME   AGE   STATUS
# test-eu-west-1   test   16        1      1Gi      2m    Running
```

### Ressources créées sur us-east-1 (standby)

```bash
KUBECONFIG=/tmp/kube-us.yaml kubectl -n postgres get svc
# NAME                              TYPE           PORT(S)
# test-postgres-primary             ExternalName   5432/TCP  ← alias vers le primaire
# test-postgres-replication         NodePort       5432:32432/TCP  ← prêt si US devient primary

KUBECONFIG=/tmp/kube-us.yaml kubectl -n postgres get secrets
# standby.test-us-east-1.credentials.postgresql...   Opaque
# postgres.test-us-east-1.credentials.postgresql...  Opaque
```

### Vérifier la réplication

```bash
KUBECONFIG=/tmp/kube-eu.yaml kubectl -n postgres exec -it test-eu-west-1-0 -- \
  psql -U postgres -c "SELECT client_addr, state, sent_lsn, replay_lsn FROM pg_stat_replication;"

#  client_addr  | state     | sent_lsn  | replay_lsn
# --------------+-----------+-----------+-----------
#  172.18.0.x   | streaming | 0/5000000 | 0/5000000
```

Si `pg_stat_replication` est vide après 2 minutes, voir la section Pannes courantes.

---

## Tester le failover manuel (EU → US)

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

Vérifie que US est devenu primaire et EU standby :

```bash
KUBECONFIG=/tmp/kube-us.yaml kubectl -n postgres exec -it test-us-east-1-0 -- \
  psql -U postgres -c "SELECT pg_is_in_recovery();"
# → f  (false = primaire)

KUBECONFIG=/tmp/kube-eu.yaml kubectl -n postgres exec -it test-eu-west-1-0 -- \
  psql -U postgres -c "SELECT pg_is_in_recovery();"
# → t  (true = standby)
```

Le service `test-postgres-primary` sur EU pointe maintenant vers l'IP du node US,
découverte automatiquement par l'opérateur — **aucune modification manuelle**.

### Retour arrière (US → EU)

Le failover inverse fonctionne identiquement car l'opérateur a créé
`test-postgres-replication` (NodePort 32432) sur EU dès le début :

```bash
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system \
  patch pgmc test-postgres --type=json -p='[
    {"op": "replace", "path": "/spec/clusters/0/role", "value": "primary"},
    {"op": "replace", "path": "/spec/clusters/1/role", "value": "standby"}
  ]'
```

---

## Tester le failover automatique

Active l'auto-failover pour que l'opérateur promeuve automatiquement le standby
si le primaire est injoignable pendant 3 réconciliations consécutives (~90s) :

```bash
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system patch pgmc test-postgres \
  --type=merge -p='{"spec":{"autoFailover":{"enabled":true,"primaryUnreachableThreshold":3}}}'
```

Simule une panne du primaire :

```bash
kind delete cluster --name eu-west-1
```

Observe l'auto-promotion :

```bash
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system get pgmc -w
# NAME            PHASE       PRIMARY     READY
# test-postgres   Degraded    eu-west-1   False   ← failureCount monte
# test-postgres   FailingOver eu-west-1   False   ← seuil atteint
# test-postgres   Ready       us-east-1   True    ← us-east-1 promu
```

Le compteur est visible dans le status :

```bash
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system \
  get pgmc test-postgres -o jsonpath='{.status.primaryFailureCount}'
```

---

## Pannes courantes

### `pg_stat_replication` vide après 2 minutes

Le service de réplication n'a pas encore de endpoints. Vérifie :

```bash
# Le service créé par l'opérateur doit avoir des Endpoints
KUBECONFIG=/tmp/kube-eu.yaml kubectl -n postgres \
  describe svc test-postgres-replication
# Endpoints doit afficher une IP:5432, pas "<none>"
```

Si `Endpoints: <none>`, le pod Zalando n'est pas encore en `Running` ou le sélecteur
`spilo-role=master` ne matche pas encore (le pod démarre). Attends 30 secondes.

```bash
# Vérifie les labels du pod
KUBECONFIG=/tmp/kube-eu.yaml kubectl -n postgres get pods --show-labels | grep spilo-role
```

Vérifie la connectivité depuis un pod standby :

```bash
KUBECONFIG=/tmp/kube-us.yaml kubectl -n postgres exec -it test-us-east-1-0 -- \
  pg_isready -h test-postgres-primary.postgres.svc.cluster.local -p 32432
```

### L'endpoint affiché dans le status est vide

```bash
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system \
  get pgmc test-postgres -o jsonpath='{.status.clusters[*].endpoint}'
```

Si vide, l'opérateur n'a pas pu découvrir l'IP du node. Vérifie les logs de l'opérateur
pour `"ensuring replication service"`. Cause possible : le node n'est pas encore Ready.

```bash
KUBECONFIG=/tmp/kube-eu.yaml kubectl get nodes
```

### `storageClass: standard` introuvable

```bash
KUBECONFIG=/tmp/kube-eu.yaml kubectl get storageclass
```

Selon la version de kind, c'est `standard` ou `local-path`. Mets à jour
`postgresqlSpec.volume.storageClass` en conséquence.

### Erreur `acid.zalan.do/v1` unknown

```bash
KUBECONFIG=/tmp/kube-eu.yaml kubectl get crd postgresqls.acid.zalan.do
```

Si absent, Zalando n'est pas installé — reprend l'étape 2.

---

## Nettoyage complet

```bash
kind delete cluster --name tech
kind delete cluster --name eu-west-1
kind delete cluster --name us-east-1

rm -f /tmp/kube-tech.yaml /tmp/kube-eu.yaml /tmp/kube-us.yaml \
      /tmp/kubeconfig-eu-for-tech.yaml /tmp/kubeconfig-us-for-tech.yaml \
      /tmp/test-pgmc.yaml
```

---

## Résumé des commandes utiles

```bash
# Statut PostgresMC + endpoints découverts
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system get pgmc
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system describe pgmc test-postgres
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system \
  get pgmc test-postgres -o jsonpath='{range .status.clusters[*]}{.name}: {.endpoint}{"\n"}{end}'

# Événements
KUBECONFIG=/tmp/kube-tech.yaml kubectl -n postgres-system \
  get events --sort-by='.lastTimestamp' --field-selector involvedObject.name=test-postgres

# Services opérateur sur chaque cluster
KUBECONFIG=/tmp/kube-eu.yaml kubectl -n postgres get svc
KUBECONFIG=/tmp/kube-us.yaml kubectl -n postgres get svc

# État Zalando
KUBECONFIG=/tmp/kube-eu.yaml kubectl -n postgres get postgresql
KUBECONFIG=/tmp/kube-us.yaml kubectl -n postgres get postgresql

# Vérifier la réplication
KUBECONFIG=/tmp/kube-eu.yaml kubectl -n postgres exec -it test-eu-west-1-0 -- \
  psql -U postgres -c "SELECT client_addr, state FROM pg_stat_replication;"

# Connexion psql directe
KUBECONFIG=/tmp/kube-eu.yaml kubectl -n postgres exec -it test-eu-west-1-0 -- psql -U postgres
KUBECONFIG=/tmp/kube-us.yaml kubectl -n postgres exec -it test-us-east-1-0 -- psql -U postgres

# Logs opérateur (dans le terminal go run)
# Logs pods PostgreSQL
KUBECONFIG=/tmp/kube-eu.yaml kubectl -n postgres logs -f test-eu-west-1-0
KUBECONFIG=/tmp/kube-us.yaml kubectl -n postgres logs -f test-us-east-1-0
```
