const state = {
  apiKey: sessionStorage.getItem("barq.control.key") || "",
  hooks: [],
  health: null,
  selected: null,
};

const $ = (selector) => document.querySelector(selector);
const routeList = $("#route-list");
const emptyState = $("#empty-state");
const drawer = $("#drawer");
const scrim = $("#scrim");
const credentialsDialog = $("#credentials-dialog");
const hookDialog = $("#hook-dialog");
let toastTimer;

function toast(message, error = false) {
  const node = $("#toast");
  node.textContent = message;
  node.classList.toggle("error", error);
  node.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => node.classList.remove("show"), 3200);
}

async function api(path, options = {}) {
  const headers = new Headers(options.headers || {});
  if (state.apiKey) headers.set("Authorization", `Bearer ${state.apiKey}`);
  if (options.body && !headers.has("Content-Type")) headers.set("Content-Type", "application/json");
  const response = await fetch(path, {...options, headers});
  if (response.status === 204) return null;
  const text = await response.text();
  let body = null;
  try { body = text ? JSON.parse(text) : null; } catch { body = {message: text}; }
  if (!response.ok) {
    const error = new Error(body?.message || `${response.status} ${response.statusText}`);
    error.status = response.status;
    throw error;
  }
  return body;
}

function splitList(value) {
  return value.split(",").map((item) => item.trim()).filter(Boolean);
}

async function load() {
  try {
    state.health = await api("/health/ready");
    renderHealth(true);
  } catch (error) {
    renderHealth(false);
    toast(`Data plane: ${error.message}`, true);
  }
  if (!state.apiKey) {
    credentialsDialog.showModal();
    return;
  }
  await loadHooks();
}

async function loadHooks() {
  try {
    const result = await api("/v1/webhooks");
    state.hooks = result.webhooks || [];
    renderHooks();
  } catch (error) {
    if (error.status === 401 || error.status === 403) credentialsDialog.showModal();
    toast(error.message, true);
  }
}

function renderHealth(online) {
  const dot = $("#system-dot");
  dot.classList.toggle("online", online);
  dot.classList.toggle("offline", !online);
  $("#system-status").textContent = online ? "SYSTEM READY" : "DATA PLANE DOWN";
  $("#system-version").textContent = online ? `CORE ${state.health.version}` : "CORE —";
  $("#metric-plane").textContent = online ? "READY" : "DOWN";
  const capabilities = state.health?.capabilities || [];
  $("#metric-capabilities").textContent = capabilities.length ? capabilities.join(" / ") : "no capabilities";
}

function renderHooks() {
  const query = $("#search").value.trim().toLowerCase();
  const hooks = state.hooks.filter((hook) => {
    const text = [hook.name, hook.url, hook.scope?.tenant, hook.scope?.database, ...(hook.events || [])].join(" ").toLowerCase();
    return text.includes(query);
  });
  routeList.replaceChildren(...hooks.map(routeNode));
  emptyState.hidden = hooks.length !== 0;
  $("#metric-total").textContent = String(state.hooks.length).padStart(2, "0");
  $("#metric-active").textContent = String(state.hooks.filter((hook) => hook.enabled).length).padStart(2, "0");
}

function routeNode(hook) {
  const row = document.createElement("article");
  row.className = `route ${hook.enabled ? "active" : ""}`;
  row.tabIndex = 0;
  row.setAttribute("role", "button");
  row.setAttribute("aria-label", `Open ${hook.name}`);

  const signal = document.createElement("span");
  signal.className = "route-signal";
  const identity = document.createElement("div");
  const name = document.createElement("h3");
  name.textContent = hook.name;
  const scope = document.createElement("p");
  scope.textContent = `${hook.scope.tenant} / ${hook.scope.database} · REV ${hook.active_revision}`;
  identity.append(name, scope);
  const url = document.createElement("p");
  url.className = "route-url";
  url.textContent = hook.url;
  const tags = document.createElement("div");
  tags.className = "tags";
  (hook.events || []).slice(0, 3).forEach((event) => {
    const tag = document.createElement("span");
    tag.className = "tag";
    tag.textContent = event;
    tags.append(tag);
  });
  const arrow = document.createElement("span");
  arrow.className = "route-arrow";
  arrow.textContent = "→";
  row.append(signal, identity, url, tags, arrow);
  row.addEventListener("click", () => openDrawer(hook));
  row.addEventListener("keydown", (event) => {
    if (event.key === "Enter" || event.key === " ") openDrawer(hook);
  });
  return row;
}

