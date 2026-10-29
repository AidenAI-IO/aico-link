"""Small experiment UI and routes, separate from the existing device job UI."""
from __future__ import annotations

import ipaddress
from http import HTTPStatus
from runner.experiment import SAFE_ID


def handle_experiment_request(handler, path: str, *, post: bool = False) -> bool:
    manager = handler.server.app.experiments
    if path == "/experiments" and not post:
        handler._send_html(EXPERIMENT_HTML)
        return True
    if path == "/api/experiments/settings":
        try:
            if post:
                address = ipaddress.ip_address(str(handler.client_address[0]).split("%", 1)[0])
                mapped = getattr(address, "ipv4_mapped", None)
                if not (address.is_loopback or (mapped and mapped.is_loopback)):
                    handler._send_json({"error": "Experiment settings can only be saved from a local connection"}, status=HTTPStatus.FORBIDDEN)
                    return True
                settings = manager.save_settings(handler._read_json())
            else:
                settings = manager.settings()
            handler._send_json({"settings": settings})
        except (ValueError, OSError) as exc:
            handler._send_json({"error": str(exc)}, status=HTTPStatus.BAD_REQUEST)
        return True
    if path == "/api/experiments":
        try:
            if post:
                payload = handler._read_json()
                if any(isinstance(agent, dict) and agent.get("kind") in {"pi", "openclaw"}
                       for agent in payload.get("agents", [])):
                    remote = str(handler.client_address[0]).split("%", 1)[0]
                    address = ipaddress.ip_address(remote)
                    mapped = getattr(address, "ipv4_mapped", None)
                    if not (address.is_loopback or (mapped is not None and mapped.is_loopback)):
                        handler._send_json(
                            {"error": "External agent experiments can only be started from a local connection"},
                            status=HTTPStatus.FORBIDDEN,
                        )
                        return True
                environment_id = str(payload.pop("environment_id", "") or "").strip()
                if environment_id:
                    if payload.get("environment_url"):
                        raise ValueError("Choose a managed environment or an external URL, not both")
                    environment = next((item for item in handler.server.app.list_mobilegym_environments()
                                        if item["id"] == environment_id), None)
                    if environment is None or environment.get("status") != "running":
                        raise ValueError("The selected MobileGym environment is not running; start it or choose another")
                    payload["environment_url"] = environment["public_endpoint"]
                elif not payload.get("environment_url"):
                    raise ValueError("Select a running MobileGym environment or start one on this page")
                handler._send_json({"experiment": manager.start(payload)}, status=HTTPStatus.CREATED)
            else:
                handler._send_json({"experiments": manager.list(), "suites": manager.suites(),
                                    "environments": handler.server.app.list_mobilegym_environments(),
                                    "settings": manager.settings()})
        except (ValueError, OSError) as exc:
            handler._send_json({"error": str(exc)}, status=HTTPStatus.BAD_REQUEST)
        return True
    if path.startswith("/api/experiments/") and post:
        parts = path.strip("/").split("/")
        if len(parts) == 4 and parts[3] == "rejudge":
            remote = ipaddress.ip_address(str(handler.client_address[0]).split("%", 1)[0])
            mapped = getattr(remote, "ipv4_mapped", None)
            if not (remote.is_loopback or (mapped is not None and mapped.is_loopback)):
                handler._send_json({"error": "Judge retry can only be started from a local connection"},
                                   status=HTTPStatus.FORBIDDEN)
                return True
            try:
                handler._send_json(manager.retry_judge(parts[2]), status=HTTPStatus.ACCEPTED)
            except (ValueError, OSError) as exc:
                handler._send_json({"error": str(exc)}, status=HTTPStatus.BAD_REQUEST)
        elif len(parts) == 4 and parts[3] == "stop" and manager.stop(parts[2]):
            handler._send_json({"stopping": True})
        else:
            handler.send_error(HTTPStatus.NOT_FOUND)
        return True
    if path.startswith("/experiments/") and not post:
        parts = path.strip("/").split("/")
        if len(parts) < 3 or not SAFE_ID.fullmatch(parts[1]):
            handler.send_error(HTTPStatus.NOT_FOUND)
            return True
        root = (manager.root / parts[1]).resolve()
        target = root.joinpath(*parts[2:]).resolve()
        # Serve reports and evidence only; runtime configuration is never here.
        allowed = {"report.html", "manifest.json", "comparison.json", "results.jsonl", "results.original.jsonl", "metrics.json",
                   "suite.json", "history.json", "trace.json", "episode.json", "native-history.json",
                   "native-events.jsonl", "native-output.json", "native-transcript.jsonl", "mcp-audit.jsonl", "stderr.log", "daemon-setup.log", "setup.log", "summary.md",
                   "business-tools.json", "judge.json", "environment_state.json", "initial_environment_state.json", "setup.json", "release-error.txt", "pre.jpg", "post.jpg"}
        step_image = target.name.startswith("step_") and target.suffix in {".jpg", ".png"}
        if root not in target.parents or (target.name not in allowed and not step_image) or not target.is_file():
            handler.send_error(HTTPStatus.NOT_FOUND)
        elif target.name == "report.html":
            handler._send_html(target.read_text(encoding="utf-8"))
        elif step_image or target.name in {"pre.jpg", "post.jpg"}:
            data = target.read_bytes()
            handler.send_response(HTTPStatus.OK)
            handler.send_header("Content-Type", "image/png" if target.suffix == ".png" else "image/jpeg")
            handler.send_header("Content-Length", str(len(data)))
            handler.end_headers()
            handler.wfile.write(data)
        else:
            handler._send_text(target.read_text(encoding="utf-8"))
        return True
    return False


