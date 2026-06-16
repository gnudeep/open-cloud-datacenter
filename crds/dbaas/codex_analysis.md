# DBaaS CRD Analysis

Fresh analysis of the current `DBInstance` CRD, controller implementation, and
one live Harvester deployment.

## Scope

Reviewed files:

- `api/v1alpha1/dbinstance_types.go`
- `api/v1alpha1/zz_generated.deepcopy.go`
- `config/crd/bases/dbaas.opencloud.wso2.com_dbinstances.yaml`
- `config/rbac/role.yaml`
- `config/samples/dbaas_v1alpha1_dbinstance.yaml`
- `internal/controller/dbinstance_controller.go`
- `internal/controller/immutable_drift_test.go`
- `internal/harvester/client.go`
- `internal/harvester/client_test.go`
- `internal/harvester/cloudinit.go`
- `internal/gateway/gateway.go`
- `internal/gateway/gateway_test.go`
- `README.md`, `ARCHITECTURE.md`, `USAGE.md`, `DEPLOYMENT.md`,
  `DEFERRED.md`, and `claude_analysis.md`

Verification performed:

- `go test ./...` passes.
- Live validation against the Harvester kubeconfig at `~/Downloads/local-2.yaml`
  confirmed `tenant-test/test-orders` is `available`, the VM/VMI is running at
  `192.168.40.50`, credentials work from inside the guest, and the runtime
  storage/monitoring/network findings below are observable on the cluster.

The code fixes in this review are local repository changes. They still need a
controller image build/push and cluster rollout before they affect the live
deployment.

## Executive Summary

The code is materially stronger than the first reviewed snapshot. Cleanup is
no longer fire-and-forget, the metrics Service is tracked and RBAC permits
deleting it, monitoring creation is idempotent, immutable-field drift is
checked on modify/start/stop, default normalization is centralized, gateway
create no longer accepts a namespace it cannot later manage, and tests now
cover the most recent helper-level regressions.

The main remaining risks are narrower and more operational:

- New VMs now format and mount the `pgdata` disk at `/var/lib/postgresql`, but
  existing VMs created before this fix need a one-time in-guest migration or
  recreation before their data is actually on the pgdata volume.
- DataVolume resize still only expands the Kubernetes block device request;
  the guest filesystem is not grown by the controller.
- `PhaseFailed` is terminal. After any `fail(...)`, future reconciles just
  requeue; correcting the spec does not retry the failed phase.
- Monitoring now uses manual Endpoints pointed at the VM IP and installs
  `prometheus-postgres-exporter`, but scraping still depends on cluster
  node/pod routing to the VM VLAN.
- Several accepted spec changes remain no-op or create-time-only but can
  still be marked observed, especially `staticNetwork`, `vmPassword`, and the
  reserved fields.
- User-controlled bootstrap values such as `dbName` and `masterUsername` are
  not sufficiently validated or quoted before being sourced by shell and
  interpolated into SQL.

## Resource Model

The generated CRD is:

- Group: `dbaas.opencloud.wso2.com`
- Version: `v1alpha1`
- Kind: `DBInstance`
- Plural: `dbinstances`
- Short name: `dbi`
- Scope: `Namespaced`
- Status subresource: enabled

Required `spec` fields:

- `dbInstanceClass`
- `allocatedStorage`
- `networkRef`

Printer columns:

- `status.phase`
- `spec.dbInstanceClass`
- `status.endpoint.address`
- `metadata.creationTimestamp`

Notable status fields:

- `status.appliedSpec`
- `status.resources.metricsServiceName`
- `status.conditions` and `status.readReplicas`, currently declared but not
  written

## Implemented Lifecycle

The reconciler advances through these phases:

1. `NetworkProvisioned`
   - Checks only that `spec.networkRef` is non-empty.
   - Records the value in `status.resources.nadName`.
   - Does not verify that the referenced NAD exists.

2. `StorageProvisioned`
   - Creates CDI DataVolume `pg-<name>-data`.
   - Defaults `storageType` to `longhorn`.

3. `VMCreated`
   - Resolves `dbInstanceClass` against the in-code `InstanceClasses` map.
   - Defaults `masterUsername`, `dbName`, `osImage`, `storageType`, and
     `port` in controller code.
   - Generates credentials, TLS material, cloud-init data, and a KubeVirt VM.
   - Writes credentials and cloud-init data into Secret
     `pg-<name>-credentials`.
   - Bootstrap formats/mounts `/dev/vdb` at `/var/lib/postgresql` and installs
     `prometheus-postgres-exporter`.
   - Records an `AppliedSpec` snapshot for selected immutable fields.

