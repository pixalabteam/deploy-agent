# deploy-agent

A small Go server that runs permanently on a Docker host (a VPS, for example) and receives
signed webhooks from CI after an image has been built. It then pulls the image from your
registry, starts a new container, health-checks it, updates an nginx upstream (load
balancer), reloads nginx and stops the old container. Zero downtime, with automatic rollback
if the new container is not healthy.

One agent serves any number of projects on the same host (see "Projects"). Each project
consists of HTTP services (several instances behind nginx) and/or workers (a single
container without a port).

## Design decisions

- **Docker CLI, not the Docker Engine SDK.** The agent runs on the Docker host, so it execs
  `docker ...` through `os/exec`: zero dependencies beyond the standard library, and every
  command is visible in the agent log, so you can copy it and rerun it by hand.
- **HMAC-SHA256 authentication** (`X-Signature-256: sha256=<hex>`, the same scheme GitHub
  uses for its own webhooks) instead of a bearer token in the URL. The secret never shows
  up in CI logs/history or in nginx access logs.
- **Blue/green with fixed ports per "slot"** (each HTTP instance, and each worker) instead of
  dynamic port allocation. Every slot owns two ports and alternates between them on every
  deploy, so the new and old containers never compete for a port, and the old container
  keeps running until the new one passes its health check. This is the core of the
  zero-downtime mechanism.
- **State in a local JSON file** (`/var/lib/deploy-agent/state.json`) so the agent remembers
  which ports/containers are active after a restart. No database: the agent runs as a
  single instance per host.
- **Only one nginx include file per HTTP service.** The agent never generates or edits your
  main vhost; the vhost must `include` the upstream file beforehand (see
  `deploy/nginx/vhost.conf.example`). The agent never touches unrelated nginx config.
- **A deploy stops at the first failing instance** (sequential rolling update, not
  parallel). Instances already updated in the same run keep the new image; the failing
  instance and the ones after it keep the old image. Call the webhook again after fixing the
  problem; the agent never retries endlessly.

## Install on the host

```bash
# Build (on a machine with Go 1.24+, or directly on the host)
GOOS=linux GOARCH=amd64 go build -o deploy-agent ./cmd/deploy-agent

# On the host
sudo useradd --system --home-dir /var/lib/deploy-agent --create-home deploy-agent
sudo usermod -aG docker deploy-agent   # so it can call the `docker` CLI
sudo install -d -o deploy-agent -g deploy-agent /opt/deploy-agent
sudo install -m 0755 -o deploy-agent -g deploy-agent deploy-agent /opt/deploy-agent/deploy-agent
sudo mkdir -p /etc/deploy-agent
sudo cp deploy/deploy-agent.env.example /etc/deploy-agent/deploy-agent.env
sudo cp deploy/projects.json.example /etc/deploy-agent/projects.json
sudo chmod 600 /etc/deploy-agent/deploy-agent.env
# ... set DEPLOY_WEBHOOK_SECRET and edit projects.json ...

sudo cp deploy/systemd/deploy-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now deploy-agent
```

