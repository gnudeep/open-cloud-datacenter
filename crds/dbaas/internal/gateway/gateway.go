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

// Package gateway exposes a thin HTTP REST layer over the DBInstance CRD.
// Every handler simply reads or writes the custom resource via the
// controller-runtime client; the actual database provisioning is handled
// asynchronously by the controller. Mutating requests therefore return
// 202 Accepted — callers poll GET /dbinstances/{name} for the latest status.
//
// This initial pass has no authentication. Place the gateway behind an
// auth-enforcing ingress or API gateway (OIDC, mTLS, or API key) before
// exposing it outside the cluster.
package gateway

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dbaasv1 "github.com/wso2/open-cloud-datacenter/crds/dbaas/api/v1alpha1"
)

// Server holds shared state used by every request handler. Adding new
// dependencies (OIDC validator, quota enforcer) means adding a field here
// rather than threading parameters through every function.
type Server struct {
	k8sClient client.Client
}

// RunGateway starts the HTTP gateway and blocks until the server exits.
func RunGateway(addr string, k8sClient client.Client) error {
	return http.ListenAndServe(addr, NewHandler(k8sClient))
}

// NewHandler builds the gateway's HTTP handler: route registration without
// the listening server, so it can be exercised directly in tests.
func NewHandler(k8sClient client.Client) http.Handler {
	srv := &Server{k8sClient: k8sClient}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/dbinstances", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			srv.handleListInstances(w, r)
		case http.MethodPost:
			srv.handleCreateInstance(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	})
	mux.HandleFunc("/dbinstances/", func(w http.ResponseWriter, r *http.Request) {
		srv.handleInstanceRoute(w, r)
	})

	return mux
}

// defaultNamespace returns the namespace DBInstances are read from and written
// to. A future auth layer will derive this per-tenant from the caller identity.
func defaultNamespace() string {
	if ns := os.Getenv("DBAAS_DEFAULT_NAMESPACE"); ns != "" {
		return ns
	}
	return "default"
}

// handleListInstances handles GET /dbinstances.
func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	var instances dbaasv1.DBInstanceList
	if err := s.k8sClient.List(r.Context(), &instances, client.InNamespace(defaultNamespace())); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, instances)
}

// handleCreateInstance handles POST /dbinstances — "create db".
func (s *Server) handleCreateInstance(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	var instance dbaasv1.DBInstance
	if err := json.NewDecoder(r.Body).Decode(&instance); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if instance.Name == "" {
		writeError(w, http.StatusBadRequest, "metadata.name is required")
		return
	}
	if instance.APIVersion == "" {
		instance.APIVersion = dbaasv1.GroupVersion.String()
	}
	if instance.Kind == "" {
		instance.Kind = "DBInstance"
	}
	if instance.Namespace == "" {
		instance.Namespace = defaultNamespace()
	}
	if err := s.k8sClient.Create(r.Context(), &instance); err != nil {
		if apierrors.IsAlreadyExists(err) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, instance)
}

// handleInstanceRoute dispatches /dbinstances/{name} and /dbinstances/{name}/{action}.
func (s *Server) handleInstanceRoute(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/dbinstances/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "instance name is required")
		return
	}

	name := parts[0]
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			s.handleGetInstance(w, r, name)
		case http.MethodPatch:
			s.handleModifyInstance(w, r, name)
		case http.MethodDelete:
			s.handleDeleteInstance(w, r, name)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}

	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	switch parts[1] {
	case "start":
		s.handleSetRunning(w, r, name, true)
	case "stop":
		s.handleSetRunning(w, r, name, false)
	default:
		writeError(w, http.StatusNotFound, "unsupported action")
	}
}

// handleGetInstance handles GET /dbinstances/{name} — "describe db".
func (s *Server) handleGetInstance(w http.ResponseWriter, r *http.Request, name string) {
	var instance dbaasv1.DBInstance
	if err := s.k8sClient.Get(r.Context(), types.NamespacedName{Namespace: defaultNamespace(), Name: name}, &instance); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, instance)
}

// modifyRequest is the set of DBInstance spec fields that may be changed on an
// existing instance. Every field is a pointer so the handler can tell "not
// supplied" apart from a zero value; only non-nil fields are applied.
type modifyRequest struct {
	DBInstanceClass       *string `json:"dbInstanceClass,omitempty"`
	AllocatedStorage      *int    `json:"allocatedStorage,omitempty"`
	BackupRetentionPeriod *int    `json:"backupRetentionPeriod,omitempty"`
	PreferredBackupWindow *string `json:"preferredBackupWindow,omitempty"`
	DeletionProtection    *bool   `json:"deletionProtection,omitempty"`
	Running               *bool   `json:"running,omitempty"`
}

// handleModifyInstance handles PATCH /dbinstances/{name} — "modify db".
// The controller picks up the spec change and reconciles it (resize, backup
// window change, power state, etc.).
func (s *Server) handleModifyInstance(w http.ResponseWriter, r *http.Request, name string) {
	defer r.Body.Close()

	var req modifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	var instance dbaasv1.DBInstance
	if err := s.k8sClient.Get(r.Context(), types.NamespacedName{Namespace: defaultNamespace(), Name: name}, &instance); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if req.DBInstanceClass != nil {
		instance.Spec.DBInstanceClass = *req.DBInstanceClass
	}
	if req.AllocatedStorage != nil {
		instance.Spec.AllocatedStorage = *req.AllocatedStorage
	}
	if req.BackupRetentionPeriod != nil {
		instance.Spec.BackupRetentionPeriod = *req.BackupRetentionPeriod
	}
	if req.PreferredBackupWindow != nil {
		instance.Spec.PreferredBackupWindow = *req.PreferredBackupWindow
	}
	if req.DeletionProtection != nil {
		instance.Spec.DeletionProtection = *req.DeletionProtection
	}
	if req.Running != nil {
		instance.Spec.Running = req.Running
	}

	if err := s.k8sClient.Update(r.Context(), &instance); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, instance)
}

// handleDeleteInstance handles DELETE /dbinstances/{name} — "delete db".
func (s *Server) handleDeleteInstance(w http.ResponseWriter, r *http.Request, name string) {
	instance := &dbaasv1.DBInstance{}
	instance.Name = name
	instance.Namespace = defaultNamespace()
	instance.APIVersion = dbaasv1.GroupVersion.String()
	instance.Kind = "DBInstance"

	if err := s.k8sClient.Delete(r.Context(), instance); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "deletion requested", "name": name})
}

// handleSetRunning handles POST /dbinstances/{name}/start and /stop —
// "start db" and "stop db". It flips spec.running; the controller drives the
// KubeVirt VM power state to match.
func (s *Server) handleSetRunning(w http.ResponseWriter, r *http.Request, name string, running bool) {
	var instance dbaasv1.DBInstance
	if err := s.k8sClient.Get(r.Context(), types.NamespacedName{Namespace: defaultNamespace(), Name: name}, &instance); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	instance.Spec.Running = boolPtr(running)
	if err := s.k8sClient.Update(r.Context(), &instance); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, instance)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func boolPtr(v bool) *bool {
	return &v
}
