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

package harvester

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	harvesterbuilder "github.com/harvester/harvester/pkg/builder"
	"github.com/harvester/harvester/pkg/util"
	dbaasv1 "github.com/wso2/open-cloud-datacenter/crds/dbaas/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	kubevirtv1 "kubevirt.io/api/core/v1"

	harvesterhciov1beta1 "github.com/harvester/harvester/pkg/apis/harvesterhci.io/v1beta1"
	harvesterclientset "github.com/harvester/harvester/pkg/generated/clientset/versioned"
	cdiclientset "kubevirt.io/client-go/containerizeddataimporter"
	kvclientset "kubevirt.io/client-go/kubevirt"
)

const (
	vmiPhaseRunning = "Running"
	// dataNetInterface is the VM's tenant-facing NIC, bridged onto the
	// Multus NAD from spec.networkRef. Tenant clients (psql / app pods
	// on the same VLAN) reach the DB through this interface; the
	// published status.endpoint.address is this interface's IP.
	dataNetInterface = "data-net"
)

// TypedClient manages Harvester resources through Harvester's generated
// clientset and standard Kubernetes typed clients.
type TypedClient struct {
	Clientset         harvesterclientset.Interface
	KubeClient        kubernetes.Interface
	KvClientset       kvclientset.Interface
	CdiClientset      cdiclientset.Interface
	GrafanaURL        string
	MgmtLogicalSwitch string
}

var _ ClientInterface = (*TypedClient)(nil)

func NewTypedClient(config *rest.Config, grafanaURL string) (*TypedClient, error) {
	clientset, err := harvesterclientset.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	kvClientset, err := kvclientset.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	cdiClient, err := cdiclientset.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return NewTypedClientWithClientsets(clientset, kubeClient, kvClientset, cdiClient, grafanaURL), nil
}

func NewTypedClientWithClientsets(clientset harvesterclientset.Interface, kubeClient kubernetes.Interface, kvClientset kvclientset.Interface, cdiClient cdiclientset.Interface, grafanaURL string) *TypedClient {
	return &TypedClient{Clientset: clientset, KubeClient: kubeClient, KvClientset: kvClientset, CdiClientset: cdiClient, GrafanaURL: grafanaURL}
}

func (c *TypedClient) ResizeDataVolume(ctx context.Context, ns, vmName, dvName string, newSizeGB int) error {
	newReq := resource.MustParse(fmt.Sprintf("%dGi", newSizeGB))
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		vm, err := c.Clientset.KubevirtV1().VirtualMachines(ns).Get(ctx, vmName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		pvcs, err := VolumeClaimTemplates(vm)
		if err != nil {
			return err
		}
		// Index into the slice rather than range-copy: the mutation below must
		// reach the element that gets re-marshalled, independent of whether
		// VolumeClaimTemplates returns pointers or values.
		found := false
		for i := range pvcs {
			if pvcs[i].Name != dvName {
				continue
			}
			found = true
			// Grow-only: Harvester expands the live PVC only when the annotation
			// request exceeds the current size and silently ignores anything <=
			// (vm_controller.go createPVCsFromAnnotation). Skip the write unless
			// we're actually growing, so we never rewrite an unchanged annotation
			// (needless VM update + self-triggered reconcile) nor leave the
			// annotation understating the real PVC. The caller rejects true shrinks.
			if cur, ok := pvcs[i].Spec.Resources.Requests[corev1.ResourceStorage]; ok && newReq.Cmp(cur) <= 0 {
				return nil
			}
			if pvcs[i].Spec.Resources.Requests == nil {
				pvcs[i].Spec.Resources.Requests = corev1.ResourceList{}
			}
			pvcs[i].Spec.Resources.Requests[corev1.ResourceStorage] = newReq
			break
		}
		if !found {
			return fmt.Errorf("data volume claim template %s not found on VM %s/%s", dvName, ns, vmName)
		}
		data, err := json.Marshal(pvcs)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", util.AnnotationVolumeClaimTemplates, err)
		}
		if vm.Annotations == nil {
			vm.Annotations = map[string]string{}
		}
		vm.Annotations[util.AnnotationVolumeClaimTemplates] = string(data)
		_, err = c.Clientset.KubevirtV1().VirtualMachines(ns).Update(ctx, vm, metav1.UpdateOptions{})
		return err
	})
}

