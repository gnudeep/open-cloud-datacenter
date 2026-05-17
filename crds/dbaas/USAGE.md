# dbaas controller — usage guide

Copy-paste-ready `kubectl` commands for installing the controller and
managing `DBInstance` databases on a Harvester cluster. Read
[`ARCHITECTURE.md`](./ARCHITECTURE.md) first if you want to understand what
each command actually does.

## Prerequisites

- A Harvester HCI cluster (KubeVirt + CDI installed by Harvester itself).
- A Multus `NetworkAttachmentDefinition` representing the VLAN the database
  VM should live on — typically the same VLAN as your Rancher/RKE2
  workloads. Note the `namespace/name`; you will set it as `spec.networkRef`.
- A `kubectl` context pointed at that cluster:
  ```sh
  kubectl cluster-info
  kubectl get nodes
  kubectl get net-attach-def -A          # confirm your VLAN NAD exists
  ```
- The manager image built and pushed where the cluster can pull it. From
  the repo root (`crds/dbaas/`):
  ```sh
  make docker-buildx IMG=<registry>/<name>:<tag>     # cross-build linux/amd64
  ```
- Optional but useful: `virtctl` (KubeVirt's CLI) for VM console access,
  `psql` on your laptop for direct database checks.

## Install the controller

```sh
# Install the DBInstance CRD into the cluster.
make install

# Verify:
kubectl get crd dbinstances.dbaas.opencloud.wso2.com
kubectl explain dbinstances.spec | head -30

# Deploy the manager + RBAC (namespace dbaas-system).
make deploy IMG=<registry>/<name>:<tag>

# Verify:
kubectl -n dbaas-system get deploy,pod
kubectl -n dbaas-system logs deploy/dbaas-controller-manager --tail=20
```

The Deployment runs one replica in `dbaas-system`. The container exposes
`:8081` (health) and serves the REST gateway on `:8080`.

## Create a database

Create a tenant namespace (or use an existing one). The controller drops
every Harvester object for this instance into the same namespace:

```sh
kubectl create namespace tenant-acme
```

Write a `DBInstance` manifest. Only `dbInstanceClass`, `allocatedStorage`,
and `networkRef` are required:

```sh
cat <<EOF | kubectl apply -f -
apiVersion: dbaas.opencloud.wso2.com/v1alpha1
kind: DBInstance
metadata:
  name: orders-prod
  namespace: tenant-acme
spec:
  dbInstanceClass: db.t3.medium           # 2 CPU, 4 GiB RAM, 150 conns
  allocatedStorage: 50                    # GiB for pgdata
  networkRef: default/vm-net-100          # ns/name of your Multus NAD
  engineVersion: "16"
  dbName: orders
  masterUsername: orders_admin
  manageMasterUserPassword: true          # autogenerate + store in Secret
  deletionProtection: true
  running: true
EOF
```

Watch it provision — the columns come from the CRD's `printcolumn` markers:

```sh
kubectl -n tenant-acme get dbi -w
# NAME           PHASE        CLASS           ENDPOINT       AGE
# orders-prod    creating     db.t3.medium                  3s
# orders-prod    creating     db.t3.medium                  18s
# orders-prod    creating     db.t3.medium    10.50.0.42    95s
# orders-prod    available    db.t3.medium    10.50.0.42    2m10s
```

Look at the detail view at any point:

```sh
kubectl -n tenant-acme describe dbi orders-prod
kubectl -n tenant-acme get dbi orders-prod -o yaml | yq '.status'
```

Useful jsonpath one-liners:

```sh
# Current phase + internal step
kubectl -n tenant-acme get dbi orders-prod \
  -o jsonpath='{.status.phase}{"\t"}{.status.provisioningPhase}{"\n"}'

# JDBC URL
kubectl -n tenant-acme get dbi orders-prod \
  -o jsonpath='{.status.endpoint.jdbcUrl}{"\n"}'

# All owned Harvester objects this instance has created so far
kubectl -n tenant-acme get dbi orders-prod \
  -o jsonpath='{.status.resources}{"\n"}' | jq .
```

## Get credentials and the CA cert

The credentials Secret is named `pg-<name>-credentials` in the instance's
namespace. Its name is also echoed at `status.masterUserSecret.name`:

```sh
SECRET=$(kubectl -n tenant-acme get dbi orders-prod \
  -o jsonpath='{.status.masterUserSecret.name}')

# Admin user and password
kubectl -n tenant-acme get secret "$SECRET" \
  -o jsonpath='{.data.admin_user}' | base64 -d ; echo
kubectl -n tenant-acme get secret "$SECRET" \
  -o jsonpath='{.data.admin_password}' | base64 -d ; echo
```

The CA cert lives on the CR itself at `status.caCertPem` (so clients can
pin it without pulling the Secret):

```sh
kubectl -n tenant-acme get dbi orders-prod \
  -o jsonpath='{.status.caCertPem}' > orders-prod-ca.crt
```

## Connect to the database

You need a pod *on the VLAN the VM is bridged to* (`spec.networkRef`).
Run a one-off `psql` pod with that NAD attached:

```sh
# Replace 'default/vm-net-100' with your spec.networkRef.
kubectl -n tenant-acme run pgtest \
  --rm -it --restart=Never \
  --image=postgres:16 \
  --overrides='{
    "metadata": {"annotations": {"k8s.v1.cni.cncf.io/networks": "default/vm-net-100"}}
  }' -- bash

# Inside the pod:
psql "host=10.50.0.42 port=5432 dbname=orders user=orders_admin sslmode=require"
# password: paste from the secret above
```

For verified TLS, mount the CA cert from a ConfigMap:

```sh
kubectl -n tenant-acme create configmap orders-prod-ca \
  --from-file=ca.crt=orders-prod-ca.crt
```

Then add a volume to the debug pod that mounts the ConfigMap at `/ca/` and
use `sslmode=verify-ca sslrootcert=/ca/ca.crt`.

## Stop / start / modify

These are all `kubectl patch` against the CR. The reconciler picks the
change up within one reconcile loop and reflects it in `status.phase`.

```sh
# Stop  — frees CPU/RAM, keeps storage. status.phase -> stopping -> stopped
kubectl -n tenant-acme patch dbi orders-prod --type=merge \
  -p '{"spec":{"running":false}}'

# Start — boots the VM back up
kubectl -n tenant-acme patch dbi orders-prod --type=merge \
  -p '{"spec":{"running":true}}'

# Resize (live)  — VM and DataVolume resize in parallel. status.phase -> modifying
kubectl -n tenant-acme patch dbi orders-prod --type=merge \
  -p '{"spec":{"dbInstanceClass":"db.m5.large","allocatedStorage":200}}'

# Toggle deletion protection
kubectl -n tenant-acme patch dbi orders-prod --type=merge \
  -p '{"spec":{"deletionProtection":false}}'
```

You can confirm a modify took effect by watching `observedGeneration`
catch up to `metadata.generation`:

```sh
kubectl -n tenant-acme get dbi orders-prod -o jsonpath='gen={.metadata.generation} observed={.status.observedGeneration}{"\n"}'
```

## Delete a database

The controller refuses to delete an instance with `deletionProtection: true`
— that's by design.

```sh
# 1. Disable protection if it's on.
kubectl -n tenant-acme patch dbi orders-prod --type=merge \
  -p '{"spec":{"deletionProtection":false}}'

# 2. Delete the CR. The finalizer dbaas.opencloud.wso2.com/cleanup runs
#    TeardownAll, which removes the ServiceMonitor, VM, DataVolume and
#    credentials Secret in parallel. The NAD and the namespace are NOT
#    touched — they belong to the cluster operator.
kubectl -n tenant-acme delete dbi orders-prod

# 3. Confirm the VM is gone.
kubectl -n tenant-acme get vm,vmi,dv,secret -l \
  dbaas.opencloud.wso2.com/instance=orders-prod
```

If a delete hangs, the finalizer is the usual cause — `kubectl get dbi
orders-prod -o yaml` and look at `metadata.finalizers` and `status.message`.

## Use the REST gateway instead of kubectl

The gateway exposes the same six operations as plain HTTP. It listens on
`:8080` in the manager pod. There's no Service for it in this version, so
forward the port from the pod directly:

```sh
kubectl -n dbaas-system port-forward \
  deploy/dbaas-controller-manager 8080:8080 &

GATEWAY=http://localhost:8080
```

The gateway reads/writes `DBInstance`s in the namespace named by the
`DBAAS_DEFAULT_NAMESPACE` env var (default `default`). If you want it to
operate on `tenant-acme`, edit the manager Deployment and set
`DBAAS_DEFAULT_NAMESPACE=tenant-acme`.

**Authentication.** Every request except `/healthz` must include an
`Authorization: Bearer <token>` header. The gateway forwards the call to
the K8s API server using *that* token, so authn / RBAC / audit happen on
your identity — not the controller's. Easiest way to get a token:

```sh
# Option A: short-lived token for an existing ServiceAccount the caller controls.
TOKEN=$(kubectl create token <serviceaccount> -n <ns> --duration=1h)

# Option B: your own user token (if the cluster has OIDC; depends on setup).
# Option C: read it from your kubeconfig (works for token-based auth).
TOKEN=$(kubectl config view --raw -o jsonpath='{.users[0].user.token}')

AUTH="Authorization: Bearer $TOKEN"
```

That ServiceAccount must have RBAC for `dbinstances` in the gateway's
namespace; otherwise the K8s API server returns `403 Forbidden` and the
gateway propagates it. A minimal Role for a tenant operator:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: dbaas-operator
  namespace: tenant-acme
rules:
- apiGroups: ["dbaas.opencloud.wso2.com"]
  resources: ["dbinstances"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
```

The curl examples below assume `$AUTH` is set as above.

```sh
# create
curl -sS -H "$AUTH" -X POST "$GATEWAY/dbinstances" \
  -H "Content-Type: application/json" \
  -d '{
    "metadata": {"name": "orders-prod"},
    "spec": {
      "dbInstanceClass": "db.t3.medium",
      "allocatedStorage": 50,
      "networkRef": "default/vm-net-100",
      "dbName": "orders",
      "masterUsername": "orders_admin",
      "manageMasterUserPassword": true,
      "deletionProtection": true
    }
  }' | jq .

# describe
curl -sS -H "$AUTH" "$GATEWAY/dbinstances/orders-prod" | jq '.status'

# modify — only dbInstanceClass and allocatedStorage are actually applied today.
# The reconciler refuses any change to immutable fields (dbName, networkRef,
# osImage, port, …) and reports `status.phase=failed` with a clear message.
# Backup-window / retention fields are accepted by the PATCH endpoint but
# not yet implemented end-to-end on the VM side.
curl -sS -H "$AUTH" -X PATCH "$GATEWAY/dbinstances/orders-prod" \
  -H "Content-Type: application/json" \
  -d '{
    "dbInstanceClass": "db.m5.large",
    "allocatedStorage": 200
  }' | jq '.status.phase'

