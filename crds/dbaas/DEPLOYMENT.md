# dbaas controller — deployment topology

What a working deployment looks like end-to-end, with the concrete values from
the validated run on Harvester 1.7.1. Companions: [`ARCHITECTURE.md`](./ARCHITECTURE.md)
explains the design; [`USAGE.md`](./USAGE.md) is the copy-paste install + operate
guide. This file is the picture in between — what shape everything takes after
`make deploy` runs and one `DBInstance` is `Available`.

## Diagram

```
┌────────────────────────────────────────────────────────────────────────────┐
│  Operator (this Mac)                                                       │
│     kubectl --kubeconfig=local-2.yaml  ──╮                                 │
│     curl http://localhost:8080 + Bearer ─╯  (via port-forward)             │
└────────────────────────────────┬───────────────────────────────────────────┘
                                 │  HTTPS  (auth = caller's token)
                                 ▼
┌────────────────────────────────────────────────────────────────────────────┐
│  Harvester 1.7.1   ·   RKE2 v1.34.3   ·   node1 / node2 / node3            │
│  Kubernetes API:  https://192.168.10.100/k8s/clusters/local                │
│                                                                            │
│  ┌──────────────────────────────────────────────────────────────────────┐  │
│  │  namespace: dbaas-system                                              │  │
│  │                                                                       │  │
│  │   deploy/dbaas-controller-manager   image wso2vick/ocd-dbaas:v0.2.4   │  │
│  │   ┌─────────────────────────────────────────────────────────────┐    │  │
│  │   │  controller-runtime manager                                  │    │  │
│  │   │   • reconciler      ─ watches DBInstance, writes K8s objects │    │  │
│  │   │   • REST gateway    ─ :8080, per-request K8s client signed   │    │  │
│  │   │                       with the caller's bearer token          │    │  │
│  │   │   • metrics :8443   healthz :8081                             │    │  │
│  │   └─────────────────────────┬───────────────────────────────────┘    │  │
│  └─────────────────────────────┼───────────────────────────────────────┘  │
│                                │ K8s API ops                               │
│                                ▼                                           │
│  ┌──────────────────────────────────────────────────────────────────────┐  │
│  │  namespace: tenant-test                                               │  │
│  │                                                                       │  │
│  │   dbi/test-orders   phase=available   class=db.t3.small               │  │
│  │   status.endpoint = 192.168.40.50:5432                                │  │
│  │   jdbc:postgresql://192.168.40.50:5432/testdb?ssl=true&sslmode=verify-ca│  │
│  │                                                                       │  │
│  │   Owned objects (Harvester / KubeVirt / CDI / Prometheus):            │  │
│  │     vm/pg-test-orders + vmi/pg-test-orders                            │  │
│  │     dv/pg-test-orders-os    (cloned from VirtualMachineImage)         │  │
│  │     dv/pg-test-orders-data  (blank, Longhorn RWO, 20 GiB)             │  │
│  │     secret/pg-test-orders-credentials                                 │  │
│  │       admin_user/password, repl/exporter pw, luks key,                │  │
│  │       ca_cert/key, server_cert/key, userdata, networkdata             │  │
│  │     svc/pg-test-orders-metrics (ClusterIP None, :9187)                │  │
│  │     servicemonitor/pg-test-orders-monitor                             │  │
│  └─────────────────────────────┬───────────────────────────────────────┘  │
│                                │  virt-launcher schedules                  │
│                                ▼                                           │
│  ╔══════════════════════════════════════════════════════════════════╗     │
│  ║  KubeVirt VM  "pg-test-orders"  on node3                          ║     │
│  ║                                                                    ║     │
│  ║   Ubuntu 24.04 (image default/ubuntu-2404-server, downloaded from  ║     │
│  ║                 cloud-images.ubuntu.com/noble)                     ║     │
│  ║   PostgreSQL 16.13   listen=*  port=5432  ssl=on                  ║     │
│  ║   pg_hba.conf:  hostssl all all 0.0.0.0/0 scram-sha-256            ║     │
│  ║   qemu-guest-agent  (reports IP back to KubeVirt VMI status)       ║     │
│  ║                                                                    ║     │
│  ║   Disks:                                                           ║     │
│  ║     os-disk      → DataVolume pg-test-orders-os    (image-managed) ║     │
│  ║     pgdata-disk  → DataVolume pg-test-orders-data  (Longhorn)      ║     │
│  ║     cloudinit    → Secret  pg-test-orders-credentials              ║     │
│  ║                    │  userdata    → packages, bootstrap.sh, certs  ║     │
│  ║                    │  networkdata → static IP applied init-local   ║     │
│  ║                                                                    ║     │
│  ║   Single NIC "data-net"                                            ║     │
│  ║     Multus bridge → NAD default/vm-network (bridge cn-vm-br)       ║     │
│  ║     static 192.168.40.50/24   gw 192.168.40.1                      ║     │
│  ║     dns 8.8.8.8, 1.1.1.1                                           ║     │
│  ╚═══════════════════════════════════╤══════════════════════════════╝     │
│                                       │                                    │
│  ══════════════════════════════════ VLAN 400 (192.168.40.0/24) ═══════════│
│  bridge cn-vm-br on every node,  Multus NAD default/vm-network             │
│                                       │                                    │
│  Any pod / VM on the same NAD reaches │                                    │
│  the DB on L2:                        │                                    │
│                                       │                                    │
│    apiVersion: v1                     │                                    │
│    kind: Pod                          │                                    │
│    metadata:                          │                                    │
│      annotations:                     │                                    │
│        k8s.v1.cni.cncf.io/networks:   │                                    │
│          default/vm-network           │                                    │
│      ↓                                │                                    │
│    psql 'host=192.168.40.50 ─────────┘                                    │
│          dbname=testdb user=dbadmin sslmode=require'                       │
│      → SELECT version()  →  PostgreSQL 16.13 (Ubuntu 16.13-0ubuntu0.24.04)│
└────────────────────────────────────────────────────────────────────────────┘
```