func (c *TypedClient) CreatePostgresVM(ctx context.Context, p VMCreateParams) (vmName string, err error) {
	vmName = VMName(p.ID)

	image, err := c.ResolveVMImage(ctx, p.OSImage)
	if err != nil {
		return vmName, err
	}

	vm, err := c.buildPostgresVM(p, vmName, p.CloudInitSecretName,
		fmt.Sprintf("%s/%s", image.Namespace, image.Name), image.StorageClassName, true)
	if err != nil {
		return vmName, err
	}
	vm.OwnerReferences = ownerRefSlice(p.Owner)
	if _, e := c.Clientset.KubevirtV1().VirtualMachines(p.Namespace).Create(ctx, vm, metav1.CreateOptions{}); e != nil {
		err = ignoreAlreadyExists(e)
	}
	return vmName, err
}

// ownerRefSlice wraps an optional controller owner reference for ObjectMeta
// assignment (nil in → nil out, leaving OwnerReferences unset).
func ownerRefSlice(ref *metav1.OwnerReference) []metav1.OwnerReference {
	if ref == nil {
		return nil
	}
	return []metav1.OwnerReference{*ref}
}

func (c *TypedClient) GetVMIReadiness(ctx context.Context, ns, vmName string) (VMIReadiness, error) {
	vmi, err := c.Clientset.KubevirtV1().VirtualMachineInstances(ns).Get(ctx, vmName, metav1.GetOptions{})
	if err != nil {
		return VMIReadiness{}, err
	}

	readiness := VMIReadiness{
		Running: string(vmi.Status.Phase) == vmiPhaseRunning,
		VMIUID:  string(vmi.UID),
	}
	for _, iface := range vmi.Status.Interfaces {
		if iface.Name != dataNetInterface {
			continue
		}
		readiness.IP = iface.IP
		break
	}
	for _, cond := range vmi.Status.Conditions {
		switch cond.Type {
		case kubevirtv1.VirtualMachineInstanceReady:
			readiness.Ready = cond.Status == corev1.ConditionTrue
		case kubevirtv1.VirtualMachineInstanceAgentConnected:
			readiness.AgentConnected = cond.Status == corev1.ConditionTrue
		}
	}
	return readiness, nil
}

// To align behavior with kubevirt v1.1.1, we set runStrategy to Halted when stopping a VM.
// see harvester/pkg/api/vm/handler.go 142 for harvester version 1.7.1
func (c *TypedClient) StopVM(ctx context.Context, ns, vmName string) error {
	return c.updateVM(ctx, ns, vmName, func(vm *kubevirtv1.VirtualMachine) bool {
		runStrategy := kubevirtv1.RunStrategyHalted
		vm.Spec.RunStrategy = &runStrategy
		vm.Spec.Running = nil
		return true
	})
}

// StopVMForCrashLoop atomically halts the VM and records which VMI triggered
// the safety halt. The marker survives a controller crash before DBInstance
// status is persisted.
func (c *TypedClient) StopVMForCrashLoop(ctx context.Context, ns, vmName, haltedVMIUID string) error {
	if haltedVMIUID == "" {
		return fmt.Errorf("halted VMI UID must not be empty")
	}
	return c.updateVM(ctx, ns, vmName, func(vm *kubevirtv1.VirtualMachine) bool {
		runStrategy := kubevirtv1.RunStrategyHalted
		vm.Spec.RunStrategy = &runStrategy
		vm.Spec.Running = nil
		if vm.Annotations == nil {
			vm.Annotations = map[string]string{}
		}
		vm.Annotations[dbaasv1.AnnotationCrashLoopHaltedVMIUID] = haltedVMIUID
		return true
	})
}

// ClearCrashLoopHalt removes only the durable crash-loop marker. Recovery has
// already been initiated out-of-band, so this does not change VM power state.
func (c *TypedClient) ClearCrashLoopHalt(ctx context.Context, ns, vmName string) error {
	return c.updateVM(ctx, ns, vmName, func(vm *kubevirtv1.VirtualMachine) bool {
		if _, ok := vm.Annotations[dbaasv1.AnnotationCrashLoopHaltedVMIUID]; !ok {
			return false
		}
		delete(vm.Annotations, dbaasv1.AnnotationCrashLoopHaltedVMIUID)
		return true
	})
}

