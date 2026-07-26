package harvester

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	harvesterhciov1beta1 "github.com/harvester/harvester/pkg/apis/harvesterhci.io/v1beta1"
	harvesterfake "github.com/harvester/harvester/pkg/generated/clientset/versioned/fake"
	harvesterutil "github.com/harvester/harvester/pkg/util"
	dbaasv1 "github.com/wso2/open-cloud-datacenter/crds/dbaas/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	kubevirtv1 "kubevirt.io/api/core/v1"
	cdifake "kubevirt.io/client-go/containerizeddataimporter/fake"
	kvfake "kubevirt.io/client-go/kubevirt/fake"
)

// mgmtNetInterface is the legacy dynamic-client's dial-probe NIC name
// (removed in PR9 along with DialVMListener) — kept here only so the tests
// below can assert it never appears on a TypedClient-built VM.
const mgmtNetInterface = "mgmt-net"

func testVMCreateParams() VMCreateParams {
	return VMCreateParams{
		ID:                     "orders",
		Namespace:              "tenant-a",
		CPUCores:               2,
		MemoryMB:               4096,
		OSImage:                "ubuntu-22.04",
		DataVolumeRef:          "pg-orders-data",
		DataVolumeSizeGB:       20,
		DataVolumeStorageClass: "harvester-longhorn",
		NADName:                "tenant-a/vm-network",
		MasterUser:             "dbadmin",
		Port:                   5432,
		CloudInitSecretName:    "pg-orders-cloudinit",
	}
}

func TestTypedCreatePostgresVMFailsOnImageResolutionFailure(t *testing.T) {
	ctx := context.Background()
	client := newTestTypedClient()

	vmName, err := client.CreatePostgresVM(ctx, testVMCreateParams())
	if err == nil {
		t.Fatalf("CreatePostgresVM returned nil error, want image resolution error")
	}
	if _, err := client.Clientset.KubevirtV1().VirtualMachines("tenant-a").Get(ctx, vmName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("VM should not exist after image-resolution failure, got: %v", err)
	}
}

func TestResolveVMImage(t *testing.T) {
	ctx := context.Background()
	image := testTypedVMImage()
	client := newTestTypedClient(image)

	resolved, err := client.ResolveVMImage(ctx, image.Spec.DisplayName)
	if err != nil {
		t.Fatalf("ResolveVMImage returned error: %v", err)
	}
	if resolved.Namespace != "default" || resolved.Name != image.Name ||
		resolved.StorageClassName != image.Status.StorageClassName {
		t.Fatalf("resolved image = %+v", resolved)
	}
}

func TestResolveVMImageClassifiesSemanticFailures(t *testing.T) {
	ctx := context.Background()
	notReady := testTypedVMImage()
	notReady.Status.Conditions = nil
	first := testTypedVMImage()
	first.Name = "ubuntu-a"
	first.Spec.DisplayName = "duplicate"
	second := first.DeepCopy()
	second.Name = "ubuntu-b"

	tests := []struct {
		name   string
		client *TypedClient
		ref    string
		want   error
	}{
		{"empty reference", newTestTypedClient(), "", ErrVMImageReferenceInvalid},
		{"not found", newTestTypedClient(), "missing", ErrVMImageNotFound},
		{"not ready", newTestTypedClient(notReady), notReady.Name, ErrVMImageNotReady},
		{"ambiguous display name", newTestTypedClient(first, second), "duplicate", ErrVMImageAmbiguous},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.client.ResolveVMImage(ctx, tt.ref)
			if !errors.Is(err, tt.want) {
				t.Fatalf("ResolveVMImage error = %v, want errors.Is(%v)", err, tt.want)
			}
		})
	}
}