4. `WaitingForCloudInit`
   - Polls the VMI.
   - Treats VMI `Running` plus an observed data-net IP address as readiness.
   - This is still earlier than a real SQL readiness check; qemu-guest-agent
     starts before the bootstrap script finishes database/user creation.

5. `DatabaseReady`
   - Populates `status.endpoint`.
   - Builds a JDBC URL using `sslmode=verify-ca`.

6. `MonitoringDeployed`
   - Creates a selectorless headless metrics Service on port `9187`.
   - Creates manual Endpoints pointing at the VM data-net IP.
   - Creates a Prometheus `ServiceMonitor`.
   - Records both names for cleanup.
   - Marks monitoring failures as non-fatal; `Available` reconciles retry the
     idempotent monitoring setup once an endpoint is known.

7. `Available`
   - Sets `status.phase = available`.
   - Updates `status.observedGeneration`.
   - Refreshes endpoint IP every 60 seconds.
   - Refreshes/retries selectorless metrics Service, Endpoints, and
     ServiceMonitor setup.
   - Skips the status update when no status fields changed.

Implemented day-2 operations:

- Stop and start through `spec.running`.
- Resize VM CPU/memory from `dbInstanceClass`.
- Resize the `pgdata` DataVolume from `allocatedStorage`; guest filesystem
  growth is still not handled.
- Refuse changes to selected immutable fields using `status.appliedSpec`.
- Delete through a finalizer.
- Block finalizer cleanup when `spec.deletionProtection` is true.

## Field Support Matrix

| Field | Current behavior |
| --- | --- |
| `dbInstanceClass` | Required. Used at create and modify time through `InstanceClasses`. Has `MinLength=1`, but no enum validation. |
| `allocatedStorage` | Required. Creates/resizes `pg-<name>-data`; new VMs mount it at `/var/lib/postgresql`. Has `Minimum=1`, but no explicit max/shrink guard and no guest filesystem grow after day-2 resize. |
| `networkRef` | Required. Pattern-validates `namespace/name` shape. Reconciler still does not check NAD existence. |
| `dbName` | Used at VM creation and endpoint URL generation. Snapshotted as immutable. Not strongly validated or safely SQL-quoted. |
| `port` | Used in cloud-init and endpoint URL. Has `Minimum=1` and `Maximum=65535`. Snapshotted as immutable. |
| `masterUsername` | Used at VM creation. Snapshotted as immutable. Not strongly validated or safely SQL-quoted. |
| `manageMasterUserPassword` | Reserved. Still ignored. Credentials are always generated by the controller. |
| `masterUserPasswordRef` | Reserved. Still ignored. User-provided password Secrets are not read. |
| `storageType` | Used only when creating the data DataVolume. Snapshotted as immutable. |
| `backupRetentionPeriod` | Reserved/inert. Has `Minimum=0`. No backup implementation. |
| `preferredBackupWindow` | Reserved/inert. Has an `HH:MM-HH:MM` validation pattern, but no scheduler consumes it. |
| `s3BackupConfig` | Reserved/inert. Values are written into bootstrap env text only when backups are enabled, then that env file is shredded at the end of bootstrap. No backup process consumes them. |
| `engineVersion` | Reserved/inert. Snapshotted as immutable, but cloud-init installs unversioned `postgresql` packages from apt. |
| `multiAZ` | Reserved/inert. No Patroni, standby VM, or HA path exists. |
| `dbParameterGroupRef` | Reserved/inert. No `DBParameterGroup` reconciliation exists. |
| `deletionProtection` | Implemented. Deletion finalizer refuses cleanup while true. |
| `running` | Implemented for stop/start. Default is encoded as `true` in the CRD. |
| `osImage` | Used at VM creation to resolve Harvester `VirtualMachineImage`. Snapshotted as immutable. Controller default remains `ubuntu-22.04-server-cloudimg-amd64.img`. |
| `staticNetwork` | Used at VM creation to generate cloud-init network data. Partial OpenAPI validation exists. Documented immutable, but not included in `AppliedSpec`. |
| `vmPassword` | Used at VM creation to enable password auth. Documented immutable, but not included in `AppliedSpec`. |
| `tags` | Reserved/inert. Not propagated to child labels, annotations, dashboards, or monitoring. |
| `status.conditions` | Declared but never written. |
| `status.readReplicas` | Declared but never written. |
| `status.appliedSpec` | Used by `immutableDrift` to refuse selected immutable-field changes. Default normalization is now fixed for the fields it covers. |
| `status.resources.metricsServiceName` | Used by cleanup to delete the per-instance metrics Service. |

