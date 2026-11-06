package agent

import (
	"crypto/subtle"
	"html/template"
	"net/http"
)

const adminPageTmpl = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>deploy-agent</title>
<style>
  body { font: 14px/1.5 -apple-system, system-ui, sans-serif; margin: 2rem; color: #1a1a1a; background: #fafafa; }
  h1 { font-size: 1.1rem; margin-bottom: .25rem; }
  h2 { font-size: 1rem; margin: 2rem 0 .5rem; }
  .sub { color: #666; margin-bottom: 1rem; }
  table { border-collapse: collapse; margin-bottom: 1rem; min-width: 480px; }
  th, td { text-align: left; padding: .4rem .8rem; border-bottom: 1px solid #e0e0e0; }
  th { color: #666; font-weight: 600; }
  code { background: #eee; padding: .1rem .3rem; border-radius: 3px; }
  .badge { display: inline-block; padding: .1rem .5rem; border-radius: 3px; font-size: .8rem; }
  .badge-blue { background: #dbeafe; color: #1e40af; }
  .badge-green { background: #dcfce7; color: #15803d; }
  .badge-empty { background: #f3f4f6; color: #9ca3af; }
  .hint { color: #888; font-size: .85rem; }
  .actions { display: flex; gap: .5rem; flex-wrap: wrap; align-items: center; margin-bottom: 1rem; }
  button { font: inherit; padding: .35rem .8rem; border: 1px solid #ccc; border-radius: 4px; background: #fff; cursor: pointer; }
  button:hover:not(:disabled) { background: #f0f0f0; }
  button:disabled { opacity: .5; cursor: not-allowed; }
  #job { padding: .5rem .8rem; border-radius: 4px; background: #f3f4f6; margin-bottom: 1rem; white-space: pre-wrap; }
  pre { background: #eee; padding: .6rem .8rem; border-radius: 4px; overflow: auto; }
  #job.running { background: #fef9c3; } #job.ok { background: #dcfce7; } #job.error { background: #fee2e2; }
</style>
</head>
<body>
<h1>deploy-agent</h1>
<p class="sub">Control panel for deploy-agent. To change config (projects, ports, health checks...), edit <code>PROJECTS_FILE</code> or the env file on the host and restart the service.</p>

<div id="job" hidden></div>

<div class="actions">
  <label>Project <select id="project"></select></label>
  <button data-act="update" {{if not .UpdateEnabled}}disabled title="AGENT_UPDATE_URL is not set"{{end}}>Update deploy-agent</button>
</div>
<p class="hint">Re-deploy reuses the running image. Adding or removing an instance also updates the nginx upstream. Updating the agent downloads the new binary, verifies its checksum and restarts itself (the page loses its connection for a few seconds). Only one operation runs at a time, across all projects.</p>

<div id="services"></div>

<h2>Resources</h2>
<table id="host-table"><tbody></tbody></table>
<table id="containers-table"><thead><tr><th>Container</th><th>CPU</th><th>RAM</th><th>Net I/O</th><th>Block I/O</th><th>PIDs</th></tr></thead><tbody></tbody></table>
<table id="docker-disk-table"><thead><tr><th>Docker</th><th>Size</th><th>Reclaimable</th></tr></thead><tbody></tbody></table>
<p class="hint" id="res-errors"></p>

<h2>Effective project config</h2>
<pre id="config"></pre>

<script>
const $ = (id) => document.getElementById(id);
const fmtBytes = (n) => { if (!n) return "0 B"; const u = ["B","KB","MB","GB","TB"]; const i = Math.min(Math.floor(Math.log(n)/Math.log(1024)), 4); return (n/Math.pow(1024,i)).toFixed(i?1:0)+" "+u[i]; };
function cell(text, tag) { const c = document.createElement(tag || "td"); c.textContent = text; return c; }
function fill(tbody, rows) { tbody.replaceChildren(...rows.map((r) => { const tr = document.createElement("tr"); r.forEach((v, i) => tr.append(cell(v, r.header && i === 0 ? "th" : "td"))); return tr; })); }
function kv(k, v) { const r = [k, v]; r.header = true; return r; }
function el(tag, props, ...children) { const e = Object.assign(document.createElement(tag), props); e.append(...children); return e; }

// Relative URL on purpose: behind nginx the page is served under a prefix such as
// /_deploy-agent/admin/, so an absolute "/admin/api/..." would miss the agent.
async function api(method, path, body) {
  const res = await fetch("api/" + path, { method, headers: { "X-Admin-Request": "1", "Content-Type": "application/json" }, body: body ? JSON.stringify(body) : undefined });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

let busy = false;
let overview = null;
function renderJob(d) {
  const j = $("job");
  busy = d.status === "running";
  j.hidden = d.status === "idle";
  j.className = d.status;
  j.textContent = d.status + " — " + (d.project ? d.project + "/" : "") + d.service + " " + d.image + (d.error ? "\n" + d.error : "");
  document.querySelectorAll("button[data-act]").forEach((b) => { b.disabled = busy || (b.dataset.act === "update" && !{{.UpdateEnabled}}); });
}

function currentProject() { return overview && overview.projects.find((p) => p.name === $("project").value); }

function serviceSection(project, svc) {
  const run = (label, path, body) => el("button", {
    textContent: label, disabled: busy,
    onclick: async () => { try { if (confirm(label + "?")) { await api("POST", path, Object.assign({ project, service: svc.name }, body)); refreshOverview(); } } catch (e) { alert(e.message); } },
  });
  const table = el("table");
  const head = el("tr"); ["Slot", "Color", "Container", "Port", "Image"].forEach((h) => head.append(cell(h, "th")));
  table.append(el("thead", {}, head));
  const tbody = el("tbody"); table.append(tbody);
  const rows = svc.instances.map((i, n) => ["#" + n, i.color || "not deployed", i.container_name || "—", i.port || "—", i.image || "—"]);
  fill(tbody, rows.length ? rows : [["Not deployed", "", "", "", ""]]);
  const buttons = [run("Re-deploy " + svc.name, "redeploy", {})];
  if (svc.kind === "http") {
    buttons.push(run("− Remove 1 instance", "scale", { count: svc.count - 1 }), run("+ Add 1 instance", "scale", { count: svc.count + 1 }));
  }
  return el("section", {}, el("h2", {}, svc.name + " (" + svc.kind + (svc.kind === "http" ? ", " + svc.count + " instance(s)" : "") + ")"), el("div", { className: "actions" }, ...buttons), table);
}

function renderProject() {
  const p = currentProject();
  if (!p) return;
  $("services").replaceChildren(...p.services.map((svc) => serviceSection(p.name, svc)));
  $("config").textContent = JSON.stringify(p.config, null, 2);
}

async function refreshOverview() {
  try {
    overview = await api("GET", "overview");
    renderJob(overview.deployment);
    const select = $("project");
    const selected = select.value;
    if (select.options.length !== overview.projects.length) {
      select.replaceChildren(...overview.projects.map((p) => el("option", { value: p.name, textContent: p.name })));
      select.value = selected || overview.projects[0].name;
    }
    renderProject();
  } catch (e) { $("job").hidden = false; $("job").className = "error"; $("job").textContent = "Could not load status: " + e.message; }
}

async function refreshResources() {
  try {
    const r = await api("GET", "resources");
    const h = r.host;
    fill($("host-table").tBodies[0], [
      kv("CPU", h.cpu_percent.toFixed(1) + "%  (load " + h.load_1 + " / " + h.load_5 + " / " + h.load_15 + ")"),
      kv("RAM", fmtBytes(h.mem_used_bytes) + " / " + fmtBytes(h.mem_total_bytes)),
      kv("Disk /", fmtBytes(h.disk_used_bytes) + " / " + fmtBytes(h.disk_total_bytes)),
      kv("Network (total since boot)", "↓ " + fmtBytes(h.net_rx_bytes) + "  ↑ " + fmtBytes(h.net_tx_bytes)),
      kv("Disk I/O (total since boot)", "read " + fmtBytes(h.disk_read_bytes) + "  write " + fmtBytes(h.disk_write_bytes)),
    ]);
    fill($("containers-table").tBodies[0], (r.containers || []).map((c) => [c.name, c.cpu, c.memory + " (" + c.mem_percent + ")", c.net_io, c.block_io, c.pids]));
    fill($("docker-disk-table").tBodies[0], (r.docker_disk || []).map((d) => [d.type, d.size, d.reclaimable]));
    $("res-errors").textContent = (r.errors || []).join(" · ");
  } catch (e) { $("res-errors").textContent = "Could not load resources: " + e.message; }
}

$("project").addEventListener("change", renderProject);
document.querySelector('button[data-act="update"]').addEventListener("click", async () => {
  try { if (confirm("Update deploy-agent and restart?")) { await api("POST", "update"); refreshOverview(); } }
  catch (e) { alert(e.message); }
});

refreshOverview(); refreshResources();
setInterval(refreshOverview, 3000);
setInterval(refreshResources, 10000);
</script>
</body>
</html>`

var adminTemplate = template.Must(template.New("admin").Parse(adminPageTmpl))

type adminPageData struct {
	UpdateEnabled bool
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = adminTemplate.Execute(w, adminPageData{UpdateEnabled: s.cfg.AgentUpdateURL != ""})
}

// withBasicAuth protects the dashboard with its own HTTP Basic Auth credentials
// (ADMIN_USERNAME/ADMIN_PASSWORD), separate from DEPLOY_WEBHOOK_SECRET, which is a
// machine-to-machine HMAC secret and unsuitable as a password typed in a browser.
// If these two variables are not set the routes return 404, exposing no extra
// surface when the dashboard is not needed.
func (s *Server) withBasicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminUsername == "" || s.cfg.AdminPassword == "" {
			http.NotFound(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.cfg.AdminUsername)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(s.cfg.AdminPassword)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="deploy-agent"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