func TestDataVolumeNameMatchesPVCTemplateAndResizeUpdatesVMTemplate(t *testing.T) {
	ctx := context.Background()
	client := newTestTypedClient(testTypedVMImage())

	dvName := DataVolumeName("orders")
	if dvName != "pg-orders-data" {
		t.Fatalf("DataVolume name = %q, want pg-orders-data", dvName)
	}
	params := testVMCreateParams()
	params.DataVolumeRef = dvName
	if _, err := client.CreatePostgresVM(ctx, params); err != nil {
		t.Fatalf("CreatePostgresVM returned error: %v", err)
	}
	vm, err := client.Clientset.KubevirtV1().VirtualMachines("tenant-a").Get(ctx, "pg-orders", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get VM: %v", err)
	}
	templates, err := VolumeClaimTemplates(vm)
	if err != nil {
		t.Fatalf("volume claim templates: %v", err)
	}
	dataTemplate := findPVCTemplate(templates, dvName)
	if dataTemplate == nil {
		t.Fatalf("data PVC template %s not found", dvName)
	}
	storage := dataTemplate.Spec.Resources.Requests[corev1.ResourceStorage]
	if got := storage.String(); got != "20Gi" {
		t.Fatalf("Data PVC template storage = %q, want 20Gi", got)
	}

	if err := client.ResizeDataVolume(ctx, "tenant-a", "pg-orders", dvName, 30); err != nil {
		t.Fatalf("ResizeDataVolume returned error: %v", err)
	}
	vm, err = client.Clientset.KubevirtV1().VirtualMachines("tenant-a").Get(ctx, "pg-orders", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get resized VM: %v", err)
	}
	templates, err = VolumeClaimTemplates(vm)
	if err != nil {
		t.Fatalf("resized volume claim templates: %v", err)
	}
	dataTemplate = findPVCTemplate(templates, dvName)
	if dataTemplate == nil {
		t.Fatalf("data PVC template %s not found after resize", dvName)
	}
	storage = dataTemplate.Spec.Resources.Requests[corev1.ResourceStorage]
	if got := storage.String(); got != "30Gi" {
		t.Fatalf("Data PVC template storage after resize = %q, want 30Gi", got)
	}
}

// CreatePostgresVM no longer generates credentials/cloud-init (PR8 — that
// moved to internal/credentials + internal/resource; the reuse-on-reentry
// invariant is now tested there). It only builds the VM against an
// already-provisioned cloud-init Secret name.
func TestTypedCreatePostgresVMUsesSuppliedCloudInitSecret(t *testing.T) {
	ctx := context.Background()
	client := newTestTypedClient(testTypedVMImage())
	params := testVMCreateParams()

	vmName, err := client.CreatePostgresVM(ctx, params)
	if err != nil {
		t.Fatalf("CreatePostgresVM returned error: %v", err)
	}
	if vmName != "pg-orders" {
		t.Fatalf("VM name = %q, want pg-orders", vmName)
	}

	vm, err := client.Clientset.KubevirtV1().VirtualMachines("tenant-a").Get(ctx, vmName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get created VM: %v", err)
	}
	found := false
	for _, v := range vm.Spec.Template.Spec.Volumes {
		if v.Name != "cloudinit" || v.CloudInitNoCloud == nil {
			continue
		}
		found = true
		if v.CloudInitNoCloud.UserDataSecretRef == nil || v.CloudInitNoCloud.UserDataSecretRef.Name != params.CloudInitSecretName {
			t.Fatalf("UserDataSecretRef = %+v, want %q", v.CloudInitNoCloud.UserDataSecretRef, params.CloudInitSecretName)
		}
		if v.CloudInitNoCloud.NetworkDataSecretRef == nil || v.CloudInitNoCloud.NetworkDataSecretRef.Name != params.CloudInitSecretName {
			t.Fatalf("NetworkDataSecretRef = %+v, want %q", v.CloudInitNoCloud.NetworkDataSecretRef, params.CloudInitSecretName)
		}
	}
	if !found {
		t.Fatal("cloudinit volume not found on VM")
	}
}

