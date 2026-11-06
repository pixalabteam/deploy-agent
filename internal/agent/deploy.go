package agent

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/pixalabteam/deploy-agent/internal/dockerctl"
	"github.com/pixalabteam/deploy-agent/internal/nginxctl"
)

// Deployer bundles everything needed to run a deploy: Docker, nginx, on-disk
// state and configuration. One Deployer is shared by the whole process (see
// Server.deployMu) so two deploys never run at once, even across projects.
type Deployer struct {
	cfg    Config
	docker *dockerctl.Client
	state  *State
}

func NewDeployer(cfg Config, state *State) *Deployer {
	return &Deployer{cfg: cfg, docker: dockerctl.New(cfg.DockerBin), state: state}
}

func (d *Deployer) nginxFor(t Target) *nginxctl.Client {
	return &nginxctl.Client{
		UpstreamPath:   t.Svc.NginxUpstreamPath,
		UpstreamName:   t.Svc.NginxUpstreamName,
		StickySessions: t.Svc.StickySessions,
		TestCmd:        d.cfg.NginxTestCmd,
		ReloadCmd:      d.cfg.NginxReloadCmd,
	}
}

// InstanceCount is the number of instances to run: the admin-UI override when
// set, otherwise the configured instance_count.
func (d *Deployer) InstanceCount(t Target) int {
	return d.state.EffectiveCount(t.Project.Name, t.Service, t.Svc.InstanceCount)
}

func (d *Deployer) instance(t Target, i int) InstanceState {
	return d.state.Instance(t.Project.Name, t.Service, i)
}

func (d *Deployer) setInstance(t Target, i int, inst InstanceState) error {
	return d.state.SetInstance(t.Project.Name, t.Service, i, inst)
}

func otherColor(c string) string {
	if c == "blue" {
		return "green"
	}
	return "blue"
}

// portForColor: each slot has two fixed ports (blue, green), alternating on every
// deploy, so the new and old containers never compete for a port and the old one
// can stay alive until the new one passes its health check.
func portForColor(svc ServiceConfig, i int, color string) int {
	base := svc.BasePort + i*portsPerSlot
	if color == "green" {
		return base + 1
	}
	return base
}

// Deploy dispatches on the service kind.
func (d *Deployer) Deploy(ctx context.Context, t Target, image string) error {
	if t.Svc.Kind == KindWorker {
		return d.deployWorker(ctx, t, image)
	}
	return d.deployHTTP(ctx, t, image)
}

// instanceEnv builds the container environment: the service env, then the
// slot-specific layer (first instance vs the rest), sorted so runs are reproducible.
func instanceEnv(svc ServiceConfig, i int) []string {
	merged := map[string]string{}
	for k, v := range svc.Env {
		merged[k] = v
	}
	layer := svc.OtherInstancesEnv
	if i == 0 {
		layer = svc.FirstInstanceEnv
	}
	for k, v := range layer {
		merged[k] = v
	}
	if svc.Kind == KindHTTP {
		merged["PORT"] = strconv.Itoa(svc.ContainerPort)
		merged["HOST"] = "0.0.0.0"
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, k+"="+merged[k])
	}
	return env
}

// deployHTTP rolls out one instance at a time (sequentially): start the new
// container on the slot's free port, health-check it, then update nginx and stop
// the old container. It stops at the first failing instance: instances already
// moved to the new image keep it (partial rollout); the failing instance and the
// ones after it keep the old image.
func (d *Deployer) deployHTTP(ctx context.Context, t Target, image string) error {
	if err := d.docker.Pull(ctx, image); err != nil {
		return fmt.Errorf("pull image: %w", err)
	}

	for i := 0; i < d.InstanceCount(t); i++ {
		if err := d.deployInstance(ctx, t, i, image); err != nil {
			return fmt.Errorf("instance #%d: %w", i+1, err)
		}
	}
	return nil
}

