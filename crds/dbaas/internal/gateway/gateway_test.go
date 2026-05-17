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

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dbaasv1 "github.com/wso2/open-cloud-datacenter/crds/dbaas/api/v1alpha1"
)

// testToken is a placeholder bearer credential. The test clientFactory
// ignores it; we only need it to clear the auth gate.
const testToken = "test-bearer-token"

// newHandler builds a gateway handler backed by a fake client seeded with objs,
// and returns both so tests can assert on the resulting cluster state. The
// per-request clientFactory ignores the token and always returns the same
// fake client; tests that exercise the auth gate hit it directly via the
// "missing Authorization header" path.
func newHandler(t *testing.T, objs ...client.Object) (http.Handler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := dbaasv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add dbaas scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	srv := &Server{
		clientFor: func(token string) (client.Client, error) { return c, nil },
	}
	return srv.routes(), c
}

// sampleInstance returns a DBInstance in the gateway's default namespace.
func sampleInstance(name string) *dbaasv1.DBInstance {
	running := true
	return &dbaasv1.DBInstance{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: defaultNamespace()},
		Spec: dbaasv1.DBInstanceSpec{
			DBInstanceClass:  "db.t3.medium",
			AllocatedStorage: 50,
			DBName:           "myapp",
			Running:          &running,
		},
	}
}

// do issues an HTTP request against h with a valid bearer token. A string
// body is sent verbatim (handy for malformed-JSON cases); any other body is
// JSON-encoded.
func do(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	return doWithAuth(t, h, method, path, body, "Bearer "+testToken)
}

// doWithAuth is like do but takes an explicit Authorization header value
// (use "" to omit the header entirely). For testing the auth gate.
func doWithAuth(t *testing.T, h http.Handler, method, path string, body any, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
		// no body
	case string:
		r = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, r)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func getInstance(t *testing.T, c client.Client, name string) *dbaasv1.DBInstance {
	t.Helper()
	var inst dbaasv1.DBInstance
	key := types.NamespacedName{Namespace: defaultNamespace(), Name: name}
	if err := c.Get(context.Background(), key, &inst); err != nil {
		t.Fatalf("get %q: %v", name, err)
	}
	return &inst
}

func TestHealthz(t *testing.T) {
	h, _ := newHandler(t)

	// Healthz is deliberately unauthenticated.
	if rec := doWithAuth(t, h, http.MethodGet, "/healthz", nil, ""); rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz without auth: got %d, want 200", rec.Code)
	}
	if rec := do(t, h, http.MethodDelete, "/healthz", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /healthz: got %d, want 405", rec.Code)
	}
}

func TestAuthRequired(t *testing.T) {
	h, _ := newHandler(t, sampleInstance("orders"))

	cases := []struct {
		name, method, path, header string
	}{
		{"list no header", http.MethodGet, "/dbinstances", ""},
		{"create no header", http.MethodPost, "/dbinstances", ""},
		{"get no header", http.MethodGet, "/dbinstances/orders", ""},
		{"patch no header", http.MethodPatch, "/dbinstances/orders", ""},
		{"delete no header", http.MethodDelete, "/dbinstances/orders", ""},
		{"start no header", http.MethodPost, "/dbinstances/orders/start", ""},
		{"stop no header", http.MethodPost, "/dbinstances/orders/stop", ""},
		{"wrong scheme", http.MethodGet, "/dbinstances", "Basic Zm9vOmJhcg=="},
		{"empty bearer", http.MethodGet, "/dbinstances", "Bearer "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doWithAuth(t, h, tc.method, tc.path, nil, tc.header)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("got %d, want 401 (body: %s)", rec.Code, rec.Body)
			}
		})
	}
}

func TestCreateInstance(t *testing.T) {
	h, c := newHandler(t)

	rec := do(t, h, http.MethodPost, "/dbinstances", sampleInstance("orders"))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /dbinstances: got %d, want 202 (body: %s)", rec.Code, rec.Body)
	}
	inst := getInstance(t, c, "orders")
	if inst.Spec.DBInstanceClass != "db.t3.medium" || inst.Spec.AllocatedStorage != 50 {
		t.Fatalf("created instance spec not persisted: %+v", inst.Spec)
	}

	// Duplicate name -> 409.
	if rec := do(t, h, http.MethodPost, "/dbinstances", sampleInstance("orders")); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate POST: got %d, want 409", rec.Code)
	}

	// Missing metadata.name -> 400.
	noName := &dbaasv1.DBInstance{Spec: dbaasv1.DBInstanceSpec{DBInstanceClass: "db.t3.micro"}}
	if rec := do(t, h, http.MethodPost, "/dbinstances", noName); rec.Code != http.StatusBadRequest {
		t.Fatalf("POST without name: got %d, want 400", rec.Code)
	}

	// Malformed JSON -> 400.
	if rec := do(t, h, http.MethodPost, "/dbinstances", "{not json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("POST malformed JSON: got %d, want 400", rec.Code)
	}

	// Unsupported method on the collection -> 405.
	if rec := do(t, h, http.MethodPut, "/dbinstances", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /dbinstances: got %d, want 405", rec.Code)
	}

	// A body that asks for a different namespace must NOT escape into
	// that namespace — the gateway's get/patch/delete handlers only look
	// in defaultNamespace(), so accepting a foreign namespace here would
	// strand the new CR. The override should win silently.
	wrongNS := sampleInstance("orders-2")
	wrongNS.Namespace = "some-other-namespace"
	rec = do(t, h, http.MethodPost, "/dbinstances", wrongNS)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST with foreign ns: got %d, want 202 (body: %s)", rec.Code, rec.Body)
	}
	// The created object must live in defaultNamespace, not in the body's value.
	stranded := &dbaasv1.DBInstance{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "some-other-namespace", Name: "orders-2"}, stranded); !apierrors.IsNotFound(err) {
		t.Fatalf("body namespace was honoured: got err=%v, want NotFound in some-other-namespace", err)
	}
	if _ = getInstance(t, c, "orders-2"); false { /* getInstance Fatals on miss */
	}
}

