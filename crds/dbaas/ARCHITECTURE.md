# dbaas controller — architecture

A Kubernetes-native Database-as-a-Service controller that provisions managed
PostgreSQL instances as KubeVirt virtual machines on a Harvester HCI cluster.
Each database is a `DBInstance` custom resource; the controller reconciles
that resource into a running, SSL-only PostgreSQL VM with Prometheus
metrics, on a VLAN the operator names.

## At a glance

- **CRD:** `DBInstance` (group `dbaas.opencloud.wso2.com`, version
  `v1alpha1`, kind shortname `dbi`). One CR == one PostgreSQL VM.
- **Two interfaces** for callers:
  1. `kubectl apply` a `DBInstance` YAML.
  2. A small REST gateway (`:8080`) that does the same writes over HTTP.
- **Async by design.** Every mutating call (create / modify / stop /
  start / delete) returns immediately; the reconciler advances the work
  in the background and reflects progress in `status.phase` and
  `status.provisioningPhase`. Callers poll the CR (or
  `GET /dbinstances/{name}`) for state.
- **Single-NIC VM on an operator-supplied VLAN.** The VM has one network
  interface, bridged onto whatever Multus `NetworkAttachmentDefinition`
  the operator names in `spec.networkRef`. Client traffic, `apt` during
  cloud-init, and Prometheus scraping all flow through that one VLAN.
- **Separate disk for `pgdata`.** The OS disk holds Ubuntu + PostgreSQL
  binaries; a dedicated `pgdata` DataVolume holds the actual database
  content. Resizing or replacing the OS disk doesn't touch user data;
  snapshotting the data disk doesn't drag the OS around.

## Architecture

```
┌─────────────────────────────────────────────────────────────────────────┐
│  bin/manager (one binary, one Pod)                                      │
│                                                                         │
│  ┌────────────────────────┐         ┌─────────────────────────────────┐ │
│  │  REST gateway          │         │  DBInstance reconciler          │ │
│  │  internal/gateway      │         │  internal/controller            │ │
│  │  :8080  HTTP           │         │                                 │ │
│  │  create/describe/      │         │  phase-based state machine,     │ │
│  │  modify/delete/        │         │  one phase per Reconcile call   │ │
│  │  start/stop            │         │                                 │ │
│  └──────────┬─────────────┘         └────────────────┬────────────────┘ │
│             │                                        │                  │
│             │  per-request typed client              │                  │
│             │  (forwards caller's bearer token)      │                  │
│             ▼                                        ▼                  │
│  ┌───────────────────────────────────────────────────────────────────┐  │
│  │  Kubernetes API server (etcd)                                     │  │
│  │  DBInstance CRD  —  spec (desired) + status (observed)            │  │
│  └───────────────────────────────────────────────────────────────────┘  │
│                                              ▲                          │
│                                              │ dynamic client           │
│  ┌───────────────────────────────────────────┴───────────────────────┐  │
│  │  Harvester client                                                 │  │
│  │  internal/harvester                                               │  │
│  │  KubeVirt VirtualMachine / VMI, CDI DataVolume, Secret,           │  │
│  │  Service, Endpoints, Prometheus ServiceMonitor, Harvester         │  │
│  │  VirtualMachineImage (image resolution only), Probe Pod           │  │
│  └───────────────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────────┘
                                  │ Kubernetes API (typed for DBInstance,
                                  │ unstructured for KubeVirt/CDI/Prom)
                                  ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  Harvester HCI cluster                                                  │
│  KubeVirt │ CDI (Longhorn) │ Multus NAD (operator-supplied)             │
│  Prometheus Operator (optional)                                         │
└─────────────────────────────────────────────────────────────────────────┘
```

**Why dynamic client.** The reconciler never imports the
KubeVirt/CDI/Prometheus Go schemas. It builds and reads
`unstructured.Unstructured` objects against the GVRs in
`internal/harvester/client.go`. That keeps `go.mod` small and lets the
controller run on Harvester versions whose Go types we haven't pinned.

**Why a probe pod for readiness.** The controller pod runs on the
cluster pod overlay. In any deployment where `spec.networkRef` points at
an isolated VLAN — the intended use case — the controller has no L3
route to the VM's data NIC. To confirm postgres is actually accepting
TCP before marking the instance `Available`, the reconciler spawns a
one-shot Pod attached to the same Multus NAD as the VM and reads that
Pod's exit phase. See `internal/harvester/probe.go`.

