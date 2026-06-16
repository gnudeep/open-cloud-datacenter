# dbaas controller — deferred features

Running list of work the schema or docs reference but the controller does
**not** implement today. Each item has an ID for cross-referencing in
commits and PRs, a status, the source that surfaced it, what it covers, and
a rough effort estimate.

Keep this file honest:

- When a new gap is identified (review, user report, code smell), add an
  entry here with a fresh ID.
- When work lands, **delete the entry** in the same PR — don't mark it
  "done", just remove it. The doc reflects what's still owed.
- Cross-reference the ID in commit messages: e.g. `closes DEF-04`.

## Legend

| Status | Meaning |
| --- | --- |
| **Reserved-API** | Field exists in the CRD schema for forward compatibility but the reconciler ignores it. Documented as NOT YET IMPLEMENTED in godoc. Removing it later would be a breaking change, so it stays. |
| **Planned** | We intend to ship it; rough design is known. |
| **Open** | Identified gap, no design yet. |

**Effort** is a t-shirt size (S ≤ ½ day, M ½–2 days, L 2–5 days, XL > 1 week)
on top of an engineer already familiar with the codebase.

## API contract gaps

### DEF-01 · `engineVersion` is recorded but does not drive PostgreSQL version

- **Status:** Reserved-API
- **Source:** codex_analysis §2; confirmed live on Harvester 1.7.1
- **What:** `spec.engineVersion` is accepted and snapshotted but
  `bootstrap.sh` installs the OS image's apt-default PostgreSQL (Ubuntu
  24.04 → 16; older Ubuntus → older). A user can request `"15"` on a
  24.04 image and get 16 back.
- **Done when:** Either (a) bootstrap.sh installs the explicit
  `postgresql-<N>` package from the PGDG repo, or (b) the controller
  maps `engineVersion` to a different `osImage` automatically.
- **Effort:** M for option (a), L for option (b).

### DEF-02 · Real backups

- **Status:** Reserved-API
- **Source:** codex_analysis §3
- **What:** `spec.s3BackupConfig`, `spec.backupRetentionPeriod`,
  `spec.preferredBackupWindow` are recorded into `/etc/dbaas/bootstrap.env`
  on the VM but **nothing reads them**: no pgBackRest install, no schedule,
  no retention enforcement, no restore path, no `status.backup`.
- **Done when:** pgBackRest installed in the OS image / bootstrap.sh;
  systemd-timer / cron schedule honours `preferredBackupWindow`;
  `backupRetentionPeriod` prunes; `status.backup` reports last-success
  time, size, S3 path; restore flow defined (probably via a separate
  `DBSnapshot` CRD — see DEF-12).
- **Effort:** L.

### DEF-03 · User-supplied admin password

- **Status:** Reserved-API
- **Source:** codex_analysis §1
- **What:** `spec.manageMasterUserPassword` and `spec.masterUserPasswordRef`
  are both ignored. The controller always generates a 32-char random
  password into the credentials Secret.
- **Done when:** When `manageMasterUserPassword=false`, the controller
  reads the password from `masterUserPasswordRef` (a `SecretKeyRef`),
  validates the two fields are mutually exclusive at admission, and
  refuses to rotate the existing password silently.
- **Effort:** S–M (validation webhook is the bigger half).

### DEF-04 · `tags` propagation

- **Status:** Reserved-API
- **Source:** codex_analysis §11 / field matrix
- **What:** `spec.tags` is a `map[string]string` that exists in the schema
  but is not pushed to child VM labels/annotations, Service labels,
  ServiceMonitor labels, or Grafana dashboards.
- **Done when:** Tags propagated to every child object the reconciler
  creates, with a clear conflict policy vs the controller's own labels
  (`dbaas.opencloud.wso2.com/*`).
- **Effort:** S.

## Functional features

### DEF-05 · `postgres_exporter` not installed → metrics scrape target is closed

- **Status:** Open
- **Source:** codex_analysis §4; verified live
- **What:** `phaseMonitoring` creates a headless `Service` and a
  `ServiceMonitor` pointing at port 9187, but the VM doesn't run any
  exporter — Prometheus will scrape a closed port. Both objects are
  tracked for cleanup (since DEF-fix in this session), but until the
  exporter actually exists they serve no purpose.