func (c *TypedClient) updateVM(ctx context.Context, ns, vmName string, mutate func(*kubevirtv1.VirtualMachine) bool) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		vm, err := c.Clientset.KubevirtV1().VirtualMachines(ns).Get(ctx, vmName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !mutate(vm) {
			return nil
		}
		_, err = c.Clientset.KubevirtV1().VirtualMachines(ns).Update(ctx, vm, metav1.UpdateOptions{})
		return err
	})
}

// see harvester/pkg/api/vm/handler.go 138 : harvester version 1.7.1
func (c *TypedClient) StartVM(ctx context.Context, ns, vmName string) error {
	return c.KvClientset.KubevirtV1().VirtualMachines(ns).Start(ctx, vmName, &kubevirtv1.StartOptions{})
}

// Perform a Cold Resize of a VM - Stopping the exisintg VM and starting back is the responsibility of the caller.
func (c *TypedClient) ResizeVM(ctx context.Context, ns, vmName string, cpuCores, memoryMB int) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		vm, err := c.Clientset.KubevirtV1().VirtualMachines(ns).Get(ctx, vmName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if vm.Spec.Template.Spec.Domain.CPU == nil {
			vm.Spec.Template.Spec.Domain.CPU = &kubevirtv1.CPU{} // lazy init to avoid nil dereference
		}
		vm.Spec.Template.Spec.Domain.CPU.Cores = uint32(cpuCores)
		if vm.Spec.Template.Spec.Domain.Resources.Limits == nil {
			vm.Spec.Template.Spec.Domain.Resources.Limits = corev1.ResourceList{}
		}
		vm.Spec.Template.Spec.Domain.Resources.Limits[corev1.ResourceCPU] = *resource.NewQuantity(int64(cpuCores), resource.DecimalSI)
		// Memory: set limits only — the Harvester mutating webhook derives domain.memory.guest
		// from resources.limits[memory] on every VM update (pkg/webhook/.../mutator.go).
		vm.Spec.Template.Spec.Domain.Resources.Limits[corev1.ResourceMemory] = resource.MustParse(fmt.Sprintf("%dMi", memoryMB))
		_, err = c.Clientset.KubevirtV1().VirtualMachines(ns).Update(ctx, vm, metav1.UpdateOptions{})
		return err
	})
}

// Deploy the prometheus monitoring stack. Discussion : Harvester already have Prometheus operator, what to do ?
func (c *TypedClient) TeardownAll(ctx context.Context, id, ns string, refs dbaasv1.ResourceRefs) error {
	// Disk PVCs are created by Harvester from the VM's volumeClaimTemplates
	// annotation with no ownerReferences, so deleting the VM does NOT delete
	// them — they must be deleted explicitly here or they orphan (and a
	// later same-named DBInstance silently reattaches the old OS disk).
	// Collect their names from the live VM first; if the VM is already gone,
	// fall back to the naming conventions.
	diskPVCs := c.collectDiskPVCNames(ctx, id, ns, refs)

	type deleteTask struct {
		resource string
		name     string
		delete   func() error
	}
	tasks := []deleteTask{
		{"servicemonitors", refs.ServiceMonitor, func() error {
			return c.Clientset.MonitoringV1().ServiceMonitors(ns).Delete(ctx, refs.ServiceMonitor, metav1.DeleteOptions{})
		}},
		{"endpoints", refs.MetricsServiceName, func() error {
			return c.KubeClient.CoreV1().Endpoints(ns).Delete(ctx, refs.MetricsServiceName, metav1.DeleteOptions{})
		}},
		{"services", refs.MetricsServiceName, func() error {
			return c.KubeClient.CoreV1().Services(ns).Delete(ctx, refs.MetricsServiceName, metav1.DeleteOptions{})
		}},
		{"virtualmachines", refs.VMName, func() error {
			return c.Clientset.KubevirtV1().VirtualMachines(ns).Delete(ctx, refs.VMName, metav1.DeleteOptions{})
		}},
		{"secrets", refs.AdminCredentialsSecretName, func() error {
			return c.KubeClient.CoreV1().Secrets(ns).Delete(ctx, refs.AdminCredentialsSecretName, metav1.DeleteOptions{})
		}},
		{"secrets", refs.ConnectionSecretName, func() error {
			return c.KubeClient.CoreV1().Secrets(ns).Delete(ctx, refs.ConnectionSecretName, metav1.DeleteOptions{})
		}},
		{"secrets", refs.CloudInitSecretName, func() error {
			return c.KubeClient.CoreV1().Secrets(ns).Delete(ctx, refs.CloudInitSecretName, metav1.DeleteOptions{})
		}},
	}
	for _, pvcName := range diskPVCs {
		name := pvcName
		// Stray DataVolume first: the buggy DVT-based repave may have left a DV
		// that adopted the PVC; deleting the DV releases/cascades it, and the
		// direct PVC delete below covers the ownerless case. Both ignore NotFound.
		tasks = append(tasks,
			deleteTask{"datavolumes", name, func() error {
				return c.DeleteDataVolume(ctx, ns, name)
			}},
			deleteTask{"persistentvolumeclaims", name, func() error {
				return c.DeletePVC(ctx, ns, name)
			}},
		)
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []string
	)
	for _, t := range tasks {
		if t.name == "" {
			continue
		}
		wg.Add(1)
		go func(dt deleteTask) {
			defer wg.Done()
			err := dt.delete()
			if err == nil || apierrors.IsNotFound(err) {
				return // successful deletion or already gone
			}
			mu.Lock()
			errs = append(errs, fmt.Sprintf("%s/%s: %v", dt.resource, dt.name, err))
			mu.Unlock()
		}(t)
	}
	wg.Wait()
	if len(errs) > 0 {
		return fmt.Errorf("teardown: %s", strings.Join(errs, "; "))
	}
	return nil
}

