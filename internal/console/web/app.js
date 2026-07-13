const state = {
  apiKey: sessionStorage.getItem("barq.control.key") || "",
  hooks: [],
  tenants: [],
  apiKeys: [],
  health: null,
  selected: null,
  ruleSchema: null,
  rules: [],
  ruleRevision: 0,
  ruleHash: "",
  ruleHistory: [],
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
  const tenantList = $("#rules-tenant-list");
  tenantList.replaceChildren(...state.tenants.map((tenant) => {
    const option = document.createElement("option");
    option.value = tenant.id;
    option.label = tenant.name;
    return option;
  }));
  if (!$("#rules-tenant").value && state.tenants.length) {
    const tenant = state.tenants.find((item) => item.enabled) || state.tenants[0];
    $("#rules-tenant").value = tenant.id;
    $("#rules-database").value = tenant.databases?.[0] || "main";
  }
}

function ruleBasePath() {
  const tenant = $("#rules-tenant").value.trim();
  const database = $("#rules-database").value.trim();
  if (!tenant || !database) throw new Error("Tenant and database are required");
  return `/v1/tenants/${encodeURIComponent(tenant)}/databases/${encodeURIComponent(database)}`;
}

async function loadRuleWorkspace() {
  const button = $("#load-rules");
  button.disabled = true;
  button.textContent = "LOADING…";
  try {
    const base = ruleBasePath();
    const [schema, current, history] = await Promise.all([
      api(`${base}/schema`),
      api(`${base}/sync-rules`),
      api(`${base}/sync-rules/revisions`),
    ]);
    state.ruleSchema = schema;
    state.rules = (current.rules || []).map((rule) => ({...rule}));
    state.ruleRevision = current.revision;
    state.ruleHash = current.hash || "";
    state.ruleHistory = history.revisions || [];
    renderRuleWorkspace();
    toast("Sync rules loaded");
  } catch (error) {
    toast(`Sync rules: ${error.message}`, true);
    setRulePlan(error.message, false);
  } finally {
    button.disabled = false;
    button.textContent = "LOAD DATABASE";
  }
}

function renderRuleWorkspace() {
  $("#rules-revision").textContent = `REV ${state.ruleRevision}`;
  $("#rules-hash").textContent = state.ruleHash || "CLI fallback";
  $("#rules-schema-version").textContent = `SCHEMA ${state.ruleSchema?.version ?? "—"}`;
  $("#plan-rules").disabled = false;
  $("#apply-rules").disabled = false;
  $("#test-rules").disabled = false;
  renderObjectPickers();
  renderRuleList();
  renderRuleHistory();
  setRulePlan("Draft loaded. Plan checks every query without changing live devices.");
}

function renderObjectPickers() {
  const objects = state.ruleSchema?.objects || [];
  const used = new Set(state.rules.map((rule) => rule.object_type));
  const addPicker = $("#rule-object-picker");
  const testPicker = $("#rule-test-object");
  const placeholder = (text) => {
    const option = document.createElement("option");
    option.value = "";
    option.textContent = text;
    return option;
  };
  addPicker.replaceChildren(placeholder("SELECT OBJECT"), ...objects.filter((object) => !used.has(object.name)).map(objectOption));
  const priorTest = testPicker.value;
  testPicker.replaceChildren(placeholder("SELECT OBJECT"), ...objects.map(objectOption));
  if (objects.some((object) => object.name === priorTest)) testPicker.value = priorTest;
}

function objectOption(object) {
  const option = document.createElement("option");
  option.value = object.name;
  option.textContent = object.name;
  return option;
}

function renderRuleList() {
  const list = $("#rule-list");
  if (!state.rules.length) {
    const blank = document.createElement("div");
    blank.className = "rule-blank";
    blank.textContent = "NO OBJECT RULES. ALL DEVICE ACCESS IS DENIED.";
    list.replaceChildren(blank);
    return;
  }
  list.replaceChildren(...state.rules.map(ruleCard));
}

