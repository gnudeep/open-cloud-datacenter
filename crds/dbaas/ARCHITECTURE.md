# dbaas controller — architecture

A Kubernetes-native Database-as-a-Service controller that provisions managed
PostgreSQL instances as KubeVirt virtual machines on a Harvester HCI cluster.
Each database is a `DBInstance` custom resource; the controller reconciles
that resource into a running, SSL-only PostgreSQL VM with monitoring, on a
VLAN the operator names.

## At a glance

- **CRD:** `DBInstance` (group `dbaas.opencloud.wso2.com`, version `v1alpha1`,
  kind shortname `dbi`). One CR == one PostgreSQL VM.
- **Two interfaces** for callers:
  1. `kubectl apply` a `DBInstance` YAML.
  2. A small REST gateway (`:8080`) that does the same writes over HTTP.
- **Async by design.** Every mutating call (create / modify / stop / start /
  delete) returns immediately; the reconciler advances the work in the
  background and reflects progress in `status.phase` and
  `status.provisioningPhase`. Callers poll the CR (or `GET /dbinstances/{name}`)
  for state.
- **Single-NIC VM, single VLAN.** The VM has one network interface, bridged
  onto whatever Multus `NetworkAttachmentDefinition` the operator names in
  `spec.networkRef`. Client traffic, `apt` during cloud-init, and Prometheus
  scraping all flow through that one VLAN.

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
│             │  controller-runtime client.Client      │                  │
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
│  │  Service, Prometheus ServiceMonitor, Harvester                    │  │
│  │  VirtualMachineImage (image resolution only)                      │  │
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

**Why dynamic client.** The reconciler never imports the KubeVirt/CDI/Prometheus
Go schemas. It builds and reads `unstructured.Unstructured` objects against
the GVRs in `internal/harvester/client.go`. That keeps `go.mod` small and lets
the controller run on Harvester versions whose Go types we haven't pinned.

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
│   │   └── dbinstance_controller_test.go
│   ├── gateway/                    # thin HTTP layer over the CRD
│   │   ├── gateway.go
│   │   └── gateway_test.go
│   └── harvester/                  # dynamic-client wrapper
│       ├── client.go               #   VM, DataVolume, Secret, Service,
│       │                           #   ServiceMonitor, VMImage resolve,
│       │                           #   TeardownAll
│       ├── cloudinit.go            #   PostgreSQL bootstrap script
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
Harvester object it spawns (VM, DataVolume, Secret, Service, ServiceMonitor)
is created in the same namespace.

### Spec — what the caller asks for

| Field | Required | Default | Notes |
| --- | --- | --- | --- |
| `dbInstanceClass` | ✓ | — | Sizing class, e.g. `db.t3.medium`, `db.m5.large`. Maps to (CPU, memory, `max_connections`) via the `InstanceClasses` table in `api/v1alpha1/dbinstance_types.go`. |
| `allocatedStorage` | ✓ | — | `pgdata` volume size in GiB. |
| `networkRef` | ✓ | — | `namespace/name` of an existing Multus NAD (typically a Harvester VLAN NAD). The VM's only network. |
| `engineVersion` |   | `"16"` | PostgreSQL major version. |
| `dbName` |   | instance name | Initial database created on first boot. |
| `port` |   | `5432` | PostgreSQL listen port. |
| `masterUsername` |   | `dbadmin` | Admin role created on first boot. |
| `manageMasterUserPassword` |   | `false` | If true, a 32-char password is generated and stored in the credentials Secret. |
| `masterUserPasswordRef` |   | — | Alternative: read the password from an existing Secret. |
| `storageType` |   | `longhorn` | StorageClass for the `pgdata` DataVolume. |
| `osImage` |   | `ubuntu-22.04-server-cloudimg-amd64.img` | Harvester `VirtualMachineImage` name, `ns/name`, or `displayName`. The OS disk uses the image's image-managed StorageClass. |
| `vmPassword` |   | — | Console/SSH password for the `ubuntu` user. Development only — leave empty in production. |
| `running` |   | `true` | `false` stops the VM (storage preserved). |
| `deletionProtection` |   | `false` | When true, the controller refuses to teardown the instance. |
| `backupRetentionPeriod` |   | `0` | Days of pgBackRest retention. `0` disables backups. **NOT YET IMPLEMENTED** — value is recorded but no backup process runs. |
| `preferredBackupWindow` |   | — | UTC `HH:MM-HH:MM`. |
| `s3BackupConfig` |   | — | `{endpoint, bucket, region, secretRef}` for pgBackRest. **NOT YET IMPLEMENTED** — values are written to `/etc/dbaas/bootstrap.env` on the VM but no backup process consumes them. |
| `multiAZ` |   | `false` | Reserved for Patroni HA; not implemented yet. |
| `dbParameterGroupRef` |   | — | Reserved for a future `DBParameterGroup` CRD. |
| `tags` |   | — | User labels carried through to monitoring dashboards. |

