# DBaaS CRD Analysis — Round 2

A fresh static analysis after the cleanup sweep that closed most of
`codex_analysis.md`'s findings. Companion file, not replacement.

## Scope

Reviewed files (with line numbers cited inline):

- `api/v1alpha1/dbinstance_types.go`
- `config/crd/bases/dbaas.opencloud.wso2.com_dbinstances.yaml`
- `config/rbac/role.yaml`
- `internal/controller/dbinstance_controller.go`
- `internal/controller/immutable_drift_test.go` *(new)*
- `internal/harvester/client.go`
- `internal/harvester/client_test.go` *(new)*
- `internal/harvester/cloudinit.go`
- `internal/gateway/gateway.go`
- `README.md`, `ARCHITECTURE.md`, `USAGE.md`, `DEPLOYMENT.md`, `DEFERRED.md`

`make build` clean, `make test` green (`controller 20.2%, gateway 82.9%,
harvester 10.0%`). Reviewed against the assumption that
`codex_analysis.md` already documents the deeper API gaps; this file
adds what's specific to the current commit.

## Executive Summary

The Tier-1 cleanup landed cleanly. The leaks, swallowed errors, and
misleading `observedGeneration` advancement that `codex_analysis.md`
called out are demonstrably fixed in code and exercised by new unit
tests. The codebase is in noticeably better shape than the snapshot
that review was written against.

But the cleanup surfaced (or left untouched) a handful of new bugs and
contract inconsistencies. Several are small. Two are more substantive:

- **The Prometheus ServiceMonitor selector targets a label that the
  data-net VM does not present at the scrape address.** Even after
  `postgres_exporter` is installed inside the VM (DEF-05), metrics will
  not flow until the Service/ServiceMonitor topology is fixed.
- **The new immutable-drift guard in `reconcileModify` is not applied on
  the `reconcileStop` / `reconcileStart` paths.** Toggling
  `spec.running` while simultaneously changing an immutable field will
  silently advance `observedGeneration` and leave the change un-applied
  — the exact failure mode the modify guard was meant to prevent.

The rest are quality issues: code duplication, an outdated hardcoded
OS-image default, a gateway namespace inconsistency, and several
overdue hardening items in the cloud-init script (master user is
`SUPERUSER`, LUKS key generated but never used, secrets retained on
disk after install).

## What `codex_analysis.md` flagged that is now fixed (verified)

| Codex § | Topic | Verification |
| --- | --- | --- |
| §1 | Misleading `manageMasterUserPassword` / `masterUserPasswordRef` | Both fields tagged **NOT YET IMPLEMENTED** in godoc (`dbinstance_types.go:54–69`). Behaviour unchanged but contract is honest. Still owed: DEF-03. |
| §2 | `engineVersion` no-op | Tagged **NOT YET IMPLEMENTED** in godoc (`dbinstance_types.go:30–36`). DEF-01 tracks the real fix. |
| §3 | Backup fields not implemented | All three tagged in godoc (`dbinstance_types.go:79–92, 159–162`). DEF-02. |
| §4 | Metrics Service leaked on delete | `ResourceRefs.MetricsServiceName` added (`dbinstance_types.go:237–242`); set in `phaseMonitoring` (`dbinstance_controller.go:308, 311`); deleted by `TeardownAll` (`client.go:483`). RBAC was extended to include services `delete` (`role.yaml`). Idempotency proved by `TestDeployMonitoringIsIdempotent`. |
| §4 | `DeployMonitoring` had dead `vmAddr` / `pgPort` params | Removed (`client.go:427`). |
| §5 | `reconcileModify` falsely advances `observedGeneration` | `immutableDrift` introduced (`dbinstance_controller.go:432–499`); `reconcileModify` calls it before resizing (`:388–391`); `Status.AppliedSpec` snapshot taken at create (`:244–252`). `TestImmutableDriftNormalizesCreateDefaults` and `TestImmutableDriftDetectsActualImmutableChange` cover both branches. The normalization (defaulting both sides before comparison) is correctly applied. |
| §6 | `TeardownAll` swallowed errors; finalizer removed prematurely | Now returns an aggregated `error` (`client.go:475, 515–519`); `reconcileDelete` keeps the finalizer in place and requeues on failure (`:516–523`). |
| §7 | Weak CRD validation | OpenAPI markers added for `allocatedStorage` (min 1), `port` (1–65535), `backupRetentionPeriod` (≥0), `networkRef` (DNS-label/DNS-label pattern), `staticNetwork.address` (CIDR), `staticNetwork.gateway` (IPv4), `nameservers` (≥1 item), `preferredBackupWindow` (HH:MM-HH:MM). All present in the regenerated CRD. |
| §9 | `status.conditions` / `status.readReplicas` unused | Still unwritten (DEF-07, DEF-08). |
| §10 | No real reconciler / harvester-client tests | Partial fix: two unit tests now exist (`immutable_drift_test.go`, `client_test.go`) and bring controller coverage from ~4% to 20.2% and harvester from 0% to 10%. Still owed: DEF-17/18. |
| §11 | Docs and code disagree | `backupRetentionPeriod` comment fixed (default 0). `README.md` rewritten from kubebuilder TODO stub. `ARCHITECTURE.md` and `USAGE.md` flag unimplemented features. New `DEFERRED.md` tracks DEF-01..DEF-20. |

