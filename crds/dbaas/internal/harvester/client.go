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

// Package harvester wraps the Kubernetes dynamic client to drive the Harvester
// HCI APIs (KubeVirt, CDI, Prometheus Operator) that back a DBInstance: the
// PostgreSQL VM, storage, monitoring, and teardown. Networking is provided by
// an existing Multus NetworkAttachmentDefinition (spec.networkRef); the
// controller does not create or own NADs, VPCs, or subnets.
package harvester

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	dbaasv1 "github.com/wso2/open-cloud-datacenter/crds/dbaas/api/v1alpha1"
)

// GVRs for Harvester resources.
var (
	vmGVR = schema.GroupVersionResource{
		Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines",
	}
	vmiGVR = schema.GroupVersionResource{
		Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances",
	}
	dvGVR = schema.GroupVersionResource{
		Group: "cdi.kubevirt.io", Version: "v1beta1", Resource: "datavolumes",
	}
	secretGVR = schema.GroupVersionResource{
		Group: "", Version: "v1", Resource: "secrets",
	}
	serviceGVR = schema.GroupVersionResource{
		Group: "", Version: "v1", Resource: "services",
	}
	endpointsGVR = schema.GroupVersionResource{
		Group: "", Version: "v1", Resource: "endpoints",
	}
	smGVR = schema.GroupVersionResource{
		Group: "monitoring.coreos.com", Version: "v1", Resource: "servicemonitors",
	}
	vmImageGVR = schema.GroupVersionResource{
		Group: "harvesterhci.io", Version: "v1beta1", Resource: "virtualmachineimages",
	}
)

const (
	vmiPhaseRunning = "Running"
	// dataNetInterface is the VM's single NIC name. It must agree in
	// three places: the KubeVirt VM spec's `domain.devices.interfaces`,
	// the `template.spec.networks` list, and the GetVMIReadiness
	// preference loop that picks the right IP off the VMI status.
	// A single source of truth here keeps them coupled.
	dataNetInterface = "data-net"
)

// Client wraps the Kubernetes dynamic client for Harvester API calls.
type Client struct {
	Dynamic    dynamic.Interface
	GrafanaURL string
}

func NewClient(dyn dynamic.Interface, grafanaURL string) *Client {
	return &Client{Dynamic: dyn, GrafanaURL: grafanaURL}
}

// VMCreateParams bundles everything needed to create a PostgreSQL VM.
type VMCreateParams struct {
	ID             string
	Namespace      string
	CPUCores       int
	MemoryMB       int
	OSImage        string
	DataVolumeRef  string
	NADName        string
	MasterUser     string
	DBName         string
	Port           int
	MaxConnections int
	BackupEnabled  bool
	BackupWindow   string
	S3Config       *dbaasv1.S3BackupConfig
	VMPassword     string
	// StaticNetwork, when non-nil, makes the cloud-init netplan use a
	// static IPv4 config instead of DHCP. Used on VLANs without a DHCP
	// server.
	StaticNetwork *dbaasv1.NetworkConfig
}

// VMIReadiness bundles phase + IP from a single VMI fetch. The IP being
// populated is itself a strong readiness signal: qemu-guest-agent registers
// it only after our bootstrap.sh has finished `apt install postgresql ...`
// and started the agent — so the caller does not need an extra timer.
type VMIReadiness struct {
	Running bool
	IP      string
}

// ============================================================
// Storage: CDI DataVolume
// ============================================================

func (c *Client) CreateDataVolume(ctx context.Context, id, ns string, sizeGB int, storageClass string) (string, error) {
	dvName := fmt.Sprintf("pg-%s-data", id)
	dv := newUnstructured("cdi.kubevirt.io/v1beta1", "DataVolume", dvName, ns)
	dv.SetLabels(map[string]string{dbaasv1.LabelInstance: id, dbaasv1.LabelRole: "pgdata"})

	_ = unstructured.SetNestedMap(dv.Object, map[string]interface{}{}, "spec", "source", "blank")
	_ = unstructured.SetNestedStringSlice(dv.Object, []string{"ReadWriteOnce"}, "spec", "pvc", "accessModes")
	_ = unstructured.SetNestedField(dv.Object, "Block", "spec", "pvc", "volumeMode")
	_ = unstructured.SetNestedField(dv.Object, fmt.Sprintf("%dGi", sizeGB), "spec", "pvc", "resources", "requests", "storage")
	_ = unstructured.SetNestedField(dv.Object, storageClass, "spec", "pvc", "storageClassName")

	if _, e := c.Dynamic.Resource(dvGVR).Namespace(ns).Create(ctx, dv, metav1.CreateOptions{}); e != nil {
		return dvName, ignoreAlreadyExists(e)
	}
	return dvName, nil
}