# stop / start
curl -sS -H "$AUTH" -X POST "$GATEWAY/dbinstances/orders-prod/stop"  | jq '.status.phase'
curl -sS -H "$AUTH" -X POST "$GATEWAY/dbinstances/orders-prod/start" | jq '.status.phase'

# delete (after disabling protection via PATCH first)
curl -sS -H "$AUTH" -X PATCH "$GATEWAY/dbinstances/orders-prod" \
  -H "Content-Type: application/json" \
  -d '{"deletionProtection": false}'
curl -sS -H "$AUTH" -X DELETE "$GATEWAY/dbinstances/orders-prod" | jq .

# /healthz is unauthenticated:
curl -sS "$GATEWAY/healthz"
```

Responses you may see from the auth layer:

| Status | Meaning |
| --- | --- |
| `401 Unauthorized` | Missing or unparseable `Authorization: Bearer …` header, or the K8s API server didn't recognize the token. |
| `403 Forbidden` | Token is valid but the identity lacks RBAC for that operation/namespace. |

## Troubleshooting

Quick checks, in order of how often they're useful:

```sh
# Phase + last status message for every instance:
kubectl get dbi -A \
  -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,\
PHASE:.status.phase,PROV:.status.provisioningPhase,MSG:.status.message

# Controller logs (one Pod, leader-elected if multi-replica):
kubectl -n dbaas-system logs deploy/dbaas-controller-manager --tail=200 -f