## Repo layout

```
crds/dbaas/
├── cmd/main.go                     # manager entrypoint: builds client +
│                                   #   harvester client, wires reconciler
│                                   #   and gateway, starts the manager
├── api/v1alpha1/                   # DBInstance CRD Go types
│   ├── dbinstance_types.go         #   spec, status, constants, classes
│   ├── groupversion_info.go
│   └── zz_generated.deepcopy.go    # controller-gen output
├── internal/
│   ├── controller/                 # phase-based reconciler
│   │   ├── dbinstance_controller.go
│   │   ├── dbinstance_controller_test.go
│   │   ├── immutable_drift_test.go #   normalisation + drift detection
│   │   └── probe_gate_test.go      #   probeListener stub seam
│   ├── gateway/                    # thin HTTP layer over the CRD
│   │   ├── gateway.go
│   │   └── gateway_test.go
│   └── harvester/                  # dynamic-client wrapper
│       ├── client.go               #   VM, DataVolume, Secret, Service,
│       │                           #   Endpoints, ServiceMonitor,
│       │                           #   VMImage resolve, TeardownAll
│       ├── client_test.go
│       ├── cloudinit.go            #   PostgreSQL bootstrap script
│       ├── probe.go                #   one-shot Multus probe pod
│       └── tlsgen.go               #   per-instance CA + server cert
├── config/                         # kubebuilder kustomize tree
│   ├── crd/bases/…                 #   generated CRD manifest
│   ├── rbac/…                      #   generated ClusterRole + bindings
│   ├── manager/manager.yaml        #   the Deployment
│   └── samples/…
├── Dockerfile                      # distroless, multi-arch via TARGETARCH
├── Makefile                        # generate, build, docker-buildx, deploy
└── PROJECT                         # kubebuilder project metadata
```

## The DBInstance CRD

`DBInstance` is namespaced. Each CR lives in a tenant namespace and every
Harvester object it spawns (VMs, DataVolumes, Secret, Service, Endpoints,
ServiceMonitor) is created in the same namespace.

### Spec — what the caller asks for

| Field | Required | Default | Notes |
| --- | --- | --- | --- |
| `dbInstanceClass` | ✓ | — | Sizing class, e.g. `db.t3.medium`, `db.m5.large`. Maps to (CPU, memory, `max_connections`) via the `InstanceClasses` table in `api/v1alpha1/dbinstance_types.go`. |
| `allocatedStorage` | ✓ | — | `pgdata` volume size in GiB. |
| `networkRef` | ✓ | — | `namespace/name` of an existing Multus NAD (typically a Harvester VLAN NAD). The VM's only network. |
| `engineVersion` |   | `"16"` | PostgreSQL major version. **NOT YET IMPLEMENTED** — the controller installs whatever the OS image's apt repo provides; pick the OS image to pick the version. |
| `dbName` |   | instance name | Initial database created on first boot. |
| `port` |   | `5432` | PostgreSQL listen port. |
| `masterUsername` |   | `dbadmin` | Admin role created on first boot. `LOGIN CREATEDB CREATEROLE` — not `SUPERUSER`. |
| `manageMasterUserPassword` |   | `false` | If true, a 32-char password is generated and stored in the credentials Secret. **NOT YET IMPLEMENTED**: the controller always generates the password. |
| `masterUserPasswordRef` |   | — | Read the password from an existing Secret. **NOT YET IMPLEMENTED**. |
| `storageType` |   | `longhorn` | StorageClass for the `pgdata` DataVolume. |
| `osImage` |   | `ubuntu-22.04-server-cloudimg-amd64.img` | Harvester `VirtualMachineImage` name, `ns/name`, or `displayName`. The OS disk uses the image's image-managed StorageClass. |
| `vmPassword` |   | — | Console/SSH password for the `ubuntu` user. Development only — leave empty in production. |
| `staticNetwork` |   | — | `{address, gateway, nameservers, searchDomains}` for VLANs without DHCP. When unset, the NIC runs DHCP. |
| `running` |   | `true` | `false` stops the VM (storage preserved). |
| `deletionProtection` |   | `false` | When true, the controller refuses to teardown the instance. |
| `backupRetentionPeriod` |   | `0` | Days of pgBackRest retention. `0` disables backups. **NOT YET IMPLEMENTED** — value is recorded but no backup process runs. |
| `preferredBackupWindow` |   | — | UTC `HH:MM-HH:MM`. |
| `s3BackupConfig` |   | — | `{endpoint, bucket, region, secretRef}` for pgBackRest. **NOT YET IMPLEMENTED** — values are written to `/etc/dbaas/bootstrap.env` on the VM but no backup process consumes them. |
| `multiAZ` |   | `false` | Reserved for Patroni HA. **NOT YET IMPLEMENTED**. |
| `dbParameterGroupRef` |   | — | Reserved for a future `DBParameterGroup` CRD. |
| `tags` |   | — | User labels. **NOT YET IMPLEMENTED** — not propagated to child resources. |

