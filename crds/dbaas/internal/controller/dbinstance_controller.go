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

package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dbaasv1 "github.com/wso2/open-cloud-datacenter/crds/dbaas/api/v1alpha1"
	"github.com/wso2/open-cloud-datacenter/crds/dbaas/internal/harvester"
)

// probeListener is the function phaseWaitReady calls to confirm postgres
// is actually accepting TCP before marking the instance DatabaseReady.
// Package-level var so tests can stub it without running real Pods.
var probeListener = func(c *harvester.Client, ctx context.Context, ns, vmName, nadRef, ip string, port int, sn *dbaasv1.NetworkConfig) error {
	return c.ProbeVMListener(ctx, ns, vmName, nadRef, ip, port, sn)
}

// Controller-side defaults for fields the user can leave blank on the
// DBInstance spec. Centralised here so phaseStorage, phaseVM, and
// immutableDrift can't drift apart over time. A change here should be
// rare and accompanied by docs/USAGE updates.
const (
	defaultOSImage     = "ubuntu-22.04-server-cloudimg-amd64.img"
	defaultStorageType = "longhorn"
	defaultMasterUser  = "dbadmin"
	defaultPort        = 5432
)

// DBInstanceReconciler reconciles DBInstance CRDs.
// Each Reconcile call advances exactly one provisioning phase,
// updates the status, and requeues for the next phase.
type DBInstanceReconciler struct {
	client.Client
	Harvester *harvester.Client
}

// DBInstance CRD permissions.
// +kubebuilder:rbac:groups=dbaas.opencloud.wso2.com,resources=dbinstances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dbaas.opencloud.wso2.com,resources=dbinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dbaas.opencloud.wso2.com,resources=dbinstances/finalizers,verbs=update

// Harvester resources the reconciler creates and tears down on behalf of callers.
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines,verbs=get;create;update;delete
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachineinstances,verbs=get
// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=datavolumes,verbs=get;create;update;delete
// +kubebuilder:rbac:groups=harvesterhci.io,resources=virtualmachineimages,verbs=get;list
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;create;update;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;create;update;delete
// +kubebuilder:rbac:groups="",resources=endpoints,verbs=get;create;update;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;create;delete

