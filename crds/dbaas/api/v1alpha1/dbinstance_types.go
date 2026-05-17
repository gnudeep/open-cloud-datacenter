/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DBInstanceSpec defines the desired state of a managed PostgreSQL database.
//
// Field support status (v1alpha1):
//   - implemented mutable post-create: dbInstanceClass, allocatedStorage,
//     running, deletionProtection
//   - implemented immutable post-create (modify is refused): networkRef,
//     osImage, dbName, masterUsername, port, storageType, staticNetwork,
//     vmPassword, engineVersion
//   - NOT IMPLEMENTED: manageMasterUserPassword, masterUserPasswordRef,
//     multiAZ, dbParameterGroupRef, tags, s3BackupConfig,
//     backupRetentionPeriod, preferredBackupWindow. These fields exist in
//     the schema for forward compatibility but the reconciler does not
//     apply them. See ARCHITECTURE.md for the roadmap.
type DBInstanceSpec struct {
	// DBInstanceClass maps to VM CPU/RAM. e.g. "db.t3.medium", "db.m5.large".
	// Mutable: changing the class on an Available instance resizes the VM.
	// +required
	// +kubebuilder:validation:MinLength=1
	DBInstanceClass string `json:"dbInstanceClass"`

	// EngineVersion is the PostgreSQL major version, e.g. "16".
	// NOT YET IMPLEMENTED: cloud-init installs whatever PostgreSQL the OS
	// image's apt repo provides (Ubuntu 24.04 → PG 16; older → older). The
	// field is recorded but does not drive package selection.
	// +optional
	EngineVersion string `json:"engineVersion,omitempty"`

	// DBName is the initial database to create. Default: the instance name.
	// Immutable after first reconcile; modify is refused.
	// +optional
	DBName string `json:"dbName,omitempty"`

	// Port for PostgreSQL. Default 5432.
	// Immutable after first reconcile.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int `json:"port,omitempty"`

	// MasterUsername for the admin user. Default "dbadmin".
	// Immutable after first reconcile.
	// +optional
	MasterUsername string `json:"masterUsername,omitempty"`

	// ManageMasterUserPassword: if true, auto-generate the admin password
	// and store it in the credentials Secret; if false, read it from
	// MasterUserPasswordRef.
	// NOT YET IMPLEMENTED: the controller always generates a random password
	// regardless of this field's value, and never reads
	// MasterUserPasswordRef. The fields are reserved.
	// +optional
	ManageMasterUserPassword bool `json:"manageMasterUserPassword,omitempty"`

	// MasterUserPasswordRef points to a K8s Secret containing the
	// user-supplied admin password.
	// NOT YET IMPLEMENTED — see ManageMasterUserPassword.
	// +optional
	MasterUserPasswordRef *SecretKeyRef `json:"masterUserPasswordRef,omitempty"`

	// AllocatedStorage in GiB.
	// Mutable: changing this on an Available instance resizes the pgdata
	// DataVolume (only larger values are accepted by CDI/Longhorn).
	// +required
	// +kubebuilder:validation:Minimum=1
	AllocatedStorage int `json:"allocatedStorage"`

	// StorageType maps to a Longhorn StorageClass. Default "longhorn".
	// Immutable after first reconcile (StorageClass cannot change on a
	// bound PVC).
	// +optional
	StorageType string `json:"storageType,omitempty"`

	// BackupRetentionPeriod in days. 0 (default) = disabled.
	// NOT YET IMPLEMENTED: no pgBackRest install, schedule, or retention
	// enforcement runs today. The field is recorded but inert.
	// +optional
	// +kubebuilder:validation:Minimum=0
	BackupRetentionPeriod int `json:"backupRetentionPeriod,omitempty"`

	// PreferredBackupWindow in UTC, e.g. "02:00-03:00".
	// NOT YET IMPLEMENTED — see BackupRetentionPeriod.
	// +optional
	// +kubebuilder:validation:Pattern=`^([01]\d|2[0-3]):[0-5]\d-([01]\d|2[0-3]):[0-5]\d$`
	PreferredBackupWindow string `json:"preferredBackupWindow,omitempty"`

	// MultiAZ enables Patroni HA with a standby VM.
	// NOT YET IMPLEMENTED — no standby is created.
	// +optional
	MultiAZ bool `json:"multiAZ,omitempty"`

	// DBParameterGroupRef references a DBParameterGroup by name.
	// NOT YET IMPLEMENTED — the DBParameterGroup CRD does not exist in this
	// module.
	// +optional
	DBParameterGroupRef string `json:"dbParameterGroupRef,omitempty"`

	// DeletionProtection prevents accidental deletion. While true, the
	// finalizer refuses to tear the instance down.
	// Mutable.
	// +optional
	DeletionProtection bool `json:"deletionProtection,omitempty"`

	// Running controls the VM power state. false = stopped (storage preserved).
	// Mutable: toggling sets KubeVirt spec.running on the underlying VM.
	// +kubebuilder:default=true
	// +optional
	Running *bool `json:"running,omitempty"`

	// OSImage is the Harvester VirtualMachineImage to clone for the VM's
	// OS disk. Either "<ns>/<name>" or just "<name>" (resolved in the
	// "default" namespace), or the image's spec.displayName.
	// Immutable after first reconcile.
	// +optional
	OSImage string `json:"osImage,omitempty"`

	// NetworkRef is a Harvester NAD reference (namespace/name) for the VLAN
	// network the database VM attaches to. This is the VM's only network
	// interface: client traffic, package install during cloud-init, and the
	// Prometheus metrics scrape all go through it. The NAD must already exist
	// on the cluster (the controller does not create networks) and the VLAN
	// must have internet egress.
	// Immutable after first reconcile.
	// Example: "iaas-net/vm-subnet-001".
	// +required
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?\/[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	NetworkRef string `json:"networkRef"`

	// StaticNetwork, when set, configures the VM's data NIC with a static
	// IPv4 address, gateway, and DNS servers instead of running DHCP. Use
	// this on VLANs that don't have a DHCP server reachable from the VM.
	// When nil, cloud-init runs DHCP on the data NIC (the default).
	// Immutable after first reconcile (in-VM netplan reconfiguration is not
	// implemented).
	// +optional
	StaticNetwork *NetworkConfig `json:"staticNetwork,omitempty"`

	// VMPassword sets the default console/SSH password for the VM user
	// (ubuntu). For development and debugging only — leave empty in
	// production. Immutable after first reconcile.
	// +optional
	VMPassword string `json:"vmPassword,omitempty"`

	// S3BackupConfig for pgBackRest S3 target.
	// NOT YET IMPLEMENTED — values are written to /etc/dbaas/bootstrap.env
	// on the VM but no backup process consumes them.
	// +optional
	S3BackupConfig *S3BackupConfig `json:"s3BackupConfig,omitempty"`

	// Tags are user-defined labels.
	// NOT YET IMPLEMENTED — not propagated to child resources or dashboards.
	// +optional
	Tags map[string]string `json:"tags,omitempty"`
}

// SecretKeyRef points to a single key within a K8s Secret.
type SecretKeyRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// NetworkConfig is a static IPv4 configuration for the database VM's data
// NIC. When set on DBInstanceSpec.StaticNetwork, these values are written
// into cloud-init's netplan in place of `dhcp4: true`.
type NetworkConfig struct {
	// Address is the IPv4 address with CIDR prefix, e.g. "192.168.40.50/24".
	// +required
	// +kubebuilder:validation:Pattern=`^((25[0-5]|(2[0-4]|1\d|[1-9]|)\d)\.?\b){4}\/(3[0-2]|[12]?\d)$`
	Address string `json:"address"`

	// Gateway is the IPv4 default gateway, e.g. "192.168.40.1".
	// +required
	// +kubebuilder:validation:Pattern=`^((25[0-5]|(2[0-4]|1\d|[1-9]|)\d)\.?\b){4}$`
	Gateway string `json:"gateway"`

	// Nameservers are the DNS server IPs the VM should use. Supply at
	// least one — cloud-init will fail to resolve apt mirrors without DNS.
	// +required
	// +kubebuilder:validation:MinItems=1
	Nameservers []string `json:"nameservers"`

	// SearchDomains are DNS search-domain suffixes. Optional.
	// +optional
	SearchDomains []string `json:"searchDomains,omitempty"`
}

// S3BackupConfig describes the pgBackRest S3 target.
type S3BackupConfig struct {
	Endpoint string `json:"endpoint"`
	Bucket   string `json:"bucket"`
	// +optional
	Region string `json:"region,omitempty"`
	// SecretRef is a K8s Secret name with accessKey + secretKey.
	SecretRef string `json:"secretRef"`
}

// DBInstanceStatus defines the observed state of a DBInstance.
type DBInstanceStatus struct {
	// Phase matches RDS DBInstanceStatus strings.
	// +optional
	Phase string `json:"phase,omitempty"`

	// ProvisioningPhase tracks which reconcile step we're on.
	// +optional
	ProvisioningPhase string `json:"provisioningPhase,omitempty"`

	// Conditions for each sub-resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Endpoint is populated when the database is reachable.
	// +optional
	Endpoint *Endpoint `json:"endpoint,omitempty"`

	// MasterUserSecret references the K8s Secret with credentials.
	// +optional
	MasterUserSecret *MasterUserSecretRef `json:"masterUserSecret,omitempty"`

	// Resources tracks every Harvester object created for cleanup and idempotency.
	// +optional
	Resources ResourceRefs `json:"resources,omitempty"`

	// GrafanaURL is the per-instance Grafana dashboard URL.
	// +optional
	GrafanaURL string `json:"grafanaUrl,omitempty"`

	// PrometheusTarget is the scrape target for the instance's metrics exporter.
	// +optional
	PrometheusTarget string `json:"prometheusTarget,omitempty"`

	// CACertPEM is the generated CA for SSL verification.
	// +optional
	CACertPEM string `json:"caCertPem,omitempty"`

	// ReadReplicas tracks child replica identifiers.
	// +optional
	ReadReplicas []string `json:"readReplicas,omitempty"`

	// Message is a human-readable description of the current state.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration tracks which spec version has been reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// AppliedSpec is the snapshot of immutable-after-create spec fields
	// captured at first successful reconcile. The reconciler refuses to
	// advance ObservedGeneration when any of these fields differ from the
	// current spec, because the implementation cannot carry the change
	// through to the running database. Used for honest modify semantics.
	// +optional
	AppliedSpec *AppliedSpec `json:"appliedSpec,omitempty"`
}

// AppliedSpec records the subset of DBInstanceSpec fields that are
// immutable after creation in this controller's implementation. Mutable
// fields (DBInstanceClass, AllocatedStorage, Running, DeletionProtection)
// are deliberately excluded — they're allowed to change at any time.
type AppliedSpec struct {
	// +optional
	NetworkRef string `json:"networkRef,omitempty"`
	// +optional
	OSImage string `json:"osImage,omitempty"`
	// +optional
	DBName string `json:"dbName,omitempty"`
	// +optional
	MasterUsername string `json:"masterUsername,omitempty"`
	// +optional
	EngineVersion string `json:"engineVersion,omitempty"`
	// +optional
	Port int `json:"port,omitempty"`
	// +optional
	StorageType string `json:"storageType,omitempty"`
}

// Endpoint is the network address clients use to reach the database.
type Endpoint struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
	// +optional
	JDBCURL string `json:"jdbcUrl,omitempty"`
}

