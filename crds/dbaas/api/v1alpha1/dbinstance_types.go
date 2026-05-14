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
type DBInstanceSpec struct {
	// DBInstanceClass maps to CPU/RAM. e.g. "db.t3.medium", "db.m5.large".
	// +required
	DBInstanceClass string `json:"dbInstanceClass"`

	// EngineVersion is the PostgreSQL major version. Default "16".
	// +optional
	EngineVersion string `json:"engineVersion,omitempty"`

	// DBName is the initial database to create.
	// +optional
	DBName string `json:"dbName,omitempty"`

	// Port for PostgreSQL. Default 5432.
	// +optional
	Port int `json:"port,omitempty"`

	// MasterUsername for the admin user. Default "dbadmin".
	// +optional
	MasterUsername string `json:"masterUsername,omitempty"`

	// ManageMasterUserPassword: if true, auto-generate and store in a K8s Secret.
	// If false, read from MasterUserPasswordRef.
	// +optional
	ManageMasterUserPassword bool `json:"manageMasterUserPassword,omitempty"`

	// MasterUserPasswordRef points to a K8s Secret containing the user-supplied password.
	// +optional
	MasterUserPasswordRef *SecretKeyRef `json:"masterUserPasswordRef,omitempty"`

	// AllocatedStorage in GiB.
	// +required
	AllocatedStorage int `json:"allocatedStorage"`

	// StorageType maps to a Longhorn StorageClass. Default "longhorn".
	// +optional
	StorageType string `json:"storageType,omitempty"`

	// DBSubnetGroupName is the consumer VLAN CIDR or Kube-OVN subnet for app access.
	// +optional
	DBSubnetGroupName string `json:"dbSubnetGroupName,omitempty"`

	// VpcPeering configures Kube-OVN VPC peering between the DBaaS VPC and an
	// external VPC (e.g. the RKE2 cluster VPC) so application pods can reach the
	// database without being co-located in the DBaaS namespace.
	// +optional
	VpcPeering *VpcPeeringConfig `json:"vpcPeering,omitempty"`

	// BackupRetentionPeriod in days. 0 = disabled. Default 7.
	// +optional
	BackupRetentionPeriod int `json:"backupRetentionPeriod,omitempty"`

	// PreferredBackupWindow in UTC. e.g. "02:00-03:00".
	// +optional
	PreferredBackupWindow string `json:"preferredBackupWindow,omitempty"`

	// MultiAZ enables Patroni HA with a standby VM.
	// +optional
	MultiAZ bool `json:"multiAZ,omitempty"`

	// DBParameterGroupRef references a DBParameterGroup by name.
	// +optional
	DBParameterGroupRef string `json:"dbParameterGroupRef,omitempty"`

	// DeletionProtection prevents accidental deletion.
	// +optional
	DeletionProtection bool `json:"deletionProtection,omitempty"`

	// Running controls the VM power state. false = stopped (storage preserved).
	// +kubebuilder:default=true
	// +optional
	Running *bool `json:"running,omitempty"`

	// OSImage is the Harvester VM image name.
	// Default "ubuntu-22.04-server-cloudimg-amd64.img".
	// +optional
	OSImage string `json:"osImage,omitempty"`

	// NetworkRef is a Harvester NAD reference (namespace/name) for an existing
	// VLAN network to attach the VM to as its primary NIC. When set, the
	// controller skips Kube-OVN VPC/Subnet/NAD creation and uses this NAD
	// directly. Use this on clusters where Kube-OVN VPC is not available.
	// Example: "iaas-net/vm-subnet-001".
	// +optional
	NetworkRef string `json:"networkRef,omitempty"`

	// ConsumerNetwork is the Harvester NAD reference (namespace/name) for a
	// consumer VLAN that application workloads use to reach the database
	// directly via L2. When set, the VM gets a third NIC bridged to this
	// network. Example: "default/vm-net-100".
	// +optional
	ConsumerNetwork string `json:"consumerNetwork,omitempty"`

	// VMPassword sets the default console/SSH password for the VM user (ubuntu).
	// For development and debugging only — leave empty in production.
	// +optional
	VMPassword string `json:"vmPassword,omitempty"`

	// S3BackupConfig for pgBackRest S3 target.
	// +optional
	S3BackupConfig *S3BackupConfig `json:"s3BackupConfig,omitempty"`

	// Tags are user-defined labels.
	// +optional
	Tags map[string]string `json:"tags,omitempty"`
}

// SecretKeyRef points to a single key within a K8s Secret.
type SecretKeyRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// VpcPeeringConfig specifies the remote VPC and subnet to peer with.
type VpcPeeringConfig struct {
	// RemoteVpc is the name of the Kube-OVN VPC to peer with (e.g. the RKE2 cluster VPC).
	RemoteVpc string `json:"remoteVpc"`

	// RemoteSubnet is the Kube-OVN subnet name in the remote VPC.
	RemoteSubnet string `json:"remoteSubnet"`
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
	// +optional
	VPCName string `json:"vpcName,omitempty"`
	// +optional
	SubnetName string `json:"subnetName,omitempty"`
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
	// +optional
	VpcPeeringName string `json:"vpcPeeringName,omitempty"`
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
	PhaseVpcPeeringCreated   = "VpcPeeringCreated"
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
