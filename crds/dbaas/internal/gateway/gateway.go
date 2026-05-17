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
//
// Every mutating request goes through the Kubernetes API server using the
// caller's own credentials, not the manager's ServiceAccount: the caller
// provides an `Authorization: Bearer <token>` header (typically a K8s
// ServiceAccount token or an OIDC token the cluster accepts) and the gateway
// builds a per-request controller-runtime client signed with that token.
//
// The K8s API server therefore enforces authentication, authorization (RBAC)
// and audit on the caller's identity, exactly as if the caller had issued
// the request via kubectl. The gateway never elevates beyond what the caller
// is RBAC-authorized to do.
//
// Mutating requests return 202 Accepted; the controller advances the work
// asynchronously and callers poll GET /dbinstances/{name} for status.
package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dbaasv1 "github.com/wso2/open-cloud-datacenter/crds/dbaas/api/v1alpha1"
)

// clientFactory builds a controller-runtime client whose requests carry the
// supplied bearer token, so every K8s API call is attributed to the caller.
// Tests inject a fake that ignores the token; production wires it to
// rest.CopyConfig + client.New.
type clientFactory func(token string) (client.Client, error)

// Server holds shared state used by every request handler. Adding new
// dependencies (OIDC validator, quota enforcer) means adding a field here
// rather than threading parameters through every function.
type Server struct {
	clientFor clientFactory
}

// RunGateway starts the HTTP gateway and blocks until the server exits. cfg
// is the manager's rest.Config; per-request copies replace its bearer token
// with the caller's. scheme and mapper are reused across requests so we
// don't pay K8s discovery cost on every call.
func RunGateway(addr string, cfg *rest.Config, scheme *runtime.Scheme, mapper meta.RESTMapper) error {
	return http.ListenAndServe(addr, NewHandler(cfg, scheme, mapper))
}

// NewHandler builds the gateway's HTTP handler. Separated from RunGateway so
// tests can exercise the routes without binding a socket.
func NewHandler(cfg *rest.Config, scheme *runtime.Scheme, mapper meta.RESTMapper) http.Handler {
	factory := func(token string) (client.Client, error) {
		impCfg := rest.CopyConfig(cfg)
		// Override every credential source — controller-runtime would prefer
		// any of these over BearerToken if left in place.
		impCfg.BearerToken = token
		impCfg.BearerTokenFile = ""
		impCfg.Username = ""
		impCfg.Password = ""
		impCfg.AuthProvider = nil
		impCfg.ExecProvider = nil
		return client.New(impCfg, client.Options{Scheme: scheme, Mapper: mapper})
	}
	return (&Server{clientFor: factory}).routes()
}

// routes wires the HTTP handlers. Public so tests with a custom clientFor
// can build a Server directly and exercise the same routing.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	// /healthz is intentionally unauthenticated — it's the liveness probe.
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
			s.handleListInstances(w, r)
		case http.MethodPost:
			s.handleCreateInstance(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	})
	mux.HandleFunc("/dbinstances/", func(w http.ResponseWriter, r *http.Request) {
		s.handleInstanceRoute(w, r)
	})
	return mux
}

// bearerToken extracts the token from an `Authorization: Bearer <token>`
// header. Returns ("", false) if the header is missing or doesn't match the
// expected shape.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	return token, token != ""
}

// requireClient pulls the bearer token from the request, builds a per-call
// K8s client signed with it, and writes a 401/500 if either step fails.
// Returns nil to signal "stop processing".
func (s *Server) requireClient(w http.ResponseWriter, r *http.Request) client.Client {
	token, ok := bearerToken(r)
	if !ok {
		writeError(w, http.StatusUnauthorized,
			"missing or invalid Authorization: Bearer <token>")
		return nil
	}
	c, err := s.clientFor(token)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("build client: %v", err))
		return nil
	}
	return c
}

