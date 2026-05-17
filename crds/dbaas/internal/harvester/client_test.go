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

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
)

func TestDeployMonitoringIsIdempotent(t *testing.T) {
	client := NewClient(fake.NewSimpleDynamicClient(runtime.NewScheme()), "https://grafana.example")

	for i := 0; i < 2; i++ {
		svcName, smName, grafanaURL, promTarget, err := client.DeployMonitoring(context.Background(), "orders", "tenant-a")
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
}