func (d *Deployer) deployInstance(ctx context.Context, t Target, i int, image string) error {
	cur := d.instance(t, i)
	newColor := otherColor(cur.Color)
	if cur.Color == "" {
		newColor = "blue"
	}
	newPort := portForColor(t.Svc, i, newColor)
	newName := fmt.Sprintf("%s-%d-%s", t.Svc.ContainerPrefix, i+1, newColor)

	// Clean up leftovers if a previous deploy failed midway and left a container with the same name.
	if err := d.docker.RemoveForce(ctx, newName); err != nil {
		return fmt.Errorf("clean up stale container %s: %w", newName, err)
	}

	if err := d.docker.Run(ctx, dockerctl.RunOptions{
		Name:            newName,
		Image:           image,
		EnvFile:         t.Project.EnvFile,
		Env:             instanceEnv(t.Svc, i),
		Network:         t.Project.DockerNetwork,
		PublishHostPort: newPort,
		ContainerPort:   t.Svc.ContainerPort,
	}); err != nil {
		return fmt.Errorf("start new container: %w", err)
	}

	if err := d.waitHealthy(ctx, t.Svc, newPort); err != nil {
		logs := d.docker.Logs(ctx, newName, 80)
		_ = d.docker.RemoveForce(ctx, newName)
		return fmt.Errorf("health check failed, new container rolled back: %w\n--- new container log ---\n%s", err, logs)
	}

	if err := d.applyUpstream(ctx, t, d.InstanceCount(t), i, newPort); err != nil {
		logs := d.docker.Logs(ctx, newName, 40)
		_ = d.docker.RemoveForce(ctx, newName)
		return fmt.Errorf("apply nginx upstream: %w\n--- new container log ---\n%s", err, logs)
	}

	if cur.ContainerName != "" && cur.ContainerName != newName {
		if err := d.docker.RemoveForce(ctx, cur.ContainerName); err != nil {
			// The new container already receives traffic, so failing to remove the old one
			// should not count as a failed deploy; report it in the response for manual cleanup.
			return fmt.Errorf("deploy succeeded but the old container %s could not be removed (clean up manually): %w", cur.ContainerName, err)
		}
	}

	return d.setInstance(t, i, InstanceState{
		Color:         newColor,
		ContainerName: newName,
		Port:          newPort,
		Image:         image,
	})
}

// applyUpstream rewrites the FULL list of ports of every instance (not just the
// one that changed) because the upstream file is shared by the whole load balancer.
// changedInstance < 0 means no slot is being replaced.
func (d *Deployer) applyUpstream(ctx context.Context, t Target, count, changedInstance, changedPort int) error {
	var ports []int
	for i := 0; i < count; i++ {
		port := changedPort
		if i != changedInstance {
			// Instances never deployed (port 0) are skipped: nginx only points at
			// instances that actually have a running container.
			port = d.instance(t, i).Port
		}
		if port != 0 {
			ports = append(ports, port)
		}
	}
	return d.nginxFor(t).WriteAndApply(ctx, ports)
}

func (d *Deployer) waitHealthy(ctx context.Context, svc ServiceConfig, port int) error {
	deadline := time.Now().Add(time.Duration(svc.HealthCheckTimeoutSec) * time.Second)
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, svc.HealthCheckPath)
	client := &http.Client{Timeout: 5 * time.Second}

	var lastErr error
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					return nil
				}
				lastErr = fmt.Errorf("health check returned status %d", resp.StatusCode)
			} else {
				lastErr = err
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(svc.HealthCheckIntervalSec) * time.Second):
		}
	}
	return fmt.Errorf("timed out after %ds waiting for %s to become healthy: %w", svc.HealthCheckTimeoutSec, url, lastErr)
}

// deployWorker replaces the worker container with the new image. A worker has no
// HTTP endpoint to health-check, so "healthy" here means the container is still
// running (did not crash) after StabilizeSec seconds, enough to catch obvious
// startup failures (bad env, import errors) without waiting for a real job.
func (d *Deployer) deployWorker(ctx context.Context, t Target, image string) error {
	if err := d.docker.Pull(ctx, image); err != nil {
		return fmt.Errorf("pull image: %w", err)
	}

	cur := d.instance(t, 0)
	newColor := otherColor(cur.Color)
	if cur.Color == "" {
		newColor = "blue"
	}
	newName := fmt.Sprintf("%s-%s", t.Svc.ContainerPrefix, newColor)

	if err := d.docker.RemoveForce(ctx, newName); err != nil {
		return fmt.Errorf("clean up stale container %s: %w", newName, err)
	}

	if err := d.docker.Run(ctx, dockerctl.RunOptions{
		Name:    newName,
		Image:   image,
		EnvFile: t.Project.EnvFile,
		Network: t.Project.DockerNetwork,
		Env:     instanceEnv(t.Svc, 0),
	}); err != nil {
		return fmt.Errorf("start new container: %w", err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Duration(t.Svc.StabilizeSec) * time.Second):
	}

	running, err := d.docker.IsRunning(ctx, newName)
	if err != nil || !running {
		logs := d.docker.Logs(ctx, newName, 80)
		_ = d.docker.RemoveForce(ctx, newName)
		if err == nil {
			err = fmt.Errorf("container exited after %ds", t.Svc.StabilizeSec)
		}
		return fmt.Errorf("worker is not stable, rolled back: %w\n--- new container log ---\n%s", err, logs)
	}

	if cur.ContainerName != "" && cur.ContainerName != newName {
		if err := d.docker.RemoveForce(ctx, cur.ContainerName); err != nil {
			return fmt.Errorf("deploy succeeded but the old worker %s could not be removed (clean up manually): %w", cur.ContainerName, err)
		}
	}

	return d.setInstance(t, 0, InstanceState{
		Color:         newColor,
		ContainerName: newName,
		Image:         image,
	})
}
