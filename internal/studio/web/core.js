"use strict";
// Shared by every mode: DOM helpers, per-viewer storage, the toast, the
// installed-model list, the queue's event stream, and the mode router.
// Each mode file calls Studio.register(name, { … }) and owns its section.

const $ = (id) => document.getElementById(id);
const el = (tag, props = {}, ...children) => {
  const node = Object.assign(document.createElement(tag), props);
  for (const child of children) if (child != null) node.append(child);
  return node;
};
const icon = (name) => {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  const use = document.createElementNS("http://www.w3.org/2000/svg", "use");
  use.setAttribute("href", "#i-" + name);
  svg.append(use);
  return svg;
};

const store = {
  get(key, fallback) { try { const v = localStorage.getItem("studio." + key); return v == null ? fallback : JSON.parse(v); } catch { return fallback; } },
  set(key, value) { try { localStorage.setItem("studio." + key, JSON.stringify(value)); } catch {} },
};

function fmtDuration(seconds) {
  if (!isFinite(seconds) || seconds <= 0) return "";
  if (seconds < 60) return Math.max(1, Math.round(seconds)) + "s";
  const m = Math.floor(seconds / 60), s = Math.round(seconds % 60);
  if (m < 60) return s && m < 10 ? `${m}m ${s}s` : `${m} min`;
  return `${Math.floor(m / 60)}h ${m % 60}m`;
}

let toastTimer;
function toast(text) {
  const t = $("toast");
  t.textContent = text;
  t.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.remove("show"), 2400);
}

async function copy(text, done) {
  try { await navigator.clipboard.writeText(text); toast(done); } catch { toast("Copy failed — select the text instead"); }
}

function askNotify() {
  if ("Notification" in window && Notification.permission === "default") Notification.requestPermission().catch(() => {});
}

// Uploads a reference file (image or audio); resolves to its stored name or null.
async function uploadRef(blob) {
  const res = await fetch("/api/refs", { method: "POST", body: blob });
  if (!res.ok) { toast((await res.text()).trim()); return null; }
  return (await res.json()).name;
}

// A mode's library heading: title, count, and a two-click "Delete all" for
// one kind. The list reloads itself from the library-changed event.
function libraryHead(title, kind, one, many) {
  const count = el("span");
  const clear = el("button", { type: "button", className: "action danger clear-all" }, icon("trash"), "Delete all");
  let total = 0, timer;
  const disarm = () => { clearTimeout(timer); clear.classList.remove("armed"); clear.lastChild.textContent = "Delete all"; };
  clear.onclick = async () => {
    if (!clear.classList.contains("armed")) {
      clear.classList.add("armed");
      clear.lastChild.textContent = `Delete ${total} for good`;
      timer = setTimeout(disarm, 4000);
      return;
    }
    disarm();
    const res = await fetch("/api/library?kind=" + kind, { method: "DELETE" });
    if (!res.ok) { toast((await res.text()).trim()); return; }
    const { deleted } = await res.json();
    toast(`Deleted ${deleted} ${deleted === 1 ? one : many}`);
  };
  const node = el("div", { className: "library-head" }, el("h2", { textContent: title }), count, clear);
  return {
    node,
    update(n) {
      if (n !== total) disarm();
      total = n;
      count.textContent = `${n} ${n === 1 ? one : many}`;
      node.hidden = n === 0;
    },
  };
}

// The empty state of a mode with no models: one button to the Models page,
// already showing that kind.
function getModel(kind, title, text, className = "empty") {
  const browse = el("button", { type: "button", className: "action primary get-model" }, icon("models"), "Browse models");
  browse.onclick = () => Studio.browse(kind);
  const glyph = { chat: "chat", image: "image", speech: "voice", video: "video" }[kind] || "models";
  return el("div", { className }, el("div", { className: "empty-mark", ariaHidden: "true" }, icon(glyph)),
    el("h2", { textContent: title }), el("p", { textContent: text }), browse);
}