func TestListInstances(t *testing.T) {
	h, _ := newHandler(t, sampleInstance("a"), sampleInstance("b"))

	rec := do(t, h, http.MethodGet, "/dbinstances", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dbinstances: got %d, want 200", rec.Code)
	}
	var list dbaasv1.DBInstanceList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("list length: got %d, want 2", len(list.Items))
	}
}

func TestGetInstance(t *testing.T) {
	h, _ := newHandler(t, sampleInstance("orders"))

	rec := do(t, h, http.MethodGet, "/dbinstances/orders", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET existing: got %d, want 200", rec.Code)
	}
	var inst dbaasv1.DBInstance
	if err := json.Unmarshal(rec.Body.Bytes(), &inst); err != nil {
		t.Fatalf("decode instance: %v", err)
	}
	if inst.Name != "orders" {
		t.Fatalf("got instance %q, want orders", inst.Name)
	}

	if rec := do(t, h, http.MethodGet, "/dbinstances/missing", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("GET missing: got %d, want 404", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/dbinstances/", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("GET empty name: got %d, want 404", rec.Code)
	}
}

func TestModifyInstance(t *testing.T) {
	h, c := newHandler(t, sampleInstance("orders"))

	rec := do(t, h, http.MethodPatch, "/dbinstances/orders", map[string]any{
		"dbInstanceClass":       "db.m5.large",
		"allocatedStorage":      200,
		"backupRetentionPeriod": 14,
		"running":               false,
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("PATCH: got %d, want 202 (body: %s)", rec.Code, rec.Body)
	}
	inst := getInstance(t, c, "orders")
	if inst.Spec.DBInstanceClass != "db.m5.large" {
		t.Errorf("DBInstanceClass: got %q, want db.m5.large", inst.Spec.DBInstanceClass)
	}
	if inst.Spec.AllocatedStorage != 200 {
		t.Errorf("AllocatedStorage: got %d, want 200", inst.Spec.AllocatedStorage)
	}
	if inst.Spec.BackupRetentionPeriod != 14 {
		t.Errorf("BackupRetentionPeriod: got %d, want 14", inst.Spec.BackupRetentionPeriod)
	}
	if inst.Spec.Running == nil || *inst.Spec.Running {
		t.Errorf("Running: got %v, want false", inst.Spec.Running)
	}

	// Fields not supplied are left untouched.
	if inst.Spec.DBName != "myapp" {
		t.Errorf("DBName changed unexpectedly: got %q, want myapp", inst.Spec.DBName)
	}

	if rec := do(t, h, http.MethodPatch, "/dbinstances/missing", map[string]any{"allocatedStorage": 10}); rec.Code != http.StatusNotFound {
		t.Fatalf("PATCH missing: got %d, want 404", rec.Code)
	}
	if rec := do(t, h, http.MethodPatch, "/dbinstances/orders", "{bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH malformed JSON: got %d, want 400", rec.Code)
	}
}

func TestDeleteInstance(t *testing.T) {
	h, c := newHandler(t, sampleInstance("orders"))

	rec := do(t, h, http.MethodDelete, "/dbinstances/orders", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("DELETE: got %d, want 202", rec.Code)
	}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: defaultNamespace(), Name: "orders"}, &dbaasv1.DBInstance{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("instance still present after delete: err=%v", err)
	}

	if rec := do(t, h, http.MethodDelete, "/dbinstances/missing", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE missing: got %d, want 404", rec.Code)
	}
}

func TestStartStopInstance(t *testing.T) {
	stopped := sampleInstance("orders")
	running := false
	stopped.Spec.Running = &running
	h, c := newHandler(t, stopped)

	if rec := do(t, h, http.MethodPost, "/dbinstances/orders/start", nil); rec.Code != http.StatusAccepted {
		t.Fatalf("start: got %d, want 202", rec.Code)
	}
	if r := getInstance(t, c, "orders").Spec.Running; r == nil || !*r {
		t.Fatalf("after start, Running: got %v, want true", r)
	}

	if rec := do(t, h, http.MethodPost, "/dbinstances/orders/stop", nil); rec.Code != http.StatusAccepted {
		t.Fatalf("stop: got %d, want 202", rec.Code)
	}
	if r := getInstance(t, c, "orders").Spec.Running; r == nil || *r {
		t.Fatalf("after stop, Running: got %v, want false", r)
	}

	if rec := do(t, h, http.MethodPost, "/dbinstances/missing/start", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("start missing: got %d, want 404", rec.Code)
	}
	// GET on an action path is not allowed.
	if rec := do(t, h, http.MethodGet, "/dbinstances/orders/start", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET action: got %d, want 405", rec.Code)
	}
	// Unknown action -> 404.
	if rec := do(t, h, http.MethodPost, "/dbinstances/orders/restart", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown action: got %d, want 404", rec.Code)
	}
}