Use `GOARCH=arm64` for ARM hosts. To upgrade a running install, replace the binary and run
`systemctl restart deploy-agent` (or use the dashboard's self-update, below).

### nginx permissions

`deploy-agent` runs as its own user (not root) but must write files under
`/etc/nginx/conf.d/` and reload nginx. Recommended: a narrow, passwordless sudoers rule that
allows exactly two commands:

```
# /etc/sudoers.d/deploy-agent
deploy-agent ALL=(root) NOPASSWD: /usr/sbin/nginx -t, /usr/bin/systemctl reload nginx
```

Also give the `deploy-agent` user write access to `/etc/nginx/conf.d/` (or use `setfacl`, or
pre-create the upstream file owned by `deploy-agent`). `NGINX_TEST_CMD`/`NGINX_RELOAD_CMD` in
the env file default to `sudo nginx -t` / `sudo systemctl reload nginx`; change them if no
`sudo` is needed (for example when the agent runs as root).

Two problems commonly seen during installation:

- `sudo: ... If sudo is running in a container, you may need to adjust the container
  configuration to disable the flag.` when the agent calls `sudo nginx -t`/`reload nginx`
  from the systemd service (even though it works by hand as the same user). sudo tries to
  allocate a pty and fails because the agent has no controlling tty. Fix: add this near the
  top of `/etc/sudoers` (via `sudo visudo`, never by editing the file directly):
  ```
  Defaults !use_pty
  ```
  This is not caused by `NoNewPrivileges=true` in the unit; removing that line does not help.
- `create state dir: mkdir /var/lib/deploy-agent: permission denied`: the state directory
  does not exist or has the wrong owner (the `useradd --create-home` command above normally
  creates it):
  ```bash
  sudo mkdir -p /var/lib/deploy-agent
  sudo chown deploy-agent:deploy-agent /var/lib/deploy-agent
  ```

### nginx vhost

Build your real vhost from `deploy/nginx/vhost.conf.example`. **Before adding the line
`include /etc/nginx/conf.d/<project>-<service>-upstream.conf;` to the vhost, check whether
nginx already includes the whole `conf.d/` directory** (the default on Ubuntu/Debian, one
line in the `http {}` block of `/etc/nginx/nginx.conf`):

```bash
grep -n "conf.d" /etc/nginx/nginx.conf
```

If it **does**, leave the `include` line out: adding it fails with
`duplicate upstream "..."` because the upstream is defined twice. If it does **not**, keep
the include line at the top of the file, outside every `server { }` block.

### First-deploy bootstrap

The agent only writes the upstream file after `nginx -t` passes, but the vhost already says
`proxy_pass http://<upstream>`. Before the first deploy the upstream block must therefore
exist, otherwise every deploy rolls back with `host not found in upstream "..."`. Create a
placeholder once per HTTP service (the agent overwrites it with the real ports after the
first successful deploy):

```bash
sudo tee /etc/nginx/conf.d/myapp-api-upstream.conf <<'EOF'
upstream myapp_api {
    server 127.0.0.1:3101;
}
EOF
sudo chown deploy-agent:deploy-agent /etc/nginx/conf.d/myapp-api-upstream.conf
sudo nginx -t && sudo systemctl reload nginx
```

## Projects

A **project** groups several **services**: `http` (N blue/green instances behind one nginx
upstream) or `worker` (one container, no port). They are declared in the JSON file pointed
to by `PROJECTS_FILE` (required); see `deploy/projects.json.example`. Unknown or misspelled
keys are rejected at startup, not ignored.

- **Defaults**: `env_file` = `/etc/deploy-agent/<project>.env`, `container_prefix` =
  `<project>-<service>`, upstream name = `<project>_<service>`, upstream file =
  `/etc/nginx/conf.d/<project>-<service>-upstream.conf`, `instance_count` = 1,
  `container_port` = 3000, `health_check_path` = `/`. `base_port` is required. Each HTTP
  service reserves a block of 80 ports (8 instances x 10), and the agent rejects a config
  whose port blocks, `container_prefix` values or upstream files overlap.
- **Env layering**: `env_file` is passed to `docker run --env-file`; `env` is added on top;
  `first_instance_env` / `other_instances_env` are layered over `env` for slot 0 / the other
  slots (for example, run migrations on one container only). `sticky_sessions` enables
  `ip_hash` in the upstream.
- **Each HTTP service needs its own upstream file `include`d** in the vhost of that project
  (see "nginx vhost" and "First-deploy bootstrap"; repeat for every service).
- **Secrets**: `DEPLOY_WEBHOOK_SECRET` signs requests for every project. Set `webhook_secret`
  on a project so that project's CI can only deploy that project.
- **One operation at a time** for the whole agent (they share the state file and nginx
  reloads), so deploying two projects at once returns `409`.
- **Image cleanup** works per repository of the image just deployed and keeps the newest
  `image_keep_count` (default `IMAGE_KEEP_COUNT`) images; an image used by any project is
  never removed.

## Configuration (environment variables)

See `deploy/deploy-agent.env.example` for the full list with defaults. The most important:

| Variable | Meaning |
| --- | --- |
| `DEPLOY_WEBHOOK_SECRET` | Required. Secret used for the webhook HMAC signature. |
| `PROJECTS_FILE` | Required. Path of the projects JSON file. |
| `LISTEN_ADDR` | Listen address, default `:8080` (the example env file uses `127.0.0.1:8080`). |
| `STATE_FILE` | State file, default `/var/lib/deploy-agent/state.json`. |
| `NGINX_TEST_CMD` / `NGINX_RELOAD_CMD` | Commands used to validate and reload nginx. |
| `ADMIN_USERNAME` / `ADMIN_PASSWORD` | Enable the dashboard (see below). |
| `AGENT_UPDATE_URL` / `AGENT_UPDATE_TOKEN` | Enable self-update from the dashboard. |

## API

### `POST /deploy`

Header `X-Signature-256: sha256=<hex HMAC-SHA256 of the raw body, key = DEPLOY_WEBHOOK_SECRET>`.

```json
{ "project": "myapp", "service": "api", "image": "ghcr.io/example/myapp:sha-abc1234" }
```

`project` may be omitted when exactly one project is configured. `service` is a service name
declared in the project. The deploy runs in the background: the agent returns `202`
immediately (a deploy takes minutes, which would otherwise hit a 504 from nginx or a CDN
proxy), `409` if another deploy is running, `400` for a malformed body and `401` for a bad
signature. Fetch the outcome with `GET /deploy/status`.

### `GET /deploy/status[?project=<name>]` (HMAC over an empty body, like `/status`)

Returns `{"status":"idle|running|ok|error","project","service","image","error"}` for the
latest deploy of that project (without `project`: the latest deploy of any project).
`error` includes the new container's log when a rollback happened. Kept in memory only; lost
when the agent restarts.

### `GET /status` (same auth header)

Returns `{"projects": {<project>: {<service>: {"instances": [...], "count"}}}}`: the
container, port and image of every instance.

### `GET /healthz`

No auth. Health check of the agent itself (for systemd or external monitoring, not for your
application's health check).

### `GET /admin/` (dashboard)

An HTML page protected by its own HTTP Basic Auth credentials (`ADMIN_USERNAME` /
`ADMIN_PASSWORD` in the env file; deliberately not `DEPLOY_WEBHOOK_SECRET`, which is a
machine-to-machine HMAC secret). If both variables are empty (the default), every
`/admin/*` route returns `404`. Put it behind HTTPS (nginx), since Basic Auth sends the
password base64-encoded.

The page lets you:

- Pick a **project** at the top; each of its services shows an instance table and its own
  buttons.
- **Re-deploy** a service with the image that is currently running (taken from the state).
- **Add/remove instances** of an HTTP service (1-8). Adding runs the image of slot 0,
  health-checks it, then adds it to the nginx upstream. Removing detaches it from nginx
  first, then stops the container. The count is stored in the state file and overrides
  `instance_count`.
- **View resources**: host CPU/RAM/disk/network/disk I/O (read from `/proc`, Linux only),
  `docker stats` per container and `docker system df` (image/build cache size).
- **Update deploy-agent**: downloads the binary from `AGENT_UPDATE_URL` (`{arch}` is replaced
  by `amd64`/`arm64`), verifies the sha256 in `<url>.sha256`, replaces the binary and exits
  with code 75 so systemd (`Restart=on-failure`) starts the new version. `AGENT_UPDATE_TOKEN`
  (optional) is sent as a Bearer token for private repositories. The release workflow in
  this repository publishes these binaries for every `v*` tag.

Operations run in the background under the same lock as `/deploy`, so they never overlap;
progress is shown at the top of the page. Write endpoints (`POST /admin/api/*`) require the
header `X-Admin-Request: 1` and an `Origin` matching `Host` to block CSRF (browsers resend
Basic Auth automatically).

The binary must live in a directory the agent user can write to for self-update to work (the
example unit uses `/opt/deploy-agent/deploy-agent`).

## Calling it from GitHub Actions

Sign the body with HMAC using `openssl` and call the agent with `curl`; nothing else is
needed on `ubuntu-latest`:

```yaml
- name: Deploy
  env:
    DEPLOY_AGENT_URL: ${{ secrets.DEPLOY_AGENT_URL }}
    DEPLOY_AGENT_WEBHOOK_SECRET: ${{ secrets.DEPLOY_AGENT_WEBHOOK_SECRET }}
  run: |
    body=$(jq -nc --arg image "ghcr.io/example/myapp:${GITHUB_SHA::7}" \
      '{project:"myapp", service:"api", image:$image}')
    sig=$(printf '%s' "$body" | openssl dgst -sha256 -hmac "$DEPLOY_AGENT_WEBHOOK_SECRET" | sed 's/^.* //')
    curl -fsS -X POST "$DEPLOY_AGENT_URL/deploy" \
      -H "Content-Type: application/json" -H "X-Signature-256: sha256=$sig" -d "$body"
    # then poll GET $DEPLOY_AGENT_URL/deploy/status?project=myapp (signed over an empty body)
    # until "status" is "ok" or "error".
```

Expose the agent through nginx with TLS (see the `/_deploy-agent/` location in
`deploy/nginx/vhost.conf.example`); never publish the internal port directly.

## Quick manual test (no real host needed)

```bash
go build ./... && go vet ./... && go test ./...

# Run the agent with DOCKER_BIN pointing at a script that fakes `docker` to exercise the
# HTTP/state/nginx flow without real containers, e.g.:
# case "$1" in pull|rm) exit 0;; run) echo fakeid; exit 0;; inspect) echo true;; esac
```

## Image cleanup after a deploy

After every **successful** deploy the agent cleans up the disk (best effort: failures are
only logged as `warning: cleanup: ...` and never fail the deploy):

- Removes old images of the repository just deployed, keeping the newest `IMAGE_KEEP_COUNT`
  (default 2) = the running one plus the previous one for rollback. An image used by a
  container is never removed (`docker rmi` without `--force`).
- `docker image prune -f` (dangling images) and
  `docker builder prune -af --filter until=$BUILD_CACHE_MAX_AGE` (default 72h).
- Volumes and images of other repositories (postgres, redis, nginx...) are untouched.

## License

MIT, see [LICENSE](./LICENSE).
