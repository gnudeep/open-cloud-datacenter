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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dbaasv1 "github.com/wso2/open-cloud-datacenter/crds/dbaas/api/v1alpha1"
)

func TestImmutableDriftNormalizesCreateDefaults(t *testing.T) {
	inst := &dbaasv1.DBInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "orders"},
		Spec: dbaasv1.DBInstanceSpec{
			DBInstanceClass:  "db.t3.medium",
			AllocatedStorage: 50,
			NetworkRef:       "default/vm-network",
			OSImage:          "ubuntu-22.04-server-cloudimg-amd64.img",
			DBName:           "orders",
			MasterUsername:   "dbadmin",
			Port:             5432,
			StorageType:      "longhorn",
		},
		Status: dbaasv1.DBInstanceStatus{
			AppliedSpec: &dbaasv1.AppliedSpec{
				NetworkRef: "default/vm-network",
			},
		},
	}

	if drift := immutableDrift(inst); drift != "" {
		t.Fatalf("immutableDrift() = %q, want no drift", drift)
	}
}

func TestImmutableDriftDetectsActualImmutableChange(t *testing.T) {
	inst := &dbaasv1.DBInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "orders"},
		Spec: dbaasv1.DBInstanceSpec{
			DBInstanceClass:  "db.t3.medium",
			AllocatedStorage: 50,
			NetworkRef:       "default/vm-network",
			DBName:           "orders-v2",
		},
		Status: dbaasv1.DBInstanceStatus{
			AppliedSpec: &dbaasv1.AppliedSpec{
				NetworkRef:     "default/vm-network",
				OSImage:        "ubuntu-22.04-server-cloudimg-amd64.img",
				DBName:         "orders",
				MasterUsername: "dbadmin",
				Port:           5432,
				StorageType:    "longhorn",
			},
		},
	}

	if drift := immutableDrift(inst); drift != "dbName" {
		t.Fatalf("immutableDrift() = %q, want dbName", drift)
	}
}