func TestTypedCreatePostgresVMPreservesVMShape(t *testing.T) {
	ctx := context.Background()
	client := newTestTypedClient(testTypedVMImage())
	client.MgmtLogicalSwitch = "ovn-default"
	params := testVMCreateParams()
	params.DNSServerIP = "10.96.0.10/32"

	vmName, err := client.CreatePostgresVM(ctx, params)
	if err != nil {
		t.Fatalf("CreatePostgresVM returned error: %v", err)
	}
	cloudInitSecretName := params.CloudInitSecretName
	vm, err := client.Clientset.KubevirtV1().VirtualMachines("tenant-a").Get(ctx, vmName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get created VM: %v", err)
	}

	if vm.Spec.Template.ObjectMeta.Annotations["ovn.kubernetes.io/logical_switch"] != "ovn-default" {
		t.Fatalf("logical switch annotation = %q, want ovn-default", vm.Spec.Template.ObjectMeta.Annotations["ovn.kubernetes.io/logical_switch"])
	}
	if vm.Spec.Template.Spec.DNSPolicy != corev1.DNSNone {
		t.Fatalf("dnsPolicy = %q, want None", vm.Spec.Template.Spec.DNSPolicy)
	}
	if vm.Spec.Template.Spec.Domain.Memory != nil && vm.Spec.Template.Spec.Domain.Memory.Guest != nil {
		t.Fatalf("memory.guest is set before Harvester admission: %s", vm.Spec.Template.Spec.Domain.Memory.Guest.String())
	}
	memoryLimit := vm.Spec.Template.Spec.Domain.Resources.Limits[corev1.ResourceMemory]
	if got := memoryLimit.String(); got != "4Gi" {
		t.Fatalf("memory limit = %q, want 4Gi", got)
	}
	if got := vm.Spec.Template.Spec.DNSConfig.Nameservers; len(got) != 1 || got[0] != "10.96.0.10" {
		t.Fatalf("nameservers = %v, want [10.96.0.10]", got)
	}
	if got := len(vm.Spec.DataVolumeTemplates); got != 0 {
		t.Fatalf("dataVolumeTemplates = %d, want 0 for Harvester-native volumeClaimTemplates path", got)
	}
	templates, err := VolumeClaimTemplates(vm)
	if err != nil {
		t.Fatalf("volume claim templates: %v", err)
	}
	if got := len(templates); got != 2 {
		t.Fatalf("volume claim template count = %d, want 2", got)
	}
	osTemplate := findPVCTemplate(templates, "pg-orders-os")
	if osTemplate == nil {
		t.Fatalf("OS PVC template not found")
	}
	if got := osTemplate.Annotations["harvesterhci.io/imageId"]; got != "default/ubuntu-2204-postgres-v20260615" {
		t.Fatalf("OS image annotation = %q, want default/ubuntu-2204-postgres-v20260615", got)
	}
	dataTemplate := findPVCTemplate(templates, "pg-orders-data")
	if dataTemplate == nil {
		t.Fatalf("data PVC template not found")
	}
	if got := dataTemplate.Annotations["harvesterhci.io/imageId"]; got != "" {
		t.Fatalf("data PVC image annotation = %q, want empty", got)
	}
	if dataTemplate.Spec.StorageClassName == nil || *dataTemplate.Spec.StorageClassName != "harvester-longhorn" {
		t.Fatalf("data PVC storageClass = %#v, want harvester-longhorn", dataTemplate.Spec.StorageClassName)
	}
	if !vmVolumeUsesPVC(vm, "os-disk", "pg-orders-os") {
		t.Fatalf("os-disk volume does not use PVC pg-orders-os")
	}
	if !vmVolumeUsesPVC(vm, "pgdata-disk", "pg-orders-data") {
		t.Fatalf("pgdata-disk volume does not use PVC pg-orders-data")
	}
	// The VM must have only the data-net interface — mgmt-net (masquerade)
	// is removed; the readiness probe uses the QGA virtio channel instead.
	if vmHasInterface(vm, mgmtNetInterface) {
		t.Fatalf("mgmt-net interface must not be attached to the VM")
	}

	// Readiness probe must be configured as an exec probe via the guest agent.
	probe := vm.Spec.Template.Spec.ReadinessProbe
	if probe == nil {
		t.Fatalf("ReadinessProbe is not set")
	}
	if probe.Exec == nil {
		t.Fatalf("ReadinessProbe.Exec is not set")
	}
	if !strings.Contains(strings.Join(probe.Exec.Command, " "), "pg_isready") {
		t.Fatalf("ReadinessProbe command does not contain pg_isready: %v", probe.Exec.Command)
	}
	if probe.InitialDelaySeconds != 30 || probe.PeriodSeconds != 10 || probe.FailureThreshold != 12 {
		t.Fatalf("ReadinessProbe timing initial=%d period=%d failure=%d, want 30/10/12",
			probe.InitialDelaySeconds, probe.PeriodSeconds, probe.FailureThreshold)
	}

	raw, err := json.Marshal(vm)
	if err != nil {
		t.Fatalf("marshal VM: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `"secretRef":{"name":"`+cloudInitSecretName+`"}`) {
		t.Fatalf("VM JSON does not contain cloud-init secretRef: %s", body)
	}
	if !strings.Contains(body, `"networkDataSecretRef":{"name":"`+cloudInitSecretName+`"}`) {
		t.Fatalf("VM JSON does not contain cloud-init networkDataSecretRef: %s", body)
	}

	// RunStrategy must be Always so the VMI restarts after any exit (clean or crash).
	// RerunOnFailure would leave the VM permanently stopped after a clean guest shutdown,
	// which can happen during cloud-init or after a cold resize restart.
	if vm.Spec.RunStrategy == nil || *vm.Spec.RunStrategy != kubevirtv1.RunStrategyAlways {
		t.Fatalf("RunStrategy = %v, want Always", vm.Spec.RunStrategy)
	}
	// AnnotationRunStrategy must match so Harvester's patchRunStrategy webhook confirms
	// Always on every Halted→non-Halted transition instead of overriding to RerunOnFailure.
	if got := vm.Annotations[harvesterutil.AnnotationRunStrategy]; got != string(kubevirtv1.RunStrategyAlways) {
		t.Fatalf("AnnotationRunStrategy = %q, want %q", got, string(kubevirtv1.RunStrategyAlways))
	}
}

func TestTypedGetVMIReadinessUsesOnlyDataNetIP(t *testing.T) {
	ctx := context.Background()
	client := newTestTypedClient(&kubevirtv1.VirtualMachineInstance{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachineInstance"},
		ObjectMeta: metav1.ObjectMeta{Name: "pg-orders", Namespace: "tenant-a"},
		Status: kubevirtv1.VirtualMachineInstanceStatus{
			Phase: kubevirtv1.Running,
			Interfaces: []kubevirtv1.VirtualMachineInstanceNetworkInterface{
				{Name: mgmtNetInterface, IP: "10.244.0.10"},
				{Name: dataNetInterface, IP: "192.168.40.50"},
			},
		},
	})

	readiness, err := client.GetVMIReadiness(ctx, "tenant-a", "pg-orders")
	if err != nil {
		t.Fatalf("GetVMIReadiness returned error: %v", err)
	}
	if !readiness.Running || readiness.IP != "192.168.40.50" {
		t.Fatalf("readiness = %+v, want running with data-net IP", readiness)
	}
}

func TestTypedGetVMIReadinessDoesNotFallbackToMgmtNet(t *testing.T) {
	ctx := context.Background()
	client := newTestTypedClient(&kubevirtv1.VirtualMachineInstance{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachineInstance"},
		ObjectMeta: metav1.ObjectMeta{Name: "pg-orders", Namespace: "tenant-a"},
		Status: kubevirtv1.VirtualMachineInstanceStatus{
			Phase:      kubevirtv1.Running,
			Interfaces: []kubevirtv1.VirtualMachineInstanceNetworkInterface{{Name: mgmtNetInterface, IP: "10.244.0.10"}},
		},
	})

	readiness, err := client.GetVMIReadiness(ctx, "tenant-a", "pg-orders")
	if err != nil {
		t.Fatalf("GetVMIReadiness returned error: %v", err)
	}
	if !readiness.Running || readiness.IP != "" {
		t.Fatalf("readiness = %+v, want running with no IP fallback", readiness)
	}
}

func TestTypedGetVMIReadinessSurfacesConditionsAndUID(t *testing.T) {
	ctx := context.Background()
	client := newTestTypedClient(&kubevirtv1.VirtualMachineInstance{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachineInstance"},
		ObjectMeta: metav1.ObjectMeta{Name: "pg-orders", Namespace: "tenant-a", UID: "abc-123"},
		Status: kubevirtv1.VirtualMachineInstanceStatus{
			Phase: kubevirtv1.Running,
			Interfaces: []kubevirtv1.VirtualMachineInstanceNetworkInterface{
				{Name: dataNetInterface, IP: "192.168.40.50"},
			},
			Conditions: []kubevirtv1.VirtualMachineInstanceCondition{
				{Type: kubevirtv1.VirtualMachineInstanceReady, Status: corev1.ConditionTrue},
				{Type: kubevirtv1.VirtualMachineInstanceAgentConnected, Status: corev1.ConditionTrue},
			},
		},
	})

	r, err := client.GetVMIReadiness(ctx, "tenant-a", "pg-orders")
	if err != nil {
		t.Fatalf("GetVMIReadiness returned error: %v", err)
	}
	if !r.Running {
		t.Fatalf("Running = false, want true")
	}
	if r.IP != "192.168.40.50" {
		t.Fatalf("IP = %q, want 192.168.40.50", r.IP)
	}
	if !r.Ready {
		t.Fatalf("Ready = false, want true")
	}
	if !r.AgentConnected {
		t.Fatalf("AgentConnected = false, want true")
	}
	if r.VMIUID != "abc-123" {
		t.Fatalf("VMIUID = %q, want abc-123", r.VMIUID)
	}
}

func TestTypedGetVMIReadinessConditionsDefaultToFalseWhenAbsent(t *testing.T) {
	ctx := context.Background()
	client := newTestTypedClient(&kubevirtv1.VirtualMachineInstance{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachineInstance"},
		ObjectMeta: metav1.ObjectMeta{Name: "pg-orders", Namespace: "tenant-a", UID: "xyz-456"},
		Status: kubevirtv1.VirtualMachineInstanceStatus{
			Phase: kubevirtv1.Running,
			// No conditions set — VMI booting, probes not yet evaluated.
		},
	})

	r, err := client.GetVMIReadiness(ctx, "tenant-a", "pg-orders")
	if err != nil {
		t.Fatalf("GetVMIReadiness returned error: %v", err)
	}
	if r.Ready || r.AgentConnected {
		t.Fatalf("Ready=%v AgentConnected=%v, want both false when conditions absent", r.Ready, r.AgentConnected)
	}
	if r.VMIUID != "xyz-456" {
		t.Fatalf("VMIUID = %q, want xyz-456", r.VMIUID)
	}
}

func TestTypedStartStopAndResizeVM(t *testing.T) {
	ctx := context.Background()
	runStrategy := kubevirtv1.RunStrategyRerunOnFailure
	client := newTestTypedClient(&kubevirtv1.VirtualMachine{
		TypeMeta: metav1.TypeMeta{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachine"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "pg-orders", Namespace: "tenant-a",
			Annotations: map[string]string{"existing": "preserved"},
		},
		Spec: kubevirtv1.VirtualMachineSpec{
			RunStrategy: &runStrategy,
			Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					Domain: kubevirtv1.DomainSpec{
						CPU:    &kubevirtv1.CPU{Cores: 2},
						Memory: &kubevirtv1.Memory{},
					},
				},
			},
		},
	})

	// StopVM: sets RunStrategy = Halted (spec mutation, matches Harvester's stop pattern)
	if err := client.StopVM(ctx, "tenant-a", "pg-orders"); err != nil {
		t.Fatalf("StopVM returned error: %v", err)
	}
	vm, _ := client.Clientset.KubevirtV1().VirtualMachines("tenant-a").Get(ctx, "pg-orders", metav1.GetOptions{})
	if vm.Spec.RunStrategy == nil || *vm.Spec.RunStrategy != kubevirtv1.RunStrategyHalted {
		t.Fatalf("RunStrategy after StopVM = %v, want Halted", vm.Spec.RunStrategy)
	}
	if vm.Spec.Running != nil {
		t.Fatalf("Running after StopVM = %v, want nil", vm.Spec.Running)
	}
	if _, ok := vm.Annotations[dbaasv1.AnnotationCrashLoopHaltedVMIUID]; ok {
		t.Fatal("plain StopVM must not add the crash-loop marker")
	}

	// Crash-loop stop records the halted VMI UID in the same VM update.
	if err := client.StopVMForCrashLoop(ctx, "tenant-a", "pg-orders", "vmi-halted"); err != nil {
		t.Fatalf("StopVMForCrashLoop returned error: %v", err)
	}
	vm, _ = client.Clientset.KubevirtV1().VirtualMachines("tenant-a").Get(ctx, "pg-orders", metav1.GetOptions{})
	if vm.Spec.RunStrategy == nil || *vm.Spec.RunStrategy != kubevirtv1.RunStrategyHalted {
		t.Fatalf("RunStrategy after StopVMForCrashLoop = %v, want Halted", vm.Spec.RunStrategy)
	}
	if got := vm.Annotations[dbaasv1.AnnotationCrashLoopHaltedVMIUID]; got != "vmi-halted" {
		t.Fatalf("crash-loop halted VMI UID = %q, want vmi-halted", got)
	}
	if vm.Annotations["existing"] != "preserved" {
		t.Fatal("StopVMForCrashLoop removed an unrelated annotation")
	}

	// Clearing the marker preserves power state and unrelated annotations.
	if err := client.ClearCrashLoopHalt(ctx, "tenant-a", "pg-orders"); err != nil {
		t.Fatalf("ClearCrashLoopHalt returned error: %v", err)
	}
	vm, _ = client.Clientset.KubevirtV1().VirtualMachines("tenant-a").Get(ctx, "pg-orders", metav1.GetOptions{})
	if _, ok := vm.Annotations[dbaasv1.AnnotationCrashLoopHaltedVMIUID]; ok {
		t.Fatal("ClearCrashLoopHalt did not remove the marker")
	}
	if vm.Annotations["existing"] != "preserved" {
		t.Fatal("ClearCrashLoopHalt removed an unrelated annotation")
	}
	if vm.Spec.RunStrategy == nil || *vm.Spec.RunStrategy != kubevirtv1.RunStrategyHalted {
		t.Fatalf("RunStrategy after ClearCrashLoopHalt = %v, want unchanged Halted", vm.Spec.RunStrategy)
	}
	if err := client.ClearCrashLoopHalt(ctx, "tenant-a", "pg-orders"); err != nil {
		t.Fatalf("second ClearCrashLoopHalt should be a no-op: %v", err)
	}
	if err := client.StopVMForCrashLoop(ctx, "tenant-a", "pg-orders", ""); err == nil {
		t.Fatal("StopVMForCrashLoop accepted an empty halted VMI UID")
	}

	// StartVM: calls the KubeVirt start subresource API (does not mutate spec)
	if err := client.StartVM(ctx, "tenant-a", "pg-orders"); err != nil {
		t.Fatalf("StartVM returned error: %v", err)
	}

	if err := client.ResizeVM(ctx, "tenant-a", "pg-orders", 4, 8192); err != nil {
		t.Fatalf("ResizeVM returned error: %v", err)
	}
	vm, _ = client.Clientset.KubevirtV1().VirtualMachines("tenant-a").Get(ctx, "pg-orders", metav1.GetOptions{})
	cpuLimit := vm.Spec.Template.Spec.Domain.Resources.Limits[corev1.ResourceCPU]
	memLimit := vm.Spec.Template.Spec.Domain.Resources.Limits[corev1.ResourceMemory]
	if vm.Spec.Template.Spec.Domain.CPU.Cores != 4 || cpuLimit.Cmp(*resource.NewQuantity(4, resource.DecimalSI)) != 0 || memLimit.Cmp(resource.MustParse("8192Mi")) != 0 {
		t.Fatalf("resized CPU/memory = cores:%d cpuLimit:%s memLimit:%s, want cores:4 cpuLimit:4 memLimit:8192Mi",
			vm.Spec.Template.Spec.Domain.CPU.Cores, cpuLimit.String(), memLimit.String())
	}
}