const Studio = {
  modes: {},
  active: null,
  models: [],
  snapshot: { jobs: [], library: -1 },

  // mode: { mount?(section), show?(), hide?(), models?(list), snapshot?(snap, libraryChanged) }
  register(name, mode) {
    this.modes[name] = mode;
  },

  // Installed models of one kind ("text", "image", …), or chat-capable ones with "chat".
  modelsOf(kind) {
    return this.models.filter((m) => (kind === "chat" ? m.chat : m.kind === kind));
  },

  async loadModels() {
    try {
      const res = await fetch("/api/models");
      if (res.ok) this.models = await res.json();
    } catch {}
    for (const mode of Object.values(this.modes)) mode.models?.(this.models);
  },

  // Opens the Models page showing one kind (chat, image, speech, video).
  browse(kind) {
    store.set("modelsKind", kind);
    this.modes.models?.filter?.(kind);
    location.hash = "#models";
  },

  // Open the selected installed model, including when its mode is already mounted.
  async openModel(name, id) {
    if (!this.models.some((m) => m.id === id)) await this.loadModels();
    if (!this.models.some((m) => m.id === id) || !this.modes[name]) return;
    this.show(name);
    this.modes[name].selectModel?.(id);
    location.hash = "#" + name;
  },

  show(name) {
    if (!this.modes[name]) name = "chat";
    if (this.active === name) return;
    const previous = this.modes[this.active];
    previous?.hide?.();
    for (const section of document.querySelectorAll(".mode")) section.hidden = section.id !== "mode-" + name;
    for (const link of document.querySelectorAll(".rail a")) {
      if (link.dataset.mode === name) link.setAttribute("aria-current", "page");
      else link.removeAttribute("aria-current");
    }
    const mode = this.modes[name];
    if (!mode.mounted) { mode.mounted = true; mode.mount?.($("mode-" + name)); mode.models?.(this.models); mode.snapshot?.(this.snapshot, true); }
    this.active = name;
    store.set("mode", name);
    mode.show?.();
  },

  connect() {
    const source = new EventSource("/api/events");
    source.onmessage = (event) => {
      document.body.classList.remove("offline");
      const snap = JSON.parse(event.data);
      const libraryChanged = snap.library !== this.snapshot.library;
      // A finished download is a new model for every mode.
      const wasRunning = new Set((this.snapshot.downloads || []).filter((d) => d.state === "running").map((d) => d.id));
      if ((snap.downloads || []).some((d) => d.state === "done" && wasRunning.has(d.id))) this.loadModels();
      this.snapshot = snap;
      for (const mode of Object.values(this.modes)) if (mode.mounted) mode.snapshot?.(snap, libraryChanged);
    };
    source.onerror = () => {
      document.body.classList.add("offline");
      if (source.readyState === EventSource.CLOSED) { source.close(); setTimeout(() => this.connect(), 3000); }
    };
  },

  // Names the machine the page drives — the studio may run elsewhere (-on).
  async loadAbout() {
    try {
      const res = await fetch("/api/about");
      if (!res.ok) return;
      const about = await res.json();
      const chip = $("rail-host");
      chip.textContent = about.host;
      chip.title = [about.host, `${about.os}/${about.arch}`, about.backend && `chat: ${about.backend}`, about.sdBackend && `images: ${about.sdBackend}`].filter(Boolean).join(" · ");
      // Only worth a glance when the studio runs on another machine (-on).
      chip.hidden = !(about.remote && about.host);
      if (about.remote && about.host) document.title = "fornax studio · " + about.host;
    } catch {}
  },

  start() {
    const themes = ["system", "light", "dark"];
    let theme = store.get("theme", "system");
    if (!themes.includes(theme)) theme = "system";
    const toggle = $("theme-toggle");
    const applyTheme = () => {
      if (theme === "system") delete document.documentElement.dataset.theme;
      else document.documentElement.dataset.theme = theme;
      const next = themes[(themes.indexOf(theme) + 1) % themes.length];
      toggle.lastChild.textContent = theme === "system" ? "Auto" : theme === "light" ? "Light" : "Dark";
      toggle.title = `Appearance: ${theme}. Switch to ${next}.`;
      toggle.setAttribute("aria-label", toggle.title);
    };
    toggle.onclick = () => { theme = themes[(themes.indexOf(theme) + 1) % themes.length]; store.set("theme", theme); applyTheme(); };
    applyTheme();
    this.loadAbout();
    const route = () => this.show(location.hash.slice(1) || store.get("mode", "chat"));
    window.addEventListener("hashchange", route);
    document.addEventListener("visibilitychange", () => { if (!document.hidden) this.loadModels(); });
    this.loadModels().then(() => { route(); this.connect(); });
  },
};

addEventListener("DOMContentLoaded", () => Studio.start());
