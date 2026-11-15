// Aico Launcher web UI. Plain ES2020, no build step.
// Every server-provided string is inserted with textContent or as an
// attribute value; nothing from the API goes through innerHTML.
"use strict";

(() => {
  const POLL_MS = 2000;
  const BUILD_POLL_MS = 1500;
  const LOG_POLL_MS = 2000;
  const LOG_TAIL = 400;

  const $ = (id) => document.getElementById(id);

  const ui = {
    app: $("app"),
    loading: $("loading"),
    auth: $("auth-screen"),
    hostFacts: document.querySelector(".host-facts"),
    dockerStatus: $("docker-status"),
    hostPlatform: $("host-platform"),
    repoCommit: $("repo-commit"),
    instanceList: $("instance-list"),
    newInstance: $("new-instance"),
    settingsForm: $("settings-form"),
    settingsStatus: $("settings-status"),
    instanceDialog: $("instance-dialog"),
    instanceForm: $("instance-form"),
    instanceTitle: $("instance-dialog-title"),
    instName: $("inst-name"),
    instImage: $("inst-image"),
    instImageRef: $("inst-image-ref"),
    imageHelp: $("image-help"),
    buildButton: $("build-image"),
    buildProgress: $("build-progress"),
    buildBadge: $("build-badge"),
    buildText: $("build-text"),
    buildLog: $("build-log"),
    bridgeTypes: $("bridge-types"),
    bridgeFields: $("bridge-fields"),
    probeButton: $("probe-bridge"),
    probeSummary: $("probe-summary"),
    probeChecks: $("probe-checks"),
    bridgeNotes: $("bridge-notes"),
    bridgeActions: $("bridge-actions"),
    actionResult: $("action-result"),
    actionBadge: $("action-badge"),
    actionSummary: $("action-summary"),
    actionElapsed: $("action-elapsed"),
    actionChecks: $("action-checks"),
    actionOutput: $("action-output"),
    actionOutputText: $("action-output-text"),
    portConfig: $("inst-port-config"),
    portAgent: $("inst-port-agent"),
    terminal: $("inst-terminal"),
    instanceError: $("instance-error"),
    logsDialog: $("logs-dialog"),
    logsTitle: $("logs-title"),
    logsText: $("logs-text"),
    logsUpdated: $("logs-updated"),
    deleteDialog: $("delete-dialog"),
    deleteForm: $("delete-form"),
    deleteText: $("delete-text"),
    deletePurge: $("delete-purge"),
    deleteError: $("delete-error"),
    toasts: $("toasts"),
  };

  const STATUS_LABELS = {
    stopped: "Stopped",
    starting: "Starting",
    running: "Running",
    stopping: "Stopping",
    error: "Error",
    succeeded: "Succeeded",
    failed: "Failed",
  };

  let state = null;
  let renderedInstancesKey = "";
  let renderedHeaderKey = "";
  let pollTimer = 0;
  let pollPromise = null;
  let refreshQueued = false;
  let connectionLost = false;

  // ---------------------------------------------------------------------------
  // DOM helpers

  function el(tag, attrs, ...children) {
    const node = document.createElement(tag);
    if (attrs) {
      for (const [key, value] of Object.entries(attrs)) {
        if (value === undefined || value === null || value === false) continue;
        if (key === "class") node.className = value;
        else if (key === "text") node.textContent = String(value);
        else if (key === "dataset") Object.assign(node.dataset, value);
        else if (key.startsWith("on") && typeof value === "function") node.addEventListener(key.slice(2), value);
        else if (value === true) node.setAttribute(key, "");
        else node.setAttribute(key, String(value));
      }
    }
    for (const child of children.flat()) {
      if (child === undefined || child === null || child === false) continue;
      node.append(child instanceof Node ? child : document.createTextNode(String(child)));
    }
    return node;
  }

  function safeUrl(value) {
    if (typeof value !== "string" || !value) return "";
    try {
      const url = new URL(value, window.location.href);
      return url.protocol === "http:" || url.protocol === "https:" ? url.href : "";
    } catch (_) {
      return "";
    }
  }

  function str(value) {
    return value === undefined || value === null ? "" : String(value);
  }

  function shortCommit(commit) {
    const text = str(commit);
    return /^[0-9a-f]{12,}$/i.test(text) ? text.slice(0, 7) : text;
  }

  function toDate(value) {
    if (value === undefined || value === null || value === "") return null;
    let date;
    if (typeof value === "number") date = new Date(value < 1e12 ? value * 1000 : value);
    else date = new Date(value);
    return Number.isNaN(date.getTime()) ? null : date;
  }

  function relativeTime(value) {
    const date = toDate(value);
    if (!date) return str(value);
    const seconds = Math.round((Date.now() - date.getTime()) / 1000);
    if (seconds < 45) return "just now";
    const minutes = Math.round(seconds / 60);
    if (minutes < 60) return `${minutes} min ago`;
    const hours = Math.round(minutes / 60);
    if (hours < 36) return `${hours} h ago`;
    const days = Math.round(hours / 24);
    if (days < 30) return `${days} d ago`;
    return date.toLocaleDateString();
  }

  function absoluteTime(value) {
    const date = toDate(value);
    return date ? date.toLocaleString() : str(value);
  }

  function formatSize(value) {
    if (typeof value !== "number" || !Number.isFinite(value)) return str(value);
    const units = ["B", "KB", "MB", "GB", "TB"];
    let size = value;
    let unit = 0;
    while (size >= 1000 && unit < units.length - 1) {
      size /= 1000;
      unit += 1;
    }
    return `${size >= 10 || unit === 0 ? Math.round(size) : size.toFixed(1)} ${units[unit]}`;
  }

  // ---------------------------------------------------------------------------
  // API

  class ApiError extends Error {
    constructor(message, status) {
      super(message);
      this.status = status;
    }
  }

  async function api(method, path, body) {
    const init = { method, credentials: "same-origin", headers: { Accept: "application/json" } };
    if (body !== undefined) {
      init.headers["Content-Type"] = "application/json";
      init.body = JSON.stringify(body);
    }
    let response;
    try {
      response = await fetch(path, init);
    } catch (_) {
      throw new ApiError("Cannot reach the Launcher. Check that it is still running in your terminal.", 0);
    }
    let data = null;
    const text = await response.text();
    if (text) {
      try {
        data = JSON.parse(text);
      } catch (_) {
        data = null;
      }
    }
    if (response.status === 401) {
      showAuth();
    }
    if (!response.ok) {
      const message = data && typeof data.error === "string" && data.error
        ? data.error
        : `Request failed with HTTP ${response.status}.`;
      throw new ApiError(message, response.status);
    }
    return data;
  }

  // ---------------------------------------------------------------------------
  // Toasts

  function toast(message, kind = "info", timeoutMs = 6000) {
    const node = el("div", { class: "toast", role: kind === "error" ? "alert" : "status", dataset: { kind } },
      el("p", { text: message }));
    const close = el("button", { type: "button", class: "btn btn-quiet", text: "Dismiss" });
    close.addEventListener("click", () => node.remove());
    node.append(close);
    ui.toasts.append(node);
    if (timeoutMs > 0) setTimeout(() => node.remove(), timeoutMs);
    while (ui.toasts.children.length > 4) ui.toasts.firstElementChild.remove();
    return node;
  }

  // ---------------------------------------------------------------------------
  // Auth and connection

  function showAuth() {
    ui.loading.hidden = true;
    ui.app.hidden = true;
    ui.auth.hidden = false;
    ui.hostFacts.hidden = true;
    for (const dialog of document.querySelectorAll("dialog[open]")) dialog.close();
  }

  function showApp() {
    ui.loading.hidden = true;
    ui.auth.hidden = true;
    ui.app.hidden = false;
    ui.hostFacts.hidden = false;
  }

  // ---------------------------------------------------------------------------
  // Polling

  // A call made while a fetch is in flight queues one more fetch and resolves
  // after it, so callers that await this after an action see post-action state.
  function refreshState() {
    if (pollPromise) {
      refreshQueued = true;
      return pollPromise;
    }
    pollPromise = fetchState().finally(() => {
      pollPromise = null;
      if (refreshQueued) {
        refreshQueued = false;
        return refreshState();
      }
      return undefined;
    });
    return pollPromise;
  }

  async function fetchState() {
    try {
      const next = await api("GET", "/api/state");
      if (connectionLost) {
        connectionLost = false;
        toast("Reconnected to the Launcher.", "ok", 3000);
      }
      state = next || {};
      showApp();
      renderAll();
    } catch (error) {
      if (error.status === 0 && !connectionLost) {
        connectionLost = true;
        toast(error.message, "error", 0);
      } else if (error.status && error.status !== 401) {
        toast(error.message, "error");
      }
      if (!state && error.status !== 401) {
        ui.loading.textContent = error.message;
      }
    }
  }

  function schedulePoll() {
    clearTimeout(pollTimer);
    // Only reopening the token link can authenticate, so stop polling on 401.
    if (document.hidden || !ui.auth.hidden) return;
    pollTimer = setTimeout(async () => {
      await refreshState();
      schedulePoll();
    }, POLL_MS);
  }

  document.addEventListener("visibilitychange", async () => {
    if (document.hidden || !ui.auth.hidden) {
      clearTimeout(pollTimer);
      return;
    }
    await refreshState();
    schedulePoll();
  });

  // ---------------------------------------------------------------------------
  // Rendering: header

  function renderAll() {
    renderHeader();
    renderInstances();
    renderSettings();
    if (ui.instanceDialog.open) refreshImageOptions();
  }

  function renderHeader() {
    const host = state.host || {};
    const docker = host.docker || {};
    const launcher = state.launcher || {};
    const key = JSON.stringify([host.os, host.arch, docker, launcher.repo_commit]);
    if (key === renderedHeaderKey) return;
    renderedHeaderKey = key;

    if (docker.available) {
      ui.dockerStatus.dataset.state = "ok";
      ui.dockerStatus.textContent = docker.version ? `Docker ${docker.version}` : "Available";
      ui.dockerStatus.removeAttribute("title");
    } else {
      ui.dockerStatus.dataset.state = "error";
      ui.dockerStatus.textContent = docker.error || "Not available";
      ui.dockerStatus.title = docker.error || "";
    }
    ui.hostPlatform.textContent = [host.os, host.arch].filter(Boolean).join(" / ") || "Unknown";
    ui.repoCommit.textContent = shortCommit(launcher.repo_commit) || "Unknown";
    ui.repoCommit.title = launcher.state_dir ? `State directory: ${launcher.state_dir}` : "";
  }

  // ---------------------------------------------------------------------------
  // Rendering: instances

  function bridgeType(type) {
    return (state && Array.isArray(state.bridge_types) ? state.bridge_types : []).find((item) => item.type === type);
  }

  function findInstance(id) {
    return (state && Array.isArray(state.instances) ? state.instances : []).find((item) => item.id === id);
  }

  function bridgeSummary(instance) {
    const bridge = instance.bridge || {};
    const spec = bridgeType(bridge.type);
    const config = bridge.config || {};
    const label = spec ? spec.label : str(bridge.type) || "Unknown";
    const parts = [];
    const fields = spec && Array.isArray(spec.fields) ? spec.fields : [];
    for (const field of fields) {
      const value = config[field.name];
      if (value === undefined || value === null || value === "" || typeof value === "boolean") continue;
      // A stored 0 in a number field means "use the default" (for example the
      // vphone guest SSH port), so it is not worth showing.
      if (field.kind === "number" && Number(value) === 0) continue;
      if (field.name === "adb_path" && value === field.default) continue;
      parts.push(`${field.label || field.name}: ${value}`);
    }
    if (!fields.length) {
      for (const [name, value] of Object.entries(config)) {
        if (value !== "" && value !== null && typeof value !== "object") parts.push(`${name}: ${value}`);
      }
    }
    return { label, detail: parts.join(", ") };
  }

  function statusBadge(status) {
    return el("span", { class: "badge", dataset: { status: status || "stopped" }, text: STATUS_LABELS[status] || str(status) || "Unknown" });
  }

  function linkButton(href, text, focusKey) {
    const url = safeUrl(href);
    if (!url) return null;
    return el("a", {
      class: "btn btn-link",
      href: url,
      target: "_blank",
      rel: "noopener noreferrer",
      text,
      dataset: { focusKey },
    });
  }

  function actionButton(text, focusKey, onclick, extra = {}) {
    return el("button", { type: "button", class: `btn ${extra.class || ""}`.trim(), text, dataset: { focusKey }, onclick, disabled: extra.disabled, title: extra.title });
  }

  function renderInstances() {
    const instances = Array.isArray(state.instances) ? state.instances : [];
    const key = JSON.stringify([instances, state.bridge_types, Math.floor(Date.now() / 60000)]);
    if (key === renderedInstancesKey) return;
    renderedInstancesKey = key;

    const active = document.activeElement;
    const focusKey = active && ui.instanceList.contains(active) ? active.dataset.focusKey : "";

    if (!instances.length) {
      ui.instanceList.replaceChildren(el("div", { class: "empty" },
        el("p", { text: "No instances yet." }),
        el("p", null, el("button", { type: "button", class: "btn btn-primary", text: "Create your first instance", dataset: { focusKey: "empty:new" }, onclick: () => openInstanceDialog(null) }))));
    } else {
      ui.instanceList.replaceChildren(...instances.map(renderInstance));
    }

    if (focusKey) {
      const target = ui.instanceList.querySelector(`[data-focus-key="${CSS.escape(focusKey)}"]`);
      if (target && !target.disabled) target.focus();
    }
  }

  function renderInstance(instance) {
    const id = str(instance.id);
    const status = str(instance.status) || "stopped";
    const urls = instance.urls || {};
    const ports = instance.ports || {};
    const bridge = bridgeSummary(instance);
    const editable = status === "stopped" || status === "error";
    const busy = status === "starting" || status === "stopping";

    const meta = el("dl", { class: "instance-meta" },
      el("dt", { text: "Image" }), el("dd", null, el("code", { text: str(instance.image) || "None" })),
      el("dt", { text: "Bridge" }), el("dd", { text: bridge.detail ? `${bridge.label} (${bridge.detail})` : bridge.label }));
    if (instance.device_type) {
      meta.append(el("dt", { text: "Device" }), el("dd", { text: instance.device_type }));
    }
    if (instance.bridge_endpoint) {
      meta.append(el("dt", { text: "Endpoint" }), el("dd", null, el("code", { text: instance.bridge_endpoint })));
    }
    if (ports.config_web || ports.agent_web) {
      const portText = [
        ports.config_web ? `Config Web ${ports.config_web}` : "",
        ports.agent_web ? `Agent Web UI ${ports.agent_web}` : "",
      ].filter(Boolean).join(", ");
      meta.append(el("dt", { text: "Ports" }), el("dd", { text: portText }));
    }
    if (instance.updated_at) {
      meta.append(el("dt", { text: "Updated" }), el("dd", { text: relativeTime(instance.updated_at), title: absoluteTime(instance.updated_at) }));
    }

    const main = el("div", { class: "instance-main" },
      el("div", { class: "instance-title" }, el("h3", { text: str(instance.name) || id }), statusBadge(status)),
      meta);
    if (busy && instance.step) {
      main.append(el("p", { class: "instance-step", role: "status", text: instance.step }));
    }
    if (status === "error" && instance.error) {
      main.append(el("p", { class: "instance-error", text: instance.error }));
    }

    const controls = el("div", { class: "instance-controls" });
    if (status === "running" || status === "starting") {
      controls.append(actionButton("Stop", `${id}:stop`, () => instanceAction(instance, "stop")));
    } else if (status === "stopping") {
      controls.append(actionButton("Stopping", `${id}:stop`, null, { disabled: true }));
    } else {
      controls.append(actionButton("Start", `${id}:start`, () => instanceAction(instance, "start"), { class: "btn-primary" }));
      // An error instance may still hold a running Agent container and its ports.
      if (status === "error") {
        controls.append(actionButton("Stop", `${id}:stop`, () => instanceAction(instance, "stop")));
      }
    }
    controls.append(actionButton("Logs", `${id}:logs`, () => openLogs(instance)));
    if (editable) {
      controls.append(actionButton("Edit", `${id}:edit`, () => openInstanceDialog(instance)));
    }
    controls.append(actionButton("Delete", `${id}:delete`, () => openDelete(instance), {
      class: "btn-danger",
      disabled: !editable,
      title: editable ? null : "Stop the instance before deleting it",
    }));

    const links = el("div", { class: "instance-links" },
      linkButton(urls.config_web, "Open Config Web", `${id}:config`),
      linkButton(urls.agent_web, "Open Agent Web UI", `${id}:agent`),
      linkButton(urls.terminal, "Open Terminal", `${id}:terminal`),
      linkButton(urls.environment_web, "Open Environment", `${id}:environment`));

    return el("article", { class: "instance", dataset: { status }, "aria-label": str(instance.name) || id },
      main,
      el("div", { class: "instance-side" }, controls),
      links);
  }

  async function instanceAction(instance, action) {
    try {
      await api("POST", `/api/instances/${encodeURIComponent(instance.id)}/${action}`, {});
      toast(action === "start" ? `Starting ${instance.name}.` : `Stopping ${instance.name}.`, "info", 3000);
    } catch (error) {
      if (error.status !== 401) toast(error.message, "error");
    }
    await refreshState();
  }

  // ---------------------------------------------------------------------------
  // Settings

  let settingsDirty = false;
  // Bumped on every edit so a save only clears edits it actually submitted.
  let settingsRevision = 0;

  function settingsFields() {
    const form = ui.settingsForm;
    return {
      base_image: form.elements.base_image,
      go_download_base: form.elements.go_download_base,
      mobilegym_node_image: form.elements.mobilegym_node_image,
      pass_host_proxy: form.elements.pass_host_proxy,
    };
  }

  function renderSettings() {
    if (settingsDirty || ui.settingsForm.contains(document.activeElement)) return;
    const settings = state.settings || {};
    const build = settings.build || {};
    const fields = settingsFields();
    fields.base_image.value = str(build.base_image);
    fields.go_download_base.value = str(build.go_download_base);
    fields.mobilegym_node_image.value = str(build.mobilegym_node_image);
    fields.pass_host_proxy.checked = Boolean(settings.pass_host_proxy);
  }

  ui.settingsForm.addEventListener("input", () => {
    settingsDirty = true;
    settingsRevision += 1;
    ui.settingsStatus.textContent = "Unsaved changes";
  });

  ui.settingsForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    const fields = settingsFields();
    const body = {
      build: {
        base_image: fields.base_image.value.trim(),
        go_download_base: fields.go_download_base.value.trim(),
        mobilegym_node_image: fields.mobilegym_node_image.value.trim(),
      },
      pass_host_proxy: fields.pass_host_proxy.checked,
    };
    const revision = settingsRevision;
    ui.settingsStatus.textContent = "Saving";
    try {
      const saved = await api("PUT", "/api/settings", body);
      if (state && saved) state.settings = saved;
      // Edits made while the save was pending stay dirty and on screen.
      if (revision !== settingsRevision) return;
      settingsDirty = false;
      ui.settingsStatus.textContent = "Settings saved";
      renderSettings();
    } catch (error) {
      ui.settingsStatus.textContent = revision === settingsRevision ? "" : "Unsaved changes";
      if (error.status !== 401) toast(error.message, "error");
    }
  });

  // ---------------------------------------------------------------------------
  // Instance dialog

  const form = {
    editingId: null,
    type: "",
    configs: {},       // type -> {fieldName: value}, so switching types keeps input
    probeOptions: {},  // type -> {fieldName: [{value, label}]}
    actionResults: {}, // type -> {name, label, result}
    preselectImage: "",
  };
  // Incremented each time the dialog opens, so a request started in an earlier
  // dialog never acts on a newer one.
  let dialogGeneration = 0;

  function localImages() {
    return state && Array.isArray(state.images) ? state.images : [];
  }

  function imageOptionLabel(image) {
    const tag = str(image.tag) || str(image.ref);
    const details = [];
    if (image.version) details.push(`version ${image.version}`);
    if (image.commit) details.push(shortCommit(image.commit));
    if (image.created_at) details.push(relativeTime(image.created_at));
    if (image.size !== undefined && image.size !== null && image.size !== "") details.push(formatSize(image.size));
    return details.length ? `${tag} (${details.join(", ")})` : tag;
  }

  function refreshImageOptions() {
    const images = localImages();
    const select = ui.instImage;
    const previous = form.preselectImage || select.value;
    const key = JSON.stringify(images.map((image) => [image.ref, image.version, image.commit, image.created_at]));
    if (select.dataset.key !== key || form.preselectImage) {
      const options = images.map((image) => el("option", { value: str(image.ref), text: imageOptionLabel(image) }));
      if (!images.length) {
        options.unshift(el("option", { value: "", text: "No local aico-link images. Build one or enter a reference." }));
      }
      select.replaceChildren(...options);
      select.dataset.key = key;
      if (previous && images.some((image) => image.ref === previous)) select.value = previous;
      if (form.preselectImage && select.value === form.preselectImage) form.preselectImage = "";
    }
    select.disabled = !images.length || Boolean(ui.instImageRef.value.trim());
  }

  function renderBridgeTypes() {
    const types = state && Array.isArray(state.bridge_types) ? state.bridge_types : [];
    const nodes = types.map((spec) => {
      const disabled = !spec.available;
      const input = el("input", {
        type: "radio",
        name: "bridge_type",
        value: str(spec.type),
        id: `bt-${spec.type}`,
        disabled,
        checked: spec.type === form.type,
        "aria-describedby": `bt-${spec.type}-desc`,
      });
      input.addEventListener("change", () => {
        if (!input.checked) return;
        saveBridgeConfig();
        delete form.actionResults[form.type];
        form.type = spec.type;
        // A save error from the previous type (e.g. its unticked
        // confirmation) no longer applies.
        setFormError("");
        clearProbe();
        renderBridgeFields();
      });
      const description = disabled && spec.unavailable_reason
        ? el("span", { class: "bridge-reason", id: `bt-${spec.type}-desc`, text: spec.unavailable_reason })
        : el("span", { class: "bridge-desc", id: `bt-${spec.type}-desc`, text: str(spec.description) });
      return el("label", { class: "bridge-option", for: `bt-${spec.type}`, dataset: { disabled: String(disabled) } },
        input,
        el("span", { class: "bridge-label", text: str(spec.label) || str(spec.type) }),
        description);
    });
    ui.bridgeTypes.replaceChildren(...nodes);
    if (!types.length) {
      ui.bridgeTypes.append(el("p", { class: "field-help", text: "The Launcher reported no environment bridge types." }));
    }
  }

  function fieldId(name) {
    return `bf-${form.type}-${name}`;
  }

  function currentSpec() {
    return bridgeType(form.type);
  }

  function renderBridgeFields() {
    const spec = currentSpec();
    const fields = spec && Array.isArray(spec.fields) ? spec.fields : [];
    const config = form.configs[form.type] || {};
    const probed = form.probeOptions[form.type] || {};

    const nodes = fields.map((field) => {
      const id = fieldId(field.name);
      const helpId = `${id}-help`;
      const value = Object.prototype.hasOwnProperty.call(config, field.name) ? config[field.name] : field.default;
      const label = el("label", { for: id }, str(field.label) || field.name,
        field.required ? el("span", { class: "req", "aria-hidden": "true", text: " *" }) : null);
      const help = field.help ? el("p", { class: "field-help", id: helpId, text: field.help }) : null;
      const common = { id, name: field.name, required: Boolean(field.required), "aria-describedby": help ? helpId : null };

      if (field.kind === "checkbox") {
        const errorId = `${id}-error`;
        const describedBy = [help ? helpId : "", field.required ? errorId : ""].filter(Boolean).join(" ") || null;
        const input = el("input", { ...common, type: "checkbox", checked: Boolean(value), "aria-describedby": describedBy });
        const error = field.required
          ? el("p", { class: "field-error", id: errorId, hidden: true, text: "Check this box to continue." })
          : null;
        if (error) {
          input.addEventListener("change", () => {
            if (!input.checked) return;
            input.removeAttribute("aria-invalid");
            error.hidden = true;
          });
        }
        return el("div", { class: "field" }, el("div", { class: "field-check" }, input, label), help, error);
      }

      if (field.kind === "select") {
        const choices = [];
        const seen = new Set();
        const addChoice = (option) => {
          const optionValue = str(option && option.value);
          if (seen.has(optionValue)) return;
          seen.add(optionValue);
          choices.push(el("option", { value: optionValue, text: str(option.label) || optionValue }));
        };
        const dynamic = Array.isArray(probed[field.name]) ? probed[field.name] : [];
        (Array.isArray(field.options) ? field.options : []).forEach(addChoice);
        dynamic.forEach(addChoice);
        const current = str(value);
        if (current && !seen.has(current)) addChoice({ value: current, label: `${current} (saved)` });
        const placeholder = field.dynamic_options && !dynamic.length
          ? "Run Check to list choices"
          : (field.placeholder || "Select");
        if (!field.required || !current) {
          choices.unshift(el("option", { value: "", text: placeholder }));
        }
        const select = el("select", common, ...choices);
        select.value = current;
        return el("div", { class: "field" }, label, select, help);
      }

      const isNumber = field.kind === "number";
      const input = el("input", {
        ...common,
        type: isNumber ? "number" : "text",
        inputmode: isNumber ? "numeric" : null,
        spellcheck: "false",
        autocomplete: "off",
        placeholder: str(field.placeholder) || null,
      });
      // Show a stored 0 ("use the default") as blank so the placeholder shows.
      input.value = isNumber && value !== "" && Number(value) === 0 ? "" : str(value);
      return el("div", { class: "field" }, label, input, help);
    });

    ui.bridgeFields.replaceChildren(...nodes);
    if (spec && !fields.length) {
      ui.bridgeFields.append(el("p", { class: "field-help", text: "This bridge type has nothing to configure." }));
    }
    ui.probeButton.disabled = !spec || !spec.available;
    renderBridgeNotes(spec);
    renderBridgeActions();
    renderActionResult();
  }

  function renderBridgeNotes(spec) {
    const notes = spec && Array.isArray(spec.notes) ? spec.notes.map(str).filter(Boolean) : [];
    ui.bridgeNotes.replaceChildren(...notes.map((note) => el("li", { text: note })));
    ui.bridgeNotes.hidden = !notes.length;
  }

  function readBridgeConfig() {
    const spec = currentSpec();
    const config = {};
    const missing = [];
    const unconfirmed = [];
    const fields = spec && Array.isArray(spec.fields) ? spec.fields : [];
    for (const field of fields) {
      const input = $(fieldId(field.name));
      if (!input) continue;
      if (field.kind === "checkbox") {
        // Its change listener clears the invalid mark once it is checked.
        config[field.name] = input.checked;
        if (field.required && !input.checked) unconfirmed.push({ field, input });
        continue;
      }
      input.removeAttribute("aria-invalid");
      const raw = input.value.trim();
      if (!raw) {
        if (field.required) {
          missing.push(field.label || field.name);
          input.setAttribute("aria-invalid", "true");
        }
        continue;
      }
      if (field.kind === "number") {
        const number = Number(raw);
        if (!Number.isFinite(number)) {
          missing.push(`${field.label || field.name} (not a number)`);
          input.setAttribute("aria-invalid", "true");
          continue;
        }
        config[field.name] = number;
      } else {
        config[field.name] = raw;
      }
    }
    return { config, missing, unconfirmed };
  }

  // A required checkbox is a confirmation: only Save marks it, so Check and
  // bridge actions stay usable before the user has confirmed.
  function markUnconfirmed(unconfirmed) {
    for (const { input } of unconfirmed) {
      input.setAttribute("aria-invalid", "true");
      const error = $(`${input.id}-error`);
      if (error) error.hidden = false;
    }
  }

  // The current field values as typed, without validating or marking them.
  function bridgeInputValues() {
    const spec = currentSpec();
    const fields = spec && Array.isArray(spec.fields) ? spec.fields : [];
    const values = {};
    for (const field of fields) {
      const input = $(fieldId(field.name));
      if (!input) continue;
      values[field.name] = field.kind === "checkbox" ? input.checked : input.value;
    }
    return values;
  }

  function saveBridgeConfig() {
    if (!form.type) return;
    form.configs[form.type] = bridgeInputValues();
  }

  function clearProbe() {
    ui.probeSummary.textContent = "";
    delete ui.probeSummary.dataset.ok;
    ui.probeChecks.hidden = true;
    ui.probeChecks.replaceChildren();
  }

  function setFormError(message) {
    ui.instanceError.textContent = message || "";
    ui.instanceError.hidden = !message;
  }

  function openInstanceDialog(instance) {
    if (!state) return;
    dialogGeneration += 1;
    form.editingId = instance ? instance.id : null;
    form.configs = {};
    form.probeOptions = {};
    form.actionResults = {};
    form.preselectImage = "";
    setFormError("");
    clearProbe();

    ui.instanceTitle.textContent = instance ? `Edit ${str(instance.name)}` : "New instance";
    ui.instName.value = instance ? str(instance.name) : "";

    ui.instImage.dataset.key = "";
    refreshImageOptions();
    const images = localImages();
    const imageRef = instance ? str(instance.image) : "";
    if (imageRef && images.some((image) => image.ref === imageRef)) {
      ui.instImage.value = imageRef;
      ui.instImageRef.value = "";
    } else {
      ui.instImageRef.value = imageRef;
      if (!imageRef && images.length) ui.instImage.value = str(newestImage(images).ref);
    }

    const types = Array.isArray(state.bridge_types) ? state.bridge_types : [];
    if (instance && instance.bridge) {
      form.type = str(instance.bridge.type);
      form.configs[form.type] = { ...(instance.bridge.config || {}) };
    } else {
      const firstAvailable = types.find((spec) => spec.available);
      form.type = firstAvailable ? firstAvailable.type : "";
    }
    renderBridgeTypes();
    renderBridgeFields();

    const ports = (instance && instance.ports) || {};
    ui.portConfig.value = str(ports.config_web);
    ui.portAgent.value = str(ports.agent_web);
    ui.terminal.checked = Boolean(instance && instance.enable_terminal);
    const advanced = ui.instanceForm.querySelector("details.advanced");
    advanced.open = Boolean(ports.config_web || ports.agent_web || (instance && instance.enable_terminal));

    syncBuildFromState();
    ui.instanceDialog.showModal();
    ui.instName.focus();
  }

  function newestImage(images) {
    return images.reduce((best, image) => {
      const a = toDate(best.created_at);
      const b = toDate(image.created_at);
      return b && (!a || b > a) ? image : best;
    }, images[0]);
  }

  function readPort(input, label, errors) {
    const raw = input.value.trim();
    input.removeAttribute("aria-invalid");
    if (!raw) return undefined;
    const port = Number(raw);
    if (!Number.isInteger(port) || port < 1 || port > 65535) {
      errors.push(`${label} must be a whole number from 1 to 65535`);
      input.setAttribute("aria-invalid", "true");
      return undefined;
    }
    return port;
  }

  function collectInstance() {
    const errors = [];
    const name = ui.instName.value.trim();
    ui.instName.removeAttribute("aria-invalid");
    if (!name) {
      errors.push("Enter a name");
      ui.instName.setAttribute("aria-invalid", "true");
    }
    const image = ui.instImageRef.value.trim() || ui.instImage.value;
    if (!image) errors.push("Choose a local image, build one, or enter an image reference");

    const spec = currentSpec();
    if (!spec) errors.push("Choose an environment bridge");
    else if (!spec.available) errors.push(`${spec.label} is not available: ${spec.unavailable_reason || "unavailable on this host"}`);
    const { config, missing, unconfirmed } = readBridgeConfig();
    if (missing.length) errors.push(`Fill in ${missing.join(", ")}`);
    if (unconfirmed.length) {
      markUnconfirmed(unconfirmed);
      errors.push(`Confirm ${unconfirmed.map(({ field }) => `"${str(field.label) || field.name}"`).join(", ")}`);
    }

    const ports = {};
    const configPort = readPort(ui.portConfig, "Config Web port", errors);
    const agentPort = readPort(ui.portAgent, "Agent Web UI port", errors);
    if (configPort !== undefined) ports.config_web = configPort;
    if (agentPort !== undefined) ports.agent_web = agentPort;
    if (configPort !== undefined && configPort === agentPort) errors.push("Config Web and Agent Web UI need different ports");

    return {
      errors,
      body: {
        name,
        image,
        bridge: { type: form.type, config },
        ports,
        enable_terminal: ui.terminal.checked,
      },
    };
  }

  let saving = false;

  ui.instanceForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (saving) return;
    const andStart = event.submitter && event.submitter.value === "start";
    const { errors, body } = collectInstance();
    if (errors.length) {
      setFormError(`${errors.join(". ")}.`);
      const invalid = ui.instanceForm.querySelector('[aria-invalid="true"]');
      if (invalid) invalid.focus();
      return;
    }
    setFormError("");
    saving = true;
    const generation = dialogGeneration;
    const buttons = ui.instanceForm.querySelectorAll(".sheet-foot button");
    buttons.forEach((button) => { button.disabled = true; });
    try {
      const saved = form.editingId
        ? await api("PUT", `/api/instances/${encodeURIComponent(form.editingId)}`, body)
        : await api("POST", "/api/instances", body);
      const id = (saved && saved.id) || form.editingId;
      if (andStart && id) {
        try {
          await api("POST", `/api/instances/${encodeURIComponent(id)}/start`, {});
          toast(`Saved and starting ${body.name}.`, "ok", 3000);
        } catch (error) {
          toast(`Saved ${body.name}, but it did not start: ${error.message}`, "error");
        }
      } else {
        toast(`Saved ${body.name}.`, "ok", 3000);
      }
      if (generation === dialogGeneration) ui.instanceDialog.close();
      await refreshState();
    } catch (error) {
      if (error.status === 401) return;
      if (generation === dialogGeneration && ui.instanceDialog.open) setFormError(error.message);
      else toast(`Could not save ${body.name}: ${error.message}`, "error");
    } finally {
      saving = false;
      buttons.forEach((button) => { button.disabled = false; });
    }
  });

  ui.instanceDialog.addEventListener("close", () => {
    clearInterval(elapsedTimer);
    form.editingId = null;
    ui.newInstance.focus();
  });

  ui.newInstance.addEventListener("click", () => openInstanceDialog(null));

  ui.instImageRef.addEventListener("input", () => {
    ui.instImage.disabled = Boolean(ui.instImageRef.value.trim()) || !localImages().length;
  });

  // ---------------------------------------------------------------------------
  // Bridge probe

  ui.probeButton.addEventListener("click", async () => {
    const spec = currentSpec();
    if (!spec) return;
    const type = form.type;
    const generation = dialogGeneration;
    const { config } = readBridgeConfig();
    const probedValues = JSON.stringify(bridgeInputValues());
    // A result only describes the dialog, bridge type and settings it probed.
    const current = () => generation === dialogGeneration && ui.instanceDialog.open && form.type === type;
    // Settings edited while the probe was pending invalidate its result.
    const staleValues = () => {
      if (JSON.stringify(bridgeInputValues()) === probedValues) return false;
      clearProbe();
      return true;
    };
    ui.probeButton.disabled = true;
    ui.probeSummary.textContent = "Checking";
    delete ui.probeSummary.dataset.ok;
    try {
      const result = (await api("POST", `/api/bridge-types/${encodeURIComponent(type)}/probe`, { config })) || {};
      if (!current() || staleValues()) return;
      saveBridgeConfig();
      form.probeOptions[type] = result.options && typeof result.options === "object" ? result.options : {};
      // Preselect the only dynamic choice when the field is still empty.
      const saved = form.configs[type] || {};
      for (const [name, options] of Object.entries(form.probeOptions[type])) {
        if (Array.isArray(options) && options.length === 1 && !saved[name]) saved[name] = str(options[0].value);
      }
      form.configs[type] = saved;
      renderBridgeFields();
      renderProbe(result);
    } catch (error) {
      if (!current() || staleValues()) return;
      ui.probeSummary.textContent = error.status === 401 ? "" : error.message;
      ui.probeSummary.dataset.ok = "false";
    } finally {
      if (generation === dialogGeneration) ui.probeButton.disabled = !currentSpec() || !currentSpec().available;
    }
  });

  function renderProbe(result) {
    const checks = Array.isArray(result.checks) ? result.checks : [];
    const summary = [result.ok ? "Ready" : "Not ready"];
    if (result.device_type) summary.push(`device type ${result.device_type}`);
    ui.probeSummary.textContent = summary.join(", ");
    ui.probeSummary.dataset.ok = String(Boolean(result.ok));
    renderChecks(ui.probeChecks, checks);
  }

  function renderChecks(list, checks) {
    list.replaceChildren(...checks.map((check) => el("li", { dataset: { ok: String(Boolean(check && check.ok)) } },
      el("span", { class: "check-mark", text: check && check.ok ? "OK" : "Failed" }),
      el("span", null,
        el("strong", { text: str(check && check.name) }),
        check && check.detail ? el("span", { class: "check-detail", text: ` ${check.detail}` }) : null))));
    list.hidden = !checks.length;
  }

  // ---------------------------------------------------------------------------
  // Bridge actions
  //
  // Actions such as dependency installs can run for minutes. One action runs
  // at a time; it outlives the dialog, and its result is reported in a toast
  // when the dialog no longer shows its bridge type.

  let runningAction = null; // {type, name, label, startedAt}
  let elapsedTimer = 0;
  let renderedActionEntry = null;

  function bridgeActions(spec) {
    return spec && Array.isArray(spec.actions)
      ? spec.actions.filter((action) => action && typeof action.name === "string" && action.name)
      : [];
  }

  function bridgeTypeLabel(type) {
    return str((bridgeType(type) || {}).label) || type;
  }

  function waitingMessage() {
    return `Waiting for ${runningAction.label} (${bridgeTypeLabel(runningAction.type)}) to finish.`;
  }

  function renderBridgeActions() {
    const spec = currentSpec();
    // Rebuilding the buttons would drop keyboard focus to <body>; remember
    // which action had focus and move it to the new button afterwards.
    const active = document.activeElement;
    const focusedAction = active && active !== ui.bridgeActions && ui.bridgeActions.contains(active)
      ? str(active.dataset.action)
      : "";
    const otherRunning = Boolean(runningAction && runningAction.type !== form.type);
    const buttons = bridgeActions(spec).map((action) => {
      const label = str(action.label) || action.name;
      const busy = Boolean(runningAction && runningAction.type === form.type && runningAction.name === action.name);
      return el("button", {
        type: "button",
        class: "btn",
        text: busy ? `${label}…` : label,
        title: otherRunning ? waitingMessage() : (str(action.help) || null),
        "aria-busy": busy ? "true" : null,
        // While another action runs the buttons stay focusable (aria-disabled
        // plus the guard in runBridgeAction) so focus is not lost.
        "aria-disabled": runningAction ? "true" : null,
        disabled: !spec.available,
        dataset: { action: action.name },
        onclick: () => runBridgeAction(spec, action),
      });
    });
    ui.bridgeActions.replaceChildren(...buttons);
    if (focusedAction) {
      const target = buttons.find((button) => button.dataset.action === focusedAction && !button.disabled);
      (target || ui.probeButton).focus();
    }
  }

  async function runBridgeAction(spec, action) {
    if (runningAction) return;
    const type = spec.type;
    const label = str(action.label) || action.name;
    const { config } = readBridgeConfig();
    runningAction = { type, name: action.name, label, startedAt: Date.now() };
    delete form.actionResults[type];
    renderBridgeActions();
    renderActionResult();
    let result;
    let failure = null;
    try {
      result = (await api("POST", `/api/bridge-types/${encodeURIComponent(type)}/actions/${encodeURIComponent(action.name)}`, { config })) || {};
    } catch (error) {
      failure = error;
    }
    runningAction = null;
    if (failure && failure.status === 401) return;
    const outcome = failure
      ? { ok: false, message: failure.message, checks: [], output: "" }
      : result;
    const shown = ui.instanceDialog.open && form.type === type;
    if (shown) {
      form.actionResults[type] = { name: action.name, label, result: outcome };
    } else {
      const message = str(outcome.message) || (outcome.ok ? "Done." : "Failed.");
      toast(`${label} (${str(spec.label) || type}): ${message}`, outcome.ok ? "ok" : "error", outcome.ok ? 6000 : 0);
    }
    if (ui.instanceDialog.open) {
      renderBridgeActions();
      renderActionResult();
    }
  }

  function formatElapsed(ms) {
    const seconds = Math.max(0, Math.floor(ms / 1000));
    return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, "0")}`;
  }

  function renderActionResult() {
    clearInterval(elapsedTimer);
    const running = runningAction && runningAction.type === form.type ? runningAction : null;
    const entry = form.actionResults[form.type];
    // An action of another bridge type blocks this type's actions; say so
    // instead of showing unexplained disabled buttons.
    const waiting = !running && !entry && runningAction ? runningAction : null;
    if (!running && !entry && !waiting) {
      ui.actionResult.hidden = true;
      return;
    }
    ui.actionResult.hidden = false;
    if (running || waiting) {
      ui.actionResult.dataset.state = "running";
      ui.actionBadge.hidden = false;
      ui.actionBadge.dataset.status = "running-build";
      ui.actionBadge.textContent = running ? "Running" : "Busy";
      ui.actionSummary.textContent = running
        ? `${running.label}. This can take a few minutes.`
        : `${waitingMessage()} Actions for this bridge type are available again afterwards.`;
      const started = (running || waiting).startedAt;
      const tick = () => { ui.actionElapsed.textContent = formatElapsed(Date.now() - started); };
      tick();
      elapsedTimer = setInterval(tick, 1000);
      ui.actionChecks.hidden = true;
      ui.actionChecks.replaceChildren();
      ui.actionOutput.hidden = true;
      return;
    }
    const result = entry.result || {};
    const ok = Boolean(result.ok);
    ui.actionResult.dataset.state = ok ? "ok" : "failed";
    ui.actionBadge.hidden = false;
    ui.actionBadge.dataset.status = ok ? "ok" : "failed";
    ui.actionBadge.textContent = ok ? "Done" : "Failed";
    const message = str(result.message);
    ui.actionSummary.textContent = message ? `${entry.label}: ${message}` : entry.label;
    ui.actionElapsed.textContent = "";
    renderChecks(ui.actionChecks, Array.isArray(result.checks) ? result.checks : []);
    const output = str(result.output);
    ui.actionOutput.hidden = !output;
    // Re-renders (for example after Check) keep the user's open/closed choice.
    if (renderedActionEntry !== entry) {
      renderedActionEntry = entry;
      ui.actionOutputText.textContent = output;
      ui.actionOutput.open = Boolean(output) && !ok;
    }
    scrollOutputToEnd();
  }

  function scrollOutputToEnd() {
    const pre = ui.actionOutputText;
    if (ui.actionOutput.open) pre.scrollTop = pre.scrollHeight;
  }

  ui.actionOutput.addEventListener("toggle", scrollOutputToEnd);

  // ---------------------------------------------------------------------------
  // Image builds

  let build = null;
  let buildTimer = 0;
  // The dialog a build may auto-select its image in, with that dialog's image
  // choice when the build started; a later choice or dialog keeps its image.
  let buildOrigin = null;
  let buildSelectedIn = 0;

  function imageChoice() {
    return JSON.stringify([ui.instImageRef.value.trim(), ui.instImage.value]);
  }

  function trackBuildOrigin() {
    buildOrigin = { generation: dialogGeneration, choice: imageChoice() };
  }

  function syncBuildFromState() {
    if (build && build.status === "running") return;
    const builds = state && Array.isArray(state.builds) ? state.builds : [];
    const running = builds.filter((item) => item.status === "running");
    if (running.length) {
      build = running[running.length - 1];
      trackBuildOrigin();
      renderBuild();
      pollBuild();
    } else {
      renderBuild();
    }
  }

  function renderBuild() {
    if (!build) {
      ui.buildProgress.hidden = true;
      return;
    }
    ui.buildProgress.hidden = false;
    const status = str(build.status);
    ui.buildBadge.dataset.status = status === "running" ? "running-build" : status;
    ui.buildBadge.textContent = status === "running" ? "Building" : (STATUS_LABELS[status] || status);
    const ref = str(build.image_ref);
    let text = ref;
    if (status === "running" && build.started_at) text = `${ref}, started ${relativeTime(build.started_at)}`;
    else if (status === "succeeded") text = buildSelectedIn === dialogGeneration ? `${ref} is ready and selected.` : `${ref} is ready.`;
    else if (status === "failed") text = `${ref} failed. Open the build log for details.`;
    ui.buildText.textContent = text;

    const log = ui.buildLog;
    const atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 24;
    log.textContent = str(build.log_tail);
    if (atBottom) log.scrollTop = log.scrollHeight;
    ui.buildButton.disabled = status === "running";
    if (status === "failed") ui.buildProgress.querySelector("details").open = true;
  }

  async function pollBuild() {
    clearTimeout(buildTimer);
    if (!build || build.status !== "running") return;
    // Only reopening the token link can authenticate, so stop polling on 401.
    if (!ui.auth.hidden) return;
    buildTimer = setTimeout(async () => {
      try {
        const next = await api("GET", `/api/builds/${encodeURIComponent(build.id)}`);
        if (next) build = next;
      } catch (error) {
        if (error.status === 401) return;
        if (error.status === 404) build.status = "failed";
      }
      const select = build.status === "succeeded" && ui.instanceDialog.open && buildOrigin
        && buildOrigin.generation === dialogGeneration && buildOrigin.choice === imageChoice();
      if (select) buildSelectedIn = dialogGeneration;
      renderBuild();
      if (build.status === "running") {
        pollBuild();
      } else if (build.status === "succeeded") {
        if (select) {
          form.preselectImage = str(build.image_ref);
          ui.instImageRef.value = "";
          ui.instImage.disabled = false;
        }
        toast(`Built ${build.image_ref}.`, "ok");
        await refreshState();
        if (ui.instanceDialog.open) refreshImageOptions();
      } else {
        toast(`Image build failed for ${build.image_ref}.`, "error");
      }
    }, BUILD_POLL_MS);
  }

  ui.buildButton.addEventListener("click", async () => {
    ui.buildButton.disabled = true;
    try {
      trackBuildOrigin();
      build = await api("POST", "/api/images/build", {});
      renderBuild();
      pollBuild();
    } catch (error) {
      ui.buildButton.disabled = false;
      if (error.status !== 401) setFormError(error.message);
    }
  });

  // ---------------------------------------------------------------------------
  // Logs

  const logs = { instance: null, source: "agent", timer: 0 };
  const logTabs = Array.from(ui.logsDialog.querySelectorAll('[role="tab"]'));

  function selectLogTab(source, focus) {
    logs.source = source;
    for (const tab of logTabs) {
      const selected = tab.dataset.source === source;
      tab.setAttribute("aria-selected", String(selected));
      tab.tabIndex = selected ? 0 : -1;
      if (selected) {
        ui.logsDialog.querySelector('[role="tabpanel"]').setAttribute("aria-labelledby", tab.id);
        if (focus) tab.focus();
      }
    }
    ui.logsText.textContent = "Loading";
    ui.logsText.dataset.fresh = "true";
    loadLogs();
  }

  for (const tab of logTabs) {
    tab.addEventListener("click", () => selectLogTab(tab.dataset.source, false));
    tab.addEventListener("keydown", (event) => {
      const index = logTabs.indexOf(tab);
      let next = -1;
      if (event.key === "ArrowRight") next = (index + 1) % logTabs.length;
      else if (event.key === "ArrowLeft") next = (index - 1 + logTabs.length) % logTabs.length;
      else if (event.key === "Home") next = 0;
      else if (event.key === "End") next = logTabs.length - 1;
      if (next >= 0) {
        event.preventDefault();
        selectLogTab(logTabs[next].dataset.source, true);
      }
    });
  }

  async function loadLogs() {
    clearTimeout(logs.timer);
    if (!logs.instance || !ui.logsDialog.open) return;
    const source = logs.source;
    const id = logs.instance.id;
    try {
      const result = await api("GET", `/api/instances/${encodeURIComponent(id)}/logs?source=${encodeURIComponent(source)}&tail=${LOG_TAIL}`);
      if (!ui.logsDialog.open || logs.source !== source || !logs.instance || logs.instance.id !== id) return;
      const pre = ui.logsText;
      const fresh = pre.dataset.fresh === "true";
      const atBottom = fresh || pre.scrollHeight - pre.scrollTop - pre.clientHeight < 24;
      const text = str(result && result.text);
      if (pre.textContent !== text || fresh) pre.textContent = text || "No log output yet.";
      delete pre.dataset.fresh;
      if (atBottom) pre.scrollTop = pre.scrollHeight;
      ui.logsUpdated.textContent = `Updated ${new Date().toLocaleTimeString()}`;
    } catch (error) {
      if (logs.source === source && error.status !== 401) {
        ui.logsText.textContent = error.message;
        delete ui.logsText.dataset.fresh;
      }
    }
    if (ui.logsDialog.open && !document.hidden) logs.timer = setTimeout(loadLogs, LOG_POLL_MS);
  }

  function openLogs(instance) {
    logs.instance = instance;
    ui.logsTitle.textContent = `Logs for ${str(instance.name) || str(instance.id)}`;
    ui.logsUpdated.textContent = "";
    ui.logsDialog.showModal();
    selectLogTab(logs.source, true);
  }

  ui.logsDialog.addEventListener("close", () => {
    clearTimeout(logs.timer);
    const id = logs.instance && logs.instance.id;
    logs.instance = null;
    if (id) focusInstanceControl(id, "logs");
  });

  document.addEventListener("visibilitychange", () => {
    if (!document.hidden && ui.logsDialog.open) loadLogs();
  });

  // ---------------------------------------------------------------------------
  // Delete

  let deleting = null;

  function openDelete(instance) {
    deleting = instance;
    ui.deleteText.textContent = `Delete ${str(instance.name) || str(instance.id)}? Its container and settings in the Launcher are removed.`;
    ui.deletePurge.checked = false;
    ui.deleteError.hidden = true;
    ui.deleteDialog.showModal();
    ui.deleteDialog.querySelector("[data-close]").focus();
  }

  ui.deleteForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (!deleting) return;
    const instance = deleting;
    const query = ui.deletePurge.checked ? "?purge_data=1" : "";
    const confirmButton = $("delete-confirm");
    confirmButton.disabled = true;
    try {
      await api("DELETE", `/api/instances/${encodeURIComponent(instance.id)}${query}`);
      ui.deleteDialog.close();
      toast(`Deleted ${instance.name}.`, "ok", 3000);
      await refreshState();
    } catch (error) {
      if (error.status !== 401) {
        ui.deleteError.textContent = error.message;
        ui.deleteError.hidden = false;
      }
    } finally {
      confirmButton.disabled = false;
    }
  });

  ui.deleteDialog.addEventListener("close", () => {
    const id = deleting && deleting.id;
    deleting = null;
    if (id && findInstance(id)) focusInstanceControl(id, "delete");
    else ui.newInstance.focus();
  });

  function focusInstanceControl(id, action) {
    const target = ui.instanceList.querySelector(`[data-focus-key="${CSS.escape(`${id}:${action}`)}"]`);
    if (target && !target.disabled) target.focus();
  }

  // ---------------------------------------------------------------------------
  // Shared dialog behaviour

  for (const button of document.querySelectorAll("dialog [data-close]")) {
    button.addEventListener("click", () => button.closest("dialog").close());
  }

  // ---------------------------------------------------------------------------
  // Start

  refreshState().then(schedulePoll);
})();
