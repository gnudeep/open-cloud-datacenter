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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
)

func TestDeployMonitoringIsIdempotent(t *testing.T) {
	client := NewClient(fake.NewSimpleDynamicClient(runtime.NewScheme()), "https://grafana.example")

	for i := 0; i < 2; i++ {
		svcName, smName, grafanaURL, promTarget, err := client.DeployMonitoring(context.Background(), "orders", "tenant-a", "192.168.40.50")
		if err != nil {
			t.Fatalf("DeployMonitoring call %d returned error: %v", i+1, err)
		}
		if svcName != "pg-orders-metrics" {
			t.Fatalf("service name = %q, want pg-orders-metrics", svcName)
		}
		if smName != "pg-orders-monitor" {
			t.Fatalf("ServiceMonitor name = %q, want pg-orders-monitor", smName)
		}
		if grafanaURL != "https://grafana.example/d/dbaas-orders/postgresql-orders" {
			t.Fatalf("Grafana URL = %q", grafanaURL)
		}
		if promTarget != "pg-orders-metrics.tenant-a.svc:9187" {
			t.Fatalf("Prometheus target = %q", promTarget)
		}
	}

	svc, err := client.Dynamic.Resource(serviceGVR).Namespace("tenant-a").Get(context.Background(), "pg-orders-metrics", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Service: %v", err)
	}
	if selector, found, err := unstructured.NestedMap(svc.Object, "spec", "selector"); err != nil || found || len(selector) != 0 {
		t.Fatalf("Service selector = %v, found=%t, err=%v; want no selector", selector, found, err)
	}

	ep, err := client.Dynamic.Resource(endpointsGVR).Namespace("tenant-a").Get(context.Background(), "pg-orders-metrics", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Endpoints: %v", err)
	}
	subsets, found, err := unstructured.NestedSlice(ep.Object, "subsets")
	if err != nil || !found || len(subsets) != 1 {
		t.Fatalf("Endpoint subsets = %v, found=%t, err=%v", subsets, found, err)
	}
	subset := subsets[0].(map[string]interface{})
	addresses := subset["addresses"].([]interface{})
	address := addresses[0].(map[string]interface{})
	if address["ip"] != "192.168.40.50" {
		t.Fatalf("Endpoint IP = %v, want 192.168.40.50", address["ip"])
	}
}