## Resolved Or Improved Since Last Review

### Cleanup and RBAC are safer

- `ResourceRefs` includes `metricsServiceName`.
- `phaseMonitoring` records both the Service and ServiceMonitor names.
- `TeardownAll` deletes ServiceMonitor, metrics Service, VM, data DataVolume,
  and Secret.
- `TeardownAll` ignores `NotFound` but aggregates other delete errors.
- `reconcileDelete` keeps the finalizer and requeues when teardown fails.
- Service RBAC now includes `delete`.

Remaining caveat:

- The OS DataVolume created through the VM `dataVolumeTemplates` is still not
  explicitly tracked in `status.resources`.

### Modify semantics are more honest

- `AppliedSpec` snapshots selected immutable fields at VM creation.
- `immutableDrift` normalizes both the applied snapshot and current spec before
  comparison for `osImage`, `dbName`, `masterUsername`, `port`, and
  `storageType`.
- `reconcileModify`, `reconcileStop`, and `reconcileStart` all call
  `immutableDrift` before advancing `observedGeneration`.
- Focused tests cover default normalization and a real immutable-field change.

Remaining caveat:

- `staticNetwork` and `vmPassword` are documented immutable but are not in
  `AppliedSpec`.
- Reserved fields that are not implemented can still be changed and marked
  observed.

### Monitoring creation is idempotent and points at the VM IP

- `DeployMonitoring` now treats `AlreadyExists` as success for both Service and
  ServiceMonitor, and updates existing Service/Endpoints/ServiceMonitor
  objects.
- Service create errors are no longer silently ignored.
- The metrics Service no longer selects the virt-launcher pod; it uses manual
  Endpoints for the VM data-net IP.
- Bootstrap installs and configures `prometheus-postgres-exporter`.
- A focused unit test covers repeated `DeployMonitoring` calls.

Remaining caveat:

- Scraping still requires the Prometheus/node network path to reach the VM VLAN
  IP.

### Gateway namespace handling is consistent

- `POST /dbinstances` now overwrites `metadata.namespace` with the gateway's
  configured default namespace.
- Existing get/patch/delete/start/stop behavior also targets that namespace.
- Gateway tests cover the namespace override.

### Bootstrap hardening improved

- The generated master user is now `CREATEDB CREATEROLE`, not `SUPERUSER`.
- New VMs move PostgreSQL data onto the dedicated `pgdata` disk before
  database configuration.
- `/etc/dbaas/bootstrap.env` is shredded or removed after bootstrap.
- The unused `luks_key` is no longer written into the generated Secret or
  bootstrap env.

## Current Main Findings

### 1. Failed resources do not recover after spec fixes

`fail(...)` sets:

- `status.phase = failed`
- `status.provisioningPhase = Failed`

The reconcile switch handles `PhaseFailed` by requeueing after 30 seconds
without retrying the failed step. Because modify handling only runs when
`status.phase == available`, a user who fixes a bad `dbInstanceClass`,
`osImage`, or transient Harvester error can remain stuck in `failed`.

Impact:

- Admission-time gaps become operationally expensive: late reconcile failures
  are effectively terminal.
- Users may need to delete and recreate the DBInstance, or manually patch
  status, to recover from fixable errors.

Recommended fix:

- On generation change while failed, clear `PhaseFailed` and retry from the
  phase implied by `status.resources`.
- Alternatively make individual phase failures retryable unless the error is
  explicitly terminal.
- Add tests for "fail, fix spec, reconcile succeeds".

### 2. Day-2 storage resize does not grow the guest filesystem

The controller resizes the `pgdata` DataVolume when `allocatedStorage`
changes, but it does not rescan the block device inside the guest or run
`resize2fs` on the mounted filesystem.

Impact:

- The Kubernetes PVC/DataVolume can show the larger requested size while
  PostgreSQL still sees the old filesystem size.

Recommended fix:

- Add an in-guest grow step after successful DataVolume expansion, or surface a
  condition that resize is pending guest filesystem growth.

### 3. Monitoring depends on VM VLAN routing