// ResolveVMImage resolves a name or display name and verifies that the image is
// imported and has the storage class required to clone it.
func (c *TypedClient) ResolveVMImage(ctx context.Context, ref string) (ResolvedVMImage, error) {
	if ref == "" {
		return ResolvedVMImage{}, fmt.Errorf("%w: reference is empty", ErrVMImageReferenceInvalid)
	}

	ns, spec := "default", ref
	if i := strings.Index(ref, "/"); i > 0 {
		ns, spec = ref[:i], ref[i+1:]
	}
	if spec == "" {
		return ResolvedVMImage{}, fmt.Errorf("%w: empty image name in reference %q", ErrVMImageReferenceInvalid, ref)
	}

	img, e := c.Clientset.HarvesterhciV1beta1().VirtualMachineImages(ns).Get(ctx, spec, metav1.GetOptions{})
	if e == nil {
		return readyVMImageFields(ns, spec, img)
	}
	if !apierrors.IsNotFound(e) {
		return ResolvedVMImage{}, e
	}

	// fallback: search by displayName
	list, e := c.Clientset.HarvesterhciV1beta1().VirtualMachineImages(ns).List(ctx, metav1.ListOptions{})
	if e != nil {
		return ResolvedVMImage{}, e
	}

	var matched []harvesterhciov1beta1.VirtualMachineImage
	for _, item := range list.Items {
		if item.Spec.DisplayName == spec {
			matched = append(matched, item)
		}
	}

	switch len(matched) {
	case 0:
		return ResolvedVMImage{}, fmt.Errorf("%w: no VirtualMachineImage in namespace %q matching name or displayName %q", ErrVMImageNotFound, ns, spec)
	case 1:
		return readyVMImageFields(ns, matched[0].Name, &matched[0])
	default:
		return ResolvedVMImage{}, fmt.Errorf("%w: %d VirtualMachineImages in namespace %q share displayName %q", ErrVMImageAmbiguous, len(matched), ns, spec)
	}
}

func readyVMImageFields(ns, name string, img *harvesterhciov1beta1.VirtualMachineImage) (ResolvedVMImage, error) {
	if !isVMImageImported(img) {
		return ResolvedVMImage{}, fmt.Errorf("%w: VirtualMachineImage %s/%s is not imported yet (status.conditions missing ImageImported=True)", ErrVMImageNotReady, ns, name)
	}
	sc, err := resolveImageStorageClassName(img)
	if err != nil {
		return ResolvedVMImage{}, err
	}
	return ResolvedVMImage{Namespace: ns, Name: name, StorageClassName: sc}, nil
}

func isVMImageImported(image *harvesterhciov1beta1.VirtualMachineImage) bool {
	if image == nil {
		return false
	}
	return harvesterhciov1beta1.ImageImported.IsTrue(image)
}

func resolveImageStorageClassName(image *harvesterhciov1beta1.VirtualMachineImage) (string, error) {
	if image == nil {
		return "", fmt.Errorf("nil image")
	}
	if image.Status.StorageClassName != "" {
		return image.Status.StorageClassName, nil
	}
	return "", fmt.Errorf("%w: VM image %s/%s does not have a StorageClass yet (not initialized)",
		ErrVMImageNotReady, image.Namespace, image.Name)
}

