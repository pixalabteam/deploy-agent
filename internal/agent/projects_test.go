package agent

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

const testProject = "app"

func twoProjectConfig(t *testing.T) Config {
	t.Helper()
	cfg := Config{
		WebhookSecret: "global", ImageKeepCount: 2, HealthCheckTimeoutSec: 5, HealthCheckIntervalSec: 1, WorkerStabilizeSec: 1,
		Projects: map[string]ProjectConfig{
			testProject: {Services: map[string]ServiceConfig{"api": {Kind: KindHTTP, BasePort: 3101}}},
			"shop":      {WebhookSecret: "shop-secret", Services: map[string]ServiceConfig{"web": {Kind: KindHTTP, BasePort: 4000}, "jobs": {Kind: KindWorker}}},
		},
	}
	if err := cfg.finalize(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestFinalizeFillsDefaultsPerProject(t *testing.T) {
	cfg := twoProjectConfig(t)
	web := cfg.Projects["shop"].Services["web"]
	if web.ContainerPrefix != "shop-web" || web.NginxUpstreamName != "shop_web" ||
		web.NginxUpstreamPath != "/etc/nginx/conf.d/shop-web-upstream.conf" || web.InstanceCount != 1 || web.HealthCheckPath != "/" {
		t.Errorf("unexpected defaults: %+v", web)
	}
	if cfg.Projects["shop"].EnvFile != "/etc/deploy-agent/shop.env" {
		t.Errorf("env file default = %q", cfg.Projects["shop"].EnvFile)
	}
	if cfg.Projects["shop"].Services["jobs"].StabilizeSec != 1 {
		t.Error("worker should inherit WORKER_STABILIZE_SEC")
	}
}

func TestFinalizeRejectsCollisions(t *testing.T) {
	cases := map[string]func(*Config){
		"overlapping ports": func(c *Config) {
			s := c.Projects["shop"].Services["web"]
			s.BasePort = 3150
			c.Projects["shop"].Services["web"] = s
		},
		"duplicate prefix": func(c *Config) {
			s := c.Projects["shop"].Services["web"]
			s.ContainerPrefix = "app-api"
			c.Projects["shop"].Services["web"] = s
		},
		"unknown kind": func(c *Config) {
			c.Projects["shop"].Services["web"] = ServiceConfig{Kind: "cron"}
		},
		"missing base port": func(c *Config) {
			c.Projects["shop"].Services["web"] = ServiceConfig{Kind: KindHTTP}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := Config{WebhookSecret: "g", ImageKeepCount: 2, HealthCheckTimeoutSec: 5, HealthCheckIntervalSec: 1,
				Projects: map[string]ProjectConfig{
					testProject: {Services: map[string]ServiceConfig{"api": {Kind: KindHTTP, BasePort: 3101, ContainerPrefix: "app-api"}}},
					"shop":      {Services: map[string]ServiceConfig{"web": {Kind: KindHTTP, BasePort: 4000}}},
				}}
			mutate(&cfg)
			if err := cfg.finalize(); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestResolveProject(t *testing.T) {
	cfg := twoProjectConfig(t)
	if _, err := cfg.ResolveProject(""); err == nil {
		t.Error("empty project must fail when several are configured")
	}
	if _, err := cfg.ResolveProject("nope"); err == nil {
		t.Error("unknown project must fail")
	}
	delete(cfg.Projects, testProject)
	if got, _ := cfg.ResolveProject(""); got != "shop" {
		t.Errorf("sole project -> %q, want shop", got)
	}
	cfg.Projects["blog"] = ProjectConfig{}
	if _, err := cfg.ResolveProject(""); err == nil {
		t.Error("ambiguous project must fail")
	}
}

func TestLoadProjectsFileRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projects.json")
	os.WriteFile(path, []byte(`{"projects":{"shop":{"services":{"web":{"kind":"http","base_port":4000,"helth_check_path":"/x"}}}}}`), 0o600)
	if _, err := loadProjectsFile(path); err == nil {
		t.Error("a typo in a key must be reported, not silently ignored")
	}
}

func deployRequestTo(srv *Server, method, path, body, secret string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Signature-256", sign(secret, body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestProjectSecretOnlyAuthorizesItsOwnProject(t *testing.T) {
	srv := NewServer(twoProjectConfig(t), &State{path: filepath.Join(t.TempDir(), "state.json")})

	other := `{"project":"app","service":"api"}`
	if rec := deployRequestTo(srv, "POST", "/deploy", other, "shop-secret"); rec.Code != http.StatusUnauthorized {
		t.Errorf("shop secret on app: got %d, want 401", rec.Code)
	}
	noProject := `{"service":"api"}`
	if rec := deployRequestTo(srv, "POST", "/deploy", noProject, "shop-secret"); rec.Code != http.StatusUnauthorized {
		t.Errorf("shop secret with default project: got %d, want 401", rec.Code)
	}
	// Right project, wrong payload: it passes auth and fails validation instead.
	own := `{"project":"shop","service":"nope","image":"x"}`
	if rec := deployRequestTo(srv, "POST", "/deploy", own, "shop-secret"); rec.Code != http.StatusBadRequest {
		t.Errorf("shop secret on shop: got %d, want 400", rec.Code)
	}
	if rec := deployRequestTo(srv, "POST", "/deploy", other, "global"); rec.Code != http.StatusBadRequest { // no image
		t.Errorf("global secret: got %d, want 400", rec.Code)
	}
}

func TestDeployStatusIsPerProject(t *testing.T) {
	srv := NewServer(twoProjectConfig(t), &State{path: filepath.Join(t.TempDir(), "state.json")})
	srv.setDeployment(deployResponse{Status: "ok", Project: "shop", Service: "web", Image: "shop:1"})
	srv.setDeployment(deployResponse{Status: "error", Project: testProject, Service: "api", Image: "api:2"})

	check := func(path, wantProject, wantStatus string) {
		t.Helper()
		rec := deployRequestTo(srv, "GET", path, "", "global")
		body := rec.Body.String()
		if rec.Code != http.StatusOK || !strings.Contains(body, `"project":"`+wantProject+`"`) || !strings.Contains(body, `"status":"`+wantStatus+`"`) {
			t.Errorf("%s: got %d %s", path, rec.Code, body)
		}
	}
	check("/deploy/status", testProject, "error") // no project given: latest overall
	check("/deploy/status?project=shop", "shop", "ok")
	if rec := deployRequestTo(srv, "GET", "/deploy/status?project=nope", "", "global"); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown project: got %d, want 400", rec.Code)
	}
}
