# dbaas controller — deployment verification recipes

Copy-paste checks to confirm a fresh deployment actually does what the
schema and docs claim. Use these after a new image rolls out, after a
controller change touches an end-to-end behaviour, or before declaring
"works on Harvester 1.7.x" on a different cluster.

Every recipe is annotated with what it proves and which commit /
DEF-NN / C-NN it traces back to, so a future review can re-derive the
intent from the diff log.

Companion docs:

- [`USAGE.md`](./USAGE.md) — install + day-2 operations
- [`DEPLOYMENT.md`](./DEPLOYMENT.md) — topology of a working deploy
- [`DEFERRED.md`](./DEFERRED.md) — what is intentionally not yet checked
- [`ARCHITECTURE.md`](./ARCHITECTURE.md) — design rationale

## Conventions used below

```sh
# Replace these for your environment:
KC=~/Downloads/local-2.yaml                          # path to your Harvester kubeconfig
TENANT=tenant-test                                   # the namespace you applied DBInstances in
INSTANCE=test-orders                                 # the DBInstance you want to verify
VLAN_NAD=default/vm-network                          # the Multus NAD spec.networkRef points at
```

Every block assumes `KUBECONFIG=$KC` or that you'll pass
`--kubeconfig="$KC"` to each call (the examples below show the latter
for clarity).

## 0. Pre-flight

Before any of the property checks, prove the basics:

```sh
# Cluster reachable + kubeconfig valid
kubectl --kubeconfig="$KC" cluster-info

# CRD installed
kubectl --kubeconfig="$KC" get crd dbinstances.dbaas.opencloud.wso2.com

# Manager Pod running the image you expect
kubectl --kubeconfig="$KC" -n dbaas-system get pod \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.phase}{"\t"}{.spec.containers[0].image}{"\n"}{end}'

# Manager log shows leader election + DBInstance controller started, no errors
kubectl --kubeconfig="$KC" -n dbaas-system logs deploy/dbaas-controller-manager --tail=10
```

Expected: `cluster-info` returns the Harvester K8s API URL; the CRD is
present; one manager Pod is `Running` with the right image; logs show
"Successfully acquired lease", "Starting Controller", "Starting
workers" — no panics, no immediate `error` lines.

## 1. End-to-end smoke (the canonical "does it work" test)

```sh
# Apply a small instance
kubectl --kubeconfig="$KC" create namespace "$TENANT" 2>/dev/null || true
cat <<EOF | kubectl --kubeconfig="$KC" apply -f -
apiVersion: dbaas.opencloud.wso2.com/v1alpha1
kind: DBInstance
metadata:
  name: $INSTANCE
  namespace: $TENANT
spec:
  dbInstanceClass: db.t3.small
  allocatedStorage: 20
  networkRef: $VLAN_NAD
  osImage: default/ubuntu-2404-server
  engineVersion: "16"
  dbName: testdb
  masterUsername: dbadmin
  manageMasterUserPassword: true
  staticNetwork:
    address: 192.168.40.50/24
    gateway: 192.168.40.1
    nameservers: [8.8.8.8, 1.1.1.1]
EOF

# Watch — expect available within ~3 min on Ubuntu cloudimg, ~80 s with a baked image
kubectl --kubeconfig="$KC" -n "$TENANT" get dbi -w
```

**Pass criteria:** `phase=available`, `status.endpoint.address` populated
with the static IP, no `status.message` indicating failure. Last
observed end-to-end time on Harvester 1.7.1 with `v0.2.7` and stock
Ubuntu cloudimg was 174 s.

## 2. Real SQL round-trip (proves the database actually serves)

Spin up a probe pod on the same VLAN, install `psql`, run a few
queries, exercise DDL + DML:

```sh
USER=$(kubectl --kubeconfig="$KC" -n "$TENANT" get secret \
  pg-${INSTANCE}-credentials -o jsonpath='{.data.admin_user}' | base64 -d)
PASS=$(kubectl --kubeconfig="$KC" -n "$TENANT" get secret \
  pg-${INSTANCE}-credentials -o jsonpath='{.data.admin_password}' | base64 -d)
IP=$(kubectl --kubeconfig="$KC" -n "$TENANT" get dbi $INSTANCE \
  -o jsonpath='{.status.endpoint.address}')

cat <<YAML | kubectl --kubeconfig="$KC" apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: verifyprobe
  namespace: default
  annotations:
    k8s.v1.cni.cncf.io/networks: $VLAN_NAD
spec:
  restartPolicy: Never
  containers:
  - name: probe
    image: nicolaka/netshoot
    command: ["sleep", "300"]
    securityContext: { capabilities: { add: ["NET_ADMIN", "NET_RAW"] } }
YAML

kubectl --kubeconfig="$KC" -n default wait --for=condition=Ready pod/verifyprobe --timeout=60s

kubectl --kubeconfig="$KC" -n default exec verifyprobe -- sh -c "
  apk add --no-cache postgresql-client >/dev/null 2>&1
  ip addr add 192.168.40.99/24 dev net1 2>/dev/null
  ip link set net1 up
  ip route add $IP/32 dev net1 2>/dev/null
  PGPASSWORD='$PASS' psql 'host=$IP port=5432 dbname=testdb user=$USER sslmode=require' -c \"
    SELECT current_user, current_database(), version();
    CREATE TABLE IF NOT EXISTS smoke (ts timestamptz DEFAULT now(), n int);
    INSERT INTO smoke (n) VALUES (1);
    SELECT count(*) FROM smoke;
  \"
"

kubectl --kubeconfig="$KC" -n default delete pod verifyprobe --wait=false
```

**Pass criteria:** `psql` connects, version reports the expected
PostgreSQL major (Ubuntu 24.04 → 16), `CREATE TABLE` + `INSERT` +
`SELECT` all succeed.

## 3. Property verification

Each property below has a quick `Probe`, the `Expected` output, and the
`Origin` (commit hash / C-NN / DEF-NN) so a future reader can re-derive
why this check exists.

### 3.1 Master role is not SUPERUSER (Origin: C-06, commit `b8fd553`)

```sh
PGPASSWORD=$PASS psql "host=$IP user=$USER dbname=testdb sslmode=require" -c "
  SELECT rolname, rolsuper, rolcreatedb, rolcreaterole
    FROM pg_roles WHERE rolname='$USER';
"
```

**Expected:** `rolsuper=f, rolcreatedb=t, rolcreaterole=t`. The two
positive attributes confirm the user can manage application databases
and roles; `rolsuper=f` confirms the engine's permission model still
applies to them.

Cross-check (any SUPERUSER-only function should refuse):

```sh
PGPASSWORD=$PASS psql "host=$IP user=$USER dbname=testdb sslmode=require" -c "
  SELECT pg_ls_dir('/etc/dbaas');
" 2>&1 | grep "permission denied"
```

**Expected:** `ERROR: permission denied for function pg_ls_dir`.

### 3.2 Credentials Secret has no `luks_key` (Origin: C-07, commit `8ff8508`)

```sh
kubectl --kubeconfig="$KC" -n "$TENANT" get secret \
  pg-${INSTANCE}-credentials -o jsonpath='{.data}' | jq 'keys'
```

**Expected, exact set (10 keys, NO `luks_key`):**

```json
[
  "admin_password", "admin_user",
  "ca_cert", "ca_key", "server_cert", "server_key",
  "exporter_password", "repl_password",
  "userdata", "networkdata"
]
```

If `luks_key` appears, the controller wasn't rebuilt/redeployed after
commit `8ff8508` (or an older instance is still around).

### 3.3 `/etc/dbaas/bootstrap.env` is shredded after bootstrap (Origin: C-08, commit `8ff8508`)

Requires shell access to the VM. The DBInstance applied above did **not**
set `spec.vmPassword`, so SSH password auth is off. Two paths:

**Path A — re-apply with a debug password.** Delete the existing
instance, apply with `spec.vmPassword: "tempdebug"`, then SSH from a
sidecar:

```sh
# … apply with vmPassword set, wait for available, then:
kubectl --kubeconfig="$KC" -n default exec verifyprobe -- sh -c "
  apk add --no-cache sshpass openssh-client >/dev/null 2>&1
  sshpass -p tempdebug ssh -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    -o PreferredAuthentications=password \
    -o PubkeyAuthentication=no \
    ubuntu@$IP 'ls -la /etc/dbaas/'
"
```

**Expected:** the listing should *not* contain `bootstrap.env`. The
directory itself remains (it contains `bootstrap.sh` and the TLS
material).

**Path B — `virtctl console`** (interactive — drive it yourself):

```sh
virtctl -n "$TENANT" console pg-${INSTANCE}
# log in as ubuntu / tempdebug, then:
ls -la /etc/dbaas/
```