// writeAPIError maps a Kubernetes API error to a sensible HTTP status, so
// 401 / 403 from the API server propagate back to the caller instead of
// collapsing to a generic 500.
func writeAPIError(w http.ResponseWriter, err error) {
	switch {
	case apierrors.IsNotFound(err):
		writeError(w, http.StatusNotFound, err.Error())
	case apierrors.IsAlreadyExists(err):
		writeError(w, http.StatusConflict, err.Error())
	case apierrors.IsUnauthorized(err):
		writeError(w, http.StatusUnauthorized, err.Error())
	case apierrors.IsForbidden(err):
		writeError(w, http.StatusForbidden, err.Error())
	case apierrors.IsBadRequest(err), apierrors.IsInvalid(err):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// defaultNamespace returns the namespace DBInstances are read from and
// written to. RBAC on the caller's token is what really decides whether the
// operation succeeds — this only picks where to look.
func defaultNamespace() string {
	if ns := os.Getenv("DBAAS_DEFAULT_NAMESPACE"); ns != "" {
		return ns
	}
	return "default"
}

// handleListInstances handles GET /dbinstances.
func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	c := s.requireClient(w, r)
	if c == nil {
		return
	}
	var instances dbaasv1.DBInstanceList
	if err := c.List(r.Context(), &instances, client.InNamespace(defaultNamespace())); err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, instances)
}

// handleCreateInstance handles POST /dbinstances — "create db".
func (s *Server) handleCreateInstance(w http.ResponseWriter, r *http.Request) {
	c := s.requireClient(w, r)
	if c == nil {
		return
	}
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
	// Always overwrite the namespace with the gateway's configured one.
	// Every other handler (get/patch/delete/start/stop) only looks in
	// defaultNamespace(); accepting a different value here would land the
	// new CR somewhere this gateway can never see again. The K8s API
	// server still does the final RBAC check on the caller's identity
	// for the chosen namespace.
	instance.Namespace = defaultNamespace()
	if err := c.Create(r.Context(), &instance); err != nil {
		writeAPIError(w, err)
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
	c := s.requireClient(w, r)
	if c == nil {
		return
	}
	var instance dbaasv1.DBInstance
	if err := c.Get(r.Context(), types.NamespacedName{Namespace: defaultNamespace(), Name: name}, &instance); err != nil {
		writeAPIError(w, err)
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
	c := s.requireClient(w, r)
	if c == nil {
		return
	}
	defer r.Body.Close()

	var req modifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	var instance dbaasv1.DBInstance
	if err := c.Get(r.Context(), types.NamespacedName{Namespace: defaultNamespace(), Name: name}, &instance); err != nil {
		writeAPIError(w, err)
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

	if err := c.Update(r.Context(), &instance); err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, instance)
}

// handleDeleteInstance handles DELETE /dbinstances/{name} — "delete db".
func (s *Server) handleDeleteInstance(w http.ResponseWriter, r *http.Request, name string) {
	c := s.requireClient(w, r)
	if c == nil {
		return
	}
	instance := &dbaasv1.DBInstance{}
	instance.Name = name
	instance.Namespace = defaultNamespace()
	instance.APIVersion = dbaasv1.GroupVersion.String()
	instance.Kind = "DBInstance"

	if err := c.Delete(r.Context(), instance); err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "deletion requested", "name": name})
}

// handleSetRunning handles POST /dbinstances/{name}/start and /stop —
// "start db" and "stop db". It flips spec.running; the controller drives the
// KubeVirt VM power state to match.
func (s *Server) handleSetRunning(w http.ResponseWriter, r *http.Request, name string, running bool) {
	c := s.requireClient(w, r)
	if c == nil {
		return
	}
	var instance dbaasv1.DBInstance
	if err := c.Get(r.Context(), types.NamespacedName{Namespace: defaultNamespace(), Name: name}, &instance); err != nil {
		writeAPIError(w, err)
		return
	}

	instance.Spec.Running = boolPtr(running)
	if err := c.Update(r.Context(), &instance); err != nil {
		writeAPIError(w, err)
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
