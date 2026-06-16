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
	"net"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	dbaasv1 "github.com/wso2/open-cloud-datacenter/crds/dbaas/api/v1alpha1"
)

// podGVR is the GVR for the v1 Pod resource, used by the probe path.
// Kept local to this file because the rest of the harvester client only
// drives KubeVirt / CDI / monitoring resources.
var podGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}

const (
	// probeImage is small (~5 MB) and ships nc, ip, and udhcpc in a single
	// binary, which is exactly what the probe script needs.
	probeImage = "busybox:1.36"

	// probeNicName is the local-to-the-pod name of the secondary NIC that
	// Multus attaches. It must match the "interface" key in the multus
	// annotation and the `dev` arg in the probe script.
	probeNicName = "probe-nic"

	probeContainerName  = "probe"
	probeTimeoutSeconds = 20
	probePollInterval   = 1 * time.Second
)

// ProbeVMListener tests whether the VM at vmIP is accepting TCP on `port`.
// It spawns an ephemeral Pod attached to the same Multus
// NetworkAttachmentDefinition the VM uses, runs nc from there, and reads
// the Pod's exit phase. Returns nil if nc succeeded, a non-nil error
// otherwise.
//
// Why a Pod and not net.DialTimeout from inside the controller: the
// controller almost always runs on the cluster pod overlay, which has no
// L3 route to the data VLAN the VM lives on. A Pod attached to the same
// NAD shares the VM's L2 segment and can dial directly without any
// host-level routing tricks.
//
// Concurrency / IP-collision caveats are tracked in DEFERRED.md (DEF-21).
func (c *Client) ProbeVMListener(
	ctx context.Context,
	ns, vmName, nadRef, vmIP string,
	port int,
	staticNet *dbaasv1.NetworkConfig,
) error {
	nadName, nadNs := parseNADRef(nadRef, ns)

	netAnno, err := json.Marshal([]map[string]string{{
		"name":      nadName,
		"namespace": nadNs,
		"interface": probeNicName,
	}})
	if err != nil {
		return fmt.Errorf("marshal multus annotation: %w", err)
	}

	script, err := probeScript(vmIP, port, staticNet)
	if err != nil {
		return fmt.Errorf("build probe script: %w", err)
	}

	podName := probePodName(vmName)
	pod := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      podName,
				"namespace": ns,
				"annotations": map[string]interface{}{
					"k8s.v1.cni.cncf.io/networks": string(netAnno),
				},
				"labels": map[string]interface{}{
					dbaasv1.LabelInstance: vmName,
					dbaasv1.LabelRole:     "probe",
				},
			},
			"spec": map[string]interface{}{
				"restartPolicy":         "Never",
				"activeDeadlineSeconds": int64(probeTimeoutSeconds),
				"containers": []interface{}{
					map[string]interface{}{
						"name":    probeContainerName,
						"image":   probeImage,
						"command": []interface{}{"sh", "-c", script},
						"securityContext": map[string]interface{}{
							"capabilities": map[string]interface{}{
								// Needed for `ip addr add`. Without it the
								// secondary NIC stays unaddressed and nc has
								// no source IP for the dial.
								"add": []interface{}{"NET_ADMIN"},
							},
						},
					},
				},
			},
		},
	}

	pods := c.Dynamic.Resource(podGVR).Namespace(ns)
	zero := int64(0)
	deleteOpts := metav1.DeleteOptions{GracePeriodSeconds: &zero}

	// Always best-effort delete on exit. Detached ctx so a cancelled parent
	// (e.g. reconciler timed out) doesn't leave the pod behind.
	defer func() { _ = pods.Delete(context.Background(), podName, deleteOpts) }()

	// Clean any stale pod from a previous reconcile attempt before
	// (re-)creating. We poll for actual disappearance because Delete only
	// initiates termination.
	_ = pods.Delete(ctx, podName, deleteOpts)
	for i := 0; i < 25; i++ {
		if _, gerr := pods.Get(ctx, podName, metav1.GetOptions{}); apierrors.IsNotFound(gerr) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if _, err := pods.Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create probe pod: %w", err)
	}

	deadline := time.Now().Add(time.Duration(probeTimeoutSeconds) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(probePollInterval)
		cur, err := pods.Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			continue
		}
		phase, _, _ := unstructured.NestedString(cur.Object, "status", "phase")
		switch phase {
		case "Succeeded":
			return nil
		case "Failed":
			return fmt.Errorf("probe pod failed: %s", probeFailureDetail(cur))
		}
	}
	return fmt.Errorf("probe pod %s did not complete within %ds",
		podName, probeTimeoutSeconds)
}

