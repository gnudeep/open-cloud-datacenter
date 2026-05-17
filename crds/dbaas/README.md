# dbaas

A Kubernetes operator that provisions managed PostgreSQL databases as
KubeVirt VMs on a Harvester HCI cluster. One `DBInstance` custom resource
maps to one VM with persistent storage, SSL-only PostgreSQL, an admin
credentials Secret, and optional Prometheus monitoring.

Tested on **Harvester 1.7.1** (RKE2 v1.34.3) — full end-to-end from
`kubectl apply` to `psql` round-trip in ~3 minutes.

## Pointers

- [`ARCHITECTURE.md`](./ARCHITECTURE.md) — design: components, phase state
  machine, what gets created on Harvester, repo layout, what this version
  intentionally is *not*.
- [`DEPLOYMENT.md`](./DEPLOYMENT.md) — what a working deployment looks like
  on the cluster, with the ASCII topology diagram and the two non-obvious
  Harvester gotchas (the `cloudInitNoCloud.secretRef` quirk and the
  cloud-init `networkData` channel).
- [`USAGE.md`](./USAGE.md) — copy-paste install + operate guide: `make
  install` / `make deploy`, applying a DBInstance, getting credentials,
  connecting via `psql`, stop/start/modify/delete, the REST gateway
  endpoints, troubleshooting recipes.

## What it does

- **API**: `DBInstance` in group `dbaas.opencloud.wso2.com/v1alpha1`,
  namespaced, with the standard `kubectl get dbi -A` printer columns.
- **Reconciler**: phase-based state machine (`NetworkProvisioned →
  StorageProvisioned → VMCreated → WaitingForCloudInit → DatabaseReady →
  MonitoringDeployed → Available`); idempotent and crash-safe via
  `status.resources`.
- **REST gateway**: a thin HTTP layer over the CRD exposing the same six
  operations as `kubectl`; mutations are authenticated by forwarding the
  caller's bearer token to the K8s API server (same authn/RBAC/audit path
  as `kubectl`).
- **Network model**: single NIC bridged onto a Multus
  `NetworkAttachmentDefinition` the operator supplies via
  `spec.networkRef`. DHCP by default; `spec.staticNetwork` for VLANs
  without a DHCP server.
- **Per-instance TLS**: ephemeral CA + server cert generated for each VM
  and pinned via `status.caCertPem`. `pg_hba.conf` enforces
  `hostssl … scram-sha-256` only.

## What's NOT in this version

The CRD schema is broader than the implementation. The following spec
fields are reserved for forward compatibility but **the reconciler does
not act on them today**:

| Field | Status |
| --- | --- |
| `engineVersion` | Recorded but ignored; cloud-init installs the OS image's apt-default PostgreSQL (Ubuntu 24.04 → PG 16). |
| `manageMasterUserPassword`, `masterUserPasswordRef` | Ignored; the controller always generates a random admin password into the credentials Secret. |
| `s3BackupConfig`, `backupRetentionPeriod`, `preferredBackupWindow` | Values are recorded but no pgBackRest install, schedule, or retention runs. |
| `multiAZ` | No Patroni / HA standby is created. |
| `dbParameterGroupRef` | No `DBParameterGroup` CRD exists in this module. |
| `tags` | Not propagated to child resource labels / annotations / dashboards. |
| `status.conditions`, `status.readReplicas` | Defined for forward compatibility; not written by the reconciler. |
| Per-instance `postgres_exporter` | Service + ServiceMonitor are created, but no exporter is installed inside the VM yet, so the scrape target won't return metrics. |

Each is called out in the field's godoc (`kubectl explain dbi.spec.<field>`)
and in `ARCHITECTURE.md`. They will be implemented incrementally; the
schema shape is deliberately stable so users can write manifests today
that work later.

## Quickstart

```sh
# In this directory (crds/dbaas/), from a host with kubectl + docker buildx:
make docker-buildx IMG=<registry>/<name>:<tag>
KUBECONFIG=<your-harvester-kubeconfig> make install
KUBECONFIG=<your-harvester-kubeconfig> make deploy IMG=<registry>/<name>:<tag>

# Then apply a DBInstance — full YAML and walkthrough in USAGE.md
kubectl get dbi -A -w
```

Expected time from `apply` to `phase=available`: about **3 minutes** on
stock Ubuntu cloud images, ~60 s if you pre-bake PostgreSQL into a
custom image (see `DEPLOYMENT.md`).

## Build / test / develop

```sh
make manifests generate fmt vet build   # regenerate CRD + DeepCopy, build manager
make test                               # envtest-backed unit tests
make docker-buildx IMG=...              # cross-build linux/amd64, push
make install                            # apply CRD using current kubeconfig
make deploy IMG=...                     # apply manager + RBAC
make undeploy && make uninstall         # tear it all down
```

## Part of Open Cloud Datacenter

This component lives in the [WSO2 Open Cloud
Datacenter](https://github.com/wso2/open-cloud-datacenter) initiative,
providing managed database services on Harvester HCI.

## License

Apache-2.0
