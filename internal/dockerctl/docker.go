// Package dockerctl drives Docker Engine by exec'ing the `docker` CLI (already
// present on any host running containers) instead of the Docker Engine API/Go SDK:
// fewer dependencies, and the exact commands are visible in the agent's log.
package dockerctl

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type Client struct {
	Bin string
}

func New(bin string) *Client {
	if bin == "" {
		bin = "docker"
	}
	return &Client{Bin: bin}
}

func (c *Client) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.String(), fmt.Errorf("docker %s: %w (stderr: %s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func (c *Client) Pull(ctx context.Context, image string) error {
	_, err := c.run(ctx, "pull", image)
	return err
}

// RunOptions describes a new container. Env is a list of "KEY=VALUE" appended
// AFTER --env-file, so it can override variables from the env file when needed
// (e.g. PROCESS_TYPE/MIGRATE).
type RunOptions struct {
	Name            string
	Image           string
	EnvFile         string
	Env             []string
	PublishHostPort int // 0 = do not publish a port (worker)
	ContainerPort   int
	Network         string
	RestartPolicy   string // defaults to "unless-stopped"
}

func (c *Client) Run(ctx context.Context, opts RunOptions) error {
	args := []string{"run", "-d", "--name", opts.Name}
	restart := opts.RestartPolicy
	if restart == "" {
		restart = "unless-stopped"
	}
	args = append(args, "--restart", restart)
	if opts.EnvFile != "" {
		args = append(args, "--env-file", opts.EnvFile)
	}
	for _, kv := range opts.Env {
		args = append(args, "-e", kv)
	}
	if opts.Network != "" {
		args = append(args, "--network", opts.Network)
	}
	if opts.PublishHostPort > 0 {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", opts.PublishHostPort, opts.ContainerPort))
	}
	args = append(args, opts.Image)
	_, err := c.run(ctx, args...)
	return err
}

// RemoveForce stops and removes a container if it exists; "no such container" is
// not an error because the agent also calls it to clean up old containers that may be gone.
func (c *Client) RemoveForce(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	_, err := c.run(ctx, "rm", "-f", name)
	if err != nil && strings.Contains(err.Error(), "No such container") {
		return nil
	}
	return err
}

// IsRunning reports whether the container is in the "running" state. Used to
// confirm a new worker did not crash right after start (a worker has no HTTP
// health endpoint to check like an HTTP service does).
func (c *Client) IsRunning(ctx context.Context, name string) (bool, error) {
	out, err := c.run(ctx, "inspect", "-f", "{{.State.Running}}", name)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "true", nil
}

// Logs returns the last N log lines, attached to errors when a new container is
// unhealthy or fails to run, so failures can be debugged from the webhook response without SSH.
func (c *Client) Logs(ctx context.Context, name string, tailLines int) string {
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := c.run(lctx, "logs", "--tail", fmt.Sprintf("%d", tailLines), name)
	if err != nil {
		return fmt.Sprintf("(could not fetch logs: %v)", err)
	}
	return out
}

// RepositoryOf strips the tag and digest from an image reference, e.g.
// "ghcr.io/org/repo:abc123" -> "ghcr.io/org/repo". A registry port such as
// "localhost:5000/app" is preserved because only a colon after the last slash
// counts as a tag separator.
func RepositoryOf(image string) string {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		image = image[:i]
	}
	return image
}

// ListImageRefs returns "repo:tag" references of a repository, newest first
// (the order `docker image ls` already uses). Untagged (dangling) images are
// left to PruneDanglingImages.
func (c *Client) ListImageRefs(ctx context.Context, repository string) ([]string, error) {
	out, err := c.run(ctx, "image", "ls", repository, "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasSuffix(line, ":<none>") {
			continue
		}
		refs = append(refs, line)
	}
	return refs, nil
}

// RemoveImage deletes one image reference without --force, so Docker refuses
// to delete an image that a container (even a stopped one) still uses.
func (c *Client) RemoveImage(ctx context.Context, ref string) error {
	_, err := c.run(ctx, "rmi", ref)
	return err
}

func (c *Client) PruneDanglingImages(ctx context.Context) error {
	_, err := c.run(ctx, "image", "prune", "-f")
	return err
}

// PruneBuildCache never touches volumes; `docker builder prune` only manages
// build cache.
func (c *Client) PruneBuildCache(ctx context.Context, olderThan string) error {
	_, err := c.run(ctx, "builder", "prune", "-af", "--filter", "until="+olderThan)
	return err
}

// ContainerStat is one row of `docker stats --no-stream`.
type ContainerStat struct {
	Name    string `json:"name"`
	CPU     string `json:"cpu"`
	Memory  string `json:"memory"`
	MemPerc string `json:"mem_percent"`
	NetIO   string `json:"net_io"`
	BlockIO string `json:"block_io"`
	PIDs    string `json:"pids"`
}

func (c *Client) Stats(ctx context.Context) ([]ContainerStat, error) {
	out, err := c.run(ctx, "stats", "--no-stream", "--format",
		"{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.MemPerc}}\t{{.NetIO}}\t{{.BlockIO}}\t{{.PIDs}}")
	if err != nil {
		return nil, err
	}
	var stats []ContainerStat
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), "\t")
		if len(f) != 7 {
			continue
		}
		stats = append(stats, ContainerStat{Name: f[0], CPU: f[1], Memory: f[2], MemPerc: f[3], NetIO: f[4], BlockIO: f[5], PIDs: f[6]})
	}
	return stats, nil
}

// DiskUsage is `docker system df` as "type, size, reclaimable" rows; it shows
// how much space images and build cache take, which is what filled the disk.
type DiskUsageRow struct {
	Type        string `json:"type"`
	Size        string `json:"size"`
	Reclaimable string `json:"reclaimable"`
}

func (c *Client) DiskUsage(ctx context.Context) ([]DiskUsageRow, error) {
	out, err := c.run(ctx, "system", "df", "--format", "{{.Type}}\t{{.Size}}\t{{.Reclaimable}}")
	if err != nil {
		return nil, err
	}
	var rows []DiskUsageRow
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), "\t")
		if len(f) != 3 {
			continue
		}
		rows = append(rows, DiskUsageRow{Type: f[0], Size: f[1], Reclaimable: f[2]})
	}
	return rows, nil
}
