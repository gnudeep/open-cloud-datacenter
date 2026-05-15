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
// HCI APIs (KubeVirt, CDI, Kube-OVN, Prometheus Operator) that back a
// DBInstance: networking, storage, the PostgreSQL VM, monitoring, and teardown.
package harvester

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

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
	vpcGVR = schema.GroupVersionResource{
		Group: "kubeovn.io", Version: "v1", Resource: "vpcs",
	}
	subnetGVR = schema.GroupVersionResource{
		Group: "kubeovn.io", Version: "v1", Resource: "subnets",
	}
	nadGVR = schema.GroupVersionResource{
		Group: "k8s.cni.cncf.io", Version: "v1", Resource: "network-attachment-definitions",
	}
	secretGVR = schema.GroupVersionResource{
		Group: "", Version: "v1", Resource: "secrets",
	}
	serviceGVR = schema.GroupVersionResource{
		Group: "", Version: "v1", Resource: "services",
	}
	smGVR = schema.GroupVersionResource{
		Group: "monitoring.coreos.com", Version: "v1", Resource: "servicemonitors",
	}
	vpcPeeringGVR = schema.GroupVersionResource{
		Group: "kubeovn.io", Version: "v1", Resource: "vpc-peerings",
	}
	vmImageGVR = schema.GroupVersionResource{
		Group: "harvesterhci.io", Version: "v1beta1", Resource: "virtualmachineimages",
	}
)

const vmiPhaseRunning = "Running"

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
	ID              string
	Namespace       string
	CPUCores        int
	MemoryMB        int
	OSImage         string
	DataVolumeRef   string
	SubnetName      string
	NADName         string
	MasterUser      string
	DBName          string
	Port            int
	MaxConnections  int
	BackupEnabled   bool
	BackupWindow    string
	S3Config        *dbaasv1.S3BackupConfig
	VMPassword      string
	ConsumerNetwork string
}

// VMIReadiness bundles phase, IP, and postgres-readiness from a single VMI fetch.
type VMIReadiness struct {
	Running bool
	IP      string
	Ready   bool // Running AND uptime > 3 min
}

// ============================================================
// Network: Kube-OVN VPC + Subnet + NAD
// ============================================================

