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

  const state = { token: null, tokenOpen: false, kind: store.get("modelsKind", "all"), picks: [], popular: {}, popularError: {}, pending: new Set(), fitOnly: store.get("modelsFitOnly", false), sort: store.get("modelsSort", "downloads"), searchError: "", memory: 0, backend: "", query: "", results: null, searching: false, previews: {}, downloads: [] };
  if (!["downloads", "lastModified"].includes(state.sort)) state.sort = "downloads";
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
    return [...state.downloads].reverse().find((d) => (match.key && d.key === match.key) || (match.ref && d.ref === match.ref));
  }

  async function start(body) {
    const key = body.key || body.ref;
    if (state.pending.has(key)) return;
    state.pending.add(key);
    render();
    try {
      const res = await fetch("/api/hub/downloads", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
      if (!res.ok) throw new Error((await res.text()).trim() || "Could not start download");
      const d = await res.json();
      state.downloads = [...state.downloads.filter((old) => old.id !== d.id), d];
    } catch (err) { toast(err.message || "Could not start download. Try again."); }
    finally { state.pending.delete(key); render(); }
  }

  async function cancelDownload(d) {
    try {
      const res = await fetch(`/api/hub/downloads/${d.id}`, { method: "DELETE" });
      if (!res.ok) throw new Error((await res.text()).trim());
    } catch (err) { toast(err.message || "Could not cancel download. Try again."); }
  }

  function retryNotice(text, retry) {
    const btn = el("button", { type: "button", className: "text-btn", textContent: "Try again" });
    btn.onclick = retry;
    return el("div", { className: "model-notice", role: "status" }, el("p", { textContent: text }), btn);
  }

  function progress(d) {
    const frac = d.total ? Math.max(0, Math.min(1, d.done / d.total)) : 0;
    const bar = el("div", { className: "bar" }, el("i"));
    bar.setAttribute("role", "progressbar");
    bar.setAttribute("aria-label", "Model download");
    bar.setAttribute("aria-valuemin", "0");
    bar.setAttribute("aria-valuemax", "100");
    if (d.total) bar.setAttribute("aria-valuenow", String(Math.round(frac * 100)));
    bar.firstChild.style.width = `${(frac * 100).toFixed(1)}%`;
    return bar;
  }

  // The button (or progress) a card ends with, for a pick or a search result.
  function actionFor(match, installedId, kind, request) {
    const d = downloadFor(match);
    if (installedId || d?.state === "done") {
      const id = installedId || d.model;
      const use = el("button", { type: "button", className: "text-btn" }, "Open");
      use.onclick = () => { if (MODE[kind]) Studio.openModel(MODE[kind], id); };
      return el("div", { className: "card-action" }, el("span", { className: "installed-mark", title: id }, icon("check"), "Installed"), MODE[kind] ? use : null);
    }
    if (d?.state === "running") {
      const cancel = el("button", { type: "button", className: "icon-btn small-icon", title: "Cancel" }, icon("x"));
      cancel.onclick = () => cancelDownload(d);
      const pct = d.total ? Math.max(0, Math.min(100, Math.floor((d.done / d.total) * 100))) : 0;
      return el("div", { className: "card-progress" },
        el("div", { className: "card-progress-top" }, el("span", { textContent: d.total ? `Downloading ${pct}%` : "Preparing download…" }), el("span", { className: "muted", textContent: d.total ? `${fmtBytes(d.done) || "0 MB"} of ${fmtBytes(d.total)}` : "Finding model files" }), cancel),
        progress(d));
    }
    const btn = el("button", { type: "button", className: "action primary" }, icon("down"), "Download");
    const pending = state.pending.has(request.key || request.ref);
    btn.disabled = pending;
    if (pending) btn.lastChild.textContent = "Starting…";
    btn.onclick = () => start(request);
    const nodes = [btn];
    if (d?.state === "failed") {
      nodes.unshift(el("p", { className: "card-error", textContent: d.error }));
      if (!pending) btn.lastChild.textContent = "Try again";
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
    ui.welcome.hidden = installed > 0 || !!state.query;
    ui.sort.value = state.sort;
    ui.popularTitle.textContent = state.sort === "lastModified" ? "Recently updated on Hugging Face" : "Popular on Hugging Face";
    ui.resultTitle.textContent = state.sort === "lastModified" ? "Recently updated matches" : "Search results";
  }

  function renderPicks() {
    const matching = state.picks.filter((p) => shown(p.kind));
    const picks = matching.filter((p) => !state.fitOnly || p.fit === "fits");
    ui.picks.hidden = !matching.length || !!state.query;
    ui.pickGrid.replaceChildren(...picks.map((p) => el("article", { className: "pick-card" },
      el("div", { className: "pick-top" }, el("span", { className: "kind-label", textContent: KINDS.find((k) => k.id === p.kind)?.label || p.kind }), fitPill(p.fit)),
      el("h3", { textContent: p.title }),
      el("p", { className: "pick-blurb", textContent: p.blurb }),
      el("div", { className: "pick-foot" },
        el("span", { className: "muted", textContent: fmtBytes(p.bytes) }),
        actionFor({ key: p.key }, p.installed, p.kind, { key: p.key })))));
    if (matching.length && !picks.length) ui.pickGrid.append(el("p", { className: "model-notice", textContent: "No picks fit this memory budget. Turn off the filter to see all picks, or search for a smaller model." }));
  }

  function renderInstalled() {
    const models = Studio.models.filter((m) => shown(kindOf(m)));
    ui.installed.hidden = !models.length;
    ui.installedList.replaceChildren(...models.map((m) => {
      const kind = kindOf(m);
      const use = el("button", { type: "button", className: "text-btn", textContent: "Open" });
      use.onclick = () => Studio.openModel(MODE[kind], m.id);
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
        el("span", { className: "muted", textContent: `${fmtCount(h.downloads)} downloads · ${h.ggufs.length} GGUF file${h.ggufs.length === 1 ? "" : "s"}${h.updated && Date.parse(h.updated) > 0 ? " · updated " + new Date(h.updated).toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric" }) : ""}` })),
      el("div", { className: "result-right" }, ...right));
  }

  function renderPopular() {
    ui.popular.hidden = !!state.query;
    if (state.query) return;
    const cacheKey = state.kind + ":" + state.sort;
    const list = state.popular[cacheKey];
    if (list == null) { ui.popularList.replaceChildren(el("p", { className: "muted", textContent: "Loading popular models…" })); return; }
    if (state.popularError[cacheKey]) { ui.popularList.replaceChildren(retryNotice("Couldn't reach Hugging Face. Your installed models are still available.", () => loadPopular(state.kind, true))); return; }
    if (!list.length) { ui.popularList.replaceChildren(el("p", { className: "muted", textContent: "No popular models in this kind." })); return; }
    const hint = state.kind === "image" || state.kind === "speech" || state.kind === "video" ? state.kind : "";
    ui.popularList.replaceChildren(...list.map((h) => hitRow(h, hint)));
  }

  function renderResults() {
    ui.results.hidden = !state.query;
    if (!state.query) return;
    if (state.searching && !state.results) { ui.resultList.replaceChildren(el("p", { className: "muted", textContent: "Searching Hugging Face…" })); return; }
    if (state.searchError) { ui.resultList.replaceChildren(retryNotice(state.searchError, onSearch)); return; }
    const hits = state.results || [];
    if (!hits.length) { ui.resultList.replaceChildren(el("p", { className: "muted", textContent: `No GGUF models match “${state.query}”.` })); return; }
    ui.resultList.replaceChildren(...hits.map((h) => hitRow(h, "")));
  }

  function renderDownloads() {
    // Keep every active or failed transfer reachable across searches and filters.
    const active = state.downloads.filter((d) => d.state !== "done");
    ui.downloads.hidden = !active.length;
    ui.downloadList.replaceChildren(...active.map((d) => {
      const dismiss = el("button", { type: "button", className: "icon-btn small-icon", title: d.state === "running" ? "Cancel download" : "Dismiss download" }, icon("x"));
      dismiss.onclick = () => cancelDownload(d);
      const retry = el("button", { type: "button", className: "text-btn", textContent: "Try again", disabled: state.pending.has(d.key || d.ref) });
      retry.onclick = () => start(d.key ? { key: d.key } : { ref: d.ref, kind: d.kind || "" });
      return el("div", { className: "model-row" },
        el("div", { className: "model-row-main" }, el("strong", { textContent: d.title }), el("span", { className: d.state === "failed" ? "card-error" : "muted", textContent: d.state === "failed" ? d.error : d.total ? `${fmtBytes(d.done)} of ${fmtBytes(d.total)}` : "Preparing download…" })),
        d.state === "running" ? progress(d) : retry, dismiss);
    }));
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
    const order = state.sort;
    const cacheKey = kind + ":" + order;
    if (!force && state.popular[cacheKey] && !state.popularError[cacheKey]) return;
    const q = new URLSearchParams({ sort: order });
    if (["image", "speech", "video"].includes(kind)) q.set("kind", kind);
    state.popular[cacheKey] = null;
    state.popularError[cacheKey] = false;
    render();
    try {
      const res = await fetch("/api/hub/popular?" + q);
      if (!res.ok) throw new Error();
      state.popular[cacheKey] = await res.json();
    } catch {
      state.popular[cacheKey] = [];
      state.popularError[cacheKey] = true;
    }
    if (state.kind === kind && state.sort === order) render();
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

  let searchTimer, searchSeq = 0, searchController;
  function onSearch() {
    const seq = ++searchSeq; // Invalidate in-flight work on every keystroke, including clearing.
    state.query = ui.search.value.trim();
    const query = state.query, order = state.sort;
    clearTimeout(searchTimer);
    searchController?.abort();
    state.results = null;
    state.searchError = "";
    state.searching = !!query;
    render();
    if (!query) return;
    searchTimer = setTimeout(async () => {
      const controller = new AbortController();
      searchController = controller;
      try {
        const q = new URLSearchParams({ q: query, sort: order });
        const res = await fetch("/api/hub/search?" + q, { signal: controller.signal });
        if (!res.ok) throw new Error((await res.text()).trim() || "Search failed. Try again.");
        const hits = await res.json();
        if (seq === searchSeq) state.results = hits;
      } catch (err) {
        if (seq === searchSeq && !controller.signal.aborted) state.searchError = err.message || "Couldn't reach Hugging Face. Try again.";
      } finally {
        if (seq === searchSeq) { state.searching = false; render(); }
      }
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
      ui.sort = el("select", { className: "models-sort" },
        el("option", { value: "downloads", textContent: "Most downloaded" }),
        el("option", { value: "lastModified", textContent: "Recently updated" }));
      ui.sort.setAttribute("aria-label", "Sort models");
      ui.sort.onchange = () => { state.sort = ui.sort.value; store.set("modelsSort", state.sort); onSearch(); loadPopular(); };
      ui.welcome = el("section", { className: "models-welcome" },
        el("div", { className: "welcome-mark", ariaHidden: "true" }, icon("models")),
        el("div", {}, el("span", { className: "eyebrow", textContent: "Your studio, your models" }),
          el("h2", { textContent: "Make room for your next idea." }),
          el("p", { textContent: "Start with a pick below. Check its memory fit, download once, and create on your own machine." })),
        el("span", { className: "welcome-note", textContent: "Chat · Images · Voice · Video" }));
      const head = el("header", { className: "models-head" },
        el("div", {}, el("h1", { textContent: "Find your next model" }), ui.machine, ui.token),
        el("div", { className: "models-discovery" }, ui.search, ui.sort));
      ui.downloadList = el("div", { className: "model-list" });
      ui.downloads = el("section", { className: "models-section" }, el("h2", { textContent: "Downloads" }), ui.downloadList);
      ui.pickGrid = el("div", { className: "pick-grid" });
      const fit = el("input", { type: "checkbox", checked: state.fitOnly });
      fit.onchange = () => { state.fitOnly = fit.checked; store.set("modelsFitOnly", state.fitOnly); renderPicks(); };
      ui.picks = el("section", { className: "models-section" },
        el("div", { className: "models-section-head" }, el("h2", { textContent: "Picks for this machine" }),
          el("label", { className: "fit-filter" }, fit, "Only picks that fit")), ui.pickGrid);
      ui.popularList = el("div", { className: "model-list" });
      ui.popularTitle = el("h2");
      ui.popular = el("section", { className: "models-section" }, ui.popularTitle, ui.popularList);
      ui.resultList = el("div", { className: "model-list" });
      ui.resultTitle = el("h2");
      ui.results = el("section", { className: "models-section" }, ui.resultTitle, ui.resultList);
      ui.installedList = el("div", { className: "model-list" });
      ui.installed = el("section", { className: "models-section" }, el("h2", { textContent: "On this machine" }), ui.installedList);
      section.append(el("div", { className: "models-page" }, head, ui.welcome, ui.tabs, ui.downloads, ui.installed, ui.results, ui.picks, ui.popular));
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
    filter(kind) { state.kind = kind; render(); loadPopular(kind); },
  });
})();