Monitoring now creates a selectorless Service, a manual Endpoints object
pointed at the VM data-net IP, and a ServiceMonitor. The VM bootstrap also
installs and configures `prometheus-postgres-exporter`.

Remaining issue:

- Prometheus can only scrape the endpoint if the cluster node/pod network can
  route to the VM VLAN IP.

Impact:

- In clusters where nodes cannot reach the VM VLAN, the ServiceMonitor will
  discover the right endpoint but scrape attempts still time out.

Recommended fix:

- Either route node/pod traffic to the VM VLAN or expose the exporter through a
  network path Prometheus can reach.
- Add a monitoring readiness check/condition based on actual scrapeability.

### 4. Some create-time fields still change silently after create

`staticNetwork` and `vmPassword` are documented as immutable and only used
during VM creation, but neither is recorded in `AppliedSpec` or checked by
`immutableDrift`.

Reserved fields such as `multiAZ`, `dbParameterGroupRef`,
`manageMasterUserPassword`, `masterUserPasswordRef`, `tags`, and backup
settings are also not implemented. Changes to these fields can still advance
`observedGeneration` through the generic modify path.

Impact:

- Users can see `observedGeneration` catch up even though their requested
  change was not applied to the running database.
- The previous silent-modify problem is fixed for selected immutable fields,
  but not for the entire exposed API surface.

Recommended fix:

- Add `staticNetwork` and `vmPassword` to the immutable snapshot, or reject
  them with admission after create.
- Decide how reserved fields should behave: reject changes, leave
  `observedGeneration` behind with a clear condition, or implement them.

### 5. Bootstrap input is not safely validated or quoted

User-controlled values including `dbName` and `masterUsername` are written into
`/etc/dbaas/bootstrap.env`, sourced by shell, and interpolated into SQL. The
CRD does not constrain these fields to safe PostgreSQL identifier patterns, and
the script does not quote them via PostgreSQL-safe functions.

Impact:

- Invalid names can break bootstrap.
- Malicious or accidental shell/SQL metacharacters can change the generated
  script behavior.

Recommended fix:

- Add OpenAPI or admission validation for database names and role names.
- In bootstrap, use robust shell quoting and PostgreSQL `quote_ident` /
  `quote_literal` patterns rather than direct string interpolation.
- Apply the same principle to S3 fields and other cloud-init values.

### 6. Credential API is still reserved, not implemented

The CRD still exposes:

- `manageMasterUserPassword`
- `masterUserPasswordRef`

The controller always generates a random admin password in `CreatePostgresVM`
and stores it in the generated credentials Secret. It never reads a
caller-provided Secret.

Impact:

- Callers cannot supply their own master password.
- This is documented as reserved in some places, but the field names still
  imply behavior that does not exist.

Recommended fix:

- Implement the SecretRef path and cross-field validation, or keep the fields
  explicitly reserved in every user-facing doc and sample.

### 7. `engineVersion` remains a no-op

Cloud-init still installs:

```sh
apt-get install -y postgresql postgresql-contrib jq qemu-guest-agent
```

The requested `engineVersion` does not drive package selection, image
selection, or configuration.

Impact:

- The actual PostgreSQL version is determined by the OS image and apt
  repositories.
- The field is documented as reserved, but users can still set it.

Recommended fix:

- Install versioned packages or map `engineVersion` to an image/version
  strategy.

### 8. Backup fields remain non-operational

The controller still only writes S3-related values into bootstrap env text
when backups are enabled. There is no observed:

- pgBackRest installation
- backup schedule
- retention enforcement
- restore path
- backup status

Because `bootstrap.env` is removed after bootstrap, those values are also not
left as durable runtime configuration for a future backup process.

Impact:

- Backup configuration remains declarative metadata, not working backup
  behavior.

Recommended fix:

- Keep backup fields documented as reserved until pgBackRest and status support
  exist.
- Remove backup fields from gateway patch examples until they do something.

### 9. Network existence is still not verified

`networkRef` has a `namespace/name` pattern, but `phaseNetwork` does not check
whether the referenced Multus NetworkAttachmentDefinition exists.

Impact:

- A typo can still advance through `NetworkProvisioned`.
- The real failure appears later during VM networking or readiness.

Recommended fix:

- Parse `networkRef` and `Get` the NAD during `phaseNetwork`.
- Add RBAC for `network-attachment-definitions`.

### 10. Status conditions and read replicas are still unused

The CRD declares standard `metav1.Condition` status, but the reconciler still
writes only:

