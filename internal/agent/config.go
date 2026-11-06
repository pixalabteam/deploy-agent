package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	KindHTTP   = "http"
	KindWorker = "worker"
)

// maxInstances bounds scaling; it also sizes the port block each HTTP service
// reserves (10 ports per instance slot) so projects can be checked for overlap.
const maxInstances = 8

const portsPerSlot = 10

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

// ServiceConfig describes one deployable unit of a project: an HTTP service
// (N instances behind an nginx upstream, blue/green per slot) or a worker.
type ServiceConfig struct {
	Kind            string `json:"kind"`
	ContainerPrefix string `json:"container_prefix,omitempty"`

	// HTTP only.
	InstanceCount          int    `json:"instance_count,omitempty"`
	BasePort               int    `json:"base_port,omitempty"`
	ContainerPort          int    `json:"container_port,omitempty"`
	HealthCheckPath        string `json:"health_check_path,omitempty"`
	HealthCheckTimeoutSec  int    `json:"health_check_timeout_sec,omitempty"`
	HealthCheckIntervalSec int    `json:"health_check_interval_sec,omitempty"`
	NginxUpstreamName      string `json:"nginx_upstream_name,omitempty"`
	NginxUpstreamPath      string `json:"nginx_upstream_path,omitempty"`
	StickySessions         bool   `json:"sticky_sessions,omitempty"`

	// Worker only: how long the container must stay up to count as healthy.
	StabilizeSec int `json:"stabilize_sec,omitempty"`

	Env map[string]string `json:"env,omitempty"`
	// FirstInstanceEnv/OtherInstancesEnv are layered over Env for slot 0 / the
	// remaining slots (e.g. run migrations on one container only).
	FirstInstanceEnv  map[string]string `json:"first_instance_env,omitempty"`
	OtherInstancesEnv map[string]string `json:"other_instances_env,omitempty"`
}

// ProjectConfig groups services that share an env file, Docker network and image repository policy.
type ProjectConfig struct {
	Name           string `json:"-"`
	EnvFile        string `json:"env_file,omitempty"`
	DockerNetwork  string `json:"docker_network,omitempty"`
	ImageKeepCount int    `json:"image_keep_count,omitempty"`
	// WebhookSecret, when set, also authorizes webhook calls scoped to this
	// project, so a project's CI never holds the secret that can deploy others.
	WebhookSecret string                   `json:"webhook_secret,omitempty"`
	Services      map[string]ServiceConfig `json:"services"`
}

// Target is one resolved (project, service) pair.
type Target struct {
	Project ProjectConfig
	Service string
	Svc     ServiceConfig
}

// Config holds every deploy-agent parameter. Global parameters come from
// environment variables (systemd EnvironmentFile=); the project list is read from
// PROJECTS_FILE.
type Config struct {
	ListenAddr string
	// WebhookSecret verifies HMAC-SHA256 on webhook requests (header X-Signature-256)
	// for every project.
	WebhookSecret string

	DockerBin string
	StateFile string
	// QueueFile mirrors the pending job queue so a restart does not lose jobs;
	// empty disables persistence.
	QueueFile string

	NginxTestCmd   string
	NginxReloadCmd string

	AdminUsername string
	AdminPassword string

	// ImageKeepCount is the default number of newest images per repository that
	// survive post-deploy cleanup (current + previous, so rollback needs no pull).
	ImageKeepCount int
	// BuildCacheMaxAge is passed to `docker builder prune --filter until=`.
	BuildCacheMaxAge string

	// AgentUpdateURL is where the admin UI's "update agent" downloads the new binary
	// from; "{arch}" is replaced by GOARCH and "<url>.sha256" must hold its checksum.
	// AgentUpdateToken is sent as a Bearer token (private GitHub release assets).
	AgentUpdateURL   string
	AgentUpdateToken string

	// Defaults for services that do not set their own.
	HealthCheckTimeoutSec  int
	HealthCheckIntervalSec int
	WorkerStabilizeSec     int

	Projects map[string]ProjectConfig
}