// probePodName returns a stable name. Stable so that overlapping reconciles
// for the same DBInstance collapse to a single in-flight probe pod rather
// than spamming the API server with one-shot pods.
func probePodName(vmName string) string {
	return fmt.Sprintf("dbaas-probe-%s", vmName)
}

// probeFailureDetail digs the container's terminated state out of the pod
// status so the controller's status message names a useful reason.
func probeFailureDetail(pod *unstructured.Unstructured) string {
	cs, _, _ := unstructured.NestedSlice(pod.Object, "status", "containerStatuses")
	for _, c := range cs {
		cm, _ := c.(map[string]interface{})
		if cm == nil {
			continue
		}
		term, _, _ := unstructured.NestedMap(cm, "state", "terminated")
		if term == nil {
			continue
		}
		code, _, _ := unstructured.NestedInt64(term, "exitCode")
		reason, _, _ := unstructured.NestedString(term, "reason")
		return fmt.Sprintf("exitCode=%d reason=%s", code, reason)
	}
	return "no container terminated state"
}

// parseNADRef parses the spec.networkRef format. Accepts either
// "namespace/name" or just "name"; in the latter case the DBInstance's
// own namespace is used.
func parseNADRef(ref, defaultNs string) (name, ns string) {
	if i := strings.LastIndex(ref, "/"); i > 0 {
		return ref[i+1:], ref[:i]
	}
	return ref, defaultNs
}

// probeScript builds the shell snippet run inside the probe pod. The pod
// boots with the multus secondary NIC up but unaddressed (the NAD has no
// IPAM); the script assigns an IP, then runs nc with a tight timeout.
func probeScript(vmIP string, port int, staticNet *dbaasv1.NetworkConfig) (string, error) {
	var setIP string
	if staticNet != nil {
		probeIP, prefix, err := neighborIP(vmIP, staticNet.Address)
		if err != nil {
			return "", err
		}
		setIP = fmt.Sprintf("ip link set %s up && ip addr add %s/%d dev %s",
			probeNicName, probeIP, prefix, probeNicName)
	} else {
		// DHCP path: the operator chose not to pin a static address, so
		// the VLAN presumably has DHCP. udhcpc grabs a lease for the probe.
		setIP = fmt.Sprintf("ip link set %s up && udhcpc -i %s -nq -t 3 -T 1 -f",
			probeNicName, probeNicName)
	}
	// chained with && so a failed IP setup short-circuits and the pod
	// exits non-zero, which the controller treats as "not ready, retry".
	return fmt.Sprintf("%s && nc -zvw 3 %s %d", setIP, vmIP, port), nil
}

// neighborIP picks an IP one octet away from the VM's address on the same
// subnet. This avoids needing IPAM in the NAD and keeps the probe pod
// trivially correlated with its target. Concurrent probes for sibling
// VMs whose addresses differ by exactly one are the documented collision
// case (DEF-21).
func neighborIP(vmIP, vmCIDR string) (string, int, error) {
	_, ipNet, err := net.ParseCIDR(vmCIDR)
	if err != nil {
		return "", 0, fmt.Errorf("parse staticNetwork.Address %q: %w", vmCIDR, err)
	}
	parsed := net.ParseIP(vmIP).To4()
	if parsed == nil {
		return "", 0, fmt.Errorf("vmIP %q is not an IPv4 address", vmIP)
	}

	candidate := make(net.IP, 4)
	copy(candidate, parsed)
	// Prefer +1, fall back to -1 when the VM is at the broadcast-adjacent
	// edge of /24-style ranges. Stays an IPv4 byte arithmetic.
	if parsed[3] < 254 {
		candidate[3]++
	} else {
		candidate[3]--
	}
	if !ipNet.Contains(candidate) {
		return "", 0, fmt.Errorf("derived probe IP %s outside subnet %s",
			candidate, ipNet)
	}
	prefix, _ := ipNet.Mask.Size()
	return candidate.String(), prefix, nil
}