func TestTypedTeardownDeletesConnectionSecretAndIgnoresNotFound(t *testing.T) {
	ctx := context.Background()
	client := newTestTypedClient()
	if _, err := client.KubeClient.CoreV1().Secrets("tenant-a").Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-orders-connect", Namespace: "tenant-a"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create connection Secret: %v", err)
	}
	err := client.TeardownAll(ctx, "orders", "tenant-a", dbaasv1.ResourceRefs{
		VMName:                     "pg-orders",
		DataVolumeName:             "pg-orders-data",
		AdminCredentialsSecretName: "pg-orders-credentials",
		ConnectionSecretName:       "pg-orders-connect",
		CloudInitSecretName:        "pg-orders-cloudinit",
		MetricsServiceName:         "pg-orders-metrics",
		ServiceMonitor:             "pg-orders-monitor",
	})
	if err != nil {
		t.Fatalf("TeardownAll returned error for missing resources: %v", err)
	}
	if _, err := client.KubeClient.CoreV1().Secrets("tenant-a").Get(ctx, "pg-orders-connect", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("connection Secret still exists after TeardownAll: %v", err)
	}
}

func TestTypedPrepareCloudInitForRepaveRecreatesSecretAndReattachesDisk(t *testing.T) {
	ctx := context.Background()
	client := newTestTypedClient(testTypedVMImage())
	params := testVMCreateParams()

	vmName, credName, ciName, _, err := client.CreatePostgresVM(ctx, params)
	if err != nil {
		t.Fatalf("CreatePostgresVM returned error: %v", err)
	}
	if err := client.RemoveCloudInitDisk(ctx, params.Namespace, vmName); err != nil {
		t.Fatalf("RemoveCloudInitDisk returned error: %v", err)
	}
	if err := client.DeleteSecret(ctx, params.Namespace, ciName); err != nil {
		t.Fatalf("DeleteSecret returned error: %v", err)
	}

	if err := client.PrepareCloudInitForRepave(ctx, params, vmName, credName, ciName); err != nil {
		t.Fatalf("PrepareCloudInitForRepave returned error: %v", err)
	}

	secret, err := client.KubeClient.CoreV1().Secrets(params.Namespace).Get(ctx, ciName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("cloud-init Secret was not recreated: %v", err)
	}
	userdata := secret.StringData["userdata"]
	if userdata == "" {
		userdata = string(secret.Data["userdata"])
	}
	if !strings.Contains(userdata, "ENGINE_VERSION="+params.EngineVersion) {
		t.Fatalf("cloud-init userdata does not contain requested engine version: %q", userdata)
	}

	vm, err := client.Clientset.KubevirtV1().VirtualMachines(params.Namespace).Get(ctx, vmName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get VM after repave prep: %v", err)
	}
	hasDisk := false
	for _, disk := range vm.Spec.Template.Spec.Domain.Devices.Disks {
		hasDisk = hasDisk || disk.Name == "cloudinit"
	}
	if !hasDisk {
		t.Fatalf("cloudinit disk was not reattached")
	}
	for _, volume := range vm.Spec.Template.Spec.Volumes {
		if volume.Name != "cloudinit" {
			continue
		}
		source := volume.CloudInitNoCloud
		if source == nil || source.UserDataSecretRef == nil || source.UserDataSecretRef.Name != ciName ||
			source.NetworkDataSecretRef == nil || source.NetworkDataSecretRef.Name != ciName {
			t.Fatalf("cloudinit volume source = %#v, want secret refs to %q", source, ciName)
		}
		return
	}
	t.Fatalf("cloudinit volume was not reattached")
}

