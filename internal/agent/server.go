package agent

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
)

type Server struct {
	cfg      Config
	deployer *Deployer
	state    *State

	// Jobs (deploy, scale, self-update of any project) run strictly one at a time,
	// because they share the state file and nginx reloads. Extra requests wait in
	// this FIFO queue instead of being rejected. Deploy and scale jobs are mirrored
	// to Config.QueueFile so a restart of the agent or the server loses none.
	queueMu       sync.Mutex
	queue         []job
	current       *job // job being run by the worker, nil when idle
	workerRunning bool

	// deployment is the result of the most recent deploy (running / ok / error), kept
	// in memory only: lost on agent restart; the source of truth for containers is the state file.
	deploymentMu sync.Mutex
	deployment   deployResponse
	// deploymentByProject keeps the latest result per project so one project's
	// CI polls its own deploy rather than whichever project deployed last.
	deploymentByProject map[string]deployResponse
}

func NewServer(cfg Config, state *State) *Server {
	return &Server{cfg: cfg, deployer: NewDeployer(cfg, state), state: state}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /status", s.withAuth(s.handleStatus))
	mux.HandleFunc("GET /deploy/status", s.withAuth(s.handleDeployStatus))
	mux.HandleFunc("POST /deploy", s.withAuth(s.handleDeploy))
	mux.HandleFunc("GET /admin/", s.withBasicAuth(s.handleAdmin))
	mux.HandleFunc("GET /admin/api/overview", s.withBasicAuth(s.handleAdminOverview))
	mux.HandleFunc("GET /admin/api/resources", s.withBasicAuth(s.handleAdminResources))
	mux.HandleFunc("POST /admin/api/redeploy", s.withAdminMutation(s.handleAdminRedeploy))
	mux.HandleFunc("POST /admin/api/scale", s.withAdminMutation(s.handleAdminScale))
	mux.HandleFunc("POST /admin/api/update", s.withAdminMutation(s.handleAdminUpdate))
	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// withAuth verifies HMAC-SHA256 over the raw body, header `X-Signature-256: sha256=<hex>`
// (the same scheme GitHub uses for webhooks) so workflows never expose the secret in
// a URL or log. The whole body is read into memory first because the hash needs the original bytes.
// The project is read from the body/query only to choose which secrets may sign; the
// handler uses that same value, so project A's secret cannot deploy project B.
func (s *Server) withAuth(next func(w http.ResponseWriter, r *http.Request, body []byte)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			httpError(w, http.StatusBadRequest, "cannot read body")
			return
		}

		sig := r.Header.Get("X-Signature-256")
		project, _ := s.cfg.ResolveProject(requestedProject(r, body))
		if !validSignature(s.cfg.secretsFor(project), body, sig) {
			log.Printf("auth failed from %s", r.RemoteAddr)
			httpError(w, http.StatusUnauthorized, "invalid signature")
			return
		}
		next(w, r, body)
	}
}

// requestedProject reads the project a request targets: "project" in a JSON
// body (POST) or in the query string (GET). It does not trust or validate it.
func requestedProject(r *http.Request, body []byte) string {
	if q := r.URL.Query().Get("project"); q != "" {
		return q
	}
	var req struct {
		Project string `json:"project"`
	}
	_ = json.Unmarshal(body, &req)
	return req.Project
}

func validSignature(secrets []string, body []byte, header string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	given, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	for _, secret := range secrets {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		if hmac.Equal(given, mac.Sum(nil)) {
			return true
		}
	}
	return false
}

type deployRequest struct {
	// Project may be omitted when exactly one project is configured.
	Project string `json:"project,omitempty"`
	Service string `json:"service"`
	Image   string `json:"image"`
}

type deployResponse struct {
	Status string `json:"status"` // queued | running | ok | error | idle
	// QueuePosition is 0 for the running job and 1, 2, ... for jobs waiting behind it.
	QueuePosition int    `json:"queue_position,omitempty"`
	Project       string `json:"project,omitempty"`
	Service       string `json:"service"`
	Image         string `json:"image"`
	Error         string `json:"error,omitempty"`
}

// handleDeploy returns 202 immediately and deploys in the background: a real deploy takes
// minutes, longer than nginx's proxy_read_timeout (60s) or Cloudflare's ~100s limit, so
// holding the connection open would 504 even though the deploy keeps running. Clients poll GET /deploy/status.
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request, body []byte) {
	var req deployRequest
	if err := json.Unmarshal(body, &req); err != nil {
		httpError(w, http.StatusBadRequest, "body is not valid JSON")
		return
	}
	if req.Image == "" {
		httpError(w, http.StatusBadRequest, "missing \"image\"")
		return
	}
	target, err := s.cfg.Target(req.Project, req.Service)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	desc, ok := s.enqueue(jobSpec{Kind: jobDeploy, Project: target.Project.Name, Service: req.Service, Image: req.Image})
	if !ok {
		httpError(w, http.StatusServiceUnavailable, "deploy queue is full, try again later")
		return
	}
	writeJSON(w, http.StatusAccepted, desc)
}

func (s *Server) setDeployment(d deployResponse) {
	s.deploymentMu.Lock()
	defer s.deploymentMu.Unlock()
	if d.Status != "queued" || s.deployment.Status != "running" {
		s.deployment = d
	}
	if d.Project != "" {
		if s.deploymentByProject == nil {
			s.deploymentByProject = map[string]deployResponse{}
		}
		s.deploymentByProject[d.Project] = d
	}
}

// deploymentFor returns the latest result of a project, or of any project when
// project is empty (what a client written before multi-project support expects).
func (s *Server) deploymentFor(project string) deployResponse {
	s.deploymentMu.Lock()
	defer s.deploymentMu.Unlock()
	d := s.deployment
	if project != "" {
		d = s.deploymentByProject[project]
	}
	if d.Status == "" {
		d.Status = "idle"
	}
	return d
}

func (s *Server) handleDeployStatus(w http.ResponseWriter, r *http.Request, _ []byte) {
	project := r.URL.Query().Get("project")
	if project != "" {
		if _, err := s.cfg.ResolveProject(project); err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, s.deploymentFor(project))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request, _ []byte) {
	writeJSON(w, http.StatusOK, map[string]any{"projects": s.state.Snapshot()})
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"status": "error", "error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