function ruleCard(rule, index) {
  const card = document.createElement("article");
  card.className = "rule-card";
  const head = document.createElement("div");
  head.className = "rule-card-head";
  const title = document.createElement("h3");
  title.textContent = rule.object_type;
  const presetLabel = document.createElement("label");
  presetLabel.textContent = "Preset";
  const preset = document.createElement("select");
  [["custom", "CUSTOM QUERY"], ["owner", "OWNED BY USER"], ["public", "PUBLIC READ + WRITE"], ["deny", "DENY ALL"]].forEach(([value, label]) => {
    const option = document.createElement("option");
    option.value = value;
    option.textContent = label;
    preset.append(option);
  });
  preset.value = rulePreset(rule);
  preset.addEventListener("change", () => setRulePreset(index, preset.value));
  presetLabel.append(preset);
  const remove = document.createElement("button");
  remove.className = "rule-remove";
  remove.type = "button";
  remove.title = "Remove rule and deny this object";
  remove.textContent = "×";
  remove.addEventListener("click", () => {
    state.rules.splice(index, 1);
    renderObjectPickers();
    renderRuleList();
    markRulesChanged();
  });
  head.append(title, presetLabel, remove);

  const owner = document.createElement("div");
  owner.className = "rule-owner";
  owner.hidden = preset.value !== "owner";
  const ownerLabel = document.createElement("label");
  ownerLabel.textContent = "Owner field";
  const ownerSelect = document.createElement("select");
  simpleFields(rule.object_type).forEach((field) => {
    const option = document.createElement("option");
    option.value = field.name;
    option.textContent = `${field.name} · ${field.type}`;
    ownerSelect.append(option);
  });
  ownerSelect.value = ownerField(rule) || ownerSelect.options[0]?.value || "";
  ownerSelect.addEventListener("change", () => {
    rule.read = `${ownerSelect.value} == $user.id`;
    rule.write = rule.read;
    renderRuleList();
    markRulesChanged();
  });
  ownerLabel.append(ownerSelect);
  const ownerNote = document.createElement("p");
  ownerNote.textContent = "$user.id comes from the device JWT subject.";
  owner.append(ownerLabel, ownerNote);

  const queries = document.createElement("div");
  queries.className = "rule-query-grid";
  queries.append(queryEditor(rule, "read", "READ QUERY"), queryEditor(rule, "write", "WRITE QUERY"));
  card.append(head, owner, queries);
  return card;
}

function queryEditor(rule, property, labelText) {
  const wrap = document.createElement("label");
  wrap.className = "rule-query";
  wrap.textContent = labelText;
  const textarea = document.createElement("textarea");
  textarea.spellcheck = false;
  textarea.value = rule[property] || "FALSEPREDICATE";
  textarea.addEventListener("input", () => {
    rule[property] = textarea.value;
    markRulesChanged();
  });
  const fields = document.createElement("div");
  fields.className = "field-picks";
  simpleFields(rule.object_type).forEach((field) => {
    const button = document.createElement("button");
    button.type = "button";
    button.textContent = field.name;
    button.addEventListener("click", () => {
      const start = textarea.selectionStart;
      const separator = start > 0 && !/\s$/.test(textarea.value.slice(0, start)) ? " " : "";
      textarea.setRangeText(separator + field.name, start, textarea.selectionEnd, "end");
      textarea.dispatchEvent(new Event("input"));
      textarea.focus();
    });
    fields.append(button);
  });
  const help = document.createElement("small");
  help.textContent = "Barq predicate only. No sort, limit, distinct, vector order, or table traversal.";
  wrap.append(textarea, fields, help);
  return wrap;
}

function simpleFields(objectType) {
  const object = state.ruleSchema?.objects?.find((item) => item.name === objectType);
  return (object?.properties || []).filter((field) => !field.target || field.embedded);
}

function rulePreset(rule) {
  if (rule.read === "FALSEPREDICATE" && rule.write === "FALSEPREDICATE") return "deny";
  if (rule.read === "TRUEPREDICATE" && rule.write === "TRUEPREDICATE") return "public";
  if (rule.read === rule.write && ownerField(rule)) return "owner";
  return "custom";
}

function ownerField(rule) {
  const match = /^([A-Za-z_][A-Za-z0-9_]*)\s*==\s*\$user\.id$/.exec(rule.read || "");
  return match?.[1] || "";
}

function setRulePreset(index, preset) {
  const rule = state.rules[index];
  if (preset === "deny") rule.read = rule.write = "FALSEPREDICATE";
  if (preset === "public") rule.read = rule.write = "TRUEPREDICATE";
  if (preset === "owner") {
    const fields = simpleFields(rule.object_type);
    const preferred = fields.find((field) => field.name === "owner_id") || fields.find((field) => !field.primary_key) || fields[0];
    rule.read = rule.write = preferred ? `${preferred.name} == $user.id` : "FALSEPREDICATE";
  }
  renderRuleList();
  markRulesChanged();
}

function markRulesChanged() {
  setRulePlan("Draft changed. Run plan before apply.");
}

function setRulePlan(message, good = null) {
  const output = $("#rule-plan-result");
  output.textContent = message;
  output.classList.toggle("good", good === true);
  output.classList.toggle("bad", good === false);
}