**Why this matters:** the file holds plaintext passwords. Leaving it
on disk lets anyone with snapshot / backup / root-on-VM access read
the admin password forever.

### 3.4 Cleanup leaves no orphans (Origin: claude_analysis §4/§6, commits `4de705e` / `b73070e`)

After deleting a DBInstance, every owned Harvester child should be
gone. The finalizer keeps the CR until `TeardownAll` succeeds.

```sh
kubectl --kubeconfig="$KC" -n "$TENANT" delete dbi $INSTANCE --wait=true --timeout=180s

# Expect: every list returns "No resources found" (or excludes pg-$INSTANCE-*)
kubectl --kubeconfig="$KC" -n "$TENANT" get \
  vm,vmi,dv,pvc,secret,svc,servicemonitor \
  -l dbaas.opencloud.wso2.com/instance=$INSTANCE
```

**Pass criteria:** no objects with the instance label remain. Common
historical bug: the headless `pg-$INSTANCE-metrics` Service was leaked
before `4de705e`. If it shows up here, the controller is on an old
image.

### 3.5 Static network applied at `init-local` (Origin: cloudInitNoCloud networkData wiring, commit `23f8c5a`)

If the VLAN doesn't have DHCP and the static IP isn't applied early
enough (`init-local`), `systemd-networkd-wait-online` times out and
nothing works. The fast way to see it worked:

```sh
LAUNCHER=$(kubectl --kubeconfig="$KC" -n "$TENANT" get pod \
  -l vm.kubevirt.io/name=pg-${INSTANCE} -o jsonpath='{.items[0].metadata.name}')
kubectl --kubeconfig="$KC" -n "$TENANT" logs "$LAUNCHER" -c guest-console-log \
  | grep -E "cloud-init.*init|192\.168\.40\.|enp1s0.*True" | head -10
```

**Pass criteria:** a `ci-info: | enp1s0 | True | 192.168.40.X | …` line
appears at cloud-init's `init` stage (somewhere around `Up 10-30s`).
That proves networkd brought the link up with the static config
*before* runcmd ran, which is the precondition for `apt install` over
the VLAN's egress succeeding.

### 3.6 Modify rejection for immutable fields (Origin: C-02, commit `8ae493c`)

Editing `spec.dbName` (an immutable field) on an Available instance
must fail — not silently land `observedGeneration = current`.

```sh
kubectl --kubeconfig="$KC" -n "$TENANT" patch dbi $INSTANCE --type=merge \
  -p '{"spec":{"dbName":"orders-v2"}}'

sleep 5
kubectl --kubeconfig="$KC" -n "$TENANT" get dbi $INSTANCE \
  -o jsonpath='phase={.status.phase}{"\n"}message={.status.message}{"\n"}'
```

**Pass criteria:** `phase=failed`, `message` contains
`cannot modify field(s) dbName`. Also verify
`status.observedGeneration` is **less than** `metadata.generation`
(the failure should not advance it).

The same probe with `spec.running` toggling should be a clean stop
(`phase=stopped`, no failure) — that's the C-02 fix in action: pure
running-toggles aren't refused, only running-toggles that *also*
change an immutable field.

### 3.7 Gateway forces `defaultNamespace` on create (Origin: C-03, commit `84b5154`)

The REST gateway operates on a single configured namespace. A caller
POSTing with a different `metadata.namespace` must end up in the
gateway's default, not in the body's namespace.

```sh
kubectl --kubeconfig="$KC" -n dbaas-system port-forward \
  deploy/dbaas-controller-manager 8080:8080 &
PORTFWD=$!
TOKEN=$(kubectl --kubeconfig="$KC" config view --raw \
  -o jsonpath='{.users[0].user.token}')   # adjust for your auth

curl -sS -H "Authorization: Bearer $TOKEN" -X POST http://localhost:8080/dbinstances \
  -H 'Content-Type: application/json' \
  -d '{"metadata":{"name":"foreign-ns-test","namespace":"some-other-namespace"},
       "spec":{"dbInstanceClass":"db.t3.small","allocatedStorage":20,
               "networkRef":"default/vm-network"}}' | jq '.metadata.namespace'

kill $PORTFWD
```

**Pass criteria:** the response's `metadata.namespace` is whatever
`DBAAS_DEFAULT_NAMESPACE` resolves to on the manager (usually
`default`), **not** `some-other-namespace`. Clean up the created CR
afterward.

### 3.8 `phaseAvailable` is idempotent in steady state (Origin: C-10, commit `5228d68`)

