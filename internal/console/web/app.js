const state = {
  apiKey: sessionStorage.getItem("barq.control.key") || "",
  hooks: [],
  tenants: [],
  apiKeys: [],
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
const tenantDialog = $("#tenant-dialog");
const keyDialog = $("#key-dialog");
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
  await Promise.all([loadHooks(), loadAccess()]);
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

async function loadAccess() {
  let visible = false;
  try {
    const result = await api("/v1/admin/tenants");
    state.tenants = result.tenants || [];
    renderTenants();
    $("#tenant-panel").hidden = false;
    visible = true;
  } catch (error) {
    state.tenants = [];
    $("#tenant-panel").hidden = true;
    if (error.status !== 403) toast(`Tenants: ${error.message}`, true);
  }
  try {
    const result = await api("/v1/admin/api-keys");
    state.apiKeys = result.api_keys || [];
    renderKeys();
    $("#key-panel").hidden = false;
    visible = true;
  } catch (error) {
    state.apiKeys = [];
    $("#key-panel").hidden = true;
    if (error.status !== 403) toast(`API keys: ${error.message}`, true);
  }
  $("#access-workspace").hidden = !visible;
  $("#access-nav").hidden = !visible;
}

function renderTenants() {
  const list = $("#tenant-list");
  if (!state.tenants.length) {
    list.replaceChildren(emptyAdminNode("NO TENANTS REGISTERED"));
    return;
  }
  list.replaceChildren(...state.tenants.map((tenant) => {
    const row = adminRecord(tenant.name || tenant.id, `${tenant.id} · ${(tenant.databases || []).join(" / ")}`, tenant.enabled);
    const actions = row.querySelector(".record-actions");
    actions.append(
      recordButton("EDIT", () => openTenantDialog(tenant)),
      recordButton(tenant.enabled ? "DISABLE" : "ENABLE", () => toggleTenant(tenant), tenant.enabled ? "danger" : ""),
    );
    return row;
  }));
}

function renderKeys() {
  const list = $("#key-list");
  if (!state.apiKeys.length) {
    list.replaceChildren(emptyAdminNode("NO KEYS IN THIS SCOPE"));
    return;
  }
  list.replaceChildren(...state.apiKeys.map((key) => {
    const scope = `${key.tenant} / ${key.database}`;
    const label = key.label || key.id;
    const row = adminRecord(label, `${scope} · ${(key.actions || []).join(" / ")}`, key.enabled);
    const actions = row.querySelector(".record-actions");
    if (key.enabled) {
      actions.append(
        recordButton("ROTATE", () => rotateAPIKey(key.id)),
        recordButton("REVOKE", () => revokeAPIKey(key.id), "danger"),
      );
    } else {
      actions.append(recordButton("ACTIVATE", () => activateAPIKey(key.id)));
    }
    return row;
  }));
}

function adminRecord(titleText, metaText, enabled) {
  const row = document.createElement("div");
  row.className = `admin-record ${enabled ? "" : "inactive"}`;
  const body = document.createElement("div");
  const title = document.createElement("div");
  title.className = "record-title";
  const stateDot = document.createElement("span");
  stateDot.className = "record-state";
  const heading = document.createElement("h4");
  heading.textContent = titleText;
  title.append(stateDot, heading);
  const meta = document.createElement("p");
  meta.className = "record-meta";
  meta.textContent = metaText;
  body.append(title, meta);
  const actions = document.createElement("div");
  actions.className = "record-actions";
  row.append(body, actions);
  return row;
}

function recordButton(label, handler, style = "") {
  const button = document.createElement("button");
  button.type = "button";
  button.className = style;
  button.textContent = label;
  button.addEventListener("click", handler);
  return button;
}

function emptyAdminNode(message) {
  const node = document.createElement("div");
  node.className = "empty-records";
  node.textContent = message;
  return node;
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

function showSecret(secret, title = "ONE-TIME SIGNING SECRET") {
  $("#secret-title").textContent = title;
  $("#secret-value").textContent = secret;
  $("#secret-note").textContent = title.includes("SERVICE") ? "Rotated the deployment admin key? Run barqctl access set on the server and paste this value." : "";
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
  await Promise.all([loadHooks(), loadAccess()]);
});
$("#forget-key").addEventListener("click", () => {
  state.apiKey = "";
  sessionStorage.removeItem("barq.control.key");
  $("#api-key").value = "";
  state.hooks = [];
  state.tenants = [];
  state.apiKeys = [];
  renderHooks();
  $("#access-workspace").hidden = true;
  $("#access-nav").hidden = true;
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

function openTenantDialog(tenant = null) {
  const form = $("#tenant-form");
  form.reset();
  form.dataset.editing = tenant?.id || "";
  $("#tenant-dialog-title").textContent = tenant ? "Edit tenant" : "Add tenant";
  $("#tenant-id").readOnly = Boolean(tenant);
  if (tenant) {
    $("#tenant-id").value = tenant.id;
    $("#tenant-name").value = tenant.name;
    $("#tenant-databases").value = (tenant.databases || []).join(", ");
  }
  tenantDialog.showModal();
}

async function toggleTenant(tenant) {
  const enabled = !tenant.enabled;
  if (!enabled && !confirm(`Disable ${tenant.name}? Its data stays on disk, but its keys and webhook polling stop.`)) return;
  try {
    await api(`/v1/admin/tenants/${encodeURIComponent(tenant.id)}`, {method: "PATCH", body: JSON.stringify({enabled})});
    await loadAccess();
    toast(enabled ? "Tenant enabled" : "Tenant disabled");
  } catch (error) { toast(error.message, true); }
}

async function rotateAPIKey(id) {
  if (!confirm("Rotate this key? The old key stops working as soon as the new one is made.")) return;
  try {
    const result = await api(`/v1/admin/api-keys/${encodeURIComponent(id)}:rotate`, {method: "POST", body: "{}"});
    showSecret(result.secret, "ONE-TIME SERVICE API KEY");
    await loadAccess();
    toast("API key rotated");
  } catch (error) { toast(error.message, true); }
}

async function revokeAPIKey(id) {
  if (!confirm("Revoke this API key? Its record stays for audit, but the secret stops working.")) return;
  try {
    await api(`/v1/admin/api-keys/${encodeURIComponent(id)}`, {method: "DELETE"});
    await loadAccess();
    toast("API key revoked");
  } catch (error) { toast(error.message, true); }
}

async function activateAPIKey(id) {
  try {
    await api(`/v1/admin/api-keys/${encodeURIComponent(id)}`, {method: "PATCH", body: JSON.stringify({enabled: true})});
    await loadAccess();
    toast("API key activated");
  } catch (error) { toast(error.message, true); }
}

$("#new-tenant").addEventListener("click", () => openTenantDialog());
$("#tenant-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const formNode = event.currentTarget;
  const form = new FormData(formNode);
  const id = form.get("id").trim();
  const input = {name: form.get("name").trim(), databases: splitList(form.get("databases"))};
  const editing = formNode.dataset.editing;
  try {
    if (editing) {
      await api(`/v1/admin/tenants/${encodeURIComponent(editing)}`, {method: "PATCH", body: JSON.stringify(input)});
    } else {
      await api("/v1/admin/tenants", {method: "POST", body: JSON.stringify({...input, id})});
    }
    tenantDialog.close();
    await loadAccess();
    toast(editing ? "Tenant updated" : "Tenant created");
  } catch (error) { toast(error.message, true); }
});

function openKeyDialog() {
  const form = $("#key-form");
  form.reset();
  const select = $("#key-tenant");
  select.replaceChildren();
  const global = document.createElement("option");
  global.value = "*";
  global.textContent = "Global admin (*)";
  select.append(global);
  state.tenants.filter((tenant) => tenant.enabled).forEach((tenant) => {
    const option = document.createElement("option");
    option.value = tenant.id;
    option.textContent = `${tenant.name} (${tenant.id})`;
    select.append(option);
  });
  if (state.tenants.some((tenant) => tenant.enabled)) select.selectedIndex = 1;
  select.dispatchEvent(new Event("change"));
  keyDialog.showModal();
}

$("#key-tenant").addEventListener("change", (event) => {
  const database = $("#key-form input[name='database']");
  const actions = $("#key-form input[name='actions']");
  if (event.currentTarget.value === "*") {
    database.value = "*";
    actions.value = "*";
  } else {
    if (database.value === "*") database.value = "main";
    if (actions.value === "*") actions.value = "read, write";
  }
});
$("#new-key").addEventListener("click", openKeyDialog);
$("#key-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const formNode = event.currentTarget;
  const form = new FormData(formNode);
  const input = {
    label: form.get("label").trim(),
    tenant: form.get("tenant"),
    database: form.get("database").trim(),
    actions: splitList(form.get("actions")),
  };
  try {
    const result = await api("/v1/admin/api-keys", {method: "POST", body: JSON.stringify(input)});
    keyDialog.close();
    showSecret(result.secret, "ONE-TIME SERVICE API KEY");
    await loadAccess();
    toast("API key created");
  } catch (error) { toast(error.message, true); }
});

$("#search").addEventListener("input", renderHooks);
$("#drawer-close").addEventListener("click", closeDrawer);
scrim.addEventListener("click", closeDrawer);
$("#secret-close").addEventListener("click", () => { $("#secret-panel").hidden = true; });
$("#copy-secret").addEventListener("click", async () => {
  await navigator.clipboard.writeText($("#secret-value").textContent);
  toast("Secret copied");
});
document.addEventListener("keydown", (event) => {
  if (event.key === "Escape" && drawer.classList.contains("open")) closeDrawer();
  if (event.key === "/" && !["INPUT", "TEXTAREA"].includes(document.activeElement.tagName)) {
    event.preventDefault();
    $("#search").focus();
  }
});

load();
