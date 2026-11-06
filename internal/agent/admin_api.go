package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"
)

// withAdminMutation guards state-changing admin endpoints. Basic Auth credentials
// are re-sent by the browser automatically, so a malicious page could otherwise
// POST here (CSRF). The custom header cannot be set cross-origin without a CORS
// preflight (which this server never answers), and Origin must match Host.
func (s *Server) withAdminMutation(next http.HandlerFunc) http.HandlerFunc {
	return s.withBasicAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Admin-Request") != "1" {
			httpError(w, http.StatusForbidden, "missing X-Admin-Request header")
			return
		}
		if !sameOrigin(r) {
			httpError(w, http.StatusForbidden, "Origin mismatch")
			return
		}
		next(w, r)
	})
}

// sameOrigin accepts a request when the browser says it is same-origin
// (Sec-Fetch-Site) or its Origin host matches the Host the client used. Behind
// nginx r.Host is usually the upstream address (127.0.0.1:8080) unless nginx
// forwards Host, so X-Forwarded-Host is accepted too. Trusting that header is
// safe here: a cross-site page cannot set it without a CORS preflight.
func sameOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "same-origin" {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host || u.Host == r.Header.Get("X-Forwarded-Host")
}

// adminProject is what the dashboard needs to draw one project; secrets are
// cleared so they never reach the browser.
type adminProject struct {
	Name     string         `json:"name"`
	Config   ProjectConfig  `json:"config"`
	Services []adminService `json:"services"`
}

type adminService struct {
	Name      string          `json:"name"`
	Kind      string          `json:"kind"`
	Count     int             `json:"count"`
	Instances []InstanceState `json:"instances"`
}

func (s *Server) handleAdminOverview(w http.ResponseWriter, r *http.Request) {
	snapshot := s.state.Snapshot()
	projects := make([]adminProject, 0, len(s.cfg.Projects))
	for _, name := range s.cfg.ProjectNames() {
		p := s.cfg.Projects[name]
		view := adminProject{Name: name, Config: p}
		view.Config.WebhookSecret = ""
		for _, svcName := range p.ServiceNames() {
			t := Target{Project: p, Service: svcName, Svc: p.Services[svcName]}
			svc := adminService{Name: svcName, Kind: t.Svc.Kind, Instances: []InstanceState{}}
			if st := snapshot[name][svcName]; st != nil {
				svc.Instances = st.Instances
			}
			if t.Svc.Kind == KindHTTP {
				svc.Count = s.deployer.InstanceCount(t)
			}
			view.Services = append(view.Services, svc)
		}
		projects = append(projects, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deployment": s.deploymentFor(""),
		"projects":   projects,
	})
}

func (s *Server) handleAdminResources(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.deployer.CollectResources(r.Context()))
}

func (s *Server) handleAdminRedeploy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Project string `json:"project"`
		Service string `json:"service"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "body is not valid JSON")
		return
	}
	target, err := s.cfg.Target(req.Project, req.Service)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	image := s.state.Instance(target.Project.Name, target.Service, 0).Image
	if image == "" {
		httpError(w, http.StatusConflict, "no image has been deployed yet, nothing to redeploy")
		return
	}
	s.handleDeploy(w, r, mustJSON(deployRequest{Project: target.Project.Name, Service: req.Service, Image: image}))
}

func (s *Server) handleAdminScale(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Project string `json:"project"`
		Service string `json:"service"`
		Count   int    `json:"count"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "body is not valid JSON")
		return
	}
	target, err := s.cfg.Target(req.Project, req.Service)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if target.Svc.Kind != KindHTTP {
		httpError(w, http.StatusBadRequest, "only http services can be scaled")
		return
	}
	if req.Count < 1 || req.Count > maxInstances {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("count must be between 1 and %d", maxInstances))
		return
	}
	desc, ok := s.enqueue(jobSpec{Kind: jobScale, Project: target.Project.Name, Service: req.Service, Count: req.Count})
	if !ok {
		httpError(w, http.StatusServiceUnavailable, "deploy queue is full, try again later")
		return
	}
	writeJSON(w, http.StatusAccepted, desc)
}

// handleAdminUpdate queues a self-update: it replaces the agent binary, then exits
// so systemd restarts it. The exit runs inside the queue worker, so no job starts
// in between; jobs queued behind it are restored from the queue file after the restart.
func (s *Server) handleAdminUpdate(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AgentUpdateURL == "" {
		httpError(w, http.StatusConflict, "AGENT_UPDATE_URL is not set")
		return
	}
	// Not persisted: it ends in a restart by design, so replaying it would loop.
	desc, ok := s.startJob(job{
		desc: deployResponse{Service: "agent", Image: s.cfg.updateURL()},
		fn:   s.cfg.SelfUpdate,
		afterSuccess: func() {
			log.Printf("self-update done, exiting so systemd restarts the new version")
			time.Sleep(2 * time.Second)
			os.Exit(updateExitCode)
		},
	})
	if !ok {
		httpError(w, http.StatusServiceUnavailable, "deploy queue is full, try again later")
		return
	}
	writeJSON(w, http.StatusAccepted, desc)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