### Status — what the controller reports

| Field | Meaning |
| --- | --- |
| `phase` | RDS-style lowercase: `creating`, `available`, `stopping`, `stopped`, `starting`, `modifying`, `deleting`, `failed`. |
| `provisioningPhase` | Internal reconcile step (PascalCase). See the phase table below. |
| `endpoint` | `{address, port, jdbcUrl}` once postgres answers. `jdbcUrl` uses `sslmode=verify-ca`. |
| `masterUserSecret` | `{name, status}` pointing at the K8s Secret holding the credentials. |
| `resources` | `{nadName, dataVolumeName, vmName, secretName, metricsServiceName, serviceMonitor}` — populated as phases complete, used for idempotency on restart and for `TeardownAll`. |
| `appliedSpec` | Snapshot of the immutable parts of the spec as of the original create. The modify path compares the current spec against this and refuses changes to immutable fields rather than silently dropping them. |
| `caCertPEM` | PEM-encoded CA generated per instance, so clients can pin it. |
| `grafanaUrl`, `prometheusTarget` | Set once monitoring is deployed. |
| `observedGeneration` | The `.metadata.generation` last fully reconciled. Drives the modify path. |
| `conditions` | Standard `metav1.Condition` list for future use (see DEF-07). |
| `message` | Human-readable status string. |

## REST gateway

`internal/gateway/gateway.go` exposes the six operations the controller
cares about as plain HTTP, all reading and writing the `DBInstance` CR
via a per-request controller-runtime client. Every mutating call is
asynchronous: the gateway writes the CR and returns `202 Accepted`; the
reconciler does the work.

| Verb | Path | What it does |
| --- | --- | --- |
| `GET` | `/healthz` | Liveness ping. |
| `GET` | `/dbinstances` | List `DBInstance`s in the default namespace. |
| `POST` | `/dbinstances` | **create db** — full `DBInstance` body. The gateway overwrites `metadata.namespace` to its default so a caller can't strand a resource in an unreachable namespace. |
| `GET` | `/dbinstances/{name}` | **describe db** — current spec + status. |
| `PATCH` | `/dbinstances/{name}` | **modify db** — partial spec (class, storage, backup, running, deletion protection). |
| `DELETE` | `/dbinstances/{name}` | **delete db** — sets `DeletionTimestamp`; reconciler tears resources down under the finalizer. |
| `POST` | `/dbinstances/{name}/start` | **start db** — sets `spec.running = true`. |
| `POST` | `/dbinstances/{name}/stop` | **stop db** — sets `spec.running = false`. |

The default namespace is `default`, overridable with the
`DBAAS_DEFAULT_NAMESPACE` environment variable.

**Authentication.** Every request except `/healthz` must carry
`Authorization: Bearer <token>` where `<token>` is a Kubernetes credential
the cluster's API server accepts (a ServiceAccount token, an OIDC ID
token, etc.). The gateway clones its `rest.Config`, replaces the bearer
token with the caller's, builds a per-request controller-runtime client,
and uses it for the K8s API call — so authentication, RBAC, and audit
are enforced by the K8s API server on the caller's identity, not on the
manager's ServiceAccount. RBAC denials from the API server propagate
back as `403 Forbidden`; unknown or expired tokens become `401
Unauthorized`. The manager never elevates beyond what the caller is
RBAC-authorized to do.

## Reconciler — phase state machine

`internal/controller/dbinstance_controller.go` advances exactly one phase
per `Reconcile`, persists status, and requeues. Every phase is
idempotent: it checks `status.resources` before doing work and re-enters
cleanly after a controller restart.

