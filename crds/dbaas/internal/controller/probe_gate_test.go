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
	"errors"
	"testing"

	dbaasv1 "github.com/wso2/open-cloud-datacenter/crds/dbaas/api/v1alpha1"
	"github.com/wso2/open-cloud-datacenter/crds/dbaas/internal/harvester"
)

// TestProbeListenerStubbable proves the seam phaseWaitReady uses to test
// PostgreSQL readiness can be swapped in tests. Without this, future
// refactors could accidentally remove the var-indirection and tests would
// silently start hitting the real ProbeVMListener (which spawns Pods).
func TestProbeListenerStubbable(t *testing.T) {
	orig := probeListener
	t.Cleanup(func() { probeListener = orig })

	called := 0
	probeListener = func(_ *harvester.Client, _ context.Context, _, _, _, _ string, _ int, _ *dbaasv1.NetworkConfig) error {
		called++
		return errors.New("refused")
	}

	if err := probeListener(nil, context.Background(), "ns", "vm", "default/vm-network", "192.168.40.50", 5432, nil); err == nil {
		t.Fatalf("stub should return error")
	}
	if called != 1 {
		t.Fatalf("expected stub to be called once, got %d", called)
	}

	probeListener = func(_ *harvester.Client, _ context.Context, _, _, _, _ string, _ int, _ *dbaasv1.NetworkConfig) error {
		return nil
	}
	if err := probeListener(nil, context.Background(), "ns", "vm", "default/vm-network", "192.168.40.50", 5432, nil); err != nil {
		t.Fatalf("stub success path should return nil, got %v", err)
	}
}
