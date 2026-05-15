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
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dbaasv1 "github.com/wso2/open-cloud-datacenter/crds/dbaas/api/v1alpha1"
	"github.com/wso2/open-cloud-datacenter/crds/dbaas/internal/harvester"
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
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=create;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=create

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
		storageType = "longhorn"
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
		masterUser = "dbadmin"
	}
	dbName := inst.Spec.DBName
	if dbName == "" {
		dbName = id
	}
	osImage := inst.Spec.OSImage
	if osImage == "" {
		osImage = "ubuntu-22.04-server-cloudimg-amd64.img"
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
	inst.Status.ProvisioningPhase = dbaasv1.PhaseVMCreated
	inst.Status.Message = "VM created, waiting for PostgreSQL to initialize"

	return ctrl.Result{RequeueAfter: 10 * time.Second}, r.statusUpdate(ctx, inst)
}

func (r *DBInstanceReconciler) phaseWaitReady(ctx context.Context, inst *dbaasv1.DBInstance) (ctrl.Result, error) {
	ns := inst.Namespace

	readiness, err := r.Harvester.GetVMIReadiness(ctx, ns, inst.Status.Resources.VMName)
	if err != nil || !readiness.Running || readiness.IP == "" {
		inst.Status.Message = "Waiting for VM to become ready"
		inst.Status.ProvisioningPhase = dbaasv1.PhaseWaitingForCloudInit
		_ = r.statusUpdate(ctx, inst)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if !readiness.Ready {
		inst.Status.Message = fmt.Sprintf("VM running at %s, waiting for PostgreSQL to finish initializing", readiness.IP)
		_ = r.statusUpdate(ctx, inst)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

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

	smName, grafanaURL, promTarget, err := r.Harvester.DeployMonitoring(ctx, id, ns, inst.Status.Endpoint.Address, inst.Status.Endpoint.Port)
	if err != nil {
		// Non-fatal — DB works without monitoring
		log.FromContext(ctx).Error(err, "monitoring setup failed (non-fatal)")
		inst.Status.Message = "Available (monitoring setup failed, will retry)"
	} else {
		inst.Status.Resources.ServiceMonitor = smName
		inst.Status.GrafanaURL = grafanaURL
		inst.Status.PrometheusTarget = promTarget
	}

	inst.Status.ProvisioningPhase = dbaasv1.PhaseMonitoringDeployed
	return r.advance(ctx, inst)
}

func (r *DBInstanceReconciler) phaseAvailable(ctx context.Context, inst *dbaasv1.DBInstance) (ctrl.Result, error) {
	inst.Status.Phase = dbaasv1.StatusAvailable
	inst.Status.ProvisioningPhase = dbaasv1.PhaseAvailable
	inst.Status.ObservedGeneration = inst.Generation
	inst.Status.Message = "Database instance is available"

	// Re-check the vpc-net IP on every requeue — the guest agent may report it
	// later than initial readiness, or it can change after a VM restart.
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

	return ctrl.Result{RequeueAfter: 60 * time.Second}, r.statusUpdate(ctx, inst)
}

// ============================================================
// Stop / Start / Modify / Delete
// ============================================================

func (r *DBInstanceReconciler) reconcileStop(ctx context.Context, inst *dbaasv1.DBInstance) (ctrl.Result, error) {
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
	inst.Status.Message = "Modifications applied"
	inst.Status.ObservedGeneration = inst.Generation
	return ctrl.Result{}, r.statusUpdate(ctx, inst)
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
	r.Harvester.TeardownAll(ctx, inst.Name, ns, inst.Status.Resources)
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
		return 5432
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