func Load() (Config, error) {
	cfg := Config{
		ListenAddr:       getEnv("LISTEN_ADDR", ":8080"),
		WebhookSecret:    os.Getenv("DEPLOY_WEBHOOK_SECRET"),
		DockerBin:        getEnv("DOCKER_BIN", "docker"),
		StateFile:        getEnv("STATE_FILE", "/var/lib/deploy-agent/state.json"),
		NginxTestCmd:     getEnv("NGINX_TEST_CMD", "nginx -t"),
		NginxReloadCmd:   getEnv("NGINX_RELOAD_CMD", "systemctl reload nginx"),
		AdminUsername:    os.Getenv("ADMIN_USERNAME"),
		AdminPassword:    os.Getenv("ADMIN_PASSWORD"),
		BuildCacheMaxAge: getEnv("BUILD_CACHE_MAX_AGE", "72h"),
		AgentUpdateURL:   os.Getenv("AGENT_UPDATE_URL"),
		AgentUpdateToken: os.Getenv("AGENT_UPDATE_TOKEN"),
	}

	cfg.QueueFile = getEnv("QUEUE_FILE", filepath.Join(filepath.Dir(cfg.StateFile), "queue.json"))

	var err error
	if cfg.HealthCheckTimeoutSec, err = getEnvInt("HEALTH_CHECK_TIMEOUT_SEC", 60); err != nil {
		return cfg, err
	}
	if cfg.HealthCheckIntervalSec, err = getEnvInt("HEALTH_CHECK_INTERVAL_SEC", 2); err != nil {
		return cfg, err
	}
	if cfg.WorkerStabilizeSec, err = getEnvInt("WORKER_STABILIZE_SEC", 10); err != nil {
		return cfg, err
	}
	if cfg.ImageKeepCount, err = getEnvInt("IMAGE_KEEP_COUNT", 2); err != nil {
		return cfg, err
	}
	if cfg.WebhookSecret == "" {
		return cfg, fmt.Errorf("DEPLOY_WEBHOOK_SECRET is required")
	}

	path := os.Getenv("PROJECTS_FILE")
	if path == "" {
		return cfg, fmt.Errorf("PROJECTS_FILE is required")
	}
	if cfg.Projects, err = loadProjectsFile(path); err != nil {
		return cfg, err
	}
	return cfg, cfg.finalize()
}