func TestTypedTeardownAggregatesDeleteErrors(t *testing.T) {
	ctx := context.Background()
	client := newTestTypedClient(&kubevirtv1.VirtualMachine{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachine"},
		ObjectMeta: metav1.ObjectMeta{Name: "pg-orders", Namespace: "tenant-a"},
	})
	client.Clientset.(*harvesterfake.Clientset).PrependReactor("delete", "virtualmachines", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, &apierrors.StatusError{ErrStatus: metav1.Status{Reason: metav1.StatusReasonForbidden, Message: "blocked"}}
	})

	err := client.TeardownAll(ctx, "orders", "tenant-a", dbaasv1.ResourceRefs{VMName: "pg-orders"})
	if err == nil {
		t.Fatalf("TeardownAll returned nil error, want aggregate")
	}
	if !strings.Contains(err.Error(), "virtualmachines/pg-orders") || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("aggregate error = %q", err.Error())
	}
}

// TestTypedCollectDiskPVCNamesDoesNotCollideAcrossInstances is a regression
// test for a real prefix-matching bug: instance "orders" and a separate,
// legitimately-named instance "orders-os" produce nested prefixes of each
// other ("pg-orders-os" is itself a prefix of "orders-os"'s own disk
// "pg-orders-os-os"). Tearing down "orders" via the old namespace-wide
// isOSDiskName scan would also match and delete "orders-os"'s disk. This
// must not happen once status.resources.osDiskPVCName is populated — that
// exact name should be used instead of any prefix scan.
func TestTypedCollectDiskPVCNamesDoesNotCollideAcrossInstances(t *testing.T) {
	ctx := context.Background()
	client := NewTypedClientWithClientsets(
		harvesterfake.NewSimpleClientset(), // no VM object: simulates a teardown retry after the VM is already gone
		kubefake.NewSimpleClientset(
			&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "pg-orders-os", Namespace: "tenant-a"}},
			&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "pg-orders-data", Namespace: "tenant-a"}},
			// Belongs to a DIFFERENT, coexisting instance named "orders-os" — must survive "orders"'s teardown.
			&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "pg-orders-os-os", Namespace: "tenant-a"}},
		),
		kvfake.NewSimpleClientset(),
		cdifake.NewSimpleClientset(),
		"",
	)

	refs := dbaasv1.ResourceRefs{
		VMName:         "pg-orders",
		OSDiskPVCName:  "pg-orders-os",
		DataVolumeName: "pg-orders-data",
	}
	names := client.collectDiskPVCNames(ctx, "orders", "tenant-a", refs)

	for _, foreign := range []string{"pg-orders-os-os"} {
		for _, n := range names {
			if n == foreign {
				t.Fatalf("collectDiskPVCNames(%v) = %v, must not include foreign instance's disk %q", refs, names, foreign)
			}
		}
	}
	want := map[string]bool{"pg-orders-os": true, "pg-orders-data": true}
	for _, n := range names {
		if !want[n] {
			t.Fatalf("collectDiskPVCNames(%v) = %v, unexpected entry %q", refs, names, n)
		}
		delete(want, n)
	}
	if len(want) > 0 {
		t.Fatalf("collectDiskPVCNames(%v) = %v, missing expected entries %v", refs, names, want)
	}
}

