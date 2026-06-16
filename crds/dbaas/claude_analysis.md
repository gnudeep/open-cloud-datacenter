# DBaaS CRD Analysis — Round 3

A fresh pass after the v0.2.8 → v0.2.10 work that landed Round 2's
recommendations, drove a real end-to-end deployment on Harvester 1.7.1,
and surfaced three further bugs only a live run would have caught. This
file replaces Round 2's "Recommended priority" section with status-of-each
plus a new walkthrough of how each CRD use case actually works in the
running implementation.

## Scope

Reviewed at HEAD on the `lk-dc-dev` branch (v0.2.10):

- `api/v1alpha1/dbinstance_types.go`
- `config/crd/bases/dbaas.opencloud.wso2.com_dbinstances.yaml`
- `config/rbac/role.yaml`
- `internal/controller/dbinstance_controller.go`
- `internal/controller/dbinstance_controller_test.go`
- `internal/controller/immutable_drift_test.go`
- `internal/controller/probe_gate_test.go` *(new in v0.2.9 / rewired in v0.2.10)*
- `internal/harvester/client.go`
- `internal/harvester/client_test.go`
- `internal/harvester/cloudinit.go`
- `internal/harvester/probe.go` *(new in v0.2.10)*
- `internal/harvester/tlsgen.go`
- `internal/gateway/gateway.go`
- `internal/gateway/gateway_test.go`
- `README.md`, `ARCHITECTURE.md`, `USAGE.md`, `DEPLOYMENT.md`,
  `DEFERRED.md`, `VERIFICATION.md`

`make build` clean, `make test` green (`controller 18.5 %, gateway
83.4 %, harvester 14.0 %`). End-to-end: 169 s from `kubectl apply` to
`phase=available` on a fresh VM in the validated Harvester cluster,
with `psql` DDL/DML roundtrip and `pg_up=1` from the Prometheus exporter.

## Executive Summary

Round 2's twelve findings (C-01..C-12) all landed. Three new bugs were
introduced by the same work and are also now fixed in v0.2.10:

- **C-13 — `lost+found` fooled the pgdata-migration emptiness check.**
  Caused fresh PostgreSQL clusters to come up on the wrong disk and die
  on first restart. Fixed by keying the migration off PostgreSQL's own
  `PG_VERSION` marker file instead of "is the directory empty?".
- **C-14 — `phaseWaitReady`'s direct `net.DialTimeout` cannot reach VMs
  on isolated VLANs.** The controller pod runs on the cluster overlay,
  which usually has no L3 route to the data VLAN. Replaced by a probe-pod
  pattern that attaches to the same Multus NAD as the VM.
- **C-15 — `prometheus-postgres-exporter` was started by apt's postinst
  before its env file was written, and `systemctl enable --now` is a
  no-op against an already-running daemon.** Replaced with an explicit
  `systemctl restart` so the new `DATA_SOURCE_NAME` is picked up.

Everything Round 2 said was "in noticeably better shape than the snapshot
codex reviewed" remains true, and is now backed by a live verification
trail in `VERIFICATION.md`. The remaining gaps are tracked in
`DEFERRED.md` (DEF-01..DEF-21).

## What `codex_analysis.md` flagged (Round 1) — status today

