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

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// DatabaseSpec defines the desired state of Database
type DatabaseSpec struct {
	// engine is the database engine to provision.
	// +kubebuilder:validation:Enum=postgres;mysql;mariadb
	// +required
	Engine string `json:"engine"`

	// version is the database engine version, e.g. "16" for postgres.
	// +kubebuilder:validation:MinLength=1
	// +required
	Version string `json:"version"`

	// storageGB is the requested persistent storage size in gigabytes.
	// +kubebuilder:validation:Minimum=1
	// +required
	StorageGB int32 `json:"storageGB"`

	// replicas is the number of database instances to run.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
}

// DatabasePhase represents the lifecycle phase of a Database.
type DatabasePhase string

const (
	// DatabasePhasePending means the Database has been accepted but not yet acted on.
	DatabasePhasePending DatabasePhase = "Pending"
	// DatabasePhaseProvisioning means the backing resources are being created.
	DatabasePhaseProvisioning DatabasePhase = "Provisioning"
	// DatabasePhaseReady means the Database is provisioned and serving connections.
	DatabasePhaseReady DatabasePhase = "Ready"
	// DatabasePhaseFailed means provisioning failed; see Conditions for detail.
	DatabasePhaseFailed DatabasePhase = "Failed"
)

// DatabaseStatus defines the observed state of Database.
type DatabaseStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// phase is the current lifecycle phase of the Database.
	// +optional
	Phase DatabasePhase `json:"phase,omitempty"`

	// endpoint is the host:port clients use to connect once the Database is Ready.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// observedGeneration is the .metadata.generation last reconciled by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions represent the current state of the Database resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=db
// +kubebuilder:printcolumn:name="Engine",type=string,JSONPath=`.spec.engine`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Database is the Schema for the databases API
type Database struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Database
	// +required
	Spec DatabaseSpec `json:"spec"`

	// status defines the observed state of Database
	// +optional
	Status DatabaseStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// DatabaseList contains a list of Database
type DatabaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Database `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Database{}, &DatabaseList{})
}