- **Done when:** `bootstrap.sh` installs and enables
  `prometheus-postgres-exporter` (or equivalent) bound to `:9187`;
  exporter authenticates to PostgreSQL with the `exporter_password`
  already in the credentials Secret; ServiceMonitor scrape returns
  metrics in a Prometheus query.
- **Effort:** M.

### DEF-06 · NAD existence check before advancing `NetworkProvisioned`

- **Status:** Open
- **Source:** codex_analysis §8
- **What:** `phaseNetwork` only checks `spec.networkRef != ""`. If the
  caller types a non-existent NAD name, the failure surfaces 2-3 minutes
  later when the VMI fails to attach, not at admission.
- **Done when:** `phaseNetwork` parses `<ns>/<name>` and does a `Get` on
  the `NetworkAttachmentDefinition` before advancing. Needs an extra RBAC
  perm in `internal/controller/dbinstance_controller.go`:
  `kubebuilder:rbac:groups=k8s.cni.cncf.io,resources=network-attachment-definitions,verbs=get`.
- **Effort:** S.

### DEF-07 · `status.conditions` never written

- **Status:** Open
- **Source:** codex_analysis §9
- **What:** `status.conditions` is declared on `DBInstanceStatus` using the
  standard `metav1.Condition` shape but the reconciler never writes to
  it. Conditions are the K8s-idiomatic way for downstream consumers
  (other controllers, dashboards, `kubectl wait --for=condition=...`) to
  watch readiness/failure.
- **Done when:** Conditions written for at least `Ready`, `StorageReady`,
  `VMReady`, `DatabaseReady`, `MonitoringReady`. `kubectl wait
  --for=condition=Ready dbi/foo` works. `BackupReady` follows DEF-02.
- **Effort:** M.

### DEF-08 · `status.readReplicas` never populated

- **Status:** Reserved-API
- **Source:** codex_analysis §9
- **What:** Field exists. No read-replica feature exists. Implied by
  DEF-10 (multiAZ / HA).
- **Effort:** Folded into DEF-10.

## HA, scale, and related CRDs

### DEF-09 · `DBSnapshot` CRD

- **Status:** Open
- **Source:** upstream `gnudeep/ocd-dbaas` had it; this module does not
- **What:** Point-in-time backup metadata as its own CRD. Spec:
  `dbInstanceRef`, `snapshotType` (full/diff/incr); status: S3 path,
  size, completion time. Needed if backups become real (DEF-02).
- **Done when:** CRD exists; controller reconciles snapshot creation,
  status, and (separately) restore.
- **Effort:** L (alongside DEF-02).

### DEF-10 · `multiAZ` / Patroni HA standby

- **Status:** Reserved-API
- **Source:** codex_analysis §field matrix
- **What:** `spec.multiAZ` exists but the reconciler ignores it; no
  standby VM is created.
- **Done when:** When `multiAZ: true`, a Patroni cluster is provisioned
  (primary + ≥1 standby across distinct Harvester nodes); failover
  works; `status.readReplicas` lists the standbys.
- **Effort:** XL.

### DEF-11 · `DBParameterGroup` CRD + `spec.dbParameterGroupRef`

- **Status:** Reserved-API
- **Source:** codex_analysis §field matrix
- **What:** `spec.dbParameterGroupRef` is accepted but no
  `DBParameterGroup` CRD exists in this module. Means user-supplied
  `postgresql.conf` overrides aren't possible.
- **Done when:** `DBParameterGroup` CRD scaffolded (key/value map);
  `bootstrap.sh` applies the chosen group to `postgresql.conf` /
  `pg_hba.conf` at first boot; live reloads applied via signalling
  PostgreSQL.
- **Effort:** L.

### DEF-12 · KubeVirt `runStrategy` migration

- **Status:** Open
- **Source:** session finding while reviewing Harvester 1.7.1 compat
- **What:** Controller sets `spec.running: true/false` on the KubeVirt VM
  — the original boot toggle, which Harvester 1.7.1 still accepts.
  KubeVirt is migrating toward `spec.runStrategy` (`Always` /
  `RerunOnFailure` / `Halted` / `Manual`). Newer Harvester releases may
  deprecate `running`.