```
       Pending  (or empty)
          │
          ▼
   NetworkProvisioned         echo spec.networkRef into status.resources.nadName
          │                   (no Multus / VPC creation — the operator owns the NAD)
          ▼
   StorageProvisioned         credentials Secret (admin + repl + exporter passwords,
          │                   per-instance CA + server cert, cloud-init userdata &
          │                   networkdata) + pgdata DataVolume "pg-{id}-data"
          ▼
   VMCreated                  KubeVirt VirtualMachine "pg-{id}". OS disk is templated
          │                   via dataVolumeTemplates (image clone); pgdata-disk is
          │                   the previously-created DataVolume; cloudinit is a Secret
          │                   volume. spec.running = true.
          ▼
   WaitingForCloudInit        TWO gates:
          │                     1. VMI is Running and qemu-guest-agent reported an IP
          │                     2. probe pod (Multus-attached) succeeds:
          │                        nc -zw 3 <vmIP> 5432
          ▼
   DatabaseReady              status.endpoint populated, JDBC URL emitted.
          │
          ▼
   MonitoringDeployed         ClusterIP Service "pg-{id}-metrics" (port 9187),
          │                   manually-populated Endpoints with the VM's data-net IP,
          │                   Prometheus ServiceMonitor. Failure is non-fatal.
          ▼
   Available                  status.phase = "available". Re-checks the VMI on every
                              requeue (60s); skips the status write when nothing changed.
```

### The probe-pod readiness gate

`phaseWaitReady` does not advance to `DatabaseReady` until a real TCP
connection to PostgreSQL has been made. Because the controller pod
sits on the cluster pod overlay and the VM sits on whatever VLAN
`spec.networkRef` points at, the controller usually can't dial the VM
directly. Instead, `internal/harvester/probe.go` spawns a one-shot
busybox Pod with the multus annotation:

```yaml
k8s.v1.cni.cncf.io/networks: '[{"name":"<nad-name>","namespace":"<nad-ns>","interface":"probe-nic"}]'
```

The Pod runs `ip addr add <neighbor-ip>/<prefix> dev probe-nic && nc
-zvw 3 <vmIP> 5432`. Because the NAD has no IPAM, the probe picks an IP
one octet away from the VM (e.g. VM at `.50` → probe at `.51`). The
controller polls the pod's phase, treats `Succeeded` as "ready" and
`Failed` as "not yet, retry", and **always** deletes the pod via a
deferred detached-context Delete so a cancelled reconcile can't leak it.
The IP-collision corner case (two DBInstances on the same VLAN with
adjacent IPs whose probes overlap) is tracked as DEF-21 in
`DEFERRED.md`; cloud-init phone-home to the gateway is the long-term
escape.

### Non-linear paths

- **`Failed`** — any phase can transition to `Failed` via `r.fail()`. The
  controller requeues every 30s.
- **Stop / Start** — if the instance is `Available` and `spec.running`
  flips to `false`, the reconciler shortcuts to `reconcileStop`
  (KubeVirt `spec.running = false`, preserves storage). The inverse for
  start. **Both paths run an `immutableDrift` check first** so a PATCH
  that flips `running` *and* tries to change e.g. `dbName` is refused
  rather than silently dropping the rename.
- **Modify** — if the instance is `Available` and
  `metadata.generation != status.observedGeneration`, `reconcileModify`
  runs the same `immutableDrift` check, then applies the mutable
  changes (CPU / memory via `Harvester.ResizeVM`) before advancing
  `observedGeneration`.
- **Delete** — `DeletionTimestamp` triggers `reconcileDelete`. If
  `spec.deletionProtection` is true, deletion is refused.
  `TeardownAll` deletes ServiceMonitor, Service, Endpoints, VM, DataVolume,
  and Secret in dependency order, returns an aggregated error, and the
  finalizer is removed only if everything succeeded. The NAD and the
  tenant namespace are owned by the operator and are never touched.

### What `Failed` actually persists

`r.fail(ctx, inst, reason, err)` writes `status.phase = "failed"`,
`status.provisioningPhase = "Failed"`, and `status.message = "<reason>:
<err>"`. The reconciler retries every 30s — if the underlying problem
clears, the next reconcile picks up from the last completed phase
recorded in `status.resources`.