EXPERIMENT_HTML = r"""<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Agent experiments · Aiden Benchmark</title>
<style>
:root{font-family:system-ui,sans-serif;color:#202a35;background:#f5f7f8}body{max-width:1100px;margin:32px auto;padding:0 20px}a{color:#17654b}h1{margin-bottom:8px}p{line-height:1.6}.muted{color:#546171}.panel{background:white;border:1px solid #dce1e5;border-radius:12px;padding:24px;margin:24px 0}.grid{display:grid;grid-template-columns:1fr 1fr;gap:20px}label{display:block;font-weight:600;margin-bottom:8px}input:not([type=checkbox]),select{box-sizing:border-box;width:100%;font:inherit;padding:10px;border:1px solid #aab6bd;border-radius:6px}fieldset{border:1px solid #dce1e5;border-radius:8px;padding:16px}fieldset>div{margin:12px 0}button{font:inherit;padding:10px 16px;border:1px solid #17654b;border-radius:6px;background:#17654b;color:white;cursor:pointer}button:disabled{opacity:.55;cursor:wait}button.secondary{background:white;color:#17654b}table{border-collapse:collapse;width:100%}td,th{padding:12px;text-align:left;border-bottom:1px solid #dce1e5}#error{color:#a12828;white-space:pre-wrap}.scroll{overflow:auto}@media(max-width:700px){.grid{grid-template-columns:1fr}.panel{padding:16px}body{padding:0 12px}}
</style></head><body>
<a href="/">← Device benchmarks</a><h1>Agent experiments</h1>
<p class="muted">Compare native agents on the same tasks. Each agent gets an independent run; attempts run in a seeded, shuffled order.</p>
<section class="panel" id="runStatusPanel" hidden aria-live="polite">
<strong id="runStatus"></strong><p id="runDetail"></p>
<progress id="runProgress" style="width:100%"></progress>
<p><a id="setupLog" target="_blank" rel="noopener" hidden>Setup log</a> <a id="runReport" hidden>Report</a></p>
</section>
<form id="experimentForm" class="panel">
<div class="grid"><div><label for="suite">Comparison suite</label><select id="suite" required></select></div>
<div><label for="repeats">Attempts per task and agent</label><input id="repeats" type="number" min="1" max="20" value="3" required></div></div>
<div id="environmentFields" hidden style="margin-top:20px">
<label for="environmentSelect">MobileGym environment</label>
<select id="environmentSelect"></select>
<p><button id="startEnvironment" type="button" class="secondary">Start MobileGym</button> <a id="environmentPreview" target="_blank" rel="noopener" hidden>Open simulator</a></p>
<p id="environmentStatus" class="muted" role="status" aria-live="polite"></p>
<div id="externalEnvironment" hidden><label for="environment">External MobileGym environment URL</label>
<input id="environment" type="url" placeholder="http://127.0.0.1:9090" autocomplete="off"></div>
<p class="muted">Selected agents operate the same simulator with a fresh device state for every attempt. Choose an existing environment or start one here. Model API settings are separate. Rubric tasks use Judge from the main page Settings.</p></div>
<div class="grid" style="margin-top:20px">
<fieldset><legend><label><input id="aiden" type="checkbox" checked> Aiden</label></legend>
<p>Uses the current <a href="/">Agent Config</a>, frozen at experiment creation. Each attempt starts a fresh Docker daemon.</p></fieldset>
<fieldset><legend><label><input id="pi" type="checkbox" checked> pi</label> <label><input id="openclaw" type="checkbox"> OpenClaw</label></legend>
<p class="muted">pi and OpenClaw share the model settings below for like-for-like comparisons. Each uses its own native runtime.</p>
<div><label for="provider">Provider</label><input id="provider" value="openrouter" autocomplete="off"></div>
<div><label for="model">Exact model ID</label><input id="model" placeholder="Provider model ID" autocomplete="off"></div>
<div><label for="thinking">Thinking level</label><select id="thinking"><option>off</option><option>minimal</option><option>low</option><option selected>medium</option><option>high</option><option>xhigh</option><option>max</option></select></div>
<details id="customEndpoint"><summary id="customEndpointLabel">Custom API endpoint (optional)</summary>
<p><label for="piBaseUrl">API base URL</label><input id="piBaseUrl" type="url" placeholder="https://gateway.example.com/v1" autocomplete="off"></p>
<p><label for="piApi">API protocol</label><select id="piApi"><option value="openai-completions">OpenAI Chat Completions</option><option value="openai-responses">OpenAI Responses</option><option value="anthropic-messages">Anthropic Messages</option><option value="google-generative-ai">Google Generative AI</option></select></p>
<p><label for="piKeyEnv">API key environment variable</label><input id="piKeyEnv" value="AIDEN_BENCHMARK_PI_API_KEY" autocomplete="off"></p>
<div class="grid"><div><label for="piContext">Context window (tokens)</label><input id="piContext" type="number" min="1" step="1" value="128000"></div><div><label for="piMaxTokens">Max output tokens</label><input id="piMaxTokens" type="number" min="1" step="1" value="16384"></div></div>
<p class="muted">Use the endpoint's base URL, not its full chat request path. Enter an environment variable name, not the secret key. Match token limits to your backend; these defaults are pi defaults, not detected model limits. Set Thinking to off unless your backend supports reasoning. Custom endpoint costs are unknown.</p>
</details>
<p><button id="savePi" type="button" class="secondary">Save model settings</button> <span id="piSettingsStatus" role="status"></span></p>
<p class="muted">OpenClaw requires its CLI and Node 24.22.1+ on the WebUI server; set AIDEN_BENCHMARK_OPENCLAW_EXECUTABLE and AIDEN_BENCHMARK_OPENCLAW_NODE if needed. The provider API key must be in the server environment. Personal pi settings, extensions, skills, and sessions are not loaded.</p></fieldset></div>
<p class="muted" id="scope">Aiden runs in Docker; pi and OpenClaw run on the server host in fresh working directories. These directories do not sandbox shell access. Native memory, planning, and context management remain agent-owned. Each attempt starts with fresh agent state.</p>
<label for="seed">Scheduling seed</label><input id="seed" type="number" step="1" value="0" required style="max-width:180px">
<p><button id="start" type="submit">Start experiment</button> <span id="estimate" class="muted"></span></p>
<div id="error" role="alert"></div></form>
<details class="panel"><summary>Suites unavailable for comparison</summary><ul id="incompatible"></ul></details>
<section class="panel"><h2>Experiments</h2><div class="scroll"><table><thead><tr><th>Experiment / suite</th><th>Status</th><th>Progress</th><th>Actions</th></tr></thead><tbody id="experiments"></tbody></table></div></section>
<script>
const $ = id => document.getElementById(id);
let suites = [];
let experimentsSignature = '';
let environments = [];
let environmentsSignature = '';
let submitting = false;
let startingEnvironment = false;
let settingsLoaded = false;
let piEdited = false;
async function api(url, options) {
  const response = await fetch(url, options);
  const data = await response.json();
  if (!response.ok) throw new Error(data.error || `Request failed (${response.status})`);
  return data;
}
function estimate() {
  const suite = suites.find(item => item.key === $('suite').value);
  $('environmentFields').hidden = suite?.track !== 'operations';
  const external = $('environmentSelect').value === 'external';
  const environment = environments.find(item => item.id === $('environmentSelect').value);
  $('externalEnvironment').hidden = !external;
  $('environment').required = external;
  $('provider').required = $('pi').checked || $('openclaw').checked;
  $('model').required = $('pi').checked || $('openclaw').checked;
  $('piBaseUrl').required = $('openclaw').checked;
  $('customEndpointLabel').textContent = $('openclaw').checked ? 'Custom API endpoint (required for OpenClaw)' : 'Custom API endpoint (optional)';
  if ($('openclaw').checked) $('customEndpoint').open = true;
  $('start').disabled = !settingsLoaded || submitting || (!external && environment?.status !== 'running');
  $('startEnvironment').disabled = startingEnvironment || environments.some(item => ['starting','building'].includes(item.status));
  $('environmentStatus').textContent = external ? 'Connect a MobileGym environment managed outside this WebUI.' : environment
    ? `${environment.name}: ${environment.status}${environment.message ? ' — ' + environment.message : ''}`
    : 'No environment is selected. Start MobileGym to prepare a simulator.';
  $('environmentPreview').hidden = !environment?.web_url || environment.status !== 'running';
  if (environment?.web_url) $('environmentPreview').href = environment.web_url;
  $('estimate').textContent = suite ? `${suite.tasks * Number($('repeats').value) * (Number($('aiden').checked) + Number($('pi').checked) + Number($('openclaw').checked))} planned attempts` : '';
}
async function refresh() {
  const data = await api('/api/experiments');
  if (!settingsLoaded) {
    if (data.settings?.pi && !piEdited) {
      const pi = data.settings.pi;
      for (const [field, id] of Object.entries({provider:'provider', model:'model', thinking:'thinking',
        base_url:'piBaseUrl', api:'piApi', api_key_env:'piKeyEnv', context_window:'piContext', max_tokens:'piMaxTokens'})) {
        if (pi[field] !== undefined) $(id).value = pi[field];
      }
      if (pi.base_url) $('piBaseUrl').closest('details').open = true;
      $('piSettingsStatus').textContent = 'Saved settings loaded';
    }
    settingsLoaded = true;
  }
  const latest = data.experiments[0];
  $('runStatusPanel').hidden = !latest;
  if (latest) {
    $('runStatus').textContent = `${latest.suite} — ${latest.status} (${latest.completed}/${latest.total} attempts)`;
    const current = latest.current_attempt;
    const elapsed = current ? Math.max(0,Math.floor((Date.now()-Date.parse(current.started_at))/1000)) : 0;
    $('runDetail').textContent = latest.error || latest.judge_retry_error || [latest.stage || latest.status,
      current ? `${current.agent_id} / ${current.task_id} / repeat ${current.attempt} (${elapsed}s)` : ''].filter(Boolean).join(' · ');
    $('runProgress').max = Math.max(1,latest.total);
    if (['queued','preparing'].includes(latest.status)) $('runProgress').removeAttribute('value');
    else $('runProgress').value = latest.completed;
    $('setupLog').hidden = !latest.setup_log_url;
    $('setupLog').href = latest.setup_log_url || '#';
    $('runReport').hidden = !latest.report_url;
    $('runReport').href = latest.report_url || '#';
  }
  environments = data.environments || [];
  // Logs update every poll; only rebuild the select when identity/status changes.
  const environmentSignature = JSON.stringify(environments.map(({id,name,status}) => ({id,name,status})));
  if (environmentSignature !== environmentsSignature) {
    environmentsSignature = environmentSignature;
    const selected = $('environmentSelect').value;
    $('environmentSelect').replaceChildren(new Option('Select an environment', ''),
      ...environments.map(item => new Option(`${item.name} (${item.status})`, item.id)),
      new Option('External URL (advanced)', 'external'));
    $('environmentSelect').value = selected === 'external' || environments.some(item => item.id === selected)
      ? selected : (environments.find(item => item.status === 'running')?.id || '');
  }
  $('incompatible').replaceChildren(...data.suites.filter(item => !item.compatible).map(item => {
    const entry = document.createElement('li'); entry.textContent = `${item.name}: ${item.reason}`; return entry;
  }));
  data.suites = data.suites.filter(item => item.compatible);
  if (JSON.stringify(suites) !== JSON.stringify(data.suites)) {
    const selected = $('suite').value;
    suites = data.suites;
    $('suite').replaceChildren(...suites.map(item => new Option(`${item.name} (${item.tasks} tasks, ${item.requires_judge ? 'Judge required' : 'deterministic scoring'})`, item.key)));
    if (suites.some(item => item.key === selected)) $('suite').value = selected;
  }
  const signature = JSON.stringify(data.experiments);
  if (signature === experimentsSignature) { estimate(); return; }
  experimentsSignature = signature;
  $('experiments').replaceChildren();
  for (const item of data.experiments) {
    const row = document.createElement('tr');
    for (const text of [`${item.id} / ${item.suite}`, item.error || item.judge_retry_error ? `${item.status}: ${item.error || item.judge_retry_error}` : item.status, `${item.completed}/${item.total}`]) {
      const cell = document.createElement('td'); cell.textContent = text; row.append(cell);
    }
    const actions = document.createElement('td');
    if (item.setup_log_url) { const log = document.createElement('a'); log.href = item.setup_log_url; log.textContent = 'Setup log'; log.target = '_blank'; log.rel = 'noopener'; actions.append(log, ' '); }
    if (item.report_url) { const link = document.createElement('a'); link.href = item.report_url; link.textContent = 'Report'; actions.append(link); }
    if (['completed','canceled'].includes(item.status) && item.judge_errors > 0 && item.stage !== 'Retrying Judge errors') {
      const retry = document.createElement('button'); retry.type = 'button'; retry.className = 'secondary'; retry.textContent = 'Retry Judge';
      retry.addEventListener('click', async () => { try { await api(`/api/experiments/${encodeURIComponent(item.id)}/rejudge`, {method:'POST'}); await refresh(); } catch (error) { $('error').textContent = error.message; } });
      actions.append(' ', retry);
    }
    if (['queued','preparing','running'].includes(item.status)) {
      const button = document.createElement('button'); button.type = 'button'; button.className = 'secondary'; button.textContent = 'Stop';
      button.addEventListener('click', async () => { try { await api(`/api/experiments/${encodeURIComponent(item.id)}/stop`, {method:'POST'}); await refresh(); } catch (error) { $('error').textContent = error.message; } });
      actions.append(' ', button);
    }
    row.append(actions); $('experiments').append(row);
  }
  estimate();
}
$('experimentForm').addEventListener('change', estimate);
$('startEnvironment').addEventListener('click', async () => {
  startingEnvironment = true; $('error').textContent = ''; estimate();
  try {
    const data = await api('/api/environments/mobilegym', {method:'POST',headers:{'Content-Type':'application/json'},
      body:JSON.stringify({name:'Agent comparison',parallel_envs:1})});
    await refresh();
    $('environmentSelect').value = data.environment.id;
  } catch (error) { $('error').textContent = error.message; }
  finally { startingEnvironment = false; estimate(); }
});
function piConfiguration() {
      const agent = {id:'pi',kind:'pi',provider:$('provider').value,model:$('model').value,thinking:$('thinking').value};
      if ($('piBaseUrl').value.trim()) Object.assign(agent, {
        base_url:$('piBaseUrl').value.trim(),api:$('piApi').value,api_key_env:$('piKeyEnv').value.trim(),
        context_window:Number($('piContext').value),max_tokens:Number($('piMaxTokens').value)
      });
  if (!agent.provider.trim() || !agent.model.trim()) throw new Error('Enter a provider and model before starting.');
  return agent;
}
async function savePiSettings() {
  const pi = piConfiguration();
  await api('/api/experiments/settings', {method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({pi})});
  $('piSettingsStatus').textContent = 'Saved on server';
  return pi;
}
for (const id of ['provider','model','thinking','piBaseUrl','piApi','piKeyEnv','piContext','piMaxTokens']) {
  $(id).addEventListener('input', () => { piEdited = true; $('piSettingsStatus').textContent = 'Unsaved changes'; });
}
$('savePi').addEventListener('click', async () => {
  $('savePi').disabled = true;
  try { await savePiSettings(); $('error').textContent = ''; }
  catch (error) { $('error').textContent = error.message; }
  finally { $('savePi').disabled = false; }
});
$('experimentForm').addEventListener('submit', async event => {
  event.preventDefault(); $('error').textContent = ''; submitting = true; estimate();
  try {
    const agents = [];
    if ($('aiden').checked) agents.push({id:'aiden',kind:'aiden'});
    if ($('pi').checked || $('openclaw').checked) {
      const pi = await savePiSettings();
      if ($('pi').checked) agents.push(pi);
      if ($('openclaw').checked) agents.push({...pi,id:'openclaw',kind:'openclaw'});
    }
    const payload = {suite:$('suite').value,agents,repeats:Number($('repeats').value),seed:Number($('seed').value)};
    if ($('environmentSelect').value === 'external') payload.environment_url = $('environment').value;
    else payload.environment_id = $('environmentSelect').value;
    await api('/api/experiments', {method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(payload)});
    await refresh();
    $('runStatusPanel').scrollIntoView({behavior:'smooth',block:'start'});
  } catch (error) { $('error').textContent = error.message; }
  finally { submitting = false; estimate(); }
});
async function poll() { try { await refresh(); } catch (error) { $('error').textContent = error.message; } finally { setTimeout(poll, 2500); } }
poll();
</script></body></html>"""