## New findings

Numbered `C-NN` so commits/PRs can reference them. Severity is my read of
how user-visible the bug is in normal operation.

### C-01 · ServiceMonitor selector targets the virt-launcher pod, not the data NIC — `severity: high`

`internal/harvester/client.go:438, 450`:

```go
// Service:
svc.spec.selector = {dbaas.opencloud.wso2.com/instance: <id>}
// ServiceMonitor:
sm.spec.selector.matchLabels = {dbaas.opencloud.wso2.com/metrics: "true",
                                dbaas.opencloud.wso2.com/instance: <id>}
```

The pod that carries the `dbaas.opencloud.wso2.com/instance=<id>` label
is the **virt-launcher pod** (we set it on
`vm.spec.template.metadata.labels` at `client.go:283`). The virt-launcher
runs on the cluster's pod network at its own pod IP; it does **not**
listen on `:9187`. The PostgreSQL server inside the VM listens on the
VM's `data-net` IP — a different network, a different address.

Result: even after DEF-05 (install `postgres_exporter` in the VM) is
done, Prometheus will scrape `<virt-launcher-pod-ip>:9187` and get
nothing. The "MonitoringDeployed" phase is structurally cosmetic.

Fix shape: either (a) manage an `Endpoints` (or `EndpointSlice`) object
manually, populated with the VM's `data-net` IP from `status.endpoint.address`,
or (b) drop the headless Service and use `ServiceMonitor`'s
`spec.endpoints[].targetPort` against a `Service` of type ExternalName /
manually-populated endpoints. The Service's `selector` is the wrong tool
here.

### C-02 · `reconcileStart` / `reconcileStop` advance `observedGeneration` without the immutable-drift check — `severity: medium-high`

`internal/controller/dbinstance_controller.go:362, 377`:

```go
// reconcileStop
inst.Status.ObservedGeneration = inst.Generation   // line 362
// reconcileStart
inst.Status.ObservedGeneration = inst.Generation   // line 377
```

The Reconcile dispatcher at line 90–94 routes any spec change that
*also* flips `running` straight into `reconcileStop` / `reconcileStart`,
bypassing `reconcileModify`. Both of those handlers then advance
`observedGeneration` unconditionally.

Reproducer:

```
kubectl edit dbi orders   # set spec.running=false AND change spec.dbName
                          # → controller routes to reconcileStop
                          # → observedGeneration = generation
                          # → dbName change silently lost
                          # → next reconcile sees no diff; loop is stable
```

This is the same shape as the codex-§5 bug, just on a different code
path. The fix is to call `immutableDrift(inst)` from both `reconcileStop`
and `reconcileStart` (or — better — hoist the drift check up to the
dispatcher, before deciding which sub-handler to invoke).

### C-03 · Gateway create/get/delete namespace inconsistency — `severity: medium`

`internal/gateway/gateway.go:217–218` (create) vs `:187, 273, 309, 368`
(list/get/delete/start-stop/modify):

```go
// create
if instance.Namespace == "" {
    instance.Namespace = defaultNamespace()   // honours body's namespace
}
// every other handler
... defaultNamespace() ...                    // ignores any other ns
```

A caller can `POST /dbinstances` with `metadata.namespace: "tenant-b"`,
get a `202 Accepted`, and then **never be able to `GET`, `PATCH`,
`DELETE`, `/start`, or `/stop` it** through the gateway because every
other handler hard-codes the gateway's `defaultNamespace()`. They've
just created an orphan from the gateway's perspective.

Fix shape: either (a) ignore the body's namespace entirely and always
overwrite it (the simplest), or (b) implement path-based namespace
(e.g. `/namespaces/{ns}/dbinstances/{name}`) so every operation is
namespace-aware. Option (a) is consistent with what the rest of the
gateway already assumes. Option (b) is what DEF-16 will eventually want.