### `immutableDrift` and `AppliedSpec`

At the end of a successful create, the reconciler snapshots the
defaulted spec into `status.appliedSpec`. Every subsequent
modify/stop/start checks the current spec against that snapshot
(defaulting both sides identically so an explicit default doesn't trip
the check) and refuses changes to immutable fields — `networkRef`,
`osImage`, `dbName`, `masterUsername`, `engineVersion`, `port`,
`storageType`. Without this guard, a PATCH that touches an immutable
field would silently advance `observedGeneration` and the field would
appear changed in the spec but never take effect anywhere.

## What gets created on Harvester

For an instance named `orders-prod` in namespace `tenant-acme`, the
controller creates:

| Kind | Name | Notes |
| --- | --- | --- |
| Secret (Opaque) | `pg-orders-prod-credentials` | `admin_user`, `admin_password`, `repl_password`, `exporter_password`, `ca_cert`, `ca_key`, `server_cert`, `server_key`, `userdata` (cloud-init), `networkdata` (cloud-init network-config v2). |
| CDI DataVolume | `pg-orders-prod-os` | OS disk, ~20 GiB, image-managed StorageClass. Created via `vm.spec.dataVolumeTemplates`. |
| CDI DataVolume | `pg-orders-prod-data` | `spec.allocatedStorage` GiB, `Block` mode, ReadWriteOnce, `storageClassName = spec.storageType` (default `longhorn`). Mounted in the VM as `/dev/vdb` and migrated onto `/var/lib/postgresql` during first boot. |
| KubeVirt VirtualMachine | `pg-orders-prod` | One NIC `data-net` bridged onto `spec.networkRef`; OS disk from the OS DataVolume; `pgdata-disk` from the data DataVolume; `cloudinit` sourced from the Secret. |
| Service (ClusterIP) | `pg-orders-prod-metrics` | Port `9187` → `postgres_exporter` on the VM. Selector-less; targets a manually-populated Endpoints. |
| Endpoints | `pg-orders-prod-metrics` | `subsets[0].addresses[0].ip = <vm-data-net-ip>` so kube-proxy DNATs `Service:9187` → `<vmIP>:9187`. Required because the metrics endpoint lives on the VM, not on a Pod the Service selector could match. |
| ServiceMonitor | `pg-orders-prod-monitor` | 15s scrape, matchLabels `metrics=true` + `instance=<id>`. |

All objects carry the `dbaas.opencloud.wso2.com/instance=<id>` label so
they can be located by label, and the controller tracks them by name in
`status.resources` so it doesn't depend on label-list operations during
cleanup.

### VM disk layout

```
┌─────────────────────────────────────────────────────────────────┐
│  KubeVirt VirtualMachine: pg-{id}                               │
│                                                                 │
│   Disks                                                         │
│   ──────────                                                    │
│   /dev/vda    DataVolume "pg-{id}-os"    Ubuntu rootfs +        │
│                                          PostgreSQL binaries     │
│                                          (~20 GiB, image clone)  │
│   /dev/vdb    DataVolume "pg-{id}-data"  PostgreSQL data,       │
│                                          mounted at             │
│                                          /var/lib/postgresql    │
│                                          (Longhorn, sized by    │
│                                          spec.allocatedStorage) │
│   /dev/vdc    Secret "pg-{id}-credentials" cloud-init disk      │
│                                                                 │
│   NIC                                                           │
│   ──────────                                                    │
│   data-net   Multus bridge → spec.networkRef NAD                │
│              (DHCP or spec.staticNetwork, matched on             │
│              driver=virtio_net)                                  │
│                                                                 │
│   PostgreSQL                                                    │
│   ──────────                                                    │
│   listen_addresses = '*'    port = spec.port    SSL = on        │
│   pg_hba: hostssl all all 0.0.0.0/0 scram-sha-256               │
│   (plain-text connections are rejected at the protocol level)   │
└─────────────────────────────────────────────────────────────────┘
```

That is intentionally the *whole* network model in this version. There
is no pod-network management NIC, no Kube-OVN VPC, no VPC peering. If
the database needs to be reachable from a particular set of workloads,
the operator configures that by pointing `spec.networkRef` at the right
VLAN — for example, the VLAN where the Rancher/RKE2 cluster lives.