- **Done when:** Controller writes `runStrategy` instead; stop/start
  toggles use the new field; integration verified against a newer
  KubeVirt version.
- **Effort:** S–M.

## Security hardening

### DEF-13 · Validating admission webhook for cross-field rules

- **Status:** Open
- **Source:** codex_analysis §1 (cross-field validation), §7 (advanced
  validation)
- **What:** Today's validation lives entirely in CRD OpenAPI markers
  (Tier-3 fixes). It can't express cross-field rules:
  `manageMasterUserPassword` + `masterUserPasswordRef` mutual exclusion;
  immutability post-create (so users get rejected at admission, not at
  `status.phase=failed` mid-reconcile); `staticNetwork` address inside
  the NAD's subnet; etc. kubebuilder already scaffolded the webhook
  server; just no webhooks registered.
- **Done when:** Validating webhook for `DBInstance` enforces mutual
  exclusion, post-create immutability of fields in `AppliedSpec`, basic
  cross-field invariants. Cert lifecycle in place (cert-manager).
- **Effort:** M–L (cert wiring is the slow bit).

### DEF-14 · Per-instance TLS rotation

- **Status:** Open
- **Source:** ARCHITECTURE.md "what this version isn't"
- **What:** `tlsgen.go` mints a 10-year self-signed CA + server cert per
  instance at create time. No rotation. `RenewServerCert()` exists but
  is unused.
- **Done when:** Background loop / time-based reconcile reissues the
  server cert (and CA on a longer cadence) before expiry, updates the
  in-VM cert files via `qemu-guest-agent` or a sidecar mechanism, and
  signals PostgreSQL to reload SSL.
- **Effort:** L.

### DEF-15 · TLS termination inside the gateway

- **Status:** Open
- **Source:** ARCHITECTURE.md "what this version isn't"
- **What:** The REST gateway is HTTP on `:8080`. Bearer-token auth means
  the K8s API server validates the caller's identity, but the token
  itself goes over the wire in plain HTTP. Expected to be fronted by an
  ingress that terminates TLS.
- **Done when:** Either documented + enforced with an example ingress
  manifest in `config/`, or in-process TLS with cert-manager handling.
- **Effort:** S (doc-only) to M (in-process termination).

### DEF-16 · Per-tenant / RBAC-aware gateway

- **Status:** Open
- **Source:** upstream reference had API-key middleware + tenants config
- **What:** Gateway always operates on a single namespace
  (`DBAAS_DEFAULT_NAMESPACE`, default `default`). Multi-tenant deployments
  need per-tenant namespace selection from the caller's identity (e.g.
  OIDC claim → tenant namespace).