func (c *Client) CreateVPCNetwork(ctx context.Context, id, ns, consumerVLAN string) (vpcName, subnetName, nadName string, err error) {
	vpcName = fmt.Sprintf("dbaas-%s-vpc", id)
	subnetName = fmt.Sprintf("dbaas-%s-subnet", id)
	nadName = fmt.Sprintf("dbaas-%s-nad", id)
	cidr, gw := subnetCIDRForID(id)

	// 1. Create VPC
	vpc := newUnstructured("kubeovn.io/v1", "Vpc", vpcName, "")
	_ = unstructured.SetNestedSlice(vpc.Object, []interface{}{ns}, "spec", "namespaces")
	created, e := c.Dynamic.Resource(vpcGVR).Create(ctx, vpc, metav1.CreateOptions{})
	if e != nil {
		if !apierrors.IsAlreadyExists(e) {
			err = e
			return
		}
		created, _ = c.Dynamic.Resource(vpcGVR).Get(ctx, vpcName, metav1.GetOptions{})
	}

	// 2. Create NAD (must exist before Subnet — Harvester subnet webhook validates the NAD by provider).
	provider := fmt.Sprintf("%s.%s.ovn", nadName, ns)
	nad := newUnstructured("k8s.cni.cncf.io/v1", "NetworkAttachmentDefinition", nadName, ns)
	nad.SetLabels(map[string]string{
		"network.harvesterhci.io/type":           "OverlayNetwork",
		"network.harvesterhci.io/clusternetwork": "mgmt",
		"network.harvesterhci.io/ready":          "true",
	})
	config := fmt.Sprintf(`{"cniVersion":"0.3.1","type":"kube-ovn","server_socket":"/run/openvswitch/kube-ovn-daemon.sock","provider":"%s"}`, provider)
	_ = unstructured.SetNestedField(nad.Object, config, "spec", "config")
	if _, e := c.Dynamic.Resource(nadGVR).Namespace(ns).Create(ctx, nad, metav1.CreateOptions{}); e != nil {
		if err = ignoreAlreadyExists(e); err != nil {
			return
		}
	}

	// 3. Create Subnet (provider links it to the NAD; Harvester webhook requires this).
	subnet := newUnstructured("kubeovn.io/v1", "Subnet", subnetName, "")
	_ = unstructured.SetNestedField(subnet.Object, vpcName, "spec", "vpc")
	_ = unstructured.SetNestedField(subnet.Object, provider, "spec", "provider")
	_ = unstructured.SetNestedField(subnet.Object, cidr, "spec", "cidrBlock")
	_ = unstructured.SetNestedField(subnet.Object, gw, "spec", "gateway")
	_ = unstructured.SetNestedField(subnet.Object, "IPv4", "spec", "protocol")
	_ = unstructured.SetNestedSlice(subnet.Object, []interface{}{ns}, "spec", "namespaces")
	_ = unstructured.SetNestedField(subnet.Object, true, "spec", "private")
	_ = unstructured.SetNestedField(subnet.Object, true, "spec", "enableDHCP")
	_ = unstructured.SetNestedSlice(subnet.Object, []interface{}{consumerVLAN}, "spec", "allowSubnets")
	if _, e := c.Dynamic.Resource(subnetGVR).Create(ctx, subnet, metav1.CreateOptions{}); e != nil {
		if err = ignoreAlreadyExists(e); err != nil {
			return
		}
	}

	// 4. Add static route for consumer VLAN access
	if created != nil {
		routes, _, _ := unstructured.NestedSlice(created.Object, "spec", "staticRoutes")
		routes = append(routes, map[string]interface{}{
			"cidr": consumerVLAN, "nextHopIP": "autodetect", "policy": "policyDst",
		})
		_ = unstructured.SetNestedSlice(created.Object, routes, "spec", "staticRoutes")
		_, _ = c.Dynamic.Resource(vpcGVR).Update(ctx, created, metav1.UpdateOptions{})
	}

	return
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
	luksKey := randomString(64)

	// Generate per-instance TLS: ephemeral CA + server cert signed by that CA.
	// CA key is stored in the Secret alongside DB credentials (same threat model).
	tls, tlsErr := generateTLS(vmName)
	if tlsErr != nil {
		err = fmt.Errorf("TLS generation: %w", tlsErr)
		return
	}
	caCertPEM = tls.CACertPEM

	// Store credentials and cloud-init in K8s Secret
	cloudInit := buildCloudInit(p, adminPw, replPw, exporterPw, luksKey, tls)
	secret := newUnstructured("v1", "Secret", secretName, p.Namespace)
	_ = unstructured.SetNestedField(secret.Object, "Opaque", "type")
	_ = unstructured.SetNestedField(secret.Object, map[string]interface{}{
		"admin_user":        p.MasterUser,
		"admin_password":    adminPw,
		"repl_password":     replPw,
		"exporter_password": exporterPw,
		"luks_key":          luksKey,
		"ca_cert":           tls.CACertPEM,
		"ca_key":            tls.CAKeyPEM,
		"server_cert":       tls.ServerCertPEM,
		"server_key":        tls.ServerKeyPEM,
		"userdata":          cloudInit, // referenced by VM spec; avoids plain-text in VM CR
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
				"annotations": map[string]interface{}{
					"ovn.kubernetes.io/logical_switch": p.SubnetName,
				},
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
						"interfaces": vmInterfaces(p.ConsumerNetwork),
					},
				},
				"networks": vmNetworks(p.Namespace, p.NADName, p.ConsumerNetwork),
				"volumes": []interface{}{
					map[string]interface{}{"name": "os-disk", "dataVolume": map[string]interface{}{"name": fmt.Sprintf("pg-%s-os", p.ID)}},
					map[string]interface{}{"name": "pgdata-disk", "dataVolume": map[string]interface{}{"name": p.DataVolumeRef}},
					map[string]interface{}{"name": "cloudinit", "cloudInitNoCloud": map[string]interface{}{
						"secretRef": map[string]interface{}{"name": secretName},
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
	// Prefer vpc-net (Multus bridge) over mgmt-net (pod masquerade).
	// KubeVirt lists masquerade first, so we scan all interfaces and pick
	// vpc-net explicitly; fall back to the first address if not found.
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
		if name == "vpc-net" {
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

	ready := running && time.Since(vmi.GetCreationTimestamp().Time) > 3*time.Minute
	return VMIReadiness{Running: running, IP: ip, Ready: ready}, nil
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

func (c *Client) DeployMonitoring(ctx context.Context, id, ns, vmAddr string, pgPort int) (smName, grafanaURL, promTarget string, err error) {
	smName = fmt.Sprintf("pg-%s-monitor", id)
	svcName := fmt.Sprintf("pg-%s-metrics", id)
	grafanaURL = fmt.Sprintf("%s/d/dbaas-%s/postgresql-%s", c.GrafanaURL, id, id)
	promTarget = fmt.Sprintf("%s.%s.svc:9187", svcName, ns)

	// Headless service
	svc := newUnstructured("v1", "Service", svcName, ns)
	svc.SetLabels(map[string]string{dbaasv1.LabelInstance: id, dbaasv1.LabelMetrics: "true"})
	_ = unstructured.SetNestedField(svc.Object, "ClusterIP", "spec", "type")
	_ = unstructured.SetNestedField(svc.Object, "None", "spec", "clusterIP")
	_ = unstructured.SetNestedField(svc.Object, map[string]interface{}{dbaasv1.LabelInstance: id}, "spec", "selector")
	_ = unstructured.SetNestedSlice(svc.Object, []interface{}{
		map[string]interface{}{"name": "metrics", "port": int64(9187), "targetPort": int64(9187), "protocol": "TCP"},
	}, "spec", "ports")
	_, _ = c.Dynamic.Resource(serviceGVR).Namespace(ns).Create(ctx, svc, metav1.CreateOptions{})

	// ServiceMonitor
	sm := newUnstructured("monitoring.coreos.com/v1", "ServiceMonitor", smName, ns)
	sm.SetLabels(map[string]string{dbaasv1.LabelInstance: id, "release": "prometheus"})
	_ = unstructured.SetNestedField(sm.Object, map[string]interface{}{
		"matchLabels": map[string]interface{}{dbaasv1.LabelMetrics: "true", dbaasv1.LabelInstance: id},
	}, "spec", "selector")
	_ = unstructured.SetNestedSlice(sm.Object, []interface{}{
		map[string]interface{}{"port": "metrics", "interval": "15s", "path": "/metrics"},
	}, "spec", "endpoints")
	_, err = c.Dynamic.Resource(smGVR).Namespace(ns).Create(ctx, sm, metav1.CreateOptions{})

	return
}

// ============================================================
// VPC Peering: Kube-OVN VpcPeering between DBaaS VPC and external VPC
// ============================================================

// CreateVpcPeering creates a Kube-OVN VpcPeering resource that enables
// bidirectional routing between the DBaaS VPC and a remote VPC (e.g.
// an RKE2 cluster VPC).
func (c *Client) CreateVpcPeering(ctx context.Context, id, dbVpcName, dbSubnetName, remoteVpc, remoteSubnet string) (string, error) {
	name := fmt.Sprintf("dbaas-%s-peering", id)
	peering := newUnstructured("kubeovn.io/v1", "VpcPeering", name, "")
	_ = unstructured.SetNestedField(peering.Object, dbVpcName, "spec", "localVpc")
	_ = unstructured.SetNestedField(peering.Object, remoteVpc, "spec", "remoteVpc")
	_ = unstructured.SetNestedSlice(peering.Object, []interface{}{dbSubnetName}, "spec", "localSubnets")
	_ = unstructured.SetNestedSlice(peering.Object, []interface{}{remoteSubnet}, "spec", "remoteSubnets")
	if _, err := c.Dynamic.Resource(vpcPeeringGVR).Create(ctx, peering, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return name, nil
		}
		return "", err
	}
	return name, nil
}

// ============================================================
// Teardown
// ============================================================

func (c *Client) TeardownAll(ctx context.Context, id, ns string, refs dbaasv1.ResourceRefs) {
	type deleteTask struct {
		gvr       schema.GroupVersionResource
		namespace string
		name      string
	}
	// Only delete the NAD if we created it (VPC mode). In direct NAD mode
	// (networkRef), VPCName is empty and the NAD is owned by the cluster operator.
	ownedNAD := ""
	if refs.VPCName != "" {
		ownedNAD = refs.NADName
	}
	tasks := []deleteTask{
		{smGVR, ns, refs.ServiceMonitor},
		{vmGVR, ns, refs.VMName},
		{dvGVR, ns, refs.DataVolumeName},
		{secretGVR, ns, refs.SecretName},
		{nadGVR, ns, ownedNAD},
		{subnetGVR, "", refs.SubnetName},
		{vpcPeeringGVR, "", refs.VpcPeeringName},
		{vpcGVR, "", refs.VPCName},
	}

	var wg sync.WaitGroup
	for _, t := range tasks {
		if t.name == "" {
			continue
		}
		wg.Add(1)
		go func(dt deleteTask) {
			defer wg.Done()
			if dt.namespace != "" {
				_ = c.Dynamic.Resource(dt.gvr).Namespace(dt.namespace).Delete(ctx, dt.name, metav1.DeleteOptions{})
			} else {
				_ = c.Dynamic.Resource(dt.gvr).Delete(ctx, dt.name, metav1.DeleteOptions{})
			}
		}(t)
	}
	wg.Wait()
}

// ============================================================
// Helpers
// ============================================================

func vmInterfaces(consumerNetwork string) []interface{} {
	ifaces := []interface{}{
		map[string]interface{}{"name": "mgmt-net", "masquerade": map[string]interface{}{}},
		map[string]interface{}{"name": "vpc-net", "bridge": map[string]interface{}{}},
	}
	if consumerNetwork != "" {
		ifaces = append(ifaces, map[string]interface{}{"name": "consumer-net", "bridge": map[string]interface{}{}})
	}
	return ifaces
}

func vmNetworks(namespace, nadName, consumerNetwork string) []interface{} {
	networkName := nadName
	if !strings.Contains(nadName, "/") {
		networkName = fmt.Sprintf("%s/%s", namespace, nadName)
	}
	nets := []interface{}{
		map[string]interface{}{
			"name": "mgmt-net",
			"pod":  map[string]interface{}{},
		},
		map[string]interface{}{
			"name":   "vpc-net",
			"multus": map[string]interface{}{"networkName": networkName},
		},
	}
	if consumerNetwork != "" {
		nets = append(nets, map[string]interface{}{
			"name":   "consumer-net",
			"multus": map[string]interface{}{"networkName": consumerNetwork},
		})
	}
	return nets
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

// ignoreAlreadyExists returns nil if err is an AlreadyExists API error, otherwise err.
func ignoreAlreadyExists(err error) error {
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// subnetCIDRForID uses FNV-32a to derive a /24 subnet CIDR from an instance name.
// The scheme 10.A.B.0/24 (A ∈ [100,227], B ∈ [0,255]) gives 32,768 possible subnets,
// dramatically reducing hash collisions compared to a single-byte hash.
func subnetCIDRForID(id string) (cidr, gw string) {
	h := fnv.New32a()
	h.Write([]byte(id))
	v := h.Sum32()
	a := 100 + int((v>>8)&0x7F) // 100–227
	b := int(v & 0xFF)          // 0–255
	return fmt.Sprintf("10.%d.%d.0/24", a, b),
		fmt.Sprintf("10.%d.%d.1", a, b)
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.URLEncoding.EncodeToString(b)[:n]
}