| Codex § | Topic | Status |
| --- | --- | --- |
| §1  | Misleading `manageMasterUserPassword` / `masterUserPasswordRef` | Tagged **NOT YET IMPLEMENTED** in godoc; tracked as DEF-03. |
| §2  | `engineVersion` no-op | Tagged in godoc; DEF-01. |
| §3  | Backup fields not implemented | All three tagged in godoc; DEF-02. |
| §4  | Metrics Service leaked on delete | Fixed: `ResourceRefs.MetricsServiceName` is set in `phaseMonitoring` and deleted by `TeardownAll`. Test: `TestDeployMonitoringIsIdempotent`. |
| §4  | `DeployMonitoring` had dead `vmAddr` / `pgPort` params | Removed (`client.go:437`). |
| §5  | `reconcileModify` falsely advances `observedGeneration` | Fixed: `immutableDrift` introduced and called from modify/stop/start. Tests cover both branches. |
| §6  | `TeardownAll` swallowed errors; finalizer removed prematurely | Fixed: aggregated error return; `reconcileDelete` keeps finalizer and requeues on error. |
| §7  | Weak CRD validation | Fixed: OpenAPI markers for `allocatedStorage`, `port`, `backupRetentionPeriod`, `networkRef`, `staticNetwork.*`, `preferredBackupWindow`. |
| §9  | `status.conditions` / `status.readReplicas` unused | Still unwritten (DEF-07, DEF-08). |
| §10 | No real reconciler / harvester-client tests | Partial: `immutable_drift_test.go`, `client_test.go`, `probe_gate_test.go`. Coverage: controller 18.5 %, harvester 14.0 %. DEF-17/18 still open. |
| §11 | Docs and code disagree | Fixed: godoc honesty pass; `README.md`/`ARCHITECTURE.md`/`USAGE.md` rewritten; `DEFERRED.md` and `VERIFICATION.md` added. |

## Round 2 findings (C-01..C-12) — status today

| ID | Title | Status (v0.2.10) |
| --- | --- | --- |
| **C-01** | ServiceMonitor selector targets virt-launcher pod, not data NIC | **Fixed.** Metrics flow through a manually-managed `Endpoints` populated with the VM's `data-net` IP at `client.go:473`. ServiceMonitor scrapes the `metrics` port on the Service, which DNATs to the VM. |
| **C-02** | `reconcileStop` / `reconcileStart` advance `observedGeneration` without immutable-drift check | **Fixed.** `immutableDrift` called from `reconcileStop` (`dbinstance_controller.go:424`), `reconcileStart` (`:446`), and `reconcileModify` (`:472`). |
| **C-03** | Gateway create/get/delete namespace inconsistency | **Fixed.** `handleCreateInstance` overwrites `instance.Namespace = defaultNamespace()` (`gateway.go:218`); regression test in `gateway_test.go`. |
| **C-04** | Hardcoded `osImage` default is wrong for validated cluster | **Fixed.** Single `defaultOSImage` constant in `dbinstance_controller.go:42`, referenced from `phaseVM` and `immutableDrift`. |
| **C-05** | `storageType = "longhorn"` defaulted in three places | **Fixed.** Single `defaultStorageType` constant; same pattern as C-04. |
| **C-06** | Master user is created as `SUPERUSER` | **Fixed.** `cloudinit.go:209` now `CREATE ROLE … LOGIN CREATEDB CREATEROLE`; live-verified (`rolsuper=f`). |
| **C-07** | LUKS key generated but never used | **Fixed.** Key generation and Secret storage removed; the credentials Secret no longer carries `luks_key`. |
| **C-08** | Secrets persist on the VM disk after bootstrap | **Fixed.** `shred -uz /etc/dbaas/bootstrap.env` runs at end of `bootstrap.sh` (`cloudinit.go:235`). |
| **C-09** | `data-net` interface name hardcoded in three places | **Fixed.** `dataNetInterface` constant in `client.go:76`, referenced by `GetVMIReadiness`, `vmInterfaces`, `vmNetworks`. |
| **C-10** | `phaseAvailable` issues `status.Update` on every requeue | **Fixed.** `phaseAvailable` does an `equality.Semantic.DeepEqual` against the previous status and skips the Update when unchanged. |
| **C-11** | Ginkgo smoke test doesn't exercise modify-refusal | Open. Folded into DEF-17. |
| **C-12** | Gateway tests can't catch a broken bearer-token forward | Open. Folded into DEF-17. |

## Round 3 findings (C-13..C-15) — bugs surfaced by the live run

### C-13 · pgdata migration skipped because `lost+found` fooled the emptiness check — `severity: high (fixed in v0.2.9)`