- `phase`
- `provisioningPhase`
- `message`
- `resources`
- `endpoint`
- selected metadata fields

Impact:

- `kubectl wait --for=condition=Ready dbi/...` will not work.
- Other controllers and dashboards cannot use condition-based readiness.

Recommended fix:

- Write conditions for at least `Ready`, `StorageReady`, `VMReady`,
  `DatabaseReady`, and `MonitoringReady`.
- Consider a `MonitoringReady=False` or `BackupReady=False` condition for
  optional/deferred subsystems.

### 11. Validation remains incomplete

Validation improvements are present, but gaps remain:

- `dbInstanceClass` is not an enum, so invalid class names still pass
  admission and can put the instance into terminal `failed`.
- `dbName` and `masterUsername` have no PostgreSQL-safe pattern.
- `s3BackupConfig.endpoint`, `bucket`, and `secretRef` are required but can
  still be empty strings.
- `masterUserPasswordRef.name` and `key` are required but can still be empty
  strings.
- `staticNetwork.nameservers` validates item count but not each item as an IP.
- Cross-field rules still need CEL or a webhook, such as password-mode mutual
  exclusion and post-create immutability.

### 12. Documentation drift remains

Examples:

- `ARCHITECTURE.md` still lists `status.resources` without
  `metricsServiceName` in at least one table.
- `ARCHITECTURE.md` and `DEPLOYMENT.md` still mention `luks_key`, but the code
  no longer generates or stores it.
- `ARCHITECTURE.md` still says `TeardownAll` deletes ServiceMonitor, VM,
  DataVolume, and Secret, but omits the metrics Service.
- `USAGE.md` says `TeardownAll` ignores errors, but code now aggregates errors
  and keeps the finalizer.
- `USAGE.md` suggests reading `/etc/dbaas/bootstrap.env`, but bootstrap now
  shreds/removes that file.
- Samples still show reserved fields such as `manageMasterUserPassword` and
  backup retention as if they are ordinary working options.

## Test Coverage

Current test coverage is better than before:

- Gateway routing and mutation tests cover core HTTP behavior and namespace
  overwrite on create.
- `immutableDrift` has focused tests for default normalization and real drift.
- `DeployMonitoring` has an idempotency test.
- The kubebuilder controller smoke test still verifies only the first reconcile
  finalizer path.

Remaining high-value tests:

- Phase recovery from `failed` after spec correction.
- DataVolume mount / PostgreSQL data directory behavior.
- `reconcileModify` and start/stop behavior with reserved/no-op field changes.
- `TeardownAll` aggregate errors and finalizer retention.
- VM object construction, including data disk, cloud-init, image annotation,
  labels, and network wiring.
- Monitoring endpoint topology once the Service/EndpointSlice design is fixed.

## Highest-Priority Current Issues

1. Make failed instances recoverable.
   - Correcting a spec should retry the failed phase or restart from a safe
     known phase.

2. Handle day-2 filesystem growth after `allocatedStorage` changes.
   - The DataVolume resize alone does not grow the mounted guest filesystem.

3. Validate monitoring reachability, not just object creation.
   - A ServiceMonitor should become ready only when Prometheus can scrape the
     VM exporter.

4. Harden bootstrap input handling.
   - Validate and quote database names, role names, and cloud-init values.

5. Close the remaining silent no-op modify paths.
   - Include `staticNetwork` and `vmPassword` in immutable drift, and choose a
     policy for reserved fields.

6. Keep documentation and samples aligned with code.
   - Especially `luks_key`, teardown error handling, bootstrap env deletion,
     credentials, backups, and monitoring.

## Strengths

- The phase state machine remains easy to follow.
- Resource names are deterministic.
- Status now tracks more cleanup-relevant resources.
- Finalizer behavior is safer after teardown error aggregation.
- Service deletion RBAC now matches cleanup behavior.
- Static networking is passed at the correct cloud-init `networkdata` stage.
- Per-instance TLS and CA publication are useful for client verification.
- The gateway namespace policy is now internally consistent.
- The new `DEFERRED.md` gives a practical backlog for known reserved features.
- Generated CRD docs communicate implementation limits much better than the
  first snapshot.

## Bottom Line

The CRD/operator is now more honest and safer around cleanup and selected
immutable-field changes, and the latest tests pass. The next correctness work
should focus on the runtime contract: make failed resources recover after
fixes, handle guest filesystem growth after storage resize, validate monitoring
scrapeability across the actual network path, and prevent no-op spec changes
from being marked observed.