func loadProjectsFile(path string) (map[string]ProjectConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read PROJECTS_FILE: %w", err)
	}
	var file struct {
		Projects map[string]ProjectConfig `json:"projects"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("parse PROJECTS_FILE %s: %w", path, err)
	}
	return file.Projects, nil
}

// finalize names projects, fills defaults and rejects configs that would make
// two services fight over a container name, a host port or an nginx file.
func (c *Config) finalize() error {
	if c.ImageKeepCount < 2 {
		return fmt.Errorf("IMAGE_KEEP_COUNT must be >= 2 to keep a rollback image")
	}
	if len(c.Projects) == 0 {
		return fmt.Errorf("no projects configured")
	}

	prefixes := map[string]string{}
	upstreamNames := map[string]string{}
	upstreamPaths := map[string]string{}
	type portBlock struct{ from, to int }
	blocks := map[string]portBlock{}

	for name, p := range c.Projects {
		if !nameRe.MatchString(name) {
			return fmt.Errorf("project name %q must match %s", name, nameRe)
		}
		p.Name = name
		if p.EnvFile == "" {
			p.EnvFile = "/etc/deploy-agent/" + name + ".env"
		}
		if p.ImageKeepCount == 0 {
			p.ImageKeepCount = c.ImageKeepCount
		}
		if p.ImageKeepCount < 2 {
			return fmt.Errorf("project %s: image_keep_count must be >= 2", name)
		}
		if len(p.Services) == 0 {
			return fmt.Errorf("project %s has no services", name)
		}

		for svcName, svc := range p.Services {
			where := name + "/" + svcName
			if !nameRe.MatchString(svcName) {
				return fmt.Errorf("service name %q in project %s must match %s", svcName, name, nameRe)
			}
			if svc.ContainerPrefix == "" {
				svc.ContainerPrefix = name + "-" + svcName
			}
			if other, dup := prefixes[svc.ContainerPrefix]; dup {
				return fmt.Errorf("%s and %s share container_prefix %q", where, other, svc.ContainerPrefix)
			}
			prefixes[svc.ContainerPrefix] = where

			switch svc.Kind {
			case KindWorker:
				if svc.StabilizeSec == 0 {
					svc.StabilizeSec = c.WorkerStabilizeSec
				}
			case KindHTTP:
				if err := c.finalizeHTTP(name, svcName, &svc); err != nil {
					return err
				}
				if other, dup := upstreamNames[svc.NginxUpstreamName]; dup {
					return fmt.Errorf("%s and %s share nginx_upstream_name %q", where, other, svc.NginxUpstreamName)
				}
				upstreamNames[svc.NginxUpstreamName] = where
				if other, dup := upstreamPaths[svc.NginxUpstreamPath]; dup {
					return fmt.Errorf("%s and %s share nginx_upstream_path %q", where, other, svc.NginxUpstreamPath)
				}
				upstreamPaths[svc.NginxUpstreamPath] = where
				blocks[where] = portBlock{svc.BasePort, svc.BasePort + maxInstances*portsPerSlot}
			default:
				return fmt.Errorf("%s: kind must be %q or %q", where, KindHTTP, KindWorker)
			}
			p.Services[svcName] = svc
		}
		c.Projects[name] = p
	}

	wheres := make([]string, 0, len(blocks))
	for w := range blocks {
		wheres = append(wheres, w)
	}
	sort.Strings(wheres)
	for i, a := range wheres {
		for _, b := range wheres[i+1:] {
			if blocks[a].from < blocks[b].to && blocks[b].from < blocks[a].to {
				return fmt.Errorf("%s (ports %d-%d) and %s (ports %d-%d) overlap; keep base_port at least %d apart",
					a, blocks[a].from, blocks[a].to-1, b, blocks[b].from, blocks[b].to-1, maxInstances*portsPerSlot)
			}
		}
	}
	return nil
}

func (c *Config) finalizeHTTP(project, service string, svc *ServiceConfig) error {
	where := project + "/" + service
	if svc.BasePort <= 0 {
		return fmt.Errorf("%s: base_port is required for an http service", where)
	}
	if svc.InstanceCount == 0 {
		svc.InstanceCount = 1
	}
	if svc.InstanceCount < 1 || svc.InstanceCount > maxInstances {
		return fmt.Errorf("%s: instance_count must be between 1 and %d", where, maxInstances)
	}
	if svc.ContainerPort == 0 {
		svc.ContainerPort = 3000
	}
	if svc.HealthCheckPath == "" {
		svc.HealthCheckPath = "/"
	}
	if svc.HealthCheckTimeoutSec == 0 {
		svc.HealthCheckTimeoutSec = c.HealthCheckTimeoutSec
	}
	if svc.HealthCheckIntervalSec == 0 {
		svc.HealthCheckIntervalSec = c.HealthCheckIntervalSec
	}
	if svc.NginxUpstreamName == "" {
		svc.NginxUpstreamName = strings.ReplaceAll(project+"_"+service, "-", "_")
	}
	if svc.NginxUpstreamPath == "" {
		svc.NginxUpstreamPath = "/etc/nginx/conf.d/" + project + "-" + service + "-upstream.conf"
	}
	return nil
}

// ProjectNames returns project names in stable order.
func (c Config) ProjectNames() []string {
	names := make([]string, 0, len(c.Projects))
	for n := range c.Projects {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ResolveProject maps an empty project to the only configured project; with
// several projects the name is required.
func (c Config) ResolveProject(name string) (string, error) {
	if name != "" {
		if _, ok := c.Projects[name]; !ok {
			return "", fmt.Errorf("unknown project %q", name)
		}
		return name, nil
	}
	if len(c.Projects) == 1 {
		return c.ProjectNames()[0], nil
	}
	return "", fmt.Errorf(`"project" is required (one of: %s)`, strings.Join(c.ProjectNames(), ", "))
}

func (c Config) Target(project, service string) (Target, error) {
	project, err := c.ResolveProject(project)
	if err != nil {
		return Target{}, err
	}
	p := c.Projects[project]
	svc, ok := p.Services[service]
	if !ok {
		return Target{}, fmt.Errorf("unknown service %q in project %s", service, project)
	}
	return Target{Project: p, Service: service, Svc: svc}, nil
}

// ServiceNames returns a project's service names in stable order.
func (p ProjectConfig) ServiceNames() []string {
	names := make([]string, 0, len(p.Services))
	for n := range p.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// secretsFor lists the secrets allowed to sign a request for project ("" = no
// specific project): the global one, plus the project's own when it has one.
func (c Config) secretsFor(project string) []string {
	secrets := []string{c.WebhookSecret}
	if p, ok := c.Projects[project]; ok && p.WebhookSecret != "" {
		secrets = append(secrets, p.WebhookSecret)
	}
	return secrets
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return n, nil
}
