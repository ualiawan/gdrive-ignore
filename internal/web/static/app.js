"use strict";

// ---------- helpers ----------

const $ = (sel) => document.querySelector(sel);

function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === false || v == null) continue;
    if (k === "class") el.className = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

async function api(method, path, body) {
  const res = await fetch("/api/" + path, {
    method,
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

function size(n) {
  if (!n) return "0 B";
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n : n.toFixed(n < 10 ? 1 : 0)) + " " + u[i];
}

function count(n) { return (n || 0).toLocaleString(); }

function ago(t) {
  if (!t || t.startsWith("0001")) return "never";
  const s = Math.round((Date.now() - new Date(t).getTime()) / 1000);
  if (s < 10) return "just now";
  if (s < 60) return s + " s ago";
  if (s < 3600) return Math.round(s / 60) + " min ago";
  if (s < 86400) return Math.round(s / 3600) + " h ago";
  return new Date(t).toLocaleString();
}

function basename(p) {
  const parts = p.replace(/[\\/]+$/, "").split(/[\\/]/);
  return parts[parts.length - 1] || "";
}

function joinPath(dir, name) {
  if (!dir) return name;
  return dir.replace(/[\\/]+$/, "") + "\\" + name;
}

function under(p, dir) {
  if (!p || !dir) return false;
  const a = p.toLowerCase().replace(/[\\/]+$/, ""), b = dir.toLowerCase().replace(/[\\/]+$/, "");
  return a === b || a.startsWith(b + "\\");
}

async function pickFolder(title, start) {
  try {
    const r = await api("POST", "pick-folder", { title, start: start || "", owner: window.__hwnd || 0 });
    return r.path || "";
  } catch (e) {
    alert("Could not open the folder picker: " + e.message + "\nType the path instead.");
    return "";
  }
}

function closeWindow() {
  if (window.closeWindow) window.closeWindow(); // bound by the app window
  else window.close();
}

function openFolder(path) {
  api("POST", "open", { path }).catch((e) => alert(e.message));
}

// ---------- state ----------

let state = null;
let templates = [];

async function refresh(force) {
  try {
    state = await api("GET", "state" + (force ? "?refresh=1" : ""));
    renderDrive();
    renderPairs();
  } catch (e) {
    $("#pairs").replaceChildren(h("div", { class: "note bad" }, "Lost connection to gdrive-ignore: " + e.message));
  }
}

function renderDrive() {
  const d = state.drive, el = $("#drive-status");
  el.className = "chip";
  if (!d.installed) { el.textContent = "Google Drive for Desktop not found"; el.classList.add("warn"); }
  else if (!d.running) { el.textContent = "Google Drive is not running"; el.classList.add("warn"); }
  else { el.textContent = "Google Drive is running"; el.classList.add("ok"); }
  $("#version").textContent = "Version " + state.version;
  $("#global-path").textContent = state.globalPath;
}

// ---------- pairs list ----------

const stateLabels = {
  starting: "Starting", idle: "Up to date", syncing: "Syncing", paused: "Paused",
  "needs-adopt": "Needs confirmation", error: "Error",
};

function renderInstall() {
  const box = $("#install-banner");
  if (state.runningInstalled || box.dataset.dismissed) { box.replaceChildren(); return; }
  if (box.childNodes.length) return;
  box.replaceChildren(h("div", { class: "note" },
    state.installed
      ? "This copy is not the installed one. Install it to update the installed app."
      : "gdrive-ignore is running without being installed. Install it to add it to the Start menu and start it with Windows (no admin rights needed).",
    h("div", { class: "toolbar" },
      h("button", { class: "primary", onclick: installApp }, "Install for this user"),
      h("button", { onclick: () => { box.dataset.dismissed = "1"; box.replaceChildren(); } }, "Not now"))));
}

async function installApp() {
  try {
    await api("POST", "install");
    $("#install-banner").replaceChildren(h("div", { class: "note" }, "Installed. gdrive-ignore is restarting from its new location…"));
    setTimeout(closeWindow, 1500);
  } catch (e) { alert("Install failed: " + e.message); }
}

function renderPairs() {
  renderInstall();
  const box = $("#pairs");
  if (!state.pairs.length) {
    box.replaceChildren(h("div", { class: "empty" },
      h("h2", {}, "Keep junk out of Google Drive"),
      h("p", {}, "Pick a folder, choose where its filtered copy goes, and list what to leave out."),
      h("ol", {},
        h("li", {}, "Add a folder you work in, such as a projects folder."),
        h("li", {}, "Choose a location that Google Drive syncs."),
        h("li", {}, "Add rules like node_modules/ or *.log, or use the templates."),
      ),
      h("div", {}, h("button", { class: "primary", onclick: () => openEditor() }, "Add your first folder")),
    ));
    return;
  }
  box.replaceChildren(...state.pairs.map(pairCard));
}

function pairCard(p) {
  const st = p.status || {};
  const stateKey = p.setupError ? "error" : (st.state || "starting");
  const full = st.full || {};
  const notes = [];

  if (p.setupError) notes.push(h("div", { class: "note bad" }, p.setupError));
  if (stateKey === "needs-adopt") {
    notes.push(h("div", { class: "note warn" },
      "The target folder already contains files. gdrive-ignore will add and update files there, and only ever deletes files it created itself.",
      h("div", {}, h("button", { onclick: () => act(p.id, "adopt") }, "Use this folder anyway"))));
  } else if (st.error && stateKey !== "error") {
    notes.push(h("div", { class: "note warn" }, "Some files could not be synced: " + st.error));
  } else if (st.error) {
    notes.push(h("div", { class: "note bad" }, st.error));
  }
  if (!p.location) notes.push(driveHint(p.target));
  if (st.state && !st.watching && stateKey !== "error") {
    notes.push(h("div", { class: "note warn" }, "Live watching is unavailable; changes sync on the periodic rescan."));
  }

  const modeBadge = st.mode === "hardlink"
    ? h("span", { class: "badge", title: "Files are hardlinked: no extra disk space" }, "No extra space")
    : st.mode === "copy" ? h("span", { class: "badge", title: "Files are copied into the target" }, "Copies") : null;

  const recent = st.recent || {};
  const activity = [recent.Linked + recent.Copied && `${count(recent.Linked + recent.Copied)} updated`,
    recent.Removed && `${count(recent.Removed)} removed`].filter(Boolean).join(", ");

  return h("div", { class: "card" },
    h("div", { class: "pair-head" },
      h("div", {},
        h("div", { class: "pair-name" }, p.name),
        h("div", { class: "paths" },
          h("span", {}, "From"), h("button", { class: "link", onclick: () => openFolder(p.source), title: "Open in Explorer" }, p.source),
          h("span", {}, "To"), h("button", { class: "link", onclick: () => openFolder(p.target), title: "Open in Explorer" }, p.target),
        ),
      ),
      h("div", { class: "badges" }, h("span", { class: "badge " + stateKey }, stateLabels[stateKey] || stateKey), modeBadge),
    ),
    st.lastFull && !st.lastFull.startsWith("0001")
      ? h("div", { class: "stats" },
        `${count(full.Files)} files (${size(full.Bytes)}) mirrored · ${count(full.Ignored)} items ignored · last sync ${ago(st.lastSync)}`,
        activity ? ` · recently ${activity}` : "")
      : null,
    notes,
    h("div", { class: "actions" },
      h("button", { onclick: () => act(p.id, "sync") }, "Sync now"),
      p.paused
        ? h("button", { onclick: () => act(p.id, "resume") }, "Resume")
        : h("button", { onclick: () => act(p.id, "pause") }, "Pause"),
      h("button", { onclick: () => openEditor(p) }, "Edit rules"),
      h("button", { onclick: () => removePair(p) }, "Remove"),
    ),
  );
}

function driveHint(target) {
  const inSuggested = under(target, state.suggested);
  return h("div", { class: "note warn" },
    "Google Drive does not sync this folder yet. In Google Drive, open Settings → Preferences → My Computer → Add folder, and choose ",
    h("code", {}, inSuggested ? state.suggested : target),
    inSuggested ? " (once; every folder you mirror there is then synced)." : ".",
    " You can keep the Drive option \"Sync with Google Drive\".");
}

async function act(id, action) {
  try { await api("POST", `pairs/${id}/${action}`); } catch (e) { alert(e.message); }
  refresh();
}

function removePair(p) {
  const dlg = $("#confirm-remove");
  $("#remove-mirror").checked = false;
  dlg.returnValue = "";
  dlg.onclose = async () => {
    if (dlg.returnValue !== "ok") return;
    try {
      await api("DELETE", `pairs/${p.id}` + ($("#remove-mirror").checked ? "?deleteMirror=1" : ""));
    } catch (e) { alert(e.message); }
    refresh();
  };
  dlg.showModal();
}

// ---------- rules editor ----------

function rulesEditor(container) {
  const ta = h("textarea", { spellcheck: "false", placeholder: "node_modules/\n*.log\n/build" });
  const file = h("input", { type: "file", style: "display:none" });
  const merge = (lines, comment) => {
    const have = new Set(ta.value.split(/\r?\n/).map((l) => l.trim()));
    const add = lines.map((l) => l.replace(/\r$/, "")).filter((l) => l.trim() && !have.has(l.trim()));
    if (!add.length) return;
    let text = ta.value.replace(/\s+$/, "");
    if (text) text += "\n";
    if (comment) text += "# " + comment + "\n";
    ta.value = text + add.join("\n") + "\n";
    ta.dispatchEvent(new Event("input"));
  };
  file.addEventListener("change", async () => {
    const f = file.files[0];
    if (!f) return;
    merge((await f.text()).split(/\n/), "from " + f.name);
    file.value = "";
  });
  const bar = h("div", { class: "rules-bar" },
    h("button", { type: "button", onclick: () => file.click(), title: "Load patterns from a .driveignore, .gitignore or text file" }, "Import file…"),
    h("span", { class: "sep" }, "Add:"),
    templates.map((t) => h("button", { type: "button", title: t.Patterns.join("  "), onclick: () => merge(t.Patterns, t.Name) }, t.Name)),
    file,
  );
  container.replaceChildren(bar, ta);
  return {
    get: () => ta.value,
    set: (v) => { ta.value = v || ""; },
    onInput: (f) => ta.addEventListener("input", f),
  };
}

// ---------- pair editor ----------

let editing = null;
let pairRules = null;

function locationOptions() {
  const opts = [];
  (state.drive.locations || []).filter((l) => l.exists).forEach((l, i) => {
    const kind = l.kind === "stream" ? "uses extra disk space" : "no extra space if on the same drive";
    opts.push({ value: "loc:" + i, label: `${l.label} (${kind})`, path: l.path });
  });
  if (state.suggested) {
    opts.unshift({ value: "suggested", label: `Local mirror folder ${state.suggested} (recommended: no extra space)`, path: state.suggested });
  }
  opts.push({ value: "custom", label: "Custom folder…", path: "" });
  return opts;
}

function currentLocation() {
  return locationOptions().find((o) => o.value === $("#f-location").value);
}

function updateTarget() {
  const loc = currentLocation();
  const custom = !loc || loc.value === "custom";
  if (!custom) {
    const name = basename($("#f-source").value) || "folder";
    $("#f-target").value = joinPath(loc.path, name);
  }
  updateTargetNote();
}

function updateTargetNote(preview) {
  const note = $("#target-note");
  const target = $("#f-target").value.trim();
  note.className = "note";
  if (!target) { note.replaceChildren(); return; }
  const covered = (state.drive.locations || []).find((l) => under(target, l.path));
  const parts = [];
  if (covered) {
    parts.push(`Google Drive syncs this location (${covered.label}).`);
    if (covered.kind === "stream") parts.push(" Files are copied into Drive's virtual drive, which uses extra disk space and Drive's cache.");
  } else {
    note.classList.add("warn");
    note.replaceChildren(...driveHint(target).childNodes);
    return;
  }
  if (preview) {
    parts.push(preview.hardlinkCapable ? " Files will be hardlinked: no extra space." : " Files will be copied.");
  }
  note.replaceChildren(parts.join(""));
}

function openEditor(pair) {
  editing = pair || null;
  $("#editor-title").textContent = pair ? "Edit " + pair.name : "Add folder";
  $("#editor-save").textContent = pair ? "Save" : "Save and start syncing";
  $("#editor-error").textContent = "";
  $("#preview").replaceChildren();
  $("#preview-summary").textContent = "";

  const sel = $("#f-location");
  const opts = locationOptions();
  sel.replaceChildren(...opts.map((o) => h("option", { value: o.value }, o.label)));

  pairRules = rulesEditor($("#pair-editor"));
  if (pair) {
    $("#f-source").value = pair.source;
    $("#f-target").value = pair.target;
    $("#f-name").value = pair.name;
    pairRules.set(pair.rules);
    $("#f-global").checked = pair.useGlobal;
    $("#f-gitignore").checked = pair.honorGitignore;
    $("#f-mode").value = pair.mode || "auto";
    const match = opts.find((o) => o.path && o.value !== "custom" &&
      pair.target.toLowerCase() === joinPath(o.path, basename(pair.source)).toLowerCase());
    sel.value = match ? match.value : "custom";
  } else {
    $("#f-source").value = "";
    $("#f-target").value = "";
    $("#f-name").value = "";
    pairRules.set("");
    $("#f-global").checked = true;
    $("#f-gitignore").checked = false;
    $("#f-mode").value = "auto";
    sel.value = opts[0].value;
  }
  updateTargetNote();
  $("#editor").showModal();
}

function draft() {
  return {
    name: $("#f-name").value.trim(),
    source: $("#f-source").value.trim(),
    target: $("#f-target").value.trim(),
    rules: pairRules.get(),
    useGlobal: $("#f-global").checked,
    honorGitignore: $("#f-gitignore").checked,
    mode: $("#f-mode").value,
  };
}

async function runPreview() {
  const d = draft();
  if (!d.source) { $("#editor-error").textContent = "Choose a source folder first."; return; }
  $("#editor-error").textContent = "";
  $("#preview-summary").textContent = "Scanning…";
  $("#preview-btn").disabled = true;
  try {
    const r = await api("POST", "preview", d);
    const s = r.stats;
    const ignoredBytes = r.ignored.reduce((a, e) => a + e.size, 0);
    const ignoredFiles = r.ignored.reduce((a, e) => a + e.count, 0);
    $("#preview-summary").textContent =
      `${count(s.Files)} files (${size(s.Bytes)}) will sync · ${count(s.Ignored)} items ignored: ${count(ignoredFiles)} files, ${size(ignoredBytes)}`;
    const rows = r.ignored.map((e) => h("tr", {},
      h("td", { class: "path" }, e.path + (e.dir ? "/" : "")),
      h("td", { class: "num" }, size(e.size)),
      h("td", { class: "num" }, count(e.count)),
      h("td", {}, h("code", {}, e.rule)),
      h("td", {}, e.source === "global rules" || e.source === "pair rules" ? e.source : basename(e.source) + " in " + (e.source.replace(/[\\/][^\\/]+$/, "") || "."), e.line ? ` line ${e.line}` : ""),
    ));
    const parts = [];
    if (r.ignoreFiles && r.ignoreFiles.length) {
      parts.push(h("div", { class: "files" }, "Ignore files found in the source: ", r.ignoreFiles.join(", ")));
    }
    if (rows.length) {
      parts.push(h("table", {},
        h("thead", {}, h("tr", {}, h("th", {}, "Ignored"), h("th", {}, "Size"), h("th", {}, "Files"), h("th", {}, "Rule"), h("th", {}, "From"))),
        h("tbody", {}, rows)));
    } else {
      parts.push(h("div", { class: "files" }, "Nothing is ignored with these rules."));
    }
    if (r.truncated) parts.push(h("div", { class: "files" }, `…and ${count(r.truncated)} more.`));
    $("#preview").replaceChildren(...parts);
    updateTargetNote(r);
  } catch (e) {
    $("#preview-summary").textContent = "";
    $("#editor-error").textContent = e.message;
  } finally {
    $("#preview-btn").disabled = false;
  }
}

async function saveEditor(ev) {
  ev.preventDefault();
  const d = draft();
  $("#editor-error").textContent = "";
  try {
    if (editing) {
      const changedTarget = editing.target.toLowerCase() !== d.target.toLowerCase() || editing.source.toLowerCase() !== d.source.toLowerCase();
      if (changedTarget && !confirm("Changing the folders removes the old mirror's files that gdrive-ignore created and builds a new one. Continue?")) return;
      await api("PUT", "pairs/" + editing.id, { ...editing, ...d });
    } else {
      await api("POST", "pairs", d);
    }
    $("#editor").close();
    refresh();
  } catch (e) {
    $("#editor-error").textContent = e.message;
  }
}

// ---------- global rules and settings ----------

let globalRules = null;

async function loadGlobal() {
  const g = await api("GET", "global");
  globalRules.set(g.text);
  $("#global-msg").textContent = "";
}

async function loadSettings() {
  $("#set-autostart").checked = state.autostart;
  $("#set-interval").value = state.settings.intervalMinutes;
  $("#set-msg").textContent = "";
}

// ---------- wiring ----------

function showTab(name) {
  document.querySelectorAll(".tabs button").forEach((b) => b.classList.toggle("active", b.dataset.tab === name));
  document.querySelectorAll(".tab").forEach((t) => t.classList.toggle("active", t.id === "tab-" + name));
  if (name === "global") loadGlobal();
  if (name === "settings") loadSettings();
}

async function init() {
  templates = await api("GET", "templates").catch(() => []);
  globalRules = rulesEditor($("#global-editor"));
  globalRules.onInput(() => { $("#global-msg").textContent = "Unsaved changes"; });

  document.querySelectorAll(".tabs button").forEach((b) => b.addEventListener("click", () => showTab(b.dataset.tab)));
  $("#add-pair").addEventListener("click", () => openEditor());
  $("#sync-all").addEventListener("click", () => act("all", "sync"));
  $("#editor-cancel").addEventListener("click", () => $("#editor").close());
  $("#editor-form").addEventListener("submit", saveEditor);
  $("#preview-btn").addEventListener("click", runPreview);
  $("#f-location").addEventListener("change", updateTarget);
  $("#f-source").addEventListener("change", updateTarget);
  $("#f-target").addEventListener("input", () => { $("#f-location").value = "custom"; updateTargetNote(); });
  $("#pick-source").addEventListener("click", async () => {
    const p = await pickFolder("Choose the folder to sync", $("#f-source").value);
    if (p) { $("#f-source").value = p; updateTarget(); }
  });
  $("#pick-target").addEventListener("click", async () => {
    const p = await pickFolder("Choose where the filtered copy goes", $("#f-target").value);
    if (p) { $("#f-target").value = p; $("#f-location").value = "custom"; updateTargetNote(); }
  });
  $("#global-save").addEventListener("click", async () => {
    try {
      await api("PUT", "global", { text: globalRules.get() });
      $("#global-msg").textContent = "Saved. Folders using global rules are resyncing.";
    } catch (e) { $("#global-msg").textContent = e.message; }
  });
  $("#set-save").addEventListener("click", async () => {
    try {
      await api("PUT", "settings", { intervalMinutes: Number($("#set-interval").value), autostart: $("#set-autostart").checked });
      $("#set-msg").textContent = "Saved.";
      refresh();
    } catch (e) { $("#set-msg").textContent = e.message; }
  });
  $("#open-logs").addEventListener("click", () => openFolder("logs"));
  $("#quit").addEventListener("click", async () => {
    if (!confirm("Quit gdrive-ignore? Folders stop syncing until you start it again.")) return;
    await api("POST", "quit").catch(() => {});
    closeWindow();
  });

  await refresh(true);
  setInterval(() => { if (!document.hidden) refresh(); }, 1500);
  if (location.hash === "#add") openEditor();
  else if (location.hash.startsWith("#edit-")) {
    const p = state.pairs.find((x) => x.id === location.hash.slice(6));
    if (p) openEditor(p);
  }
}

init();