## How to read it

Top to bottom, four layers:

1. **Operator** — wherever your `kubectl` / `curl` runs. The kubeconfig
   points at Harvester's Rancher-proxied K8s API (`/k8s/clusters/local`).
   When you hit the gateway, the same bearer token from kubeconfig is what
   authenticates to the K8s API, so RBAC is enforced on you, not on the
   controller's ServiceAccount.
2. **Control plane in `dbaas-system`** — the manager Pod (controller +
   gateway in one binary). The reconciler watches `DBInstance` CRs and
   reacts; the gateway is just a thin REST layer over the same K8s API.
3. **Tenant namespace** — where the user-facing `DBInstance` CR lives, and
   where every Harvester object the reconciler creates for it ends up
   (VM, VMI, two DataVolumes, the credentials Secret, the metrics Service,
   the ServiceMonitor). All discoverable with one label query:
   `kubectl -n <ns> get vm,vmi,dv,secret,svc,servicemonitor -l dbaas.opencloud.wso2.com/instance=<name>`.
4. **The VM itself** — single NIC bridged to the operator-supplied Multus
   NAD (here `default/vm-network`, which is Harvester VLAN 400 via bridge
   `cn-vm-br`). Cloud-init delivers two things at boot: the static
   `networkData` applied at `init-local` (so networkd doesn't time out),
   and the `userdata` shell bootstrap that `apt install`s PostgreSQL and
   enables `qemu-guest-agent`. Once the agent registers, the VMI's
   `status.interfaces[].ipAddress` populates and the reconciler advances
   to `Available`.

## What had to be true on the cluster

These are the prerequisites the controller doesn't manage, and which the
operator has to satisfy:

| Prereq | Why | How to satisfy |
| --- | --- | --- |
| Multus NAD on the target VLAN | The VM bridges to it as its only NIC. | Created in Harvester UI under **Networks → VM Networks**. NAD ends up in `default` (or `harvester-public`). Tell the `DBInstance` about it via `spec.networkRef: <ns>/<nad-name>`. |
| The VLAN routes to the internet | Cloud-init `apt install`s postgresql from the upstream Ubuntu mirrors during first boot. | Have an upstream router/gateway on the VLAN. We empirically verified `curl https://archive.ubuntu.com → 200` from a probe pod on the same VLAN before the test. |
| A `VirtualMachineImage` for the OS | The VM's OS DataVolume is cloned from it via the image-managed StorageClass. | Created in Harvester UI under **Images** (upload or URL). Reference it from `spec.osImage` by either the CR name (`<ns>/<name>`) or the displayName. We downloaded the stock Ubuntu 24.04 noble cloudimg. |
| A static IP on the VLAN (only if no DHCP) | The reconciler doesn't run an IPAM; it just hands what's in `spec.staticNetwork` to cloud-init. | Pick a free IP, gateway, DNS resolvers; set `spec.staticNetwork`. Omit if the VLAN has a working DHCP server. |
| `kubectl` access to the cluster | Standard kubebuilder install / deploy targets use the active kubeconfig. | Harvester UI → **Support → Download KubeConfig**. |
| A registry the nodes can pull from | The controller image. | Public registry works (we used Docker Hub `wso2vick/ocd-dbaas`); private registries need imagePullSecrets in the manager Deployment. |

## Two non-obvious gotchas this deployment hit

Both are worth knowing because they are silent failures — the reconciler
keeps reporting `WaitingForCloudInit` forever instead of saying *why*.

1. **Cloud-init `networkData` must be delivered via the dedicated
   channel, not `write_files`.** When we wrote `/etc/netplan/60-data-net.yaml`
   via cloud-init's `write_files`, it lands at the `config` stage — *after*
   `systemd-networkd-wait-online` has already given up. Apt then fails for
   lack of routing. Putting the same content in the `networkdata` key of
   the credentials Secret (and pointing `cloudInitNoCloud.networkDataSecretRef`
   at it) makes cloud-init apply it at `init-local`, before networkd starts.

2. **Harvester's VM mutating webhook strips
   `cloudInitNoCloud.userDataSecretRef`.** It preserves the older alias
   `secretRef` and the newer `networkDataSecretRef` but silently drops
   `userDataSecretRef`. The VM ends up with network config but **no user
   data**, so cloud-init's `modules:final` finishes in <1 s with nothing
   to process, no apt, no `runcmd`, no `ssh_pwauth`. The fix is to use
   the legacy `secretRef` (for userdata) plus `networkDataSecretRef`
   (for networkdata) — both pointing at the same Secret. The current code
   in `internal/harvester/client.go` does this.

## Reproducing

Once your cluster meets the prereqs above, the full sequence is:

```sh
# from crds/dbaas/
make docker-buildx IMG=<registry>/<name>:<tag>
KUBECONFIG=<harvester-kubeconfig> make install
KUBECONFIG=<harvester-kubeconfig> make deploy IMG=<registry>/<name>:<tag>

# then apply a DBInstance — full example in USAGE.md, minimum:
kubectl create namespace tenant-test
cat <<EOF | kubectl apply -f -
apiVersion: dbaas.opencloud.wso2.com/v1alpha1
kind: DBInstance
metadata:
  name: test-orders
  namespace: tenant-test
spec:
  dbInstanceClass: db.t3.small
  allocatedStorage: 20
  networkRef: default/vm-network
  osImage: default/ubuntu-2404-server
  dbName: testdb
  masterUsername: dbadmin
  manageMasterUserPassword: true
  staticNetwork:
    address: 192.168.40.50/24
    gateway: 192.168.40.1
    nameservers: [8.8.8.8, 1.1.1.1]
EOF

kubectl -n tenant-test get dbi -w
```

Expected time from `apply` to `phase=available`: about **3 minutes**
(image clone + boot + apt install + the 3-min uptime gate). End-to-end
proof on Harvester 1.7.1 in this branch's most recent test was a full SQL
roundtrip: `CREATE TABLE` → `INSERT` → `SELECT` of the inserted row, run
from a probe pod attached to the same NAD.