### Status — what the controller reports

| Field | Meaning |
| --- | --- |
| `phase` | RDS-style lowercase: `creating`, `available`, `stopping`, `stopped`, `starting`, `modifying`, `deleting`, `failed`. |
| `provisioningPhase` | Internal reconcile step (PascalCase). See the phase table below. |
| `endpoint` | `{address, port, jdbcUrl}` once the VM is reachable. `jdbcUrl` uses `sslmode=verify-ca`. |
| `masterUserSecret` | `{name, status}` pointing at the K8s Secret holding the credentials. |
| `resources` | `{nadName, dataVolumeName, vmName, secretName, serviceMonitor}` — populated as phases complete, used for idempotency on restart and for `TeardownAll`. |
| `caCertPEM` | PEM-encoded CA generated per instance, so clients can pin it. |
| `grafanaUrl`, `prometheusTarget` | Set once monitoring is deployed. |
| `observedGeneration` | The `.metadata.generation` last fully reconciled. Drives the modify path. |
| `conditions` | Standard `metav1.Condition` list for future use. |
| `message` | Human-readable status string. |

## REST gateway

`internal/gateway/gateway.go` exposes the six operations the controller cares
about as plain HTTP, all reading and writing the `DBInstance` CR via the
controller-runtime client. Every mutating call is asynchronous: the gateway
writes the CR and returns `202 Accepted`; the reconciler does the work.

| Verb | Path | What it does |
| --- | --- | --- |
| `GET` | `/healthz` | Liveness ping. |
| `GET` | `/dbinstances` | List `DBInstance`s in the default namespace. |
| `POST` | `/dbinstances` | **create db** — full `DBInstance` body. |
| `GET` | `/dbinstances/{name}` | **describe db** — current spec + status. |
| `PATCH` | `/dbinstances/{name}` | **modify db** — partial spec (class, storage, backup, running, deletion protection). |
| `DELETE` | `/dbinstances/{name}` | **delete db** — sets `DeletionTimestamp`; reconciler tears resources down under the finalizer. |
| `POST` | `/dbinstances/{name}/start` | **start db** — sets `spec.running = true`. |
| `POST` | `/dbinstances/{name}/stop` | **stop db** — sets `spec.running = false`. |

The default namespace is `default`, overridable with the `DBAAS_DEFAULT_NAMESPACE`
environment variable.

**Authentication.** Every request except `/healthz` must carry
`Authorization: Bearer <token>` where `<token>` is a Kubernetes credential
the cluster's API server accepts (a ServiceAccount token, an OIDC ID token,
etc.). The gateway clones its `rest.Config`, replaces the bearer token with
the caller's, builds a per-request controller-runtime client, and uses it
for the K8s API call — so authentication, RBAC, and audit are enforced by
the K8s API server on the caller's identity, not on the manager's
ServiceAccount. RBAC denials from the API server propagate back as `403
Forbidden`; unknown or expired tokens become `401 Unauthorized`. The
manager never elevates beyond what the caller is RBAC-authorized to do.

## Reconciler — phase state machine

`internal/controller/dbinstance_controller.go` advances exactly one phase per
`Reconcile`, persists status, and requeues. Every phase is idempotent: it
checks `status.resources` before doing work and re-enters cleanly after a
controller restart.

```
       Pending  (or empty)
          │
          ▼
   NetworkProvisioned         echo spec.networkRef into status.resources.nadName
          │
          ▼
   StorageProvisioned         CDI DataVolume "pg-{id}-data"  (Longhorn)
          │
          ▼
   VMCreated                  TLS bundle, credentials Secret, KubeVirt VM
          │                   "pg-{id}". Requeue after 10s.
          ▼
   WaitingForCloudInit        Poll VMI: must be Running + have an IP +
          │                   uptime > 3 min (heuristic for cloud-init done).
          ▼
   DatabaseReady              status.endpoint populated, JDBC URL emitted.
          │
          ▼
   MonitoringDeployed         Headless Service + Prometheus ServiceMonitor.
          │                   Failure is non-fatal.
          ▼
   Available                  status.phase = "available". Re-checks the VMI
                              IP on every requeue (every 60s).
```

Non-linear paths:

- **`Failed`** — any phase can transition to `Failed` via `r.fail()`. The
  controller requeues every 30s.
- **Stop / Start** — if the instance is `Available` and `spec.running` flips
  to `false`, the reconciler shortcuts to `reconcileStop` (KubeVirt
  `spec.running = false`, preserves storage). The inverse for start.