// VMName is the deterministic name of a DBInstance's VirtualMachine.
func VMName(id string) string {
	return fmt.Sprintf("pg-%s", id)
}

// DataVolumeName is the deterministic name of a DBInstance's data-disk PVC.
// It's a pure naming convention, not a provider call: the PVC itself is
// created later by Harvester from the VM's harvesterhci.io/volumeClaimTemplates
// annotation (see buildPostgresVM below), so there's nothing to reserve up front.
func DataVolumeName(id string) string {
	return fmt.Sprintf("pg-%s-data", id)
}

func (c *TypedClient) buildPostgresVM(p VMCreateParams, vmName, cloudInitSecretName, imageID, imageSC string, running bool) (*kubevirtv1.VirtualMachine, error) {
	annotations := map[string]string{}
	if c.MgmtLogicalSwitch != "" {
		annotations["ovn.kubernetes.io/logical_switch"] = c.MgmtLogicalSwitch
	}

	runStrategy := kubevirtv1.RunStrategyHalted
	if running {
		runStrategy = kubevirtv1.RunStrategyAlways
	}

	labels := map[string]string{dbaasv1.LabelInstance: p.ID, dbaasv1.LabelRole: "primary"}
	templateLabels := map[string]string{dbaasv1.LabelInstance: p.ID}
	osPVCName := fmt.Sprintf("pg-%s-os", p.ID)
	dataPVCName := p.DataVolumeRef
	if dataPVCName == "" {
		dataPVCName = DataVolumeName(p.ID)
	}
	dataSizeGB := p.DataVolumeSizeGB
	if dataSizeGB <= 0 {
		dataSizeGB = 1
	}
	dataStorageClass := p.DataVolumeStorageClass
	if dataStorageClass == "" {
		dataStorageClass = "longhorn"
	}

	osPVCOption := &harvesterbuilder.PersistentVolumeClaimOption{
		ImageID:          imageID,
		VolumeMode:       corev1.PersistentVolumeBlock,
		AccessMode:       corev1.ReadWriteMany,
		StorageClassName: &imageSC,
	}
	dataPVCOption := &harvesterbuilder.PersistentVolumeClaimOption{
		VolumeMode:       corev1.PersistentVolumeBlock,
		AccessMode:       corev1.ReadWriteMany, // to allow live migration all disks should be ReadWriteMany
		StorageClassName: &dataStorageClass,
	}

	vmBuilder := harvesterbuilder.NewVMBuilder("dbaas-operator").
		Name(vmName).
		Namespace(p.Namespace).
		Labels(labels).
		VirtualMachineInstanceTemplateLabels(templateLabels).
		CPU(p.CPUCores).                         // set spec.template.spec.domain.resources.limits.cpu
		Memory(fmt.Sprintf("%dMi", p.MemoryMB)). // set spec.template.spec.domain.resources.limits.memory
		RunStrategy(runStrategy).
		PVCDisk("os-disk", harvesterbuilder.DiskBusVirtio, false, false, 1, "20Gi", osPVCName, osPVCOption).
		PVCDisk("pgdata-disk", harvesterbuilder.DiskBusVirtio, false, false, 0, fmt.Sprintf("%dGi", dataSizeGB), dataPVCName, dataPVCOption).
		CloudInitDisk("cloudinit", harvesterbuilder.DiskBusVirtio, false, 0, harvesterbuilder.CloudInitSource{
			CloudInitType:         harvesterbuilder.CloudInitTypeNoCloud,
			UserDataSecretName:    cloudInitSecretName,
			NetworkDataSecretName: cloudInitSecretName,
		}).
		NetworkInterface(dataNetInterface, string(kubevirtv1.VirtIO), "", harvesterbuilder.NetworkInterfaceTypeBridge, typedVMNetworkName(p.Namespace, p.NADName))

	vm, err := vmBuilder.VM()
	if err != nil {
		return nil, fmt.Errorf("build VM with Harvester builder helpers: %w", err)
	}
	// Post build fixes
	vm.TypeMeta = metav1.TypeMeta{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachine"}
	vm.Spec.Template.ObjectMeta.Annotations = mergeStringMap(vm.Spec.Template.ObjectMeta.Annotations, annotations) // VMI/launcher-pod annotations (e.g. Kube-OVN logical switch)
	// VM-object annotations read by Harvester's control plane (webhook + VM controller).
	// AnnotationRunStrategy: Harvester's patchRunStrategy webhook reads this on every
	// Halted→non-Halted transition and patches spec.runStrategy to match. Setting it to
	// Always here ensures the webhook confirms our intent instead of overriding to RerunOnFailure.
	if vm.Annotations == nil {
		vm.Annotations = map[string]string{}
	}
	vm.Annotations[util.AnnotationRunStrategy] = string(kubevirtv1.RunStrategyAlways)
	vm.Spec.Template.Spec.Domain.CPU.Sockets = 1
	vm.Spec.Template.Spec.Domain.CPU.Threads = 1

	// Readiness probe: pg_isready runs inside the guest via the QEMU guest agent
	// virtio channel — no pod-network port exposure required.
	vm.Spec.Template.Spec.ReadinessProbe = &kubevirtv1.Probe{
		Handler: kubevirtv1.Handler{
			Exec: &corev1.ExecAction{
				Command: []string{
					"/bin/sh", "-c",
					fmt.Sprintf("pg_isready -h 127.0.0.1 -p %d -U %s -d postgres", p.Port, p.MasterUser),
				},
			},
		},
		InitialDelaySeconds: 30,
		PeriodSeconds:       10,
		TimeoutSeconds:      5,
		// SuccessThreshold=3: require 3 consecutive passes (~30s) before
		// declaring Ready again — recovery-side hysteresis so a single lucky
		// probe doesn't flap the condition back to healthy.
		SuccessThreshold: 3,
		// FailureThreshold=12 @ PeriodSeconds=10 ≈ 2 min of sustained failure
		// before Ready flips False. This probe is the single debounce for
		// database liveness: the controller treats the resulting Ready condition
		// as authoritative and does no further counting (see ensureDatabaseHealth).
		// A guest-agent disconnect also trips it, since the probe execs pg_isready
		// in-guest via the agent.
		FailureThreshold: 12,
	}

	// on Kube-OVN/VPC networking, the default DNS inherited through KubeVirt/launcher behavior can be wrong for VM bootstrapping.
	// If DNS is wrong, cloud-init may fail during apt install postgresql.. This block forces the VM path to use the intended per-VPC DNS server.
	if p.DNSServerIP != "" { // Only set when Kube-OVN/VPC is used
		dnsIP := p.DNSServerIP
		if i := strings.Index(dnsIP, "/"); i > 0 {
			dnsIP = dnsIP[:i]
		}
		vm.Spec.Template.Spec.DNSPolicy = corev1.DNSNone // to opt out of inheriting cluster DNS in Kube-OVN setup
		vm.Spec.Template.Spec.DNSConfig = &corev1.PodDNSConfig{Nameservers: []string{dnsIP}}
	}

	return vm, nil
}

func typedVMNetworkName(namespace, nadName string) string {
	if strings.Contains(nadName, "/") {
		return nadName
	}
	return fmt.Sprintf("%s/%s", namespace, nadName)
}

// VolumeClaimTemplates parses the VM's volumeClaimTemplates annotation
// into PVC templates. It returns pointers so callers can mutate entries in
// place, but callers should still index the slice (pvcs[i]) rather than
// range-copy so the mutation stays correct if this ever returns values.
func VolumeClaimTemplates(vm *kubevirtv1.VirtualMachine) ([]*corev1.PersistentVolumeClaim, error) {
	raw := vm.Annotations[util.AnnotationVolumeClaimTemplates]
	if raw == "" {
		return nil, fmt.Errorf("VM %s/%s has no %s annotation", vm.Namespace, vm.Name, util.AnnotationVolumeClaimTemplates)
	}
	var pvcs []*corev1.PersistentVolumeClaim
	if err := json.Unmarshal([]byte(raw), &pvcs); err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", util.AnnotationVolumeClaimTemplates, err)
	}
	return pvcs, nil
}

func mergeStringMap(base map[string]string, overlay map[string]string) map[string]string {
	if len(base) == 0 && len(overlay) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		out[k] = v
	}
	return out
}

// ignoreAlreadyExists returns nil if err is an AlreadyExists API error, otherwise err.
func ignoreAlreadyExists(err error) error {
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func ptr[T any](v T) *T {
	return &v
}