When v0.2.8 introduced the dedicated `pgdata` DataVolume, `bootstrap.sh`
gained a section that mounts `/dev/vdb`, copies the freshly-installed
PostgreSQL cluster onto it, then re-mounts at `/var/lib/postgresql` via
fstab. The "is the new disk empty?" guard read:

```bash
if [ -z "$(find /mnt/dbaas-pgdata -mindepth 1 -maxdepth 1 -print -quit)" ] \
   && [ -d "${PGDATA_MOUNT}" ]; then
    cp -a "${PGDATA_MOUNT}/." /mnt/dbaas-pgdata/
fi
```

`mkfs.ext4 -F -L pgdata /dev/vdb` always creates a `lost+found`
directory, so `find -mindepth 1` returns it, `[ -z … ]` is false, and the
copy is skipped. The script then mounts the empty disk at
`/var/lib/postgresql`, shadowing the real data, and PostgreSQL fails to
restart because `PG_VERSION` is gone. Verified live via the qemu-guest-
agent's `filesystemlist` subresource: vdb reported 24 576 bytes used
(i.e. `lost+found` and ext4 metadata only).

**Fix** (`cloudinit.go:160`): key the migration off PostgreSQL's own
marker file rather than an emptiness heuristic:

```bash
if [ ! -f "/mnt/dbaas-pgdata/${PG_VER}/main/PG_VERSION" ] \
   && [ -d "${PGDATA_MOUNT}/${PG_VER}/main" ]; then
    cp -a "${PGDATA_MOUNT}/." /mnt/dbaas-pgdata/
fi
```

The marker file is what PostgreSQL itself uses to recognise a valid data
directory, and it survives reboots — so on second-boot the script
correctly preserves existing data instead of overwriting it.

### C-14 · `phaseWaitReady`'s direct TCP probe can't reach the VM from the cluster overlay — `severity: high (fixed in v0.2.10)`

Round 2's recommendation #6 asked for a real readiness probe in
`phaseWaitReady` rather than trusting the qemu-guest-agent IP alone. The
first attempt added `net.DialTimeout(vmIP, port)` inside the reconciler:

```go
conn, derr := probeDial("tcp", addr, 3*time.Second)
```

In flat deployments (controller pod and VM share L3 routing) this works.
In any deployment where the VM lives on an isolated VLAN — which is the
intended use case for `spec.networkRef` — the controller pod has no
route to the VM, the dial returns `i/o timeout`, and the DBInstance is
stuck in `WaitingForCloudInit` forever. Verified live: the same probe
that times out from the controller pod returns immediately from a
netshoot pod attached to the same Multus NAD as the VM.

**Fix** (`internal/harvester/probe.go` — new file, 260 LoC): spawn a
one-shot Pod with the Multus annotation:

```yaml
k8s.v1.cni.cncf.io/networks: '[{"name":"vm-network","namespace":"default","interface":"probe-nic"}]'
```

The probe pod runs `busybox:1.36` (~5 MB, cached on nodes after first
pull), assigns itself a neighbor IP on the VM's subnet (`vmIP` ± 1 in
the last octet — collision risk tracked as DEF-21), runs
`nc -zvw 3 <vmIP> <port>`, and exits. The controller watches the pod
phase, reads `Succeeded` / `Failed`, and deletes the pod with a deferred
detached-context Delete so a cancelled reconcile doesn't leak it.

The fix is topology-independent: same NAD = same L2 segment, so the
dial succeeds wherever the VM is reachable at all.

### C-15 · `prometheus-postgres-exporter` env file written too late — `severity: medium (fixed in v0.2.10)`