func (c *Client) ResizeDataVolume(ctx context.Context, ns, dvName string, newSizeGB int) error {
	dv, err := c.Dynamic.Resource(dvGVR).Namespace(ns).Get(ctx, dvName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	_ = unstructured.SetNestedField(dv.Object, fmt.Sprintf("%dGi", newSizeGB), "spec", "pvc", "resources", "requests", "storage")
	_, err = c.Dynamic.Resource(dvGVR).Namespace(ns).Update(ctx, dv, metav1.UpdateOptions{})
	return err
}

// ============================================================
// VM: KubeVirt VirtualMachine + cloud-init + credentials Secret
// ============================================================

// resolveVMImage maps a user-supplied image reference to the underlying
// Harvester VirtualMachineImage and its image-managed StorageClass.
//
// The reference accepts:
//   - "<name>"           — looked up in the "default" namespace
//   - "<ns>/<name>"      — explicit namespace
//   - "<displayName>"    — fall-back search by VirtualMachineImage.spec.displayName
//
// Returns the resolved namespace, VMImage name, and StorageClass that the
// DataVolume must use. The DataVolume should also carry annotation
// harvesterhci.io/imageId=<ns>/<name>.
func (c *Client) resolveVMImage(ctx context.Context, ref string) (ns, name, sc string, err error) {
	ns, spec := "default", ref
	if i := strings.Index(ref, "/"); i > 0 {
		ns, spec = ref[:i], ref[i+1:]
	}

	if img, e := c.Dynamic.Resource(vmImageGVR).Namespace(ns).Get(ctx, spec, metav1.GetOptions{}); e == nil {
		name = spec
		sc, _, _ = unstructured.NestedString(img.Object, "status", "storageClassName")
		if sc == "" {
			err = fmt.Errorf("VirtualMachineImage %s/%s has no status.storageClassName yet (image not ready)", ns, name)
		}
		return
	} else if !apierrors.IsNotFound(e) {
		err = e
		return
	}

	list, e := c.Dynamic.Resource(vmImageGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if e != nil {
		err = e
		return
	}
	for _, item := range list.Items {
		dn, _, _ := unstructured.NestedString(item.Object, "spec", "displayName")
		if dn == spec {
			name = item.GetName()
			sc, _, _ = unstructured.NestedString(item.Object, "status", "storageClassName")
			if sc == "" {
				err = fmt.Errorf("VirtualMachineImage %s/%s (displayName=%s) has no status.storageClassName yet", ns, name, spec)
			}
			return
		}
	}
	err = fmt.Errorf("no VirtualMachineImage in namespace %s matching name or displayName %q", ns, spec)
	return
}

func (c *Client) CreatePostgresVM(ctx context.Context, p VMCreateParams) (vmName, secretName, caCertPEM string, err error) {
	vmName = fmt.Sprintf("pg-%s", p.ID)
	secretName = fmt.Sprintf("pg-%s-credentials", p.ID)

	// Generate credentials
	adminPw := randomString(32)
	replPw := randomString(32)
	exporterPw := randomString(24)

	// Generate per-instance TLS: ephemeral CA + server cert signed by that CA.
	// CA key is stored in the Secret alongside DB credentials (same threat model).
	tls, tlsErr := generateTLS(vmName)
	if tlsErr != nil {
		err = fmt.Errorf("TLS generation: %w", tlsErr)
		return
	}
	caCertPEM = tls.CACertPEM

	// Store credentials, cloud-init user data, and cloud-init network data
	// in a K8s Secret. KubeVirt's cloudInitNoCloud datasource reads keys
	// `userdata` and `networkdata` from this Secret and feeds them to the
	// VM at the `init-local` stage — applying networkData early enough
	// that systemd-networkd sees the right IP/gateway/DNS *before* it
	// times out (which is what bit us when we tried writing the netplan
	// via cloud-init's write_files module instead).
	cloudInit := buildCloudInit(p, adminPw, replPw, exporterPw, tls)
	networkData := buildNetworkData(p)
	secret := newUnstructured("v1", "Secret", secretName, p.Namespace)
	_ = unstructured.SetNestedField(secret.Object, "Opaque", "type")
	_ = unstructured.SetNestedField(secret.Object, map[string]interface{}{
		"admin_user":        p.MasterUser,
		"admin_password":    adminPw,
		"repl_password":     replPw,
		"exporter_password": exporterPw,
		"ca_cert":           tls.CACertPEM,
		"ca_key":            tls.CAKeyPEM,
		"server_cert":       tls.ServerCertPEM,
		"server_key":        tls.ServerKeyPEM,
		"userdata":          cloudInit,
		"networkdata":       networkData,
	}, "stringData")
	if _, e := c.Dynamic.Resource(secretGVR).Namespace(p.Namespace).Create(ctx, secret, metav1.CreateOptions{}); e != nil {
		if err = ignoreAlreadyExists(e); err != nil {
			return
		}
	}

	// Resolve the Harvester VirtualMachineImage so the OS DataVolume can use
	// the image-managed StorageClass (no cross-namespace PVC clone, no extra RBAC).
	imgNs, imgName, imgSC, err := c.resolveVMImage(ctx, p.OSImage)
	if err != nil {
		return
	}

	// Build VirtualMachine CR
	vm := newUnstructured("kubevirt.io/v1", "VirtualMachine", vmName, p.Namespace)
	vm.SetLabels(map[string]string{dbaasv1.LabelInstance: p.ID, dbaasv1.LabelRole: "primary"})

	spec := map[string]interface{}{
		"running": true,
		"dataVolumeTemplates": []interface{}{
			map[string]interface{}{
				"apiVersion": "cdi.kubevirt.io/v1beta1",
				"kind":       "DataVolume",
				"metadata": map[string]interface{}{
					"name": fmt.Sprintf("pg-%s-os", p.ID),
					"annotations": map[string]interface{}{
						"harvesterhci.io/imageId": fmt.Sprintf("%s/%s", imgNs, imgName),
					},
				},
				"spec": map[string]interface{}{
					"source": map[string]interface{}{
						"blank": map[string]interface{}{},
					},
					"pvc": map[string]interface{}{
						"accessModes":      []interface{}{"ReadWriteMany"},
						"volumeMode":       "Block",
						"storageClassName": imgSC,
						"resources": map[string]interface{}{
							"requests": map[string]interface{}{"storage": "20Gi"},
						},
					},
				},
			},
		},
		"template": map[string]interface{}{
			"metadata": map[string]interface{}{
				"labels": map[string]interface{}{dbaasv1.LabelInstance: p.ID},
			},
			"spec": map[string]interface{}{
				"domain": map[string]interface{}{
					"cpu":    map[string]interface{}{"cores": int64(p.CPUCores), "sockets": int64(1), "threads": int64(1)},
					"memory": map[string]interface{}{"guest": fmt.Sprintf("%dMi", p.MemoryMB)},
					"devices": map[string]interface{}{
						"disks": []interface{}{
							map[string]interface{}{"name": "os-disk", "disk": map[string]interface{}{"bus": "virtio"}, "bootOrder": int64(1)},
							map[string]interface{}{"name": "pgdata-disk", "disk": map[string]interface{}{"bus": "virtio"}},
							map[string]interface{}{"name": "cloudinit", "disk": map[string]interface{}{"bus": "virtio"}},
						},
						"interfaces": vmInterfaces(),
					},
				},
				"networks": vmNetworks(p.Namespace, p.NADName),
				"volumes": []interface{}{
					map[string]interface{}{"name": "os-disk", "dataVolume": map[string]interface{}{"name": fmt.Sprintf("pg-%s-os", p.ID)}},
					map[string]interface{}{"name": "pgdata-disk", "dataVolume": map[string]interface{}{"name": p.DataVolumeRef}},
					// Harvester's VM mutating webhook silently strips the
					// newer `userDataSecretRef` field while leaving
					// `networkDataSecretRef` intact — the VM ends up with
					// network config but NO user data, so cloud-init never
					// runs our packages / runcmd / ssh_pwauth and the database
					// never installs. Use the legacy `secretRef` (which
					// Harvester recognises) for the userdata key, plus
					// `networkDataSecretRef` for the networkdata key. Both
					// point at the same Secret.
					map[string]interface{}{"name": "cloudinit", "cloudInitNoCloud": map[string]interface{}{
						"secretRef":            map[string]interface{}{"name": secretName},
						"networkDataSecretRef": map[string]interface{}{"name": secretName},
					}},
				},
			},
		},
	}
	_ = unstructured.SetNestedField(vm.Object, spec, "spec")

	if _, e := c.Dynamic.Resource(vmGVR).Namespace(p.Namespace).Create(ctx, vm, metav1.CreateOptions{}); e != nil {
		err = ignoreAlreadyExists(e)
	}
	return // vmName, secretName, caCertPEM, err all set via named returns
}

// GetVMIReadiness fetches the VMI once and returns phase, IP, and postgres-readiness.
func (c *Client) GetVMIReadiness(ctx context.Context, ns, vmName string) (VMIReadiness, error) {
	vmi, err := c.Dynamic.Resource(vmiGVR).Namespace(ns).Get(ctx, vmName, metav1.GetOptions{})
	if err != nil {
		return VMIReadiness{}, err
	}

	phase, _, _ := unstructured.NestedString(vmi.Object, "status", "phase")
	running := phase == vmiPhaseRunning

	var ip string
	interfaces, _, _ := unstructured.NestedSlice(vmi.Object, "status", "interfaces")
	// Prefer data-net (Multus bridge) over mgmt-net (pod masquerade).
	// KubeVirt lists masquerade first, so we scan all interfaces and pick
	// data-net explicitly; fall back to the first address if not found.
	var fallbackIP string
	for _, iface := range interfaces {
		ifMap, ok := iface.(map[string]interface{})
		if !ok {
			continue
		}
		addr, _ := ifMap["ipAddress"].(string)
		if addr == "" {
			continue
		}
		name, _ := ifMap["name"].(string)
		if name == dataNetInterface {
			ip = addr
			break
		}
		if fallbackIP == "" {
			fallbackIP = addr
		}
	}
	if ip == "" {
		ip = fallbackIP
	}

	return VMIReadiness{Running: running, IP: ip}, nil
}

// setVMRunning sets spec.running on the VM.
func (c *Client) setVMRunning(ctx context.Context, ns, vmName string, running bool) error {
	vm, err := c.Dynamic.Resource(vmGVR).Namespace(ns).Get(ctx, vmName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	_ = unstructured.SetNestedField(vm.Object, running, "spec", "running")
	_, err = c.Dynamic.Resource(vmGVR).Namespace(ns).Update(ctx, vm, metav1.UpdateOptions{})
	return err
}

// GetSecret returns the Secret's data map (values are raw bytes).
func (c *Client) GetSecret(ctx context.Context, ns, name string) (map[string][]byte, error) {
	obj, err := c.Dynamic.Resource(secretGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	rawData, _, _ := unstructured.NestedMap(obj.Object, "data")
	out := make(map[string][]byte, len(rawData))
	for k, v := range rawData {
		if s, ok := v.(string); ok {
			decoded, err := base64.StdEncoding.DecodeString(s)
			if err == nil {
				out[k] = decoded
			}
		}
	}
	return out, nil
}

func (c *Client) StopVM(ctx context.Context, ns, vmName string) error {
	return c.setVMRunning(ctx, ns, vmName, false)
}

func (c *Client) StartVM(ctx context.Context, ns, vmName string) error {
	return c.setVMRunning(ctx, ns, vmName, true)
}

// ResizeVM updates CPU/memory on the VM spec.
func (c *Client) ResizeVM(ctx context.Context, ns, vmName string, cpuCores, memoryMB int) error {
	vm, err := c.Dynamic.Resource(vmGVR).Namespace(ns).Get(ctx, vmName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	_ = unstructured.SetNestedField(vm.Object, int64(cpuCores), "spec", "template", "spec", "domain", "cpu", "cores")
	_ = unstructured.SetNestedField(vm.Object, fmt.Sprintf("%dMi", memoryMB), "spec", "template", "spec", "domain", "memory", "guest")
	_, err = c.Dynamic.Resource(vmGVR).Namespace(ns).Update(ctx, vm, metav1.UpdateOptions{})
	return err
}

// ============================================================
// Monitoring
// ============================================================

// DeployMonitoring creates the headless metrics Service, its manual Endpoints,
// and a Prometheus
// ServiceMonitor for the instance. It returns the Service name, the
// ServiceMonitor name, the Grafana dashboard URL, and the Prometheus
// scrape target. The caller stores both names in status.resources so the
// finalizer can delete them later.
func (c *Client) DeployMonitoring(ctx context.Context, id, ns, vmIP string) (svcName, smName, grafanaURL, promTarget string, err error) {
	smName = fmt.Sprintf("pg-%s-monitor", id)
	svcName = fmt.Sprintf("pg-%s-metrics", id)
	grafanaURL = fmt.Sprintf("%s/d/dbaas-%s/postgresql-%s", c.GrafanaURL, id, id)
	promTarget = fmt.Sprintf("%s.%s.svc:9187", svcName, ns)
	if vmIP == "" {
		err = fmt.Errorf("monitoring endpoint IP is required")
		return
	}

	// Headless service. Do not use a selector: the KubeVirt virt-launcher pod
	// has the instance label, but the exporter runs inside the VM at vmIP.
	// A manual Endpoints object below points Prometheus at the guest address.
	svc := newUnstructured("v1", "Service", svcName, ns)
	svc.SetLabels(map[string]string{dbaasv1.LabelInstance: id, dbaasv1.LabelMetrics: "true"})
	_ = unstructured.SetNestedField(svc.Object, "ClusterIP", "spec", "type")
	_ = unstructured.SetNestedField(svc.Object, "None", "spec", "clusterIP")
	_ = unstructured.SetNestedSlice(svc.Object, []interface{}{
		map[string]interface{}{"name": "metrics", "port": int64(9187), "targetPort": int64(9187), "protocol": "TCP"},
	}, "spec", "ports")
	unstructured.RemoveNestedField(svc.Object, "spec", "selector")
	if err = c.createOrUpdate(ctx, serviceGVR, ns, svc, func(existing *unstructured.Unstructured) {
		existing.SetLabels(svc.GetLabels())
		_ = unstructured.SetNestedField(existing.Object, "ClusterIP", "spec", "type")
		_ = unstructured.SetNestedField(existing.Object, "None", "spec", "clusterIP")
		_ = unstructured.SetNestedSlice(existing.Object, []interface{}{
			map[string]interface{}{"name": "metrics", "port": int64(9187), "targetPort": int64(9187), "protocol": "TCP"},
		}, "spec", "ports")
		unstructured.RemoveNestedField(existing.Object, "spec", "selector")
	}); err != nil {
		return
	}

	ep := newUnstructured("v1", "Endpoints", svcName, ns)
	ep.SetLabels(map[string]string{dbaasv1.LabelInstance: id, dbaasv1.LabelMetrics: "true"})
	_ = unstructured.SetNestedSlice(ep.Object, monitoringEndpointSubsets(vmIP), "subsets")
	if err = c.createOrUpdate(ctx, endpointsGVR, ns, ep, func(existing *unstructured.Unstructured) {
		existing.SetLabels(ep.GetLabels())
		_ = unstructured.SetNestedSlice(existing.Object, monitoringEndpointSubsets(vmIP), "subsets")
	}); err != nil {
		return
	}

	// ServiceMonitor
	sm := newUnstructured("monitoring.coreos.com/v1", "ServiceMonitor", smName, ns)
	sm.SetLabels(map[string]string{dbaasv1.LabelInstance: id, "release": "prometheus"})
	_ = unstructured.SetNestedField(sm.Object, map[string]interface{}{
		"matchLabels": map[string]interface{}{dbaasv1.LabelMetrics: "true", dbaasv1.LabelInstance: id},
	}, "spec", "selector")
	_ = unstructured.SetNestedSlice(sm.Object, []interface{}{
		map[string]interface{}{"port": "metrics", "interval": "15s", "path": "/metrics"},
	}, "spec", "endpoints")
	err = c.createOrUpdate(ctx, smGVR, ns, sm, func(existing *unstructured.Unstructured) {
		existing.SetLabels(sm.GetLabels())
		_ = unstructured.SetNestedField(existing.Object, map[string]interface{}{
			"matchLabels": map[string]interface{}{dbaasv1.LabelMetrics: "true", dbaasv1.LabelInstance: id},
		}, "spec", "selector")
		_ = unstructured.SetNestedSlice(existing.Object, []interface{}{
			map[string]interface{}{"port": "metrics", "interval": "15s", "path": "/metrics"},
		}, "spec", "endpoints")
	})

	return
}

// ============================================================
// Teardown
// ============================================================

// TeardownAll deletes every Harvester resource the controller created for
// this DBInstance, in parallel, and returns an aggregated error if any
// delete failed for a reason other than NotFound. The caller (the
// reconcileDelete finalizer path) must keep the finalizer in place until
// this returns nil — otherwise orphan VMs / DataVolumes / Secrets / Services
// can be left behind after the CR is gone.
//
// The NetworkAttachmentDefinition is always assumed to be owned by the
// cluster operator (referenced via spec.networkRef) and is never deleted.
func (c *Client) TeardownAll(ctx context.Context, id, ns string, refs dbaasv1.ResourceRefs) error {
	type deleteTask struct {
		gvr       schema.GroupVersionResource
		namespace string
		name      string
	}
	tasks := []deleteTask{
		{smGVR, ns, refs.ServiceMonitor},
		{endpointsGVR, ns, refs.MetricsServiceName},
		{serviceGVR, ns, refs.MetricsServiceName},
		{vmGVR, ns, refs.VMName},
		{dvGVR, ns, refs.DataVolumeName},
		{secretGVR, ns, refs.SecretName},
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
			var err error
			if dt.namespace != "" {
				err = c.Dynamic.Resource(dt.gvr).Namespace(dt.namespace).Delete(ctx, dt.name, metav1.DeleteOptions{})
			} else {
				err = c.Dynamic.Resource(dt.gvr).Delete(ctx, dt.name, metav1.DeleteOptions{})
			}
			if err == nil || apierrors.IsNotFound(err) {
				return
			}
			mu.Lock()
			errs = append(errs, fmt.Sprintf("%s/%s: %v", dt.gvr.Resource, dt.name, err))
			mu.Unlock()
		}(t)
	}
	wg.Wait()
	if len(errs) > 0 {
		return fmt.Errorf("teardown: %s", strings.Join(errs, "; "))
	}
	return nil
}

// ============================================================
// Helpers
// ============================================================

// vmInterfaces and vmNetworks describe the VM's single NIC bridged onto the
// NAD named by spec.networkRef. Everything — client traffic, package install,
// Prometheus scrape — flows through that one interface, so we don't add a
// pod-network management NIC.
func vmInterfaces() []interface{} {
	return []interface{}{
		map[string]interface{}{"name": dataNetInterface, "bridge": map[string]interface{}{}},
	}
}

func vmNetworks(namespace, nadName string) []interface{} {
	networkName := nadName
	if !strings.Contains(nadName, "/") {
		networkName = fmt.Sprintf("%s/%s", namespace, nadName)
	}
	return []interface{}{
		map[string]interface{}{
			"name":   dataNetInterface,
			"multus": map[string]interface{}{"networkName": networkName},
		},
	}
}

func newUnstructured(apiVersion, kind, name, namespace string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata": map[string]interface{}{
			"name": name,
		},
	}}
	if namespace != "" {
		obj.SetNamespace(namespace)
	}
	return obj
}

func (c *Client) createOrUpdate(ctx context.Context, gvr schema.GroupVersionResource, ns string, desired *unstructured.Unstructured, mutateExisting func(*unstructured.Unstructured)) error {
	if _, err := c.Dynamic.Resource(gvr).Namespace(ns).Create(ctx, desired, metav1.CreateOptions{}); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return err
	}
	existing, err := c.Dynamic.Resource(gvr).Namespace(ns).Get(ctx, desired.GetName(), metav1.GetOptions{})
	if err != nil {
		return err
	}
	mutateExisting(existing)
	_, err = c.Dynamic.Resource(gvr).Namespace(ns).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

func monitoringEndpointSubsets(vmIP string) []interface{} {
	return []interface{}{
		map[string]interface{}{
			"addresses": []interface{}{
				map[string]interface{}{"ip": vmIP},
			},
			"ports": []interface{}{
				map[string]interface{}{"name": "metrics", "port": int64(9187), "protocol": "TCP"},
			},
		},
	}
}

// ignoreAlreadyExists returns nil if err is an AlreadyExists API error, otherwise err.
func ignoreAlreadyExists(err error) error {
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.URLEncoding.EncodeToString(b)[:n]
}