# Everything owned by an instance, in one query:
kubectl -n tenant-acme get vm,vmi,dv,pvc,secret,svc,servicemonitor \
  -l dbaas.opencloud.wso2.com/instance=orders-prod

# VMI phase + IP (KubeVirt's view of the VM):
kubectl -n tenant-acme get vmi pg-orders-prod \
  -o jsonpath='{.status.phase}{"\t"}{.status.interfaces[*].ipAddress}{"\n"}'

# DataVolume progress (CDI):
kubectl -n tenant-acme get dv -o wide

# Open a serial console to the VM (needs virtctl):
virtctl -n tenant-acme console pg-orders-prod
# Inside the VM, useful first-boot trails:
#   sudo journalctl -u cloud-final
#   sudo cat /var/log/cloud-init-output.log
#   sudo cat /etc/dbaas/bootstrap.env  /etc/dbaas/bootstrap.sh
#   sudo systemctl status postgresql
```

Common failure modes:

| Symptom | Where to look |
| --- | --- |
| `status.message: "NetworkRefMissing"` | `spec.networkRef` is empty — set it to the `namespace/name` of an existing Multus NAD. |
| Phase stuck on `NetworkProvisioned`/`StorageProvisioned` long enough to notice | `kubectl describe dv pg-<name>-data` — CDI usually surfaces the cause. |
| Phase stuck on `WaitingForCloudInit` | VMI exists but cloud-init hasn't reported the VLAN IP. Console into the VM and tail `/var/log/cloud-init-output.log`. The VLAN must reach upstream package mirrors. |
| Phase `failed`, `status.message: "InvalidClass: …"` | `spec.dbInstanceClass` is not in the `InstanceClasses` map in `api/v1alpha1/dbinstance_types.go`. |
| `kubectl delete dbi` hangs | Check `metadata.finalizers`. `TeardownAll` runs deletes in parallel and ignores errors; if it can't make progress, the controller logs will show why. |
| `kubectl explain dbi.spec` returns nothing | CRD didn't install or wasn't regenerated — re-run `make manifests install`. |

## Uninstall

```sh
# Remove the manager Deployment + RBAC + Service.
make undeploy

# Remove the CRD. WARNING: this also deletes every DBInstance CR cluster-wide
# (and via finalizer, every VM/DataVolume/Secret/ServiceMonitor they own).
# Run `kubectl get dbi -A` first if you're not sure.
make uninstall
```

If something fights you on uninstall, `kubectl edit dbi <name>` and clear
`metadata.finalizers` to let the CR get garbage-collected. Do that only
after confirming the underlying Harvester objects are actually gone.