async function planRules() {
  try {
    const result = await api(`${ruleBasePath()}/sync-rules:plan`, {
      method: "POST",
      body: JSON.stringify({expected_revision: state.ruleRevision, rules: state.rules}),
    });
    const changes = result.changes?.join(" · ") || `revision ${result.target_revision} is valid`;
    setRulePlan(changes, true);
    toast("Rule plan passed");
  } catch (error) {
    setRulePlan(error.message, false);
    toast(error.message, true);
  }
}

async function applyRules() {
  if (!confirm(`Apply revision ${state.ruleRevision + 1} now? Connected devices will be filtered without reconnecting.`)) return;
  try {
    await api(`${ruleBasePath()}/sync-rules`, {
      method: "PUT",
      body: JSON.stringify({expected_revision: state.ruleRevision, rules: state.rules}),
    });
    await loadRuleWorkspace();
    toast("Sync rules are live");
  } catch (error) {
    setRulePlan(error.message, false);
    toast(error.message, true);
  }
}

async function testRules() {
  const rawKey = $("#rule-test-key").value.trim();
  if (!$("#rule-test-user").value.trim() || !$("#rule-test-object").value || !rawKey) {
    toast("User, object, and primary key are required", true);
    return;
  }
  let primaryKey = rawKey;
  try { primaryKey = JSON.parse(rawKey); } catch { /* plain text key */ }
  const output = $("#rule-test-result");
  try {
    const result = await api(`${ruleBasePath()}/sync-rules:test`, {
      method: "POST",
      body: JSON.stringify({
        user_id: $("#rule-test-user").value.trim(),
        object_type: $("#rule-test-object").value,
        primary_key: primaryKey,
        rules: state.rules,
      }),
    });
    output.textContent = result.found ? `READ ${result.can_read ? "ALLOW" : "DENY"} · WRITE ${result.can_write ? "ALLOW" : "DENY"}` : "OBJECT NOT FOUND";
    output.classList.toggle("good", result.found && result.can_read && result.can_write);
    output.classList.toggle("bad", !result.found || !result.can_read || !result.can_write);
  } catch (error) {
    output.textContent = error.message;
    output.classList.remove("good");
    output.classList.add("bad");
  }
}

function renderRuleHistory() {
  const list = $("#rule-history");
  if (!state.ruleHistory.length) {
    const empty = document.createElement("p");
    empty.textContent = "No applied revisions yet. CLI fallback is revision 0.";
    list.replaceChildren(empty);
    return;
  }
  list.replaceChildren(...state.ruleHistory.map((revision) => {
    const row = document.createElement("div");
    row.className = "history-row";
    const body = document.createElement("div");
    const title = document.createElement("strong");
    title.textContent = `REV ${revision.revision}${revision.revision === state.ruleRevision ? " · ACTIVE" : ""}`;
    const meta = document.createElement("p");
    const time = revision.created_at ? new Date(revision.created_at).toLocaleString() : "unknown time";
    meta.textContent = `${revision.rules?.length || 0} rules · ${revision.source || "apply"} · ${time}`;
    body.append(title, meta);
    row.append(body);
    if (revision.revision !== state.ruleRevision) {
      const button = document.createElement("button");
      button.type = "button";
      button.textContent = "RESTORE";
      button.addEventListener("click", () => restoreRules(revision.revision));
      row.append(button);
    }
    return row;
  }));
}

async function restoreRules(revision) {
  if (!confirm(`Restore revision ${revision} as new revision ${state.ruleRevision + 1}?`)) return;
  try {
    await api(`${ruleBasePath()}/sync-rules/revisions/${revision}:restore`, {
      method: "POST",
      body: JSON.stringify({expected_revision: state.ruleRevision}),
    });
    await loadRuleWorkspace();
    toast(`Revision ${revision} restored`);
  } catch (error) { toast(error.message, true); }
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

$("#load-rules").addEventListener("click", loadRuleWorkspace);
$("#add-rule").addEventListener("click", () => {
  const objectType = $("#rule-object-picker").value;
  if (!objectType) {
    toast("Select an object first", true);
    return;
  }
  const fields = simpleFields(objectType);
  const owner = fields.find((field) => field.name === "owner_id");
  const predicate = owner ? `${owner.name} == $user.id` : "FALSEPREDICATE";
  state.rules.push({object_type: objectType, read: predicate, write: predicate});
  state.rules.sort((left, right) => left.object_type.localeCompare(right.object_type));
  renderObjectPickers();
  renderRuleList();
  markRulesChanged();
});
$("#plan-rules").addEventListener("click", planRules);
$("#apply-rules").addEventListener("click", applyRules);
$("#test-rules").addEventListener("click", testRules);

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