### C-04 · Hardcoded `osImage` default is wrong for the validated cluster — `severity: low-medium`

`internal/controller/dbinstance_controller.go:204, 439, 457`:

```go
osImage = "ubuntu-22.04-server-cloudimg-amd64.img"
```

This image name does not exist on the Harvester we validated against
(it ships `ubuntu-24.04-minimal-cloudimg-amd64.img` and the user-uploaded
`ubuntu-2404-server`). A caller who omits `spec.osImage` gets a clear
"VirtualMachineImage … not found" error during reconcile, but only after
the storage DataVolume has already been provisioned. The default is also
hardcoded in three places (two reconciler sites plus
`immutableDrift`), so changing it is error-prone.

Fix shape: either (a) remove the default entirely and refuse the
DBInstance at `phaseVM` with "spec.osImage is required" — surfaces the
error sooner and is honest about what the controller does, or (b)
publish a single named constant (e.g. `defaultOSImage` in the api or
controller package) and reference it from all three sites.

### C-05 · `storageType = "longhorn"` defaulted in three places — `severity: low`

`internal/controller/dbinstance_controller.go:165, 208, 453`:

```go
storageType := inst.Spec.StorageType
if storageType == "" {
    storageType = "longhorn"
}
```

Three identical defaulting blocks (`phaseStorage`, `phaseVM`,
`immutableDrift`). Same drift risk as C-04. Same fix shape — a single
constant referenced from all three sites.

### C-06 · Master user is created as `SUPERUSER` — `severity: medium (hardening)`

`internal/harvester/cloudinit.go:171`:

```sql
CREATE ROLE ${MASTER_USER} LOGIN SUPERUSER PASSWORD '${MASTER_PASSWORD}';
```

`SUPERUSER` bypasses every PostgreSQL permission check. RDS-style "master
user" is usually a `CREATEDB CREATEROLE LOGIN` role — powerful enough to
manage application databases but not able to disable the
postgres-internal protections. The current setup means a compromised
admin password is a compromised database engine.

Fix shape: change to `CREATE ROLE … LOGIN CREATEDB CREATEROLE PASSWORD
…`. Provide an opt-in `spec.masterUserSuperuser` field if some workloads
genuinely need it, or accept the downgrade as the only mode.

### C-07 · LUKS key generated and stored but never used — `severity: medium (false claim)`

`internal/harvester/client.go:202` generates a 64-char `luksKey`;
`client.go:229` stores it in the credentials Secret; `cloudinit.go:114`
writes it to `/etc/dbaas/bootstrap.env` on the VM. **Nothing inside the
VM ever invokes `cryptsetup`.** The pgdata volume is plaintext
ext4 on top of a Longhorn block device.

The ARCHITECTURE document (and earlier reference repo docs) imply LUKS2
encryption-at-rest for the pgdata volume; the implementation does not
provide it. This is the same "advertised more than delivered" class
codex_analysis flagged for backups.

Fix shape: either (a) tag this as **NOT YET IMPLEMENTED** like backups
and remove the generated key from the Secret (or stop generating it),
or (b) actually wire `cryptsetup luksFormat` + persistent key into
bootstrap.sh. Until one or the other happens, the Secret has a
plausible-looking field that does nothing.

### C-08 · Secrets persist on the VM disk after bootstrap — `severity: low (hardening)`

`internal/harvester/cloudinit.go:103–115` writes `/etc/dbaas/bootstrap.env`
with mode 0600 containing `MASTER_PASSWORD`, `REPL_PASSWORD`,
`EXPORTER_PASSWORD`, and `LUKS_KEY`. The file is sourced by
`bootstrap.sh` and then **left on disk** forever. Anyone who can read
the OS disk (root inside the VM, an attacker with disk access via a
restore from snapshot, an admin viewing the LUKS-unlocked image) gets
the password.

Fix shape: at the end of bootstrap.sh, `shred -u /etc/dbaas/bootstrap.env`
(or rewrite it without the secret material once postgres is configured).
The K8s Secret remains the source of truth; the on-VM copy is only
needed during first-boot configuration.

### C-09 · `data-net` interface name hardcoded in three places — `severity: low`

`internal/harvester/client.go:353` (the `GetVMIReadiness` preference
loop), `:532` (`vmInterfaces`), `:543` (`vmNetworks`). All three must
agree or IP detection silently breaks. A single `const dataNetInterface
= "data-net"` would couple them.

### C-10 · `phaseAvailable` issues a `status.Update` on every requeue — `severity: low (cost / churn)`

