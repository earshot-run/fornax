"use strict";
// Models mode: picks that fit this machine, popular Hugging Face models,
// search, downloads with live progress, and what is already installed.

(() => {
  const KINDS = [
    { id: "all", label: "All" },
    { id: "chat", label: "Chat" },
    { id: "image", label: "Image" },
    { id: "speech", label: "Voice" },
    { id: "video", label: "Video" },
  ];
  // Which studio mode a model opens in.
  const MODE = { chat: "chat", image: "image", speech: "voice", video: "video" };
  const FIT = { "fits": ["fits", "Fits"], "tight": ["tight", "Tight fit"], "won't fit": ["wont", "Too big"] };

  const state = { token: null, tokenOpen: false, kind: store.get("modelsKind", "all"), picks: [], popular: {}, popularError: {}, memory: 0, backend: "", query: "", results: null, searching: false, previews: {}, downloads: [] };
  const ui = {};

  const fmtBytes = (n) => {
    if (!n) return "";
    const gb = n / 1024 ** 3;
    return gb >= 1 ? `${gb >= 10 ? Math.round(gb) : gb.toFixed(1)} GB` : `${Math.round(n / 1024 ** 2)} MB`;
  };
  const fmtCount = (n) => (n >= 1e6 ? `${(n / 1e6).toFixed(1)}M` : n >= 1e3 ? `${(n / 1e3).toFixed(1)}K` : String(n));
  // A model's page kind: chat models are text, vision and audio.
  const kindOf = (m) => (m.chat ? "chat" : m.kind);
  const shown = (kind) => state.kind === "all" || state.kind === kind;

  function fitPill(fit) {
    const [cls, label] = FIT[fit] || ["", ""];
    return label ? el("span", { className: `fit-pill ${cls}`, textContent: label }) : null;
  }

  function downloadFor(match) {
    return state.downloads.find((d) => (match.key && d.key === match.key) || (match.ref && d.ref === match.ref));
  }

  async function start(body) {
    const res = await fetch("/api/hub/downloads", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
    if (!res.ok) toast((await res.text()).trim());
  }

  function progress(d) {
    const frac = d.total ? Math.min(1, d.done / d.total) : 0;
    const bar = el("div", { className: "bar" }, el("i"));
    bar.firstChild.style.width = `${(frac * 100).toFixed(1)}%`;
    return bar;
  }

  // The button (or progress) a card ends with, for a pick or a search result.
  function actionFor(match, installedId, kind, request) {
    const d = downloadFor(match);
    if (installedId || d?.state === "done") {
      const id = installedId || d.model;
      const use = el("button", { type: "button", className: "text-btn" }, "Open");
      use.onclick = () => { if (MODE[kind]) location.hash = "#" + MODE[kind]; };
      return el("div", { className: "card-action" }, el("span", { className: "installed-mark", title: id }, icon("check"), "Installed"), MODE[kind] ? use : null);
    }
    if (d?.state === "running") {
      const cancel = el("button", { type: "button", className: "icon-btn small-icon", title: "Cancel" }, icon("x"));
      cancel.onclick = () => fetch(`/api/hub/downloads/${d.id}`, { method: "DELETE" });
      const pct = d.total ? Math.floor((d.done / d.total) * 100) : 0;
      return el("div", { className: "card-progress" },
        el("div", { className: "card-progress-top" }, el("span", { textContent: `Downloading ${pct}%` }), el("span", { className: "muted", textContent: `${fmtBytes(d.done)} of ${fmtBytes(d.total)}` }), cancel),
        progress(d));
    }
    const btn = el("button", { type: "button", className: "action primary" }, icon("down"), "Download");
    btn.onclick = () => { btn.disabled = true; start(request); };
    const nodes = [btn];
    if (d?.state === "failed") {
      nodes.unshift(el("p", { className: "card-error", textContent: d.error }));
      btn.lastChild.textContent = "Try again";
    }
    return el("div", { className: "card-action" }, ...nodes);
  }

  function renderHead() {
    ui.tabs.replaceChildren(...KINDS.map((k) => {
      const b = el("button", { type: "button", textContent: k.label });
      b.setAttribute("aria-pressed", String(state.kind === k.id));
      b.onclick = () => { state.kind = k.id; store.set("modelsKind", k.id); render(); loadPopular(k.id); };
      return b;
    }));
    const installed = Studio.models.length;
    const bits = [];
    if (state.memory) bits.push(`${fmtBytes(state.memory)} memory`);
    if (state.backend) bits.push(`runs on ${state.backend === "cpu" ? "the CPU" : state.backend.toUpperCase()}`);
    bits.push(`${installed} installed`);
    ui.machine.textContent = bits.join(" · ");
  }

  function renderPicks() {
    const picks = state.picks.filter((p) => shown(p.kind));
    ui.picks.hidden = !picks.length || !!state.query;
    ui.pickGrid.replaceChildren(...picks.map((p) => el("article", { className: "pick-card" },
      el("div", { className: "pick-top" }, el("span", { className: "kind-label", textContent: KINDS.find((k) => k.id === p.kind)?.label || p.kind }), fitPill(p.fit)),
      el("h3", { textContent: p.title }),
      el("p", { className: "pick-blurb", textContent: p.blurb }),
      el("div", { className: "pick-foot" },
        el("span", { className: "muted", textContent: fmtBytes(p.bytes) }),
        actionFor({ key: p.key }, p.installed, p.kind, { key: p.key })))));
  }

  function renderInstalled() {
    const models = Studio.models.filter((m) => shown(kindOf(m)));
    ui.installed.hidden = !models.length;
    ui.installedList.replaceChildren(...models.map((m) => {
      const kind = kindOf(m);
      const use = el("button", { type: "button", className: "text-btn", textContent: "Open" });
      use.onclick = () => { location.hash = "#" + MODE[kind]; };
      const del = el("button", { type: "button", className: "icon-btn small-icon danger-btn", title: "Delete" }, icon("trash"));
      del.onclick = async () => {
        if (!del.classList.contains("armed")) {
          del.classList.add("armed");
          del.title = "Click again to delete";
          setTimeout(() => { del.classList.remove("armed"); del.title = "Delete"; }, 3000);
          return;
        }
        const res = await fetch(`/api/hub/models/${encodeURIComponent(m.id)}`, { method: "DELETE" });
        if (!res.ok) { toast((await res.text()).trim() || "Could not delete"); return; }
        toast(`${m.name || m.id} deleted`);
        await Studio.loadModels();
        loadPicks();
      };
      return el("div", { className: "model-row" },
        el("div", { className: "model-row-main" },
          el("strong", { textContent: m.name || m.id }),
          el("span", { className: "muted", textContent: [KINDS.find((k) => k.id === kind)?.label || m.kind, m.quant, fmtBytes(m.bytes)].filter(Boolean).join(" · ") })),
        fitPill(m.fit),
        MODE[kind] ? use : null,
        m.runtime === "apple" ? null : del);
    }));
  }

  function installedId(repo) {
    return Studio.models.find((m) => m.repo === repo)?.id || "";
  }

  // A curated pick for this repo downloads its companion files and engine arguments.
  function pickForRepo(repo) {
    return state.picks.find((p) => p.ref.startsWith("hf:" + repo + "/"));
  }

  function hitRow(h, kindHint) {
    const ref = "hf:" + h.repo;
    const id = installedId(h.repo);
    const pv = state.previews[ref];
    const previewKind = pv?.preview?.kind;
    const kind = id
      ? kindOf(Studio.models.find((m) => m.id === id))
      : previewKind === "image" || previewKind === "speech" || previewKind === "video" ? previewKind : (kindHint || "chat");
    const pick = pickForRepo(h.repo);
    const request = pick ? { key: pick.key } : { ref, bytes: pv?.preview?.bytes || 0, ...(kindHint ? { kind: kindHint } : {}) };
    const right = [];
    if (!h.ggufs.length) {
      right.push(el("span", { className: "muted", textContent: "no GGUF files" }));
    } else {
      if (pv?.preview) {
        right.push(el("span", { className: "muted", textContent: `${pv.preview.file.split("/").pop()} · ${fmtBytes(pv.preview.bytes)}` }), fitPill(pv.fit));
      } else if (pv?.loading) {
        right.push(el("span", { className: "muted", textContent: "Checking…" }));
      } else if (pv?.error) {
        right.push(el("span", { className: "card-error", textContent: pv.error }));
      } else {
        const check = el("button", { type: "button", className: "text-btn", textContent: "Check size" });
        check.onclick = () => preview(ref);
        right.push(check);
      }
      right.push(actionFor({ ref: pick ? pick.ref : ref, key: pick?.key }, id, kind, request));
    }
    return el("div", { className: "model-row result-row" },
      el("div", { className: "model-row-main" },
        el("strong", { textContent: h.repo }),
        el("span", { className: "muted", textContent: `${fmtCount(h.downloads)} downloads · ${h.ggufs.length} GGUF file${h.ggufs.length === 1 ? "" : "s"}` })),
      el("div", { className: "result-right" }, ...right));
  }

  function renderPopular() {
    ui.popular.hidden = !!state.query;
    if (state.query) return;
    const list = state.popular[state.kind];
    if (list == null) { ui.popularList.replaceChildren(el("p", { className: "muted", textContent: "Loading popular models…" })); return; }
    if (state.popularError[state.kind]) { ui.popularList.replaceChildren(el("p", { className: "muted", textContent: "Couldn't reach Hugging Face." })); return; }
    if (!list.length) { ui.popularList.replaceChildren(el("p", { className: "muted", textContent: "No popular models in this kind." })); return; }
    const hint = state.kind === "image" || state.kind === "speech" || state.kind === "video" ? state.kind : "";
    ui.popularList.replaceChildren(...list.map((h) => hitRow(h, hint)));
  }

  function renderResults() {
    ui.results.hidden = !state.query;
    if (!state.query) return;
    if (state.searching && !state.results) { ui.resultList.replaceChildren(el("p", { className: "muted", textContent: "Searching Hugging Face…" })); return; }
    const hits = state.results || [];
    if (!hits.length) { ui.resultList.replaceChildren(el("p", { className: "muted", textContent: `No GGUF models match “${state.query}”.` })); return; }
    ui.resultList.replaceChildren(...hits.map((h) => hitRow(h, "")));
  }

  function renderDownloads() {
    // Downloads that started from search results show up there; the rest
    // (picks) show on their cards. Failures from search stay visible here.
    const loose = state.downloads.filter((d) => !d.key && d.state !== "done" && !state.query);
    ui.downloads.hidden = !loose.length;
    ui.downloadList.replaceChildren(...loose.map((d) => el("div", { className: "model-row" },
      el("div", { className: "model-row-main" }, el("strong", { textContent: d.title }), el("span", { className: "muted", textContent: d.state === "failed" ? d.error : `${fmtBytes(d.done)} of ${fmtBytes(d.total)}` })),
      d.state === "running" ? progress(d) : null)));
  }

  // The Hugging Face token: only gated models (Llama, some Gemma) need one.
  function renderToken() {
    const t = state.token;
    if (!t) { ui.token.replaceChildren(); return; }
    if (t.set) {
      const who = t.account ? `Hugging Face: ${t.account}` : "Hugging Face token set";
      const nodes = [el("span", { className: "token-state", textContent: t.error ? `Hugging Face token: ${t.error}` : who })];
      if (t.source === "HF_TOKEN") nodes.push(el("span", { className: "muted", textContent: "from HF_TOKEN" }));
      else {
        const remove = el("button", { type: "button", className: "text-btn", textContent: "Remove" });
        remove.onclick = async () => { await fetch("/api/hub/token", { method: "DELETE" }); loadToken(); };
        nodes.push(remove);
      }
      ui.token.replaceChildren(...nodes);
      return;
    }
    if (!state.tokenOpen) {
      const add = el("button", { type: "button", className: "text-btn", textContent: "Add a Hugging Face token" });
      add.onclick = () => { state.tokenOpen = true; renderToken(); ui.token.querySelector("input")?.focus(); };
      ui.token.replaceChildren(add, el("span", { className: "muted", textContent: "for gated models like Llama" }));
      return;
    }
    const input = el("input", { className: "input token-input", type: "password", placeholder: "hf_…", autocomplete: "off", spellcheck: false });
    const save = el("button", { type: "button", className: "action primary", textContent: "Save" });
    const cancel = el("button", { type: "button", className: "text-btn", textContent: "Cancel" });
    const make = el("a", { href: "https://huggingface.co/settings/tokens", target: "_blank", rel: "noopener", textContent: "Create one" });
    save.onclick = async () => {
      save.disabled = true;
      const res = await fetch("/api/hub/token", { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ token: input.value }) });
      save.disabled = false;
      if (!res.ok) { toast((await res.text()).trim()); return; }
      state.tokenOpen = false;
      loadToken();
    };
    input.addEventListener("keydown", (e) => { if (e.key === "Enter") save.click(); if (e.key === "Escape") cancel.click(); });
    cancel.onclick = () => { state.tokenOpen = false; renderToken(); };
    ui.token.replaceChildren(input, save, cancel, make);
  }

  async function loadToken() {
    try {
      const res = await fetch("/api/hub/token");
      if (res.ok) state.token = await res.json();
    } catch {}
    renderToken();
  }

  function render() {
    if (!ui.root) return;
    renderHead();
    renderDownloads();
    renderPicks();
    renderPopular();
    renderInstalled();
    renderResults();
  }

  async function loadPopular(kind = state.kind, force = false) {
    if (!force && state.popular[kind] && !state.popularError[kind]) return;
    const qkind = kind === "image" || kind === "speech" || kind === "video" ? kind : "";
    try {
      const res = await fetch("/api/hub/popular" + (qkind ? `?kind=${qkind}` : ""));
      if (!res.ok) throw new Error();
      state.popular[kind] = await res.json();
      state.popularError[kind] = false;
    } catch {
      state.popular[kind] = [];
      state.popularError[kind] = true;
    }
    if (state.kind === kind) render();
  }

  async function loadPicks() {
    try {
      const res = await fetch("/api/hub/picks");
      if (!res.ok) return;
      const data = await res.json();
      state.picks = data.picks;
      state.memory = data.memory;
    } catch {}
    render();
  }

  let searchTimer, searchSeq = 0;
  function onSearch() {
    state.query = ui.search.value.trim();
    clearTimeout(searchTimer);
    if (!state.query) { state.results = null; render(); return; }
    state.searching = true;
    state.results = null;
    render();
    searchTimer = setTimeout(async () => {
      const seq = ++searchSeq;
      try {
        const res = await fetch(`/api/hub/search?q=${encodeURIComponent(state.query)}`);
        const hits = res.ok ? await res.json() : [];
        if (seq !== searchSeq) return;
        state.results = hits;
        if (!res.ok) toast("Search failed — is this machine online?");
      } catch { if (seq === searchSeq) state.results = []; }
      state.searching = false;
      render();
    }, 350);
  }

  async function preview(ref) {
    state.previews[ref] = { loading: true };
    render();
    try {
      const res = await fetch(`/api/hub/preview?ref=${encodeURIComponent(ref)}`);
      state.previews[ref] = res.ok ? await res.json() : { error: (await res.text()).trim() };
    } catch { state.previews[ref] = { error: "Could not reach Hugging Face" }; }
    render();
  }

  Studio.register("models", {
    mount(section) {
      ui.root = section;
      ui.machine = el("p", { className: "models-machine" });
      ui.search = el("input", { className: "input models-search", type: "search", placeholder: "Search Hugging Face for any GGUF model", spellcheck: false });
      ui.search.setAttribute("aria-label", "Search models");
      ui.search.addEventListener("input", onSearch);
      ui.tabs = el("div", { className: "segmented kind-tabs" });
      ui.tabs.setAttribute("role", "group");
      ui.tabs.setAttribute("aria-label", "Kind");
      ui.token = el("div", { className: "token-row" });
      const head = el("header", { className: "models-head" },
        el("div", {}, el("h1", { textContent: "Models" }), ui.machine, ui.token),
        ui.search);
      ui.downloadList = el("div", { className: "model-list" });
      ui.downloads = el("section", { className: "models-section" }, el("h2", { textContent: "Downloading" }), ui.downloadList);
      ui.pickGrid = el("div", { className: "pick-grid" });
      ui.picks = el("section", { className: "models-section" }, el("h2", { textContent: "Picks for this machine" }), ui.pickGrid);
      ui.popularList = el("div", { className: "model-list" });
      ui.popular = el("section", { className: "models-section" }, el("h2", { textContent: "Popular on Hugging Face" }), ui.popularList);
      ui.resultList = el("div", { className: "model-list" });
      ui.results = el("section", { className: "models-section" }, el("h2", { textContent: "From Hugging Face" }), ui.resultList);
      ui.installedList = el("div", { className: "model-list" });
      ui.installed = el("section", { className: "models-section" }, el("h2", { textContent: "On this machine" }), ui.installedList);
      section.append(el("div", { className: "models-page" }, head, ui.tabs, ui.downloads, ui.results, ui.picks, ui.popular, ui.installed));
      fetch("/api/about").then((r) => (r.ok ? r.json() : {})).then((a) => { state.backend = a.backend || ""; render(); }).catch(() => {});
      loadPicks();
      loadPopular();
      loadToken();
    },
    models() { render(); },
    snapshot(snap) {
      const before = state.downloads;
      state.downloads = snap.downloads || [];
      // A download that just finished changes which picks are installed.
      if (state.downloads.some((d) => d.state === "done" && before.find((b) => b.id === d.id && b.state === "running"))) loadPicks();
      render();
    },
    show() { document.title = "Models · fornax studio"; loadPicks(); loadPopular(state.kind, true); },
    filter(kind) { state.kind = kind; render(); },
  });
})();