// Reconcile is the main entry point called by controller-runtime.
func (r *DBInstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var inst dbaasv1.DBInstance
	if err := r.Get(ctx, req.NamespacedName, &inst); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil // deleted
		}
		return ctrl.Result{}, err
	}

	logger.Info("Reconciling", "name", inst.Name, "phase", inst.Status.ProvisioningPhase)

	// --- Handle deletion via finalizer ---
	if !inst.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&inst, dbaasv1.FinalizerName) {
			return r.reconcileDelete(ctx, &inst)
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer is present
	if !controllerutil.ContainsFinalizer(&inst, dbaasv1.FinalizerName) {
		controllerutil.AddFinalizer(&inst, dbaasv1.FinalizerName)
		if err := r.Update(ctx, &inst); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// --- Handle stop/start ---
	if inst.Spec.Running != nil && !*inst.Spec.Running && inst.Status.Phase == dbaasv1.StatusAvailable {
		return r.reconcileStop(ctx, &inst)
	}
	if inst.Spec.Running != nil && *inst.Spec.Running && inst.Status.Phase == dbaasv1.StatusStopped {
		return r.reconcileStart(ctx, &inst)
	}

	// --- Handle spec changes on available instance ---
	if inst.Status.Phase == dbaasv1.StatusAvailable && inst.Generation != inst.Status.ObservedGeneration {
		return r.reconcileModify(ctx, &inst)
	}

	// --- Phase-based provisioning ---
	switch inst.Status.ProvisioningPhase {
	case "", dbaasv1.PhasePending:
		return r.phaseNetwork(ctx, &inst)
	case dbaasv1.PhaseNetworkProvisioned:
		return r.phaseStorage(ctx, &inst)
	case dbaasv1.PhaseStorageProvisioned:
		return r.phaseVM(ctx, &inst)
	case dbaasv1.PhaseVMCreated, dbaasv1.PhaseWaitingForCloudInit:
		return r.phaseWaitReady(ctx, &inst)
	case dbaasv1.PhaseDatabaseReady:
		return r.phaseMonitoring(ctx, &inst)
	case dbaasv1.PhaseMonitoringDeployed:
		return r.phaseAvailable(ctx, &inst)
	case dbaasv1.PhaseAvailable:
		return r.phaseAvailable(ctx, &inst)
	case dbaasv1.PhaseFailed:
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	default:
		return ctrl.Result{}, fmt.Errorf("unknown phase: %s", inst.Status.ProvisioningPhase)
	}
}

// ============================================================
// Provisioning phases
// ============================================================

func (r *DBInstanceReconciler) phaseNetwork(ctx context.Context, inst *dbaasv1.DBInstance) (ctrl.Result, error) {
	// First entry: mark the instance as creating before doing any work.
	if inst.Status.Phase == "" {
		inst.Status.Phase = dbaasv1.StatusCreating
	}

	// Skip if already done.
	if inst.Status.Resources.NADName != "" {
		inst.Status.ProvisioningPhase = dbaasv1.PhaseNetworkProvisioned
		return r.advance(ctx, inst)
	}

	// The controller doesn't create or own networks: spec.networkRef must
	// point at an existing Multus NAD (typically a Harvester VLAN network).
	if inst.Spec.NetworkRef == "" {
		return r.fail(ctx, inst, "NetworkRefMissing",
			fmt.Errorf("spec.networkRef is required (namespace/nad of an existing Multus NetworkAttachmentDefinition)"))
	}

	inst.Status.Resources.NADName = inst.Spec.NetworkRef
	inst.Status.ProvisioningPhase = dbaasv1.PhaseNetworkProvisioned
	inst.Status.Message = fmt.Sprintf("Using network %s", inst.Spec.NetworkRef)

	return r.advance(ctx, inst)
}

func (r *DBInstanceReconciler) phaseStorage(ctx context.Context, inst *dbaasv1.DBInstance) (ctrl.Result, error) {
	if inst.Status.Resources.DataVolumeName != "" {
		inst.Status.ProvisioningPhase = dbaasv1.PhaseStorageProvisioned
		return r.advance(ctx, inst)
	}

	id := inst.Name
	ns := inst.Namespace
	storageType := inst.Spec.StorageType
	if storageType == "" {
		storageType = defaultStorageType
	}

	dvName, err := r.Harvester.CreateDataVolume(ctx, id, ns, inst.Spec.AllocatedStorage, storageType)
	if err != nil {
		return r.fail(ctx, inst, "StorageFailed", err)
	}

	inst.Status.Resources.DataVolumeName = dvName
	inst.Status.ProvisioningPhase = dbaasv1.PhaseStorageProvisioned
	inst.Status.Message = "Encrypted storage provisioned"

	return r.advance(ctx, inst)
}

func (r *DBInstanceReconciler) phaseVM(ctx context.Context, inst *dbaasv1.DBInstance) (ctrl.Result, error) {
	if inst.Status.Resources.VMName != "" {
		inst.Status.ProvisioningPhase = dbaasv1.PhaseVMCreated
		return ctrl.Result{RequeueAfter: 10 * time.Second}, r.statusUpdate(ctx, inst)
	}

	id := inst.Name
	ns := inst.Namespace

	classSpec, ok := dbaasv1.InstanceClasses[inst.Spec.DBInstanceClass]
	if !ok {
		return r.fail(ctx, inst, "InvalidClass", fmt.Errorf("unknown class: %s", inst.Spec.DBInstanceClass))
	}

	masterUser := inst.Spec.MasterUsername
	if masterUser == "" {
		masterUser = defaultMasterUser
	}
	dbName := inst.Spec.DBName
	if dbName == "" {
		dbName = id
	}
	osImage := inst.Spec.OSImage
	if osImage == "" {
		osImage = defaultOSImage
	}
	storageType := inst.Spec.StorageType
	if storageType == "" {
		storageType = defaultStorageType
	}

	vmName, secretName, caCertPEM, err := r.Harvester.CreatePostgresVM(ctx, harvester.VMCreateParams{
		ID:             id,
		Namespace:      ns,
		CPUCores:       classSpec.CPUCores,
		MemoryMB:       classSpec.MemoryMB,
		OSImage:        osImage,
		DataVolumeRef:  inst.Status.Resources.DataVolumeName,
		NADName:        inst.Status.Resources.NADName,
		MasterUser:     masterUser,
		DBName:         dbName,
		Port:           specPort(inst.Spec.Port),
		MaxConnections: classSpec.MaxConnections,
		BackupEnabled:  inst.Spec.BackupRetentionPeriod > 0,
		BackupWindow:   inst.Spec.PreferredBackupWindow,
		S3Config:       inst.Spec.S3BackupConfig,
		VMPassword:     inst.Spec.VMPassword,
		StaticNetwork:  inst.Spec.StaticNetwork,
	})
	if err != nil {
		return r.fail(ctx, inst, "VMCreateFailed", err)
	}

	inst.Status.Resources.VMName = vmName
	inst.Status.Resources.SecretName = secretName
	inst.Status.CACertPEM = caCertPEM
	inst.Status.MasterUserSecret = &dbaasv1.MasterUserSecretRef{
		Name:   secretName,
		Status: dbaasv1.SecretStatusActive,
	}
	// Snapshot the immutable fields as they were applied. reconcileModify
	// later refuses any spec change that drifts from this snapshot, so the
	// CR never reports observedGeneration=current for changes we silently
	// couldn't carry through to the running VM.
	inst.Status.AppliedSpec = &dbaasv1.AppliedSpec{
		NetworkRef:     inst.Spec.NetworkRef,
		OSImage:        osImage,
		DBName:         dbName,
		MasterUsername: masterUser,
		EngineVersion:  inst.Spec.EngineVersion,
		Port:           specPort(inst.Spec.Port),
		StorageType:    storageType,
	}
	inst.Status.ProvisioningPhase = dbaasv1.PhaseVMCreated
	inst.Status.Message = "VM created, waiting for PostgreSQL to initialize"

	return ctrl.Result{RequeueAfter: 10 * time.Second}, r.statusUpdate(ctx, inst)
}

func (r *DBInstanceReconciler) phaseWaitReady(ctx context.Context, inst *dbaasv1.DBInstance) (ctrl.Result, error) {
	ns := inst.Namespace

	// First gate: VMI is Running and the qemu-guest-agent has reported an IP.
	readiness, err := r.Harvester.GetVMIReadiness(ctx, ns, inst.Status.Resources.VMName)
	if err != nil || !readiness.Running || readiness.IP == "" {
		inst.Status.Message = "Waiting for VM to become ready"
		inst.Status.ProvisioningPhase = dbaasv1.PhaseWaitingForCloudInit
		_ = r.statusUpdate(ctx, inst)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	port := specPort(inst.Spec.Port)
	dbName := inst.Spec.DBName
	if dbName == "" {
		dbName = inst.Name
	}

	// Second gate: PostgreSQL is actually accepting TCP connections on its
	// listener. The guest-agent IP alone is too weak — the agent starts as
	// soon as `apt install` finishes, well before bootstrap.sh has moved
	// pgdata onto the dedicated disk, restarted postgres, and created the
	// admin role. A pure VMI-readiness gate has previously let a broken
	// postgres slip through as "available".
	//
	// The probe runs from a one-shot Pod attached to the same Multus NAD
	// as the VM (see harvester.ProbeVMListener for why). TCP-only — not a
	// SQL ping — keeps the controller free of a DB driver dependency;
	// postgres opens its listener only when it is genuinely ready to
	// accept SQL, so a successful dial is a sufficient signal.
	if derr := probeListener(r.Harvester, ctx, ns, inst.Status.Resources.VMName,
		inst.Status.Resources.NADName, readiness.IP, port, inst.Spec.StaticNetwork); derr != nil {
		inst.Status.Message = fmt.Sprintf("Waiting for PostgreSQL listener at %s:%d: %v",
			readiness.IP, port, derr)
		inst.Status.ProvisioningPhase = dbaasv1.PhaseWaitingForCloudInit
		_ = r.statusUpdate(ctx, inst)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	inst.Status.Endpoint = &dbaasv1.Endpoint{
		Address: readiness.IP,
		Port:    port,
		JDBCURL: fmt.Sprintf("jdbc:postgresql://%s:%d/%s?ssl=true&sslmode=verify-ca", readiness.IP, port, dbName),
	}
	inst.Status.ProvisioningPhase = dbaasv1.PhaseDatabaseReady
	inst.Status.Message = "PostgreSQL is ready"

	return r.advance(ctx, inst)
}

func (r *DBInstanceReconciler) phaseMonitoring(ctx context.Context, inst *dbaasv1.DBInstance) (ctrl.Result, error) {
	if inst.Status.Resources.ServiceMonitor != "" {
		inst.Status.ProvisioningPhase = dbaasv1.PhaseMonitoringDeployed
		return r.advance(ctx, inst)
	}

	id := inst.Name
	ns := inst.Namespace

	if inst.Status.Endpoint == nil || inst.Status.Endpoint.Address == "" {
		inst.Status.Message = "Waiting for database endpoint before monitoring setup"
		return ctrl.Result{RequeueAfter: 10 * time.Second}, r.statusUpdate(ctx, inst)
	}

	svcName, smName, grafanaURL, promTarget, err := r.Harvester.DeployMonitoring(ctx, id, ns, inst.Status.Endpoint.Address)
	if err != nil {
		// Non-fatal — DB works without monitoring. Track the Service name
		// regardless: DeployMonitoring creates the Service first, so a
		// partial failure may leave the Service behind for the finalizer
		// to clean up.
		log.FromContext(ctx).Error(err, "monitoring setup failed (non-fatal)")
		inst.Status.Message = "Available (monitoring setup failed, will retry)"
		inst.Status.Resources.MetricsServiceName = svcName
	} else {
		inst.Status.Resources.ServiceMonitor = smName
		inst.Status.Resources.MetricsServiceName = svcName
		inst.Status.GrafanaURL = grafanaURL
		inst.Status.PrometheusTarget = promTarget
	}

	inst.Status.ProvisioningPhase = dbaasv1.PhaseMonitoringDeployed
	return r.advance(ctx, inst)
}

func (r *DBInstanceReconciler) phaseAvailable(ctx context.Context, inst *dbaasv1.DBInstance) (ctrl.Result, error) {
	// Snapshot the current status before mutating so we can skip the
	// kube-apiserver round-trip when nothing actually changed. This phase
	// runs on every 60s requeue for the lifetime of every Available
	// DBInstance; a churn-free reconcile keeps audit-log volume and
	// watch-event fanout down for clusters with many databases.
	prev := inst.Status.DeepCopy()

	inst.Status.Phase = dbaasv1.StatusAvailable
	inst.Status.ProvisioningPhase = dbaasv1.PhaseAvailable
	inst.Status.ObservedGeneration = inst.Generation
	inst.Status.Message = "Database instance is available"

	// Re-check the data-net IP on every requeue — the guest agent may
	// report it later than initial readiness, or it can change after a VM
	// restart.
	readiness, _ := r.Harvester.GetVMIReadiness(ctx, inst.Namespace, inst.Status.Resources.VMName)
	if readiness.IP != "" && (inst.Status.Endpoint == nil || inst.Status.Endpoint.Address != readiness.IP) {
		port := specPort(inst.Spec.Port)
		dbName := inst.Spec.DBName
		if dbName == "" {
			dbName = inst.Name
		}
		inst.Status.Endpoint = &dbaasv1.Endpoint{
			Address: readiness.IP,
			Port:    port,
			JDBCURL: fmt.Sprintf("jdbc:postgresql://%s:%d/%s?ssl=true&sslmode=verify-ca", readiness.IP, port, dbName),
		}
		log.FromContext(ctx).Info("endpoint updated", "ip", readiness.IP)
	}

	if inst.Status.Endpoint != nil && inst.Status.Endpoint.Address != "" {
		svcName, smName, grafanaURL, promTarget, err := r.Harvester.DeployMonitoring(ctx, inst.Name, inst.Namespace, inst.Status.Endpoint.Address)
		if err != nil {
			log.FromContext(ctx).Error(err, "monitoring refresh failed (non-fatal)")
		} else {
			inst.Status.Resources.MetricsServiceName = svcName
			inst.Status.Resources.ServiceMonitor = smName
			inst.Status.GrafanaURL = grafanaURL
			inst.Status.PrometheusTarget = promTarget
		}
	}

	if equality.Semantic.DeepEqual(prev, &inst.Status) {
		// No status drift this cycle — skip the Update entirely.
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}
	return ctrl.Result{RequeueAfter: 60 * time.Second}, r.statusUpdate(ctx, inst)
}

// ============================================================
// Stop / Start / Modify / Delete
// ============================================================

func (r *DBInstanceReconciler) reconcileStop(ctx context.Context, inst *dbaasv1.DBInstance) (ctrl.Result, error) {
	// A user who flips spec.running and also edits an immutable field in
	// the same kubectl apply hits this path before reconcileModify (the
	// dispatcher routes running-toggles first). Without this guard,
	// observedGeneration silently catches up — same lie reconcileModify
	// used to tell. Refuse the whole change with a clear message.
	if drift := immutableDrift(inst); drift != "" {
		return r.fail(ctx, inst, "ImmutableFieldChanged",
			fmt.Errorf("cannot modify field(s) %s while stopping; revert or recreate the DBInstance", drift))
	}

	ns := inst.Namespace
	inst.Status.Phase = dbaasv1.StatusStopping
	inst.Status.Message = "Stopping VM"
	_ = r.statusUpdate(ctx, inst)

	if err := r.Harvester.StopVM(ctx, ns, inst.Status.Resources.VMName); err != nil {
		return r.fail(ctx, inst, "StopFailed", err)
	}

	inst.Status.Phase = dbaasv1.StatusStopped
	inst.Status.Message = "Stopped. Storage preserved."
	inst.Status.ObservedGeneration = inst.Generation
	return ctrl.Result{}, r.statusUpdate(ctx, inst)
}

func (r *DBInstanceReconciler) reconcileStart(ctx context.Context, inst *dbaasv1.DBInstance) (ctrl.Result, error) {
	// See reconcileStop above — same drift guard for the start path.
	if drift := immutableDrift(inst); drift != "" {
		return r.fail(ctx, inst, "ImmutableFieldChanged",
			fmt.Errorf("cannot modify field(s) %s while starting; revert or recreate the DBInstance", drift))
	}

	ns := inst.Namespace
	inst.Status.Phase = dbaasv1.StatusStarting
	_ = r.statusUpdate(ctx, inst)

	if err := r.Harvester.StartVM(ctx, ns, inst.Status.Resources.VMName); err != nil {
		return r.fail(ctx, inst, "StartFailed", err)
	}

	inst.Status.Phase = dbaasv1.StatusAvailable
	inst.Status.Message = "Started"
	inst.Status.ObservedGeneration = inst.Generation
	return ctrl.Result{}, r.statusUpdate(ctx, inst)
}

func (r *DBInstanceReconciler) reconcileModify(ctx context.Context, inst *dbaasv1.DBInstance) (ctrl.Result, error) {
	ns := inst.Namespace

	// Refuse the modify if any field we can't re-apply has drifted from the
	// snapshot taken at create time. Without this guard we silently set
	// observedGeneration = generation even when a user changed networkRef
	// or dbName, leaving them to believe the controller honoured it.
	if drift := immutableDrift(inst); drift != "" {
		return r.fail(ctx, inst, "ImmutableFieldChanged",
			fmt.Errorf("cannot modify field(s) %s after create; revert or recreate the DBInstance", drift))
	}

	inst.Status.Phase = dbaasv1.StatusModifying
	_ = r.statusUpdate(ctx, inst)

	classSpec, ok := dbaasv1.InstanceClasses[inst.Spec.DBInstanceClass]
	if !ok {
		return r.fail(ctx, inst, "InvalidClass", fmt.Errorf("unknown class: %s", inst.Spec.DBInstanceClass))
	}

	var wg sync.WaitGroup
	var vmErr, dvErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		vmErr = r.Harvester.ResizeVM(ctx, ns, inst.Status.Resources.VMName, classSpec.CPUCores, classSpec.MemoryMB)
	}()
	go func() {
		defer wg.Done()
		dvErr = r.Harvester.ResizeDataVolume(ctx, ns, inst.Status.Resources.DataVolumeName, inst.Spec.AllocatedStorage)
	}()
	wg.Wait()

	if vmErr != nil {
		return r.fail(ctx, inst, "ResizeVMFailed", vmErr)
	}
	if dvErr != nil {
		return r.fail(ctx, inst, "ResizeStorageFailed", dvErr)
	}

	inst.Status.Phase = dbaasv1.StatusAvailable
	inst.Status.Message = fmt.Sprintf("Resized to %s, %dGiB", inst.Spec.DBInstanceClass, inst.Spec.AllocatedStorage)
	inst.Status.ObservedGeneration = inst.Generation
	return ctrl.Result{}, r.statusUpdate(ctx, inst)
}

// immutableDrift returns a comma-separated list of immutable spec fields
// that have drifted from the snapshot recorded at create time, or "" if no
// drift exists. If the snapshot is missing (older instances created before
// the snapshot was introduced), drift is treated as zero so we don't break
// existing deployments.
func immutableDrift(inst *dbaasv1.DBInstance) string {
	a := inst.Status.AppliedSpec
	if a == nil {
		return ""
	}
	osImage := inst.Spec.OSImage
	if osImage == "" {
		osImage = defaultOSImage
	}
	dbName := inst.Spec.DBName
	if dbName == "" {
		dbName = inst.Name
	}
	masterUser := inst.Spec.MasterUsername
	if masterUser == "" {
		masterUser = defaultMasterUser
	}
	port := specPort(inst.Spec.Port)
	storageType := inst.Spec.StorageType
	if storageType == "" {
		storageType = defaultStorageType
	}

	appliedOSImage := a.OSImage
	if appliedOSImage == "" {
		appliedOSImage = defaultOSImage
	}
	appliedDBName := a.DBName
	if appliedDBName == "" {
		appliedDBName = inst.Name
	}
	appliedMasterUser := a.MasterUsername
	if appliedMasterUser == "" {
		appliedMasterUser = defaultMasterUser
	}
	appliedPort := a.Port
	if appliedPort == 0 {
		appliedPort = 5432
	}
	appliedStorageType := a.StorageType
	if appliedStorageType == "" {
		appliedStorageType = defaultStorageType
	}

	var changed []string
	if a.NetworkRef != inst.Spec.NetworkRef {
		changed = append(changed, "networkRef")
	}
	if appliedOSImage != osImage {
		changed = append(changed, "osImage")
	}
	if appliedDBName != dbName {
		changed = append(changed, "dbName")
	}
	if appliedMasterUser != masterUser {
		changed = append(changed, "masterUsername")
	}
	if a.EngineVersion != inst.Spec.EngineVersion {
		changed = append(changed, "engineVersion")
	}
	if appliedPort != port {
		changed = append(changed, "port")
	}
	if appliedStorageType != storageType {
		changed = append(changed, "storageType")
	}
	return strings.Join(changed, ",")
}

func (r *DBInstanceReconciler) reconcileDelete(ctx context.Context, inst *dbaasv1.DBInstance) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	ns := inst.Namespace

	if inst.Spec.DeletionProtection {
		inst.Status.Message = "Cannot delete: DeletionProtection is enabled"
		_ = r.statusUpdate(ctx, inst)
		return ctrl.Result{}, fmt.Errorf("deletion protection enabled")
	}

	inst.Status.Phase = dbaasv1.StatusDeleting
	inst.Status.Message = "Tearing down resources"
	_ = r.statusUpdate(ctx, inst)

	logger.Info("Tearing down child resources", "namespace", ns)
	if err := r.Harvester.TeardownAll(ctx, inst.Name, ns, inst.Status.Resources); err != nil {
		// Surface the failure on the CR and requeue. The finalizer stays
		// in place so a partial cleanup can't leave the CR garbage-collected
		// with live Harvester children behind it.
		inst.Status.Message = fmt.Sprintf("Teardown failed, will retry: %v", err)
		_ = r.statusUpdate(ctx, inst)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}
	// The tenant namespace is owned by the cluster operator (created during
	// onboarding) — never delete it. We only remove the resources we created.

	controllerutil.RemoveFinalizer(inst, dbaasv1.FinalizerName)
	return ctrl.Result{}, r.Update(ctx, inst)
}

// ============================================================
// Helpers
// ============================================================

func (r *DBInstanceReconciler) advance(ctx context.Context, inst *dbaasv1.DBInstance) (ctrl.Result, error) {
	return ctrl.Result{Requeue: true}, r.statusUpdate(ctx, inst)
}

func (r *DBInstanceReconciler) fail(ctx context.Context, inst *dbaasv1.DBInstance, reason string, err error) (ctrl.Result, error) {
	inst.Status.Phase = dbaasv1.StatusFailed
	inst.Status.ProvisioningPhase = dbaasv1.PhaseFailed
	inst.Status.Message = fmt.Sprintf("%s: %v", reason, err)
	_ = r.statusUpdate(ctx, inst)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, err
}

func (r *DBInstanceReconciler) statusUpdate(ctx context.Context, inst *dbaasv1.DBInstance) error {
	return r.Status().Update(ctx, inst)
}

// specPort returns 5432 if port is 0, otherwise port.
func specPort(port int) int {
	if port == 0 {
		return defaultPort
	}
	return port
}

// SetupWithManager registers the reconciler with controller-runtime.
func (r *DBInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbaasv1.DBInstance{}).
		Named("dbinstance").
		Complete(r)
}