`phaseAvailable` runs every 60 s for every Available DBInstance. The
status-write should not happen when nothing actually changed.

```sh
RV1=$(kubectl --kubeconfig="$KC" -n "$TENANT" get dbi $INSTANCE \
  -o jsonpath='{.metadata.resourceVersion}')
sleep 70   # > one reconcile cycle
RV2=$(kubectl --kubeconfig="$KC" -n "$TENANT" get dbi $INSTANCE \
  -o jsonpath='{.metadata.resourceVersion}')
echo "resourceVersion: $RV1 → $RV2"
```

**Pass criteria:** `RV1 == RV2`. A change means the controller wrote
status without a real diff. (Endpoint IP changes from the guest agent
do legitimately bump `resourceVersion`; only run this probe on a
stable instance with a stable IP.)

## 4. Probe pod recipes (reusable)

### 4.1 Same-VLAN psql sidecar

(Used above in §2 and §3.1.) `nicolaka/netshoot` + `apk add
postgresql-client`. Needs `NET_ADMIN` + `NET_RAW` to add the static IP
to `net1`. Picks any free IP in the VLAN's subnet (`.99` is just a
convention).

### 4.2 Same-VLAN SSH sidecar

```sh
cat <<YAML | kubectl --kubeconfig="$KC" apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: sshprobe
  namespace: default
  annotations: { k8s.v1.cni.cncf.io/networks: $VLAN_NAD }
spec:
  restartPolicy: Never
  containers:
  - name: probe
    image: nicolaka/netshoot
    command: ["sleep", "600"]
    securityContext: { capabilities: { add: ["NET_ADMIN", "NET_RAW"] } }
YAML
kubectl --kubeconfig="$KC" -n default wait --for=condition=Ready pod/sshprobe --timeout=60s

kubectl --kubeconfig="$KC" -n default exec sshprobe -- sh -c '
  apk add --no-cache sshpass openssh-client >/dev/null 2>&1
  ip addr add 192.168.40.99/24 dev net1 2>/dev/null
  ip link set net1 up
  ip route add '"$IP"'/32 dev net1 2>/dev/null
  # Replace tempdebug with your spec.vmPassword
  sshpass -p tempdebug ssh \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o PreferredAuthentications=password -o PubkeyAuthentication=no \
    ubuntu@'"$IP"' "<your command here>"
'
```

Reuse for any "look inside the VM" probe (C-08, troubleshooting
cloud-init failures, etc.).

### 4.3 DHCP probe

For when you're not sure whether the VLAN has a working DHCP server
(if not, you need `spec.staticNetwork`):

```sh
kubectl --kubeconfig="$KC" -n default exec sshprobe -- sh -c '
  udhcpc -i net1 -n -t 3 -T 2
'
```

If `udhcpc` reports `no lease, failing`, the VLAN has no DHCP and
every DBInstance applied without `spec.staticNetwork` will sit in
`WaitingForCloudInit` forever.

## 5. What is not verified by these recipes

These rely on out-of-band evidence (DEFERRED.md items, mostly):

| Property | Why it's not here |
| --- | --- |
| pgBackRest schedule / S3 backup completion | DEF-02 — feature not implemented yet, nothing to verify. |
| `postgres_exporter` metrics flow | DEF-05 — exporter isn't installed in the VM; even fixing the ServiceMonitor selector (C-01) won't help until the exporter is real. |
| `engineVersion` actually drives version | DEF-01 — `psql -c "SELECT version()"` only happens to match because the OS image's apt-default agrees. Not a property of the controller. |
| `status.conditions` populated | DEF-07 — field exists in the schema, controller doesn't write to it. |
| `multiAZ` HA failover | DEF-10 — no standby is created. |
| TLS rotation | DEF-14 — CAs are 10-year self-signed and never re-issued. |

When any of those move from DEFERRED to implemented, add a §3.x
recipe here in the same PR.

## 6. Cleanup after verification

```sh
kubectl --kubeconfig="$KC" -n default delete pod verifyprobe sshprobe --wait=false 2>/dev/null
kubectl --kubeconfig="$KC" -n "$TENANT" delete dbi $INSTANCE --wait=true --timeout=180s
# Optional: drop the tenant ns and the controller too
kubectl --kubeconfig="$KC" delete namespace "$TENANT" --wait=false
KUBECONFIG=$KC make undeploy
KUBECONFIG=$KC make uninstall
```