function openDrawer(hook) {
  state.selected = hook;
  const content = $("#drawer-content");
  content.replaceChildren();
  const title = document.createElement("h2");
  title.id = "drawer-title";
  title.textContent = hook.name;
  const url = document.createElement("p");
  url.className = "detail-url";
  url.textContent = hook.url;
  const grid = document.createElement("div");
  grid.className = "detail-grid";
  [
    ["STATUS", hook.enabled ? "ACTIVE" : "DISABLED"],
    ["REVISION", String(hook.active_revision)],
    ["TENANT", hook.scope.tenant],
    ["DATABASE", hook.scope.database],
  ].forEach(([label, value]) => {
    const cell = document.createElement("div");
    const caption = document.createElement("span");
    caption.textContent = label;
    const strong = document.createElement("strong");
    strong.textContent = value;
    cell.append(caption, strong);
    grid.append(cell);
  });
  const eventSection = document.createElement("div");
  eventSection.className = "drawer-section";
  const eventLabel = document.createElement("span");
  eventLabel.textContent = "EVENT FILTERS";
  const eventTags = document.createElement("div");
  eventTags.className = "tags";
  (hook.events || []).forEach((event) => {
    const tag = document.createElement("span");
    tag.className = "tag";
    tag.textContent = event;
    eventTags.append(tag);
  });
  eventSection.append(eventLabel, eventTags);
  const actions = document.createElement("div");
  actions.className = "drawer-actions";
  actions.append(
    actionButton("ROTATE SECRET", () => rotateSecret(hook.id)),
    actionButton("REPLAY DEAD", () => replay(hook.id)),
    actionButton("TEST TRANSFORM", () => testTransform(hook.id)),
    actionButton("DISABLE ROUTE", () => disableHook(hook.id), "danger"),
  );
  content.append(title, url, grid, eventSection, actions);
  drawer.classList.add("open");
  drawer.setAttribute("aria-hidden", "false");
  scrim.hidden = false;
}

function actionButton(label, handler, style = "") {
  const button = document.createElement("button");
  button.type = "button";
  button.className = `button ${style}`;
  button.textContent = label;
  button.addEventListener("click", handler);
  return button;
}

function closeDrawer() {
  drawer.classList.remove("open");
  drawer.setAttribute("aria-hidden", "true");
  scrim.hidden = true;
  state.selected = null;
}

async function rotateSecret(id) {
  if (!confirm("Rotate this signing secret? The old revision remains immutable, but new deliveries use the new secret.")) return;
  try {
    const result = await api(`/v1/webhooks/${encodeURIComponent(id)}:rotate-secret`, {method: "POST", body: "{}"});
    showSecret(result.secret);
    closeDrawer();
    await loadHooks();
  } catch (error) { toast(error.message, true); }
}

async function replay(id) {
  try {
    const result = await api(`/v1/webhooks/${encodeURIComponent(id)}:replay`, {method: "POST", body: "{}"});
    toast(`${result.replayed} dead deliveries queued`);
  } catch (error) { toast(error.message, true); }
}

async function disableHook(id) {
  if (!confirm("Disable this route? Existing history is kept.")) return;
  try {
    await api(`/v1/webhooks/${encodeURIComponent(id)}`, {method: "DELETE"});
    closeDrawer();
    await loadHooks();
    toast("Route disabled");
  } catch (error) { toast(error.message, true); }
}

async function testTransform(id) {
  const raw = prompt("Event context JSON", JSON.stringify({
    event: {id: "control-test", scope: state.selected.scope, cursor: 1, snapshot: 1, type: "Control.test", object_type: "Test", primary_key: "one", source: "system", committed_at: new Date().toISOString()},
    after: {message: "Test from Barq Control"}, related: {},
  }, null, 2));
  if (!raw) return;
  try {
    const body = JSON.parse(raw);
    const result = await api(`/v1/webhooks/${encodeURIComponent(id)}:test`, {method: "POST", body: JSON.stringify(body)});
    toast(`Transform OK · ${JSON.stringify(result).length} bytes`);
  } catch (error) { toast(error.message, true); }
}

function showSecret(secret) {
  $("#secret-value").textContent = secret;
  $("#secret-panel").hidden = false;
}

$("#credentials-button").addEventListener("click", () => {
  $("#api-key").value = state.apiKey;
  credentialsDialog.showModal();
});
$("#credentials-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  state.apiKey = $("#api-key").value.trim();
  if (!state.apiKey) return;
  sessionStorage.setItem("barq.control.key", state.apiKey);
  credentialsDialog.close();
  await loadHooks();
});
$("#forget-key").addEventListener("click", () => {
  state.apiKey = "";
  sessionStorage.removeItem("barq.control.key");
  $("#api-key").value = "";
  state.hooks = [];
  renderHooks();
});

$("#new-hook").addEventListener("click", () => hookDialog.showModal());
$("#empty-new-hook").addEventListener("click", () => hookDialog.showModal());
$("#hook-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const formNode = event.currentTarget;
  const form = new FormData(formNode);
  const input = {
    name: form.get("name").trim(),
    scope: {tenant: form.get("tenant").trim(), database: form.get("database").trim()},
    url: form.get("url").trim(),
    events: splitList(form.get("events")),
    object_types: splitList(form.get("object_types")),
    transform: {language: "javascript", source: form.get("source")},
  };
  try {
    const result = await api("/v1/webhooks", {method: "POST", body: JSON.stringify(input)});
    hookDialog.close();
    formNode.reset();
    showSecret(result.secret);
    await loadHooks();
    toast("Webhook route registered");
  } catch (error) { toast(error.message, true); }
});

$("#search").addEventListener("input", renderHooks);
$("#drawer-close").addEventListener("click", closeDrawer);
scrim.addEventListener("click", closeDrawer);
$("#secret-close").addEventListener("click", () => { $("#secret-panel").hidden = true; });
$("#copy-secret").addEventListener("click", async () => {
  await navigator.clipboard.writeText($("#secret-value").textContent);
  toast("Signing secret copied");
});
document.addEventListener("keydown", (event) => {
  if (event.key === "Escape" && drawer.classList.contains("open")) closeDrawer();
  if (event.key === "/" && !["INPUT", "TEXTAREA"].includes(document.activeElement.tagName)) {
    event.preventDefault();
    $("#search").focus();
  }
});

load();