`bootstrap.sh` writes `/etc/default/prometheus-postgres-exporter` (the
env file the systemd unit reads), then calls
`systemctl enable --now prometheus-postgres-exporter`. Problem: apt's
postinst already started the exporter during the earlier
`apt install` step, using the package's default env file (which has no
`DATA_SOURCE_NAME`). `systemctl enable --now` against an already-running
unit is a no-op — the new env file is never read. Live-verified:
`postgres_exporter_build_info` returned `1` (binary running) but
`pg_up` returned `0` (binary couldn't connect).

**Fix** (`cloudinit.go:228`): replace with an explicit `enable` plus
`restart`:

```bash
systemctl enable prometheus-postgres-exporter
systemctl restart prometheus-postgres-exporter
```

Live-verified post-fix: `pg_up 1`, `pg_database_size_bytes{datname="testdb"}`
scraped correctly, ServiceMonitor target healthy.

## How each use case of the CRD works

The CRD has six operator-visible use cases. This section traces each one
end-to-end through the gateway (if any), reconciler, harvester client,
and the cluster objects that actually change. File and line citations are
against v0.2.10.

### 1. Create — bring a new PostgreSQL instance up

**Entry points:**

- `kubectl apply -f dbi.yaml` against the K8s API server (the canonical
  path; what's documented in `USAGE.md`).
- `POST /v1/dbinstances` against the gateway
  (`gateway.go:195 handleCreateInstance`), which forwards the caller's
  bearer token to the K8s API so RBAC / audit are honoured upstream.
  The gateway forces `instance.Namespace = defaultNamespace()` so a
  caller can't strand a resource in an unreachable namespace (C-03).

**Reconcile flow** (`dbinstance_controller.go:117-134` dispatcher):

| Phase | Function | What lands in the cluster |
| --- | --- | --- |
| `Pending` → `NetworkProvisioned` | `phaseNetwork` (`:142`) | Records `Status.Resources.NADName = spec.networkRef`. The controller does not create the NAD; it must already exist (DEF-06 covers a pre-flight check). |
| `NetworkProvisioned` → `StorageProvisioned` | `phaseStorage` (`:168`) | Creates the per-instance credentials `Secret` (admin, replication, exporter passwords + per-instance TLS bundle via `tlsgen.go`) and the cloud-init `Secret` (userdata + networkdata). Generates the pgdata `DataVolume`. |
| `StorageProvisioned` → `VMCreated` | `phaseVM` (`:193`) | Creates the KubeVirt `VirtualMachine`. Spec: one NIC on `spec.networkRef`'s NAD, OS disk (provisioned via `dataVolumeTemplates`), separate pgdata `DataVolume` mounted as `/dev/vdb`, cloud-init via `secretRef` + `networkDataSecretRef`. Stores `Resources.VMName` / `Resources.DataVolumeName`. |
| `VMCreated` / `WaitingForCloudInit` → `DatabaseReady` | `phaseWaitReady` (`:272`) | **Two gates:** (1) `GetVMIReadiness` returns Running + IP from qemu-guest-agent; (2) `ProbeVMListener` (`probe.go:64`) spawns a Multus-attached probe pod, `nc -zvw 3 <vmIP> 5432`, requires `Succeeded`. Writes `Status.Endpoint`. |
| `DatabaseReady` → `MonitoringDeployed` | `phaseMonitoring` (`:308`) | Creates `ClusterIP` Service `pg-<id>-metrics` (port 9187) and a manually-populated `Endpoints` with the VM's `data-net` IP (this is the C-01 fix — selector-based won't work because the Service is fronting a non-Pod). Creates a `ServiceMonitor` so the Prometheus Operator scrapes it. |
| `MonitoringDeployed` → `Available` | `phaseAvailable` (`:338`) | Marks `Status.Phase = available`, writes `Endpoint` and `PrometheusTarget` if not already set. Uses `equality.Semantic.DeepEqual` against the previous status to skip the API write when nothing changed (C-10). Requeues every 60 s for liveness. |

**What happens inside the VM during `WaitingForCloudInit`:**

cloud-init runs the embedded `bootstrap.sh` (`cloudinit.go:127-235`):

1. `apt install postgresql postgresql-contrib jq qemu-guest-agent prometheus-postgres-exporter`
2. Detect `/dev/vdb` and (if `PG_VERSION` is absent) copy `/var/lib/postgresql/.`
   onto it, then fstab-mount at `/var/lib/postgresql` (C-13 fix).
3. Patch `postgresql.conf`: `listen_addresses='*'`, `ssl=on`, configured port.
4. Append `hostssl all all 0.0.0.0/0 scram-sha-256` to `pg_hba.conf`.
5. Restart postgres.
6. Create `dbadmin` (`LOGIN CREATEDB CREATEROLE` — not SUPERUSER, C-06) and
   `postgres_exporter` (with `pg_monitor`) roles.
7. Write exporter env file, **`systemctl enable` + `systemctl restart`** the
   exporter (C-15 fix).
8. `shred -uz /etc/dbaas/bootstrap.env` (C-08).

**Status fields populated:**
`status.phase`, `status.provisioningPhase`, `status.observedGeneration`,
`status.message`, `status.endpoint`, `status.caCertPem`,
`status.grafanaUrl`, `status.prometheusTarget`, `status.resources.*`,
`status.appliedSpec` (immutable snapshot for modify-time comparison).

**Idempotency:** every phase checks `Status.Resources.*` first and skips
the create when the resource is already recorded; harvester client
helpers use `createOrUpdate` to tolerate AlreadyExists.

### 2. Describe — inspect an existing instance

**Entry points:**

- `kubectl get dbinstance / dbi <name>` (the `+kubebuilder:resource:shortName=dbi`
  alias). The columns shown are wired via `+kubebuilder:printcolumn` in
  `dbinstance_types.go`: `PHASE`, `CLASS`, `ENDPOINT`, `AGE`.
- `kubectl describe dbi <name>` — surfaces conditions (when DEF-07 lands)
  and recent events.
- `GET /v1/dbinstances` (list) and `GET /v1/dbinstances/{name}` (get) on
  the gateway (`gateway.go:181, 271`). Both forward the caller's bearer
  token to the K8s API.

**No reconciler involvement** — describe is a pure read. The status the
caller sees is exactly what the reconciler last wrote during a
`statusUpdate` call. The `equality.Semantic.DeepEqual` skip in
`phaseAvailable` (C-10) keeps the `resourceVersion` from churning, so
watching clients don't see flapping events.

**Key visible fields:**
`status.endpoint.address` is the data-NIC IP (what apps connect to);
`status.endpoint.jdbcUrl` is the ready-to-paste JDBC string;
`status.masterUserSecret.name` points at the `Secret` holding
`admin_user` / `admin_password` (and the per-instance CA cert in
`ca_cert`); `status.prometheusTarget` is the `host:port` your Prometheus
Operator scrape targets.

### 3. Modify — change an existing instance's spec

**Entry points:**

- `kubectl edit dbi <name>` or `kubectl apply` with a changed YAML.
- `PATCH /v1/dbinstances/{name}` on the gateway (`gateway.go:299`)
  which does a JSON-merge against the live resource and re-applies it.

**Reconcile flow:**

The dispatcher (`dbinstance_controller.go:117-128`) routes any DBInstance
where `Status.Phase == Available` and `Generation != ObservedGeneration`
to `reconcileModify` (`:465`):

1. `immutableDrift(inst)` (`:516`) compares `Status.AppliedSpec` against
   the current `Spec`, defaulting both sides identically (so a user who
   explicitly types the same default value of an optional field doesn't
   trigger a false drift). Checks:
   `networkRef`, `osImage`, `dbName`, `masterUsername`, `engineVersion`,
   `port`, `storageType`.
2. **If any immutable field changed:** the reconcile refuses, writes
   `Status.Phase = Failed` with a `ImmutableSpecChanged` reason naming
   the offending fields, and does **not** advance `observedGeneration`.
   The user sees `kubectl describe` reporting the rejected fields and
   can revert with another `kubectl apply`.
3. **Otherwise:** the mutable fields are applied. Currently that's
   `cpuCores` / `memoryMB` (via `Harvester.ResizeVM`, which patches
   `vm.spec.template.spec.domain.cpu.cores` and `.domain.memory.guest`),
   and `Spec.Running` (delegated to the stop/start paths). Disk resizes
   are not yet wired (open work — see DEF-19 family).
4. `AppliedSpec` is refreshed to the new effective spec; `ObservedGeneration`
   is advanced.

**Tests:** `TestImmutableDriftNormalizesCreateDefaults` and
`TestImmutableDriftDetectsActualImmutableChange` exercise both branches
of the drift detector.

### 4. Stop — pause the instance without losing data

**Entry points:**

- `kubectl patch dbi <name> --type=merge -p '{"spec":{"running":false}}'`.
- `POST /v1/dbinstances/{name}/stop` on the gateway (`gateway.go:366`,
  invoked via `handleSetRunning(..., running=false)`).

**Reconcile flow:**

The dispatcher (`dbinstance_controller.go:111`) matches
`spec.running=false && status.phase=Available` and routes to
`reconcileStop` (`:418`):

1. **`immutableDrift` check first** (C-02 fix at `:424`) — if the same
   PATCH that flipped `running` also tried to change e.g. `dbName`, the
   change is refused and the stop is not performed. Without this guard
   the immutable field would silently disappear because the dispatcher
   doesn't reach `reconcileModify` once it hits the stop branch.
2. Otherwise, `Harvester.StopVM` (`client.go:407`) patches
   `vm.spec.running = false`. KubeVirt tears down the VMI but keeps the
   `VirtualMachine`, the pgdata `DataVolume`, the credentials `Secret`,
   the `Service`, and the `ServiceMonitor`. The data on `/dev/vdb`
   persists on Longhorn.
3. Status advances to `Phase = Stopped`; `ObservedGeneration` is updated;
   the controller stops reconciling on a tight loop (60 s requeue).

### 5. Start — resume a stopped instance

**Symmetric counterpart of Stop.**

**Entry points:**

- `kubectl patch dbi <name> --type=merge -p '{"spec":{"running":true}}'`.
- `POST /v1/dbinstances/{name}/start` on the gateway
  (`handleSetRunning(..., running=true)`).

**Reconcile flow** (`reconcileStart` at `:444`):

1. **`immutableDrift` check first** (C-02 fix at `:446`).
2. `Harvester.StartVM` sets `vm.spec.running = true`. KubeVirt recreates
   the VMI from the existing VM spec, attaches the same pgdata
   `DataVolume`, and cloud-init does **not** re-run (cloud-init is
   first-boot-only; `bootstrap.sh` already shredded its own state).
3. PostgreSQL boots from the existing pgdata on `/dev/vdb` (the fstab
   entry persists the mount); the qemu-guest-agent re-registers the IP.
4. Phase transitions back through `WaitingForCloudInit` →
   `DatabaseReady` → `MonitoringDeployed` → `Available`. The probe-pod
   gate still applies — postgres has to actually answer on `:5432`
   before the controller will mark it available.

### 6. Delete — tear the instance down

**Entry points:**

- `kubectl delete dbi <name>`.
- `DELETE /v1/dbinstances/{name}` on the gateway (`gateway.go:345`).

**Reconcile flow:**

K8s marks the DBInstance with `metadata.deletionTimestamp`. The finalizer
(`dbaas.opencloud.wso2.com/cleanup`, added in the first reconcile at
`:103`) keeps the object alive until cleanup actually completes.

`reconcileDelete` (`:585`) calls `Harvester.TeardownAll` (`client.go:515`),
which deletes in dependency order and returns aggregated errors:

```
ServiceMonitor → Service → Endpoints → VirtualMachine → DataVolume → Secret
```

- If `TeardownAll` returns an error, `reconcileDelete` keeps the
  finalizer in place and requeues. The DBInstance stays visible to
  `kubectl get` with a non-empty `deletionTimestamp` — operators can
  see something is stuck.
- If `TeardownAll` succeeds, the finalizer is removed and K8s garbage-
  collects the object.

**What survives a delete:** the Multus NetworkAttachmentDefinition (the
controller never owned it) and any externally-managed RBAC, namespaces,
or NAD-referenced infrastructure. Everything the controller created is
fully cleaned up (the leak that codex §4 flagged is gone).

## Strengths since the codex review (carried forward + new)

Worth calling out — things to keep, not just things that got fixed:

- `TeardownAll` returns aggregated errors and `reconcileDelete` keeps
  the finalizer until clean. **(carried from Round 2)**
- `DeployMonitoring` is idempotent for both Service and ServiceMonitor.
  **(carried)**
- `immutableDrift` is correctly normalised on both sides — defaulted
  values match defaulted snapshots — and is now applied on all three
  spec-changing paths (modify / stop / start). **(extended in Round 2 +
  C-02 fix in Round 3)**
- `MetricsServiceName`, `ServiceMonitor`, and the new `pgdata`
  `DataVolume` are all tracked in `Status.Resources` for cleanup.
- Per-instance TLS bundle (CA + server cert/key generated by
  `tlsgen.go`) is correctly mounted and used by postgres for
  `hostssl all all 0.0.0.0/0 scram-sha-256`. Live-verified.
- Field-by-field godoc honesty: every reserved field says **NOT YET
  IMPLEMENTED** in `kubectl explain` output. **(carried)**
- `DEFERRED.md` formalises gap-tracking (now DEF-01..DEF-21).
- **(new)** `phaseWaitReady` has a real readiness gate: VMI agent IP +
  TCP probe from a Multus-attached pod. Past regressions where a broken
  postgres slipped through as "Available" can't happen the same way.
- **(new)** Probe pod cleanup is detached-context deferred so a
  cancelled reconcile can't leak a pod. Live-verified after 169 s
  bootstrap: zero probe pods left in the namespace.
- **(new)** `VERIFICATION.md` documents the cluster-side checks that
  prove each fix is live, so future regressions are catchable without
  re-deriving the test commands.

## Outstanding work

Tracked individually in `DEFERRED.md`. The shape of what's left:

- **Contract maturity:** DEF-01 (engineVersion), DEF-02 (backups),
  DEF-03 (user-supplied master password), DEF-09 (`DBSnapshot`),
  DEF-10 (multiAZ / Patroni), DEF-11 (`DBParameterGroup`). All require
  spec changes or new CRDs.
- **Operational hardening:** DEF-06 (NAD existence pre-flight),
  DEF-07 (`status.conditions`), DEF-13 (validating webhook),
  DEF-14 (TLS rotation), DEF-15 (gateway TLS), DEF-16 (per-tenant
  gateway RBAC), DEF-19 (pre-baked OS image), DEF-20 (smaller OS disk).
- **Testing:** DEF-17 (controller integration tests; absorbs C-11,
  C-12), DEF-18 (harvester-client tests).
- **Probe pod topology:** DEF-21 (IP-collision risk on shared VLANs;
  long-term escape is cloud-init phone-home to the gateway).

## Bottom line

The codebase has moved from "good design, several silent failure modes"
(codex's snapshot) through "all twelve cleanup items landed" (Round 2)
to "validated end-to-end on real Harvester, with the three bugs that
emerged also closed" (Round 3 / v0.2.10). The hardest-to-trust failure
modes — silent error swallowing, falsely-advancing `observedGeneration`,
broken pgdata migration, controller-blind probe — are now demonstrably
fixed and have tests *and* cluster-side verification recipes. What's
left is contract maturity (the unimplemented CRD fields) and operational
hardening (webhooks, conditions, RBAC-aware gateway). Nothing on the
outstanding list changes the architecture; it's all forward feature work
on a now-solid base.