// TestTypedCollectDiskPVCNamesFallsBackForPreFixOrphans confirms the legacy
// scan still finds an orphaned OS disk when neither the live VM nor
// status.resources.osDiskPVCName is available — i.e. an instance torn down
// mid-teardown before this field existed. This path is scoped narrowly
// (see collectDiskPVCNames doc comment) but must still work, or pre-fix
// orphans would leak instead of being cleaned up.
func TestTypedCollectDiskPVCNamesFallsBackForPreFixOrphans(t *testing.T) {
	ctx := context.Background()
	client := NewTypedClientWithClientsets(
		harvesterfake.NewSimpleClientset(),
		kubefake.NewSimpleClientset(
			&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "pg-orders-os-v20260615", Namespace: "tenant-a"}},
		),
		kvfake.NewSimpleClientset(),
		cdifake.NewSimpleClientset(),
		"",
	)

	refs := dbaasv1.ResourceRefs{VMName: "pg-orders"} // no OSDiskPVCName — pre-fix instance
	names := client.collectDiskPVCNames(ctx, "orders", "tenant-a", refs)

	found := false
	for _, n := range names {
		if n == "pg-orders-os-v20260615" {
			found = true
		}
	}
	if !found {
		t.Fatalf("collectDiskPVCNames(%v) = %v, want it to still find the pre-fix orphan pg-orders-os-v20260615", refs, names)
	}
}