- **Done when:** Gateway derives target namespace from the validated
  caller identity; admission permits caller A to operate only on
  namespace X. May or may not need its own auth layer in front (today
  the K8s API server's RBAC enforces final authorization).
- **Effort:** M.

## Testing

### DEF-17 · Reconciler unit/integration tests

- **Status:** Open
- **Source:** codex_analysis §10
- **What:** Only the kubebuilder smoke test exists (verifies finalizer
  add). No tests for phase transitions, image-resolution failures,
  `reconcileModify` refusal, finalizer cleanup completeness, or
  Harvester error propagation.
- **Done when:** Phase-transition coverage with mocked harvester client;
  finalizer cleanup paths verified; immutable-drift refusal tested;
  TeardownAll error aggregation tested.
- **Effort:** M.

### DEF-18 · `internal/harvester/client.go` object-construction tests

- **Status:** Open
- **Source:** codex_analysis §10
- **What:** Zero unit tests on the harvester client. The VM spec it
  builds is invisible until a real cluster rejects it. Easy to regress
  silently.
- **Done when:** Unit tests assert the shape of the `unstructured`
  objects `CreatePostgresVM`, `CreateDataVolume`, `DeployMonitoring`
  produce, including label conventions, annotations
  (`harvesterhci.io/imageId`), and the cloudInitNoCloud secretRef pair.
- **Effort:** S–M.

## Performance / operations

### DEF-19 · Pre-baked PostgreSQL OS image

- **Status:** Open
- **Source:** session investigation; documented in `DEPLOYMENT.md`
- **What:** Currently the bootstrap.sh `apt install postgresql ...` runs
  on every first boot — costs ~30–60 s and requires VLAN egress to
  Ubuntu mirrors. A custom image with postgres + qemu-guest-agent
  pre-installed would skip both.
- **Done when:** A `Makefile` target / CI pipeline builds the image
  (virt-customize on a Linux amd64 host) and publishes it; docs in
  `DEPLOYMENT.md` updated; default `spec.osImage` documented.
- **Effort:** M (the build itself is the script in `DEPLOYMENT.md`;
  publishing infra is the bigger part).

### DEF-20 · Smaller OS disk to shorten CDI clone time

- **Status:** Open
- **Source:** timing breakdown
- **What:** OS DataVolume is 20 GiB (hard-coded in
  `CreatePostgresVM`'s `dataVolumeTemplates`). Longhorn clone time
  scales with size. A leaner image + smaller OS disk would shave seconds
  off every provision.
- **Done when:** OS disk size made configurable (or trimmed); validated
  PostgreSQL still has space for typical install + logs + a small data
  buffer.
- **Effort:** S.

### DEF-21 · Probe-pod IP collisions on shared VLANs

- **Status:** Open
- **Source:** v0.2.10 phaseWaitReady probe-pod design
- **What:** `phaseWaitReady` confirms postgres is accepting TCP by
  spawning a one-shot Pod attached to the same Multus NAD as the VM
  (see `internal/harvester/probe.go`). Because the NAD has no IPAM, the
  probe pod picks a static neighbor IP — `vmIP` ± 1 in the last octet —
  for its secondary NIC. If two DBInstances on the same VLAN have
  addresses that differ by exactly one (e.g., `.50` and `.51`) and their
  probes overlap in time, one probe's IP will collide with the other
  VM's real address, causing a transient probe failure and an extra
  reconcile cycle (the controller will retry). Single-DBInstance or
  well-spaced address allocations are unaffected.
- **Done when:** Either (a) the probe pod uses an IPAM-backed NAD
  (whereabouts / DHCP), (b) the controller allocates probe IPs from an
  operator-supplied pool, or (c) readiness is signalled out-of-band via
  cloud-init phone-home to the gateway, removing the L2 dependency
  entirely.
- **Effort:** M (option c is the architecturally cleaner path and the
  one to invest in if probe-pod churn becomes a real concern).

## Summary table

| ID | Title | Status | Effort | Source |
| --- | --- | --- | --- | --- |
| DEF-01 | `engineVersion` drives version | Reserved-API | M–L | codex §2 |
| DEF-02 | Real backups (pgBackRest) | Reserved-API | L | codex §3 |
| DEF-03 | User-supplied admin password | Reserved-API | S–M | codex §1 |
| DEF-04 | `tags` propagation | Reserved-API | S | codex §11 |
| DEF-05 | `postgres_exporter` install | Open | M | codex §4 |
| DEF-06 | NAD existence check | Open | S | codex §8 |
| DEF-07 | `status.conditions` written | Open | M | codex §9 |
| DEF-08 | `status.readReplicas` populated | Reserved-API | (DEF-10) | codex §9 |
| DEF-09 | `DBSnapshot` CRD | Open | L | upstream |
| DEF-10 | `multiAZ` / Patroni HA | Reserved-API | XL | codex |
| DEF-11 | `DBParameterGroup` CRD | Reserved-API | L | codex |
| DEF-12 | KubeVirt `runStrategy` migration | Open | S–M | session |
| DEF-13 | Validating admission webhook | Open | M–L | codex §1, §7 |
| DEF-14 | Per-instance TLS rotation | Open | L | ARCHITECTURE |
| DEF-15 | TLS termination in the gateway | Open | S–M | ARCHITECTURE |
| DEF-16 | Per-tenant / RBAC-aware gateway | Open | M | upstream |
| DEF-17 | Reconciler tests | Open | M | codex §10 |
| DEF-18 | Harvester client tests | Open | S–M | codex §10 |
| DEF-19 | Pre-baked PostgreSQL OS image | Open | M | session |
| DEF-20 | Smaller OS disk | Open | S | timing |
| DEF-21 | Probe-pod IP collisions on shared VLAN | Open | M | session |