`internal/controller/dbinstance_controller.go:320–343`. The function
unconditionally sets `Phase`, `ProvisioningPhase`, `ObservedGeneration`,
`Message`, and conditionally `Endpoint`, then calls
`r.statusUpdate(ctx, inst)` on every 60 s requeue. Most of those
assignments are idempotent and the kube-apiserver dedupes the write,
but for many DBInstances on a busy cluster this still costs
serialization, audit-log volume, and watch-event fanout.

Fix shape: compute whether anything actually changed (Endpoint or
ObservedGeneration) and skip the `Update` call when nothing did.

### C-11 · Ginkgo controller smoke test does not exercise the new modify-refusal — `severity: very low`

`internal/controller/dbinstance_controller_test.go` only calls
`Reconcile` once on a fresh CR (it adds the finalizer and returns).
`immutable_drift_test.go` covers the helper directly, but no test
calls `reconcileModify` against a stored `AppliedSpec` and asserts the
status transitions. Worth adding when DEF-17 expands.

### C-12 · Gateway tests can't catch a broken bearer-token forward — `severity: low`

`internal/gateway/gateway_test.go` injects a `clientFactory` that
**ignores the token argument** and returns the same fake client every
call. That's fine for testing handler routing, but it means we can't
catch a regression where the gateway accidentally hands the manager's
ServiceAccount token to the K8s API instead of the caller's. The
factory is invoked, the test passes, the security property is
broken in production. Worth an integration-style test against envtest
where the factory really swaps tokens.

## Strengths since the codex review

Worth calling out — these are the things to keep, not just things that
got fixed:

- `TeardownAll` returns aggregated errors and `reconcileDelete` properly
  keeps the finalizer until clean (the leak that mattered most is gone).
- `DeployMonitoring` is now idempotent (handles `AlreadyExists` for both
  Service and ServiceMonitor at `client.go:442, 455`).
- Service creation errors now propagate (no more `_, _ = ...`).
- `immutableDrift` is *correctly normalised on both sides* — defaulted
  values match defaulted snapshots, so a user who explicitly types the
  default value of an optional field doesn't accidentally trigger drift.
  `TestImmutableDriftNormalizesCreateDefaults` proves it.
- `MetricsServiceName` and `ServiceMonitor` are both tracked in
  `Status.Resources` (the field that drives cleanup).
- The 3-min uptime gate is gone and replaced with a stricter readiness
  signal (qemu-guest-agent IP registration).
- Field-by-field godoc honesty: every reserved field says **NOT YET
  IMPLEMENTED** in `kubectl explain` output.
- The new `DEFERRED.md` formalises the gap-tracking and gives every
  remaining piece an ID for cross-referencing in PRs.

## Recommended priority for the next sweep

In order of "user-visible bug, smallest fix first":

1. **C-02** — extend the immutable-drift check to `reconcileStart` /
   `reconcileStop`. Same helper, same status pattern. ~10 lines + a test.
2. **C-03** — make the gateway namespace policy consistent. Either
   overwrite body's namespace on create (one-liner) or route namespace
   in the URL. ~5 lines for the simpler choice.
3. **C-04** + **C-05** — single `defaultStorageType` and `defaultOSImage`
   constants, referenced from all sites. Removes the drift risk and
   makes future "decide the right default per cluster" trivial. ~15 lines.
4. **C-09** — single `dataNetInterface` constant. ~5 lines.
5. **C-06** — change master user from `SUPERUSER` to `CREATEDB
   CREATEROLE`. One SQL line. Document the intent.
6. **C-07** + **C-08** — decide: drop LUKS key + decide what to do with
   bootstrap.env. If keeping the field for forward compatibility, add
   it to the **NOT YET IMPLEMENTED** list and stop persisting it.
7. **C-10** — only update status when something changed. ~10 lines.
8. **C-01** — design + implement Endpoints / Service shape so
   ServiceMonitor actually scrapes the VM. This is bigger; needs to
   match whatever DEF-05 (`postgres_exporter`) decides. Pair them.
9. **C-11, C-12** — fold into DEF-17 (real reconciler tests).

## Bottom line

The codebase is materially better than the snapshot codex reviewed. The
hardest-to-trust failure modes (silent error swallowing in teardown,
falsely-advancing `observedGeneration` for modifies) are now demonstrably
fixed and have tests. What's left fits into two buckets: (a) small
bugs/duplication that are quick to clean up (C-02 through C-10), and
(b) one real design hole around monitoring that needs a paired decision
with DEF-05. Nothing here changes the architecture or the CRD shape;
it's all internal correctness and consistency work.