## Cloud-init bootstrap

`internal/harvester/cloudinit.go` builds two cloud-init payloads — a
`userdata` blob and a `networkdata` blob — both stored in the credentials
Secret and surfaced to the VM as a `cloudInitNoCloud` disk.

**Network is configured at `init-local` stage** via cloud-init
network-config v2 (the `networkdata` key on the Secret):

```yaml
version: 2
ethernets:
  data:
    match:
      driver: virtio_net
    # if spec.staticNetwork is nil:
    dhcp4: true
    # if spec.staticNetwork is set:
    dhcp4: false
    addresses: [<address>]
    routes:
      - to: default
        via: <gateway>
    nameservers:
      addresses: [<ns1>, <ns2>]
```

This runs *before* systemd-networkd starts, so the NIC has its IP,
gateway, and DNS before any module tries to talk to the network. (A
`write_files` netplan stanza would land at the `config` stage, *after*
`apt update` had already failed for lack of routing.)

**The bootstrap script** is then embedded in `userdata`'s
`write_files` + `runcmd` sections. On first boot, in order:

1. `apt install postgresql postgresql-contrib jq qemu-guest-agent
   prometheus-postgres-exporter` (over the VLAN — that VLAN must reach
   upstream package mirrors). The package installs are done from
   `bootstrap.sh`'s `runcmd`, not cloud-init's `packages:` directive,
   because minimal cloud images strip the `package_update_upgrade_install`
   module.
2. **Migrate pgdata onto `/dev/vdb`.** If the disk has no `PG_VERSION`
   marker file (i.e. it's a fresh, just-`mkfs.ext4`'d disk), the
   freshly-installed PostgreSQL cluster is copied onto it. The disk is
   then mounted at `/var/lib/postgresql` via an fstab entry keyed by
   UUID. The PG_VERSION marker — not directory emptiness — is used
   because `mkfs.ext4` always creates `lost+found`, which would defeat a
   naive empty-directory check (this exact bug, C-13, was caught and
   fixed during live validation).
3. Patch `postgresql.conf`: `listen_addresses = '*'`, configured `port`,
   `max_connections` from the instance class, and `ssl_*` paths
   pointing at `/etc/ssl/certs/pg-server.crt` etc.
4. Append `hostssl all all 0.0.0.0/0 scram-sha-256` and
   `hostssl replication all 0.0.0.0/0 scram-sha-256` to `pg_hba.conf` —
   remote plain-text auth is rejected.
5. Restart PostgreSQL (with the new pgdata location and config).
6. Create the admin role — `LOGIN CREATEDB CREATEROLE`, **not
   `SUPERUSER`** — and the `postgres_exporter` role with `pg_monitor`
   granted. Create the initial database.
7. Write `/etc/default/prometheus-postgres-exporter` with the
   exporter's `DATA_SOURCE_NAME`. **`systemctl enable` followed by an
   explicit `systemctl restart`** — `enable --now` is a no-op against
   the daemon apt's postinst has already started, so the new env file
   would never be picked up (C-15).
8. `shred -uz /etc/dbaas/bootstrap.env` so the credentials don't live
   on the OS disk after first boot. The K8s Secret remains the source
   of truth.

The per-instance CA + server cert come from
`internal/harvester/tlsgen.go`: a fresh RSA-2048 self-signed CA per
instance, used to sign a server cert valid for 10 years. Rotation is
out of scope (DEF-14). `RenewServerCert` is exported so a future phase
can add the VLAN IP as a SAN once it's known.

## Deployment

The kubebuilder scaffolding handles deployment:

```sh
# Cross-build linux/amd64 from a Mac and push to a registry:
make docker-buildx IMG=ghcr.io/<you>/dbaas-controller:v0.2.10

# Install the CRD into the cluster pointed at by ~/.kube/config:
make install

# Deploy the manager Deployment + RBAC (namespace dbaas-system):
make deploy IMG=ghcr.io/<you>/dbaas-controller:v0.2.10

# Tear it all down:
make undeploy && make uninstall
```

The RBAC the manager runs with (generated from kubebuilder markers in
`internal/controller/dbinstance_controller.go` via `make manifests`)
covers:

- `dbaas.opencloud.wso2.com` `dbinstances` (+ `/status`, `/finalizers`) —
  full CRUD.