func newTestTypedClient(objs ...runtime.Object) *TypedClient {
	return NewTypedClientWithClientsets(harvesterfake.NewSimpleClientset(objs...), kubefake.NewSimpleClientset(), kvfake.NewSimpleClientset(), cdifake.NewSimpleClientset(), "")
}

func findPVCTemplate(pvcs []*corev1.PersistentVolumeClaim, name string) *corev1.PersistentVolumeClaim {
	for _, pvc := range pvcs {
		if pvc.Name == name {
			return pvc
		}
	}
	return nil
}

func vmVolumeUsesPVC(vm *kubevirtv1.VirtualMachine, volumeName, claimName string) bool {
	for _, volume := range vm.Spec.Template.Spec.Volumes {
		if volume.Name == volumeName && volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == claimName {
			return true
		}
	}
	return false
}

func vmHasInterface(vm *kubevirtv1.VirtualMachine, interfaceName string) bool {
	for _, iface := range vm.Spec.Template.Spec.Domain.Devices.Interfaces {
		if iface.Name == interfaceName {
			return true
		}
	}
	return false
}

func vmInterfaceHasPort(vm *kubevirtv1.VirtualMachine, interfaceName string, port int32) bool {
	for _, iface := range vm.Spec.Template.Spec.Domain.Devices.Interfaces {
		if iface.Name != interfaceName {
			continue
		}
		for _, ifacePort := range iface.Ports {
			if ifacePort.Port == port && ifacePort.Protocol == "TCP" {
				return true
			}
		}
	}
	return false
}

func testTypedVMImage() *harvesterhciov1beta1.VirtualMachineImage {
	return &harvesterhciov1beta1.VirtualMachineImage{
		TypeMeta:   metav1.TypeMeta{APIVersion: "harvesterhci.io/v1beta1", Kind: "VirtualMachineImage"},
		ObjectMeta: metav1.ObjectMeta{Name: "ubuntu-2204-postgres-v20260615", Namespace: "default"},
		Spec:       harvesterhciov1beta1.VirtualMachineImageSpec{DisplayName: "Ubuntu 22.04 PostgreSQL v20260615"},
		Status: harvesterhciov1beta1.VirtualMachineImageStatus{
			StorageClassName: "longhorn-image-ubuntu",
			Conditions: []harvesterhciov1beta1.Condition{
				{Type: harvesterhciov1beta1.ImageImported, Status: corev1.ConditionTrue},
			},
		},
	}
}
