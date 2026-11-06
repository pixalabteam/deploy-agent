package agent

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

func newTestServer(t *testing.T, apiBasePort int) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	log := dir + "/docker.log"
	docker := dir + "/docker"
	if err := os.WriteFile(docker, []byte("#!/bin/sh\necho \"$@\" >> "+log+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		DockerBin: docker, AdminUsername: "admin", AdminPassword: "pw", WebhookSecret: "global-secret",
		NginxTestCmd: "true", NginxReloadCmd: "true",
		ImageKeepCount: 2, BuildCacheMaxAge: "72h",
		HealthCheckTimeoutSec: 5, HealthCheckIntervalSec: 1, WorkerStabilizeSec: 1,
		Projects: map[string]ProjectConfig{
			testProject: {Services: map[string]ServiceConfig{
				"api": {Kind: KindHTTP, ContainerPrefix: "api", InstanceCount: 1, BasePort: apiBasePort, ContainerPort: 3333, NginxUpstreamPath: dir + "/upstream.conf"},
			}},
		},
	}
	if err := cfg.finalize(); err != nil {
		t.Fatal(err)
	}
	state := &State{path: dir + "/state.json"}
	return NewServer(cfg, state), log
}

func apiTarget(t *testing.T, srv *Server) Target {
	t.Helper()
	target, err := srv.cfg.Target("", "api")
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func adminRequest(srv *Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.SetBasicAuth("admin", "pw")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAdminMutationRejectsCrossSiteRequests(t *testing.T) {
	srv, _ := newTestServer(t, 40000)
	if rec := adminRequest(srv, "POST", "/admin/api/scale", `{"service":"api","count":2}`, nil); rec.Code != http.StatusForbidden {
		t.Errorf("missing header: got %d, want 403", rec.Code)
	}
	rec := adminRequest(srv, "POST", "/admin/api/scale", `{"service":"api","count":2}`, map[string]string{"X-Admin-Request": "1", "Origin": "https://evil.example"})
	if rec.Code != http.StatusForbidden {
		t.Errorf("foreign origin: got %d, want 403", rec.Code)
	}
}

func TestAdminRedeployWithoutDeployedImageConflicts(t *testing.T) {
	srv, _ := newTestServer(t, 40000)
	rec := adminRequest(srv, "POST", "/admin/api/redeploy", `{"service":"api"}`, map[string]string{"X-Admin-Request": "1"})
	if rec.Code != http.StatusConflict {
		t.Errorf("got %d, want 409", rec.Code)
	}
}

func TestScaleUpThenDown(t *testing.T) {
	healthy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	healthy.Listener = listener
	healthy.Start()
	defer healthy.Close()
	port := listener.Addr().(*net.TCPAddr).Port

	// Slot 1's "blue" port is base+10, which is where the fake healthy server listens.
	srv, logFile := newTestServer(t, port-10)
	if err := srv.state.SetInstance(testProject, "api", 0, InstanceState{Color: "blue", ContainerName: "api-1-blue", Port: port - 10, Image: "img:1"}); err != nil {
		t.Fatal(err)
	}

	if err := srv.deployer.Scale(t.Context(), apiTarget(t, srv), 2); err != nil {
		t.Fatalf("scale up: %v", err)
	}
	if got := srv.deployer.InstanceCount(apiTarget(t, srv)); got != 2 {
		t.Fatalf("count after scale up = %d, want 2", got)
	}
	upstream, _ := os.ReadFile(apiTarget(t, srv).Svc.NginxUpstreamPath)
	if !strings.Contains(string(upstream), "127.0.0.1:"+itoa(port-10)) || !strings.Contains(string(upstream), "127.0.0.1:"+itoa(port)) {
		t.Errorf("upstream should list both instances:\n%s", upstream)
	}

	if err := srv.deployer.Scale(t.Context(), apiTarget(t, srv), 1); err != nil {
		t.Fatalf("scale down: %v", err)
	}
	if got := srv.deployer.InstanceCount(apiTarget(t, srv)); got != 1 {
		t.Fatalf("count after scale down = %d, want 1", got)
	}
	upstream, _ = os.ReadFile(apiTarget(t, srv).Svc.NginxUpstreamPath)
	if strings.Contains(string(upstream), "127.0.0.1:"+itoa(port)+" ") || strings.Contains(string(upstream), "127.0.0.1:"+itoa(port)+";") {
		t.Errorf("upstream should no longer list the removed instance:\n%s", upstream)
	}
	calls, _ := os.ReadFile(logFile)
	if !strings.Contains(string(calls), "rm -f api-2-blue") {
		t.Errorf("removed instance container not stopped:\n%s", calls)
	}

	if err := srv.deployer.Scale(t.Context(), apiTarget(t, srv), 0); err == nil {
		t.Error("scaling to 0 must be rejected")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestAdminPageUsesRelativeAPIPaths(t *testing.T) {
	srv, _ := newTestServer(t, 40000)
	body := adminRequest(srv, "GET", "/admin/", "", nil).Body.String()
	if strings.Contains(body, `fetch("/admin`) {
		t.Error("admin page must call the API with relative URLs so it works behind an nginx prefix")
	}
}

func TestAdminMutationAcceptsProxiedSameOrigin(t *testing.T) {
	srv, _ := newTestServer(t, 40000)
	cases := map[string]map[string]string{
		"sec-fetch-site":   {"X-Admin-Request": "1", "Origin": "https://api.example.com", "Sec-Fetch-Site": "same-origin"},
		"x-forwarded-host": {"X-Admin-Request": "1", "Origin": "https://api.example.com", "X-Forwarded-Host": "api.example.com"},
	}
	for name, headers := range cases {
		rec := adminRequest(srv, "POST", "/admin/api/redeploy", `{"service":"api"}`, headers)
		if rec.Code != http.StatusConflict { // passed the guard, then "no image deployed yet"
			t.Errorf("%s: got %d, want 409", name, rec.Code)
		}
	}
}