- **Modify** — if the instance is `Available` and `metadata.generation !=
  status.observedGeneration`, `reconcileModify` resizes the VM (CPU, memory)
  and the `pgdata` DataVolume *in parallel*, then sets `observedGeneration`.
- **Delete** — `DeletionTimestamp` triggers `reconcileDelete`. If
  `spec.deletionProtection` is true, deletion is refused. Otherwise
  `TeardownAll` deletes ServiceMonitor, VM, DataVolume and Secret in parallel,
  then the finalizer is removed. The NAD and the tenant namespace are owned
  by the operator and are never touched.

### What `Failed` actually persists

`r.fail(ctx, inst, reason, err)` writes `status.phase = "failed"`,
`status.provisioningPhase = "Failed"`, and `status.message = "<reason>: <err>"`.
The reconciler retries every 30s — if the underlying problem clears, the next
reconcile picks up from the last completed phase recorded in
`status.resources`.

## What gets created on Harvester

For an instance named `orders-prod` in namespace `tenant-acme`, the controller
creates:

| Kind | Name | Notes |
| --- | --- | --- |
| Secret (Opaque) | `pg-orders-prod-credentials` | `admin_user`, `admin_password`, `repl_password`, `exporter_password`, `luks_key`, `ca_cert`, `ca_key`, `server_cert`, `server_key`, `userdata` (cloud-init). |
| CDI DataVolume | `pg-orders-prod-data` | `spec.allocatedStorage` GiB, `Block` mode, ReadWriteOnce, `storageClassName = spec.storageType` (default `longhorn`). |
| KubeVirt VirtualMachine | `pg-orders-prod` | One NIC `data-net` bridged onto `spec.networkRef`; OS disk cloned from a Harvester `VirtualMachineImage`; `pgdata-disk` from the DataVolume; `cloudinit` sourced from the Secret. |
| Service (headless) | `pg-orders-prod-metrics` | Selects on `dbaas.opencloud.wso2.com/instance=<id>`, port `9187` (would be `postgres_exporter`). **NOT YET IMPLEMENTED**: the Service is created, but the VM doesn't run an exporter, so Prometheus will scrape a closed port until that's wired in. |
| ServiceMonitor | `pg-orders-prod-monitor` | 15s scrape, matchLabels `metrics=true` + `instance=<id>`. |

All objects carry the `dbaas.opencloud.wso2.com/instance=<id>` label so they
can be located by label, and the controller tracks them by name in
`status.resources` so it doesn't depend on label-list operations during
cleanup.

### VM network layout

```
┌─────────────────────────────────────────────────────────────────┐
│  KubeVirt VirtualMachine: pg-{id}                               │
│                                                                 │
│   Disks                                                         │
│   ──────────                                                    │
│   os-disk        DataVolume "pg-{id}-os"   (20Gi, image clone,  │
│                                            image-managed SC)    │
│   pgdata-disk    DataVolume "pg-{id}-data" (Longhorn, sized by  │
│                                            spec.allocatedStorage)│
│   cloudinit      Secret "pg-{id}-credentials" (userdata key)    │
│                                                                 │
│   NIC                                                           │
│   ──────────                                                    │
│   data-net       Multus bridge -> spec.networkRef NAD           │
│                  (DHCP, matched on driver=virtio_net)           │
│                                                                 │
│   PostgreSQL                                                    │
│   ──────────                                                    │
│   listen_addresses = '*'    port = spec.port    SSL = on        │
│   pg_hba: hostssl all all 0.0.0.0/0 scram-sha-256               │
│   (plain-text connections are rejected at the protocol level)   │
└─────────────────────────────────────────────────────────────────┘
```

That is intentionally the *whole* network model in this version. There is no
pod-network management NIC, no Kube-OVN VPC, no VPC peering. If the database
needs to be reachable from a particular set of workloads, the operator
configures that by pointing `spec.networkRef` at the right VLAN — for
example, the VLAN where the Rancher/RKE2 cluster lives.

## Cloud-init bootstrap

`internal/harvester/cloudinit.go` generates the `userdata` written into the
credentials Secret and referenced by the VM as a `cloudInitNoCloud` disk. On
first boot the VM:

1. Installs `postgresql`, `postgresql-contrib`, `jq`, `qemu-guest-agent` via
   `apt` (over the VLAN — that VLAN must reach upstream package mirrors).
2. Writes `/etc/netplan/60-data-net.yaml` matching on `driver: virtio_net`,
   so DHCP runs regardless of how the kernel names the NIC.
3. Writes `/etc/dbaas/bootstrap.env` (credentials, port, max connections,
   LUKS key, optional S3 backup config) and `/etc/dbaas/bootstrap.sh`.