- `kubevirt.io` `virtualmachines` — `get;create;update;delete`;
  `virtualmachineinstances` — `get`.
- `cdi.kubevirt.io` `datavolumes` — `get;create;update;delete`.
- `harvesterhci.io` `virtualmachineimages` — `get;list` (image
  resolution only).
- `monitoring.coreos.com` `servicemonitors` —
  `get;create;update;delete`.
- core `secrets` — `get;create;delete`.
- core `services` — `get;create;update;delete`.
- core `endpoints` — `get;create;update;delete`.
- core `pods` — `get;create;delete` (for the readiness probe pod).

## What this version intentionally is not

These were part of the upstream reference but were removed in earlier
commits to keep the surface small and predictable, or are explicitly
deferred. They're worth knowing about so you don't go looking for the
code:

- **No Kube-OVN VPC / Subnet / VpcPeering.** The reconciler does not
  create or peer networks. `spec.networkRef` is the *only* network
  model.
- **No second management NIC.** Removed along with `spec.consumerNetwork`.
  The VM has one interface and that VLAN carries everything.
- **No `DBSnapshot` / `DBParameterGroup` CRDs** yet (the reference repo
  has them; this module has only `DBInstance`).
- **No `multiAZ` / Patroni HA** — the field exists in the spec but the
  reconciler ignores it.
- **No working `engineVersion`** — the field is recorded but cloud-init
  installs whatever PostgreSQL the OS image's apt repo provides
  (Ubuntu 24.04 → PG 16). Set the right OS image to get the right
  version.
- **No user-supplied admin password.** `manageMasterUserPassword` and
  `masterUserPasswordRef` are both ignored; the controller always
  generates a random password into the credentials Secret.
- **No real backups.** `s3BackupConfig` / `backupRetentionPeriod` /
  `preferredBackupWindow` values are surfaced in
  `/etc/dbaas/bootstrap.env` on the VM but no pgBackRest install,
  schedule, or retention enforcement runs.
- **No `tags` propagation** — declared but not pushed to any child
  resource labels / annotations / dashboards.
- **No `status.conditions` / `status.readReplicas`** — fields exist in
  the schema for forward compatibility but the reconciler doesn't write
  them.
- **No TLS termination inside the gateway.** Authentication is enforced
  via K8s API server delegation (bearer-token forwarding), but the HTTP
  endpoint itself is plain. Front it with an ingress that terminates
  TLS for production exposure.
- **No automated TLS rotation.** Per-instance CAs are valid 10 years
  and not currently re-issued.
- **No encryption-at-rest enforced by the controller.** The pgdata
  DataVolume is plaintext ext4 on Longhorn. Longhorn-level disk
  encryption (if configured by the operator) is in scope at the
  cluster layer, not here.
- **No pre-baked OS image with PostgreSQL.** Every first boot runs
  `apt install postgresql … prometheus-postgres-exporter`, adding
  ~30–60 s and requiring VLAN egress to the package mirrors. DEF-19
  tracks shipping a pre-baked image.

A complete list of deferred features with status, source, and
effort estimates lives in `DEFERRED.md` (DEF-01..DEF-21).

## Where to look first when something goes wrong

- `kubectl get dbi -A` — quickest view of every instance and its phase.
- `kubectl describe dbi <name>` — `status.message` carries the last
  error; `status.resources` shows which Harvester objects were created.
- `kubectl logs -n dbaas-system deploy/dbaas-controller-manager` —
  controller logs include the phase transitions and probe-pod outcomes.
- `kubectl get vm,vmi,dv,secret,svc,endpoints,servicemonitor -n <tenant-ns>
  -l dbaas.opencloud.wso2.com/instance=<name>` — every owned object in
  one query.
- `kubectl get events -n <tenant-ns> --field-selector
  involvedObject.name=dbaas-probe-pg-<name>` — events from the most
  recent readiness probe (useful when the probe is failing).
- Inside the VM: `journalctl -u cloud-final` and `/etc/dbaas/bootstrap.sh`
  for first-boot debugging. `journalctl -u postgresql` for postgres-side
  errors. `systemctl status prometheus-postgres-exporter` if metrics
  show `pg_up=0`.
- `VERIFICATION.md` documents the cluster-side checks that prove each
  of the v0.2.7→v0.2.10 fixes is live, with the exact commands.