// MasterUserSecretRef references the K8s Secret holding the master credentials.
type MasterUserSecretRef struct {
	Name string `json:"name"`
	// Status is "active" or "impaired".
	Status string `json:"status"`
}

// ResourceRefs tracks every Harvester resource the controller created.
// Each field is populated as the corresponding phase completes. On controller
// restart, the reconciler reads these to skip completed phases. All resources
// live in the DBInstance's own namespace — read it via inst.Namespace, not
// from this struct.
type ResourceRefs struct {
	// NADName is the Multus NetworkAttachmentDefinition the VM's data NIC
	// attaches to. The controller does not create the NAD; this just records
	// the reference from spec.networkRef so callers can see it on the CR.
	// +optional
	NADName string `json:"nadName,omitempty"`
	// +optional
	DataVolumeName string `json:"dataVolumeName,omitempty"`
	// +optional
	VMName string `json:"vmName,omitempty"`
	// +optional
	SecretName string `json:"secretName,omitempty"`
	// +optional
	ServiceMonitor string `json:"serviceMonitor,omitempty"`
	// MetricsServiceName is the headless Service Prometheus scrapes through.
	// Tracked separately from ServiceMonitor so the finalizer's TeardownAll
	// can delete it (forgetting it leaves orphan Services in the tenant ns).
	// +optional
	MetricsServiceName string `json:"metricsServiceName,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=dbi
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.dbInstanceClass`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint.address`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DBInstance represents a managed PostgreSQL database on Harvester HCI.
// Namespaced — each DBInstance lives in a tenant namespace. All Harvester
// child resources (VM, DataVolume, Secret, Service, ServiceMonitor) are
// created in the same namespace as the DBInstance.
type DBInstance struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DBInstanceSpec   `json:"spec,omitempty"`
	Status DBInstanceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DBInstanceList contains a list of DBInstance.
type DBInstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DBInstance `json:"items"`
}

const (
	// Status.ProvisioningPhase values (internal reconcile steps).
	PhasePending             = "Pending"
	PhaseNetworkProvisioned  = "NetworkProvisioned"
	PhaseStorageProvisioned  = "StorageProvisioned"
	PhaseVMCreated           = "VMCreated"
	PhaseWaitingForCloudInit = "WaitingForCloudInit"
	PhaseDatabaseReady       = "DatabaseReady"
	PhaseMonitoringDeployed  = "MonitoringDeployed"
	PhaseAvailable           = "Available"
	PhaseFailed              = "Failed"

	// Status.Phase values (RDS-compatible lowercase strings).
	StatusCreating  = "creating"
	StatusAvailable = "available"
	StatusStopping  = "stopping"
	StatusStopped   = "stopped"
	StatusStarting  = "starting"
	StatusModifying = "modifying"
	StatusDeleting  = "deleting"
	StatusFailed    = "failed"

	// MasterUserSecretRef.Status values.
	SecretStatusActive   = "active"
	SecretStatusImpaired = "impaired"

	// Label keys applied to all Harvester resources owned by a DBInstance.
	LabelInstance = "dbaas.opencloud.wso2.com/instance"
	LabelRole     = "dbaas.opencloud.wso2.com/role"
	LabelMetrics  = "dbaas.opencloud.wso2.com/metrics"

	// FinalizerName triggers controller-side teardown of Harvester resources.
	FinalizerName = "dbaas.opencloud.wso2.com/cleanup"
)

// InstanceClassSpec maps RDS-style class names to Harvester VM resources.
type InstanceClassSpec struct {
	CPUCores       int
	MemoryMB       int
	MaxConnections int
}

// InstanceClasses is the catalog of supported instance classes.
var InstanceClasses = map[string]InstanceClassSpec{
	"db.t3.micro":   {1, 1024, 50},
	"db.t3.small":   {1, 2048, 100},
	"db.t3.medium":  {2, 4096, 150},
	"db.t3.large":   {2, 8192, 200},
	"db.t3.xlarge":  {4, 16384, 300},
	"db.m5.large":   {2, 8192, 200},
	"db.m5.xlarge":  {4, 16384, 400},
	"db.m5.2xlarge": {8, 32768, 600},
	"db.m5.4xlarge": {16, 65536, 1000},
	"db.r5.large":   {2, 16384, 300},
	"db.r5.xlarge":  {4, 32768, 500},
	"db.r5.2xlarge": {8, 65536, 800},
}

func init() {
	SchemeBuilder.Register(&DBInstance{}, &DBInstanceList{})
}