4. Drops the per-instance CA + server cert under `/etc/ssl/{certs,private}/`.
5. Runs `bootstrap.sh`:
   - Edits `postgresql.conf` for `listen_addresses`, `port`, `max_connections`,
     and `ssl_*` paths.
   - Appends `hostssl … scram-sha-256` lines to `pg_hba.conf` (no plain-text
     remote auth).
   - Restarts PostgreSQL, then creates the admin role (if missing) and the
     initial database (if missing).

The CA and server cert come from `internal/harvester/tlsgen.go`: a fresh
RSA-2048 self-signed CA per instance, used to sign a server cert valid for 10
years (rotation is out of scope). `RenewServerCert` is exported so a future
phase can add the VLAN IP as a SAN once it's known.

## Deployment

The kubebuilder scaffolding handles deployment:

```sh
# Cross-build linux/amd64 from a Mac and push to a registry:
make docker-buildx IMG=ghcr.io/<you>/dbaas-controller:v0.1.0

# Install the CRD into the cluster pointed at by ~/.kube/config:
make install

# Deploy the manager Deployment + RBAC (namespace dbaas-system):
make deploy IMG=ghcr.io/<you>/dbaas-controller:v0.1.0

# Tear it all down:
make undeploy && make uninstall
```

The RBAC the manager runs with (generated from kubebuilder markers in
`internal/controller/dbinstance_controller.go` via `make manifests`) covers:

- `dbaas.opencloud.wso2.com` `dbinstances` (+ `/status`, `/finalizers`) — full CRUD.
- `kubevirt.io` `virtualmachines` — CRUD; `virtualmachineinstances` — read.
- `cdi.kubevirt.io` `datavolumes` — CRUD.
- `harvesterhci.io` `virtualmachineimages` — read (for image resolution).
- `monitoring.coreos.com` `servicemonitors` — create/delete.
- core `secrets` — CRUD; `services` — create.

## What this version intentionally is not

These were part of the upstream reference but were removed in earlier commits
to keep the surface small and predictable. They're worth knowing about so
you don't go looking for the code:

- **No Kube-OVN VPC / Subnet / VpcPeering.** The reconciler does not create or
  peer networks. `spec.networkRef` is the *only* network model.
- **No second management NIC.** Removed along with `spec.consumerNetwork`. The
  VM has one interface and that VLAN carries everything.
- **No `DBSnapshot` / `DBParameterGroup` CRDs** yet (the reference repo has
  them; this module has only `DBInstance`).
- **No `multiAZ` / Patroni HA** — the field exists in the spec but the
  reconciler ignores it.
- **No working `engineVersion`** — the field is recorded but cloud-init
  installs whatever PostgreSQL the OS image's apt repo provides (Ubuntu
  24.04 → PG 16). Set the right OS image to get the right version.
- **No user-supplied admin password.** `manageMasterUserPassword` and
  `masterUserPasswordRef` are both ignored; the controller always
  generates a random password into the credentials Secret.
- **No real backups.** `s3BackupConfig` / `backupRetentionPeriod` /
  `preferredBackupWindow` values are surfaced in `/etc/dbaas/bootstrap.env`
  on the VM but no pgBackRest install, schedule, or retention enforcement
  runs.
- **No working monitoring exporter.** The Service and ServiceMonitor are
  created and tracked for cleanup, but the VM doesn't run
  `postgres_exporter` — the scrape target is closed.
- **No `tags` propagation** — declared but not pushed to any child
  resource labels / annotations / dashboards.
- **No `status.conditions` / `status.readReplicas`** — fields exist in
  the schema for forward compatibility but the reconciler doesn't write
  them.
- **No TLS termination inside the gateway.** Authentication is enforced
  via K8s API server delegation (bearer-token forwarding), but the HTTP
  endpoint itself is plain. Front it with an ingress that terminates TLS
  for production exposure.
- **No automated TLS rotation.** Per-instance CAs are valid 10 years and
  not currently re-issued.

## Where to look first when something goes wrong

- `kubectl get dbi -A` — quickest view of every instance and its phase.
- `kubectl describe dbi <name>` — `status.message` carries the last error;
  `status.resources` shows which Harvester objects were created.
- `kubectl logs -n dbaas-system deploy/dbaas-controller-manager` — controller
  logs include the phase transitions.
- `kubectl get vm,vmi,dv,secret,svc,servicemonitor -n <tenant-ns> -l dbaas.opencloud.wso2.com/instance=<name>`
  — every owned object in one query.
- Inside the VM: `journalctl -u cloud-final` and `/etc/dbaas/bootstrap.sh`
  for first-boot debugging.
