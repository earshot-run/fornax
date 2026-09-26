"use strict";
// Video mode: composer, queue tiles, a gallery of clips that play on hover,
// and a viewer. Its own scope: image.js owns the page-level names.

(() => {
  const RATIOS = [
    { id: "16:9", w: 16, h: 9, label: "Wide" },
    { id: "9:16", w: 9, h: 16, label: "Tall" },
    { id: "1:1", w: 1, h: 1, label: "Square" },
    { id: "4:3", w: 4, h: 3, label: "Classic" },
  ];
  // Long side in pixels. Video cost grows with pixels × frames, and this
  // runs on a laptop GPU, so the tiers stay small.
  const TIERS = [
    { id: "draft", label: "Draft", side: 384 },
    { id: "small", label: "Small", side: 512 },
    { id: "standard", label: "Standard", side: 704 },
  ];
  // Wan wants 4n+1 frames. sd-cli writes 24 fps unless a model's args say otherwise.
  const FRAMES = [17, 33, 49, 81, 121];
  const FPS = 24;
  const PHASES = { preparing: "Getting ready", loading: "Loading model", decoding: "Decoding frames", saving: "Saving" };

  const key = (k) => "video." + k;
  const state = {
    models: [],
    videos: [],
    jobs: [],
    start: store.get(key("start"), null),
    ratio: store.get(key("ratio"), "16:9"),
    tier: store.get(key("tier"), "small"),
    frames: store.get(key("frames"), 33),
    seedLocked: store.get(key("seedLocked"), false),
    viewing: -1,
    fresh: new Set(),
    seen: null,
  };
  const ui = {};
  const tiles = new Map();

  const seconds = (frames) => frames / FPS;
  const fmtClip = (frames) => {
    const s = seconds(frames);
    return (s < 10 ? s.toFixed(1) : Math.round(s)) + "s";
  };

  function dims(ratioId = state.ratio, tierId = state.tier) {
    const ratio = RATIOS.find((r) => r.id === ratioId) || RATIOS[0];
    const tier = TIERS.find((t) => t.id === tierId) || TIERS[1];
    const snap = (v) => Math.max(128, Math.round(v / 32) * 32);
    const long = tier.side, short = long * Math.min(ratio.w, ratio.h) / Math.max(ratio.w, ratio.h);
    return ratio.w >= ratio.h ? { width: snap(long), height: snap(short) } : { width: snap(short), height: snap(long) };
  }

  // The last run of this model says what the next costs: a step grows a bit
  // faster than pixels × frames (attention), the VAE decode about linearly.
  function estimate(modelId, width, height, frames, steps, withStart) {
    const measured = state.videos.filter((v) => v.model === modelId && v.stepSeconds > 0);
    const past = measured.find((v) => (v.refs?.length > 0) === withStart) || measured[0];
    if (!past) return null;
    const scale = (width * height * frames) / (past.width * past.height * (past.frames || 1));
    const step = past.stepSeconds * Math.pow(scale, 1.15);
    const decode = (past.decodeSeconds || 0) * scale;
    const overhead = Math.max(0, (past.seconds || 0) - past.stepSeconds * past.steps - (past.decodeSeconds || 0));
    return { total: overhead + step * steps + decode, step, decode };
  }

  // ---------- composer ----------

  function field(label, control, hint) {
    const head = el("div", { className: "label" }, el("span", { textContent: label }));
    if (hint) head.append(hint);
    return el("div", { className: "field" }, head, control);
  }

  function buildComposer() {
    ui.model = el("select", { id: "video-model" });
    ui.prompt = el("textarea", { id: "video-prompt", placeholder: "A paper boat drifting down a rain-soaked street, slow push in, soft evening light" });
    ui.negative = el("textarea", { className: "small", placeholder: "What to keep out: blur, flicker, text" });
    ui.sizeOut = el("output");
    ui.ratios = el("div", { className: "segmented", role: "group", ariaLabel: "Aspect ratio" });
    ui.tierHint = el("span", { className: "hint" });
    ui.tiers = el("div", { className: "segmented", role: "group", ariaLabel: "Resolution" });
    ui.lengthOut = el("output");
    ui.frames = el("div", { className: "segmented", role: "group", ariaLabel: "Length" });
    ui.start = el("div", { className: "start-frame" });
    ui.startFile = el("input", { type: "file", accept: "image/png,image/jpeg,image/webp", hidden: true });
    ui.steps = el("input", { type: "range", min: 4, max: 50, step: 1 });
    ui.stepsOut = el("output");
    ui.seed = el("input", { className: "input", inputMode: "numeric", placeholder: "Random" });
    ui.seedHint = el("span", { className: "hint" });
    ui.seedLock = el("button", { type: "button", className: "icon-btn", title: "Keep this seed" }, icon("lock"));
    ui.seedRoll = el("button", { type: "button", className: "icon-btn", title: "New seed" }, icon("dice"));
    ui.advanced = el("details", {},
      el("summary", {}, icon("chev"), "More options"),
      el("div", { className: "advanced" },
        el("div", { className: "field" }, el("div", { className: "label" }, el("label", { textContent: "Steps" }), ui.stepsOut), ui.steps),
        field("Seed", el("div", { className: "seed-row" }, ui.seed, ui.seedLock, ui.seedRoll), ui.seedHint),
        field("Negative prompt", ui.negative)));
    ui.generate = el("button", { className: "generate", type: "button" }, el("span", { textContent: "Generate" }), el("kbd", { textContent: /Mac|iPhone|iPad/.test(navigator.platform) ? "⌘ ↵" : "Ctrl ↵" }));
    ui.estimate = el("span");
    ui.queueNote = el("span");

    const modelLabel = el("label", { className: "label", htmlFor: "video-model", textContent: "Model" });
    const promptLabel = el("label", { className: "label", htmlFor: "video-prompt", textContent: "Prompt" });
    ui.composer = el("aside", { className: "composer" },
      el("div", { className: "composer-scroll" },
        el("div", { className: "field" }, modelLabel, ui.model),
        el("div", { className: "field" }, promptLabel, ui.prompt),
        field("Start frame", ui.start, el("span", { className: "hint", textContent: "optional · image to video" })),
        field("Shape", ui.ratios, ui.sizeOut),
        field("Detail", ui.tiers, ui.tierHint),
        field("Length", ui.frames, ui.lengthOut),
        ui.advanced,
        ui.startFile),
      el("div", { className: "composer-foot" }, ui.generate, el("div", { className: "foot-note" }, ui.estimate, ui.queueNote)));
    return ui.composer;
  }

  function segment(container, items, isOn, onPick, render) {
    container.replaceChildren(...items.map((item) => {
      const b = el("button", { type: "button" }, ...render(item));
      b.setAttribute("aria-pressed", String(isOn(item)));
      b.onclick = () => onPick(item);
      return b;
    }));
  }

  function renderSegments() {
    segment(ui.ratios, RATIOS, (r) => r.id === state.ratio, (r) => { state.ratio = r.id; store.set(key("ratio"), r.id); renderSegments(); }, (r) => {
      const big = 14, w = r.w >= r.h ? big : Math.round(big * r.w / r.h), h = r.h >= r.w ? big : Math.round(big * r.h / r.w);
      const glyph = el("span", { className: "ratio-glyph" });
      glyph.style.width = w + "px"; glyph.style.height = h + "px";
      return [el("span", { className: "ratio-box" }, glyph), r.label];
    });
    segment(ui.tiers, TIERS, (t) => t.id === state.tier, (t) => { state.tier = t.id; store.set(key("tier"), t.id); renderSegments(); }, (t) => [t.label]);
    segment(ui.frames, FRAMES, (f) => f === state.frames, (f) => { state.frames = f; store.set(key("frames"), f); renderSegments(); }, (f) => [fmtClip(f)]);
    const { width, height } = dims();
    ui.sizeOut.textContent = `${width} × ${height}`;
    ui.lengthOut.textContent = `${state.frames} frames at ${FPS} fps`;
    updateEstimate();
  }

  function renderModels() {
    if (!ui.model) return;
    state.models = Studio.modelsOf("video");
    const wanted = store.get(key("model"), null);
    ui.model.replaceChildren(...state.models.map((m) => el("option", { value: m.id, textContent: m.name || m.id })));
    if (state.models.some((m) => m.id === wanted)) ui.model.value = wanted;
    ui.model.disabled = state.models.length === 0;
    ui.generate.disabled = state.models.length === 0;
    updateEstimate();
    renderGrid();
  }

  function renderStart() {
    if (state.start) {
      const remove = el("button", { type: "button", title: "Remove" }, icon("x"));
      remove.onclick = (e) => { e.stopPropagation(); setStart(null); };
      const img = el("img", { src: "/files/refs/" + state.start, alt: "Start frame" });
      ui.start.replaceChildren(el("div", { className: "start-thumb" }, img, remove),
        el("div", { className: "start-copy" }, el("strong", { textContent: "Starts from this frame" }), el("span", { textContent: "The prompt describes what happens next." })));
      ui.start.classList.add("set");
    } else {
      const add = el("button", { type: "button", className: "start-add" }, icon("image"),
        el("span", {}, el("strong", { textContent: "Add a start frame" }), el("span", { textContent: "Drop, paste or pick an image" })));
      add.onclick = () => ui.startFile.click();
      ui.start.replaceChildren(add);
      ui.start.classList.remove("set");
    }
    updateEstimate();
  }

  function setStart(name) {
    state.start = name;
    store.set(key("start"), name);
    renderStart();
  }

  async function addStart(blob) {
    if (!blob) return;
    if (!/^image\/(png|jpeg|webp)$/.test(blob.type)) { toast("A start frame must be PNG, JPEG or WebP"); return; }
    const name = await uploadRef(blob);
    if (name) setStart(name);
  }

  function renderSeed() {
    ui.seedLock.setAttribute("aria-pressed", String(state.seedLocked));
    ui.seedHint.textContent = state.seedLocked ? "kept between runs" : "random each run";
    ui.seed.placeholder = state.seedLocked ? "Seed" : "Random";
  }

  function updateEstimate() {
    if (!ui.steps) return;
    const { width, height } = dims();
    const steps = Number(ui.steps.value);
    ui.stepsOut.textContent = steps;
    if (!state.models.length) { ui.estimate.textContent = ""; ui.tierHint.textContent = ""; return; }
    const est = estimate(ui.model.value, width, height, state.frames, steps, !!state.start);
    ui.estimate.textContent = est ? `About ${fmtDuration(est.total)} per clip` : "The first run measures this machine";
    ui.tierHint.textContent = est ? `${fmtDuration(est.step)} a step` : "";
  }

  function readRequest() {
    const seedText = ui.seed.value.trim();
    const { width, height } = dims();
    return {
      model: ui.model.value,
      prompt: ui.prompt.value.trim(),
      negative: ui.negative.value.trim(),
      width, height,
      frames: state.frames,
      steps: Number(ui.steps.value),
      seed: state.seedLocked && /^\d+$/.test(seedText) ? Number(seedText) : -1,
      refs: state.start ? [state.start] : [],
    };
  }

  async function submit(request) {
    if (!request.model) { toast("Add a video model first"); return; }
    if (!request.prompt) { ui.prompt.focus(); toast("Describe the clip first"); return; }
    askNotify();
    const res = await fetch("/api/jobs", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ kind: "video", ...request }) });
    if (!res.ok) { toast((await res.text()).trim()); return; }
    const job = await res.json();
    if (state.seedLocked && !ui.seed.value) ui.seed.value = job.seed;
  }

  function saveDraft() {
    store.set(key("prompt"), ui.prompt.value);
    store.set(key("negative"), ui.negative.value);
    store.set(key("steps"), Number(ui.steps.value));
    store.set(key("seed"), ui.seed.value);
    store.set(key("model"), ui.model.value);
  }

  function autosize() {
    ui.prompt.style.height = "auto";
    ui.prompt.style.height = Math.min(ui.prompt.scrollHeight + 2, 320) + "px";
  }

  // ---------- queue + gallery ----------

  function pick(v) {
    return { model: v.model, prompt: v.prompt, negative: v.negative || "", width: v.width, height: v.height, frames: v.frames, steps: v.steps, seed: v.seed, refs: v.refs || [] };
  }

  function progressOf(job) {
    const est = estimate(job.model, job.width, job.height, job.frames, job.total, job.refs?.length > 0);
    const stepCost = job.stepSeconds || (est && est.step) || 0;
    const decode = (est && est.decode) || stepCost * 4;
    const sampleTotal = stepCost * job.total;
    if (!sampleTotal) return job.phase === "sampling" ? job.step / job.total * 0.8 : 0.02;
    const whole = sampleTotal + decode;
    if (job.phase === "sampling") return (job.step * stepCost) / whole;
    if (job.phase === "decoding" || job.phase === "saving") {
      const into = job.phase === "saving" ? decode : Math.min(decode * 0.95, (Date.now() - (job.decodeFrom || Date.now())) / 1000);
      return Math.min(0.99, (sampleTotal + into) / whole);
    }
    return 0.02;
  }

  function remaining(job) {
    const est = estimate(job.model, job.width, job.height, job.frames, job.total, job.refs?.length > 0);
    const stepCost = job.stepSeconds || (est && est.step);
    if (!stepCost) return est ? est.total : null;
    const decode = (est && est.decode) || 0;
    if (job.phase === "sampling") return stepCost * (job.total - job.step) + decode;
    if (job.phase === "decoding") return decode ? Math.max(1, decode - (Date.now() - (job.decodeFrom || Date.now())) / 1000) : null;
    return est ? est.total : null;
  }

  function jobTile(job) {
    let t = tiles.get("job:" + job.id);
    if (!t || t.state !== job.state) {
      const canvas = el("div", { className: "job-canvas" });
      const phase = el("span", { className: "job-phase" });
      const cancel = el("button", { type: "button", className: "icon-btn small-icon" }, icon("x"));
      cancel.onclick = () => fetch(`/api/jobs/${job.id}`, { method: "DELETE" });
      const body = el("div", { className: "job-body" }, el("div", { className: "job-top" }, phase, cancel));
      t = { state: job.state, canvas, phase, cancel, body, preview: 0 };
      t.node = el("div", { className: `tile job ${job.state}` }, canvas, body);
      canvas.append(el("span", { className: "clip-badge", textContent: `${fmtClip(job.frames)} · ${job.width}×${job.height}` }));
      if (job.refs?.length) canvas.prepend(el("img", { className: "job-start", src: "/files/refs/" + job.refs[0], alt: "" }));
      if (job.state === "failed") {
        const retry = el("button", { type: "button", className: "text-btn", textContent: "Try again" });
        retry.onclick = () => { fetch(`/api/jobs/${job.id}`, { method: "DELETE" }); submit(pick(job)); };
        const log = el("button", { type: "button", className: "text-btn", textContent: "Copy error" });
        log.onclick = () => copy(job.error, "Error copied");
        body.append(el("pre", { className: "job-error mono", textContent: job.error }), el("div", { className: "job-actions" }, retry, log));
      } else {
        t.bar = el("i");
        t.elapsed = el("span");
        t.left = el("span");
        if (job.state === "running") body.append(el("div", { className: "bar" }, t.bar), el("div", { className: "job-meta" }, t.elapsed, t.left));
        body.append(el("div", { className: "job-prompt", textContent: job.prompt }));
      }
      tiles.set("job:" + job.id, t);
    }
    t.canvas.style.aspectRatio = `${job.width} / ${job.height}`;
    if (job.preview && job.preview !== t.preview) {
      t.preview = job.preview;
      const img = el("img", { className: "job-preview", src: `/files/previews/${job.id}.webp?v=${job.preview}`, alt: "" });
      img.onload = () => { t.canvas.querySelector(".job-preview")?.remove(); t.canvas.prepend(img); };
    }
    if (job.phase === "decoding" && !t.decodeFrom) t.decodeFrom = Date.now();
    job.decodeFrom = t.decodeFrom;
    t.cancel.title = job.state === "failed" ? "Dismiss" : "Cancel";
    if (job.state === "failed") {
      t.phase.textContent = "Didn't finish";
    } else if (job.state === "queued") {
      const ahead = state.jobs.filter((j) => j.state === "queued" || j.state === "running").indexOf(job);
      t.phase.textContent = ahead > 0 ? `Queued · ${ahead} ahead` : "Queued";
    } else {
      t.phase.textContent = job.phase === "sampling" ? (job.step ? `Step ${job.step} of ${job.total}` : "Sampling") : (PHASES[job.phase] || "Starting");
      t.bar.style.width = (progressOf(job) * 100).toFixed(1) + "%";
      t.elapsed.textContent = fmtDuration((Date.now() - Date.parse(job.started)) / 1000) || "0s";
      const left = remaining(job);
      t.left.textContent = left != null ? `~${fmtDuration(left)} left` : "";
    }
    return t.node;
  }

  function videoTile(video, index) {
    let t = tiles.get("vid:" + video.id);
    if (!t) {
      // The #t fragment makes the browser paint the first frame as a poster.
      const player = el("video", { src: `/files/library/${video.id}.webm#t=0.001`, muted: true, loop: true, playsInline: true, preload: "metadata" });
      player.width = video.width; player.height = video.height;
      const node = el("button", { type: "button", className: "tile clip" }, player,
        el("span", { className: "clip-badge", textContent: fmtClip(video.frames || 1) }),
        el("span", { className: "clip-play" }, icon("play")),
        el("span", { className: "caption", textContent: video.prompt }));
      node.addEventListener("mouseenter", () => { player.play().catch(() => {}); });
      node.addEventListener("mouseleave", () => { player.pause(); player.currentTime = 0; });
      node.addEventListener("focus", () => { player.play().catch(() => {}); });
      node.addEventListener("blur", () => { player.pause(); });
      if (state.fresh.has(video.id)) {
        node.classList.add("fresh");
        node.addEventListener("animationend", () => node.classList.remove("fresh"), { once: true });
      }
      t = { node };
      tiles.set("vid:" + video.id, t);
    }
    t.node.onclick = () => openViewer(index);
    return t.node;
  }

  function renderGrid() {
    if (!ui.grid) return;
    const active = state.jobs.filter((j) => j.state !== "canceled");
    const nodes = [...active.map(jobTile), ...state.videos.map(videoTile)];
    const live = new Set([...active.map((j) => "job:" + j.id), ...state.videos.map((v) => "vid:" + v.id)]);
    for (const k of tiles.keys()) if (!live.has(k)) tiles.delete(k);
    state.fresh.clear();
    if (nodes.length !== ui.grid.children.length || nodes.some((n, i) => ui.grid.children[i] !== n)) ui.grid.replaceChildren(...nodes);
    ui.head.update(state.videos.length);
    renderEmpty(nodes.length === 0);
    const running = state.jobs.find((j) => j.state === "running");
    const queued = state.jobs.filter((j) => j.state === "queued").length;
    ui.queueNote.textContent = queued ? `${queued} queued` : "";
    if (Studio.active === "video") document.title = running ? `${Math.round(progressOf(running) * 100)}% · fornax studio` : "fornax studio";
  }

  function renderEmpty(show) {
    if (!show) { ui.empty.replaceChildren(); return; }
    if (!state.models.length) {
      ui.empty.replaceChildren(getModel("video", "No video models yet", "Download one to start. Wan 2.2 makes short clips from a prompt or a still."));
      return;
    }
    ui.empty.replaceChildren(el("div", { className: "empty" },
      el("h2", { textContent: "No clips yet" }),
      el("p", { textContent: "Describe a shot on the left, or start from an image. Short and small is fast: a draft clip is a good first run." })));
  }

  async function loadVideos() {
    const res = await fetch("/api/library?kind=video");
    if (!res.ok) return;
    const videos = await res.json();
    if (state.seen) for (const v of videos) if (!state.seen.has(v.id)) state.fresh.add(v.id);
    const added = state.seen ? videos.filter((v) => !state.seen.has(v.id)) : [];
    state.seen = new Set(videos.map((v) => v.id));
    state.videos = videos;
    renderGrid();
    updateEstimate();
    if (added.length) notifyDone(added[0]);
    if (state.viewing >= 0) refreshViewer();
  }

  function notifyDone(video) {
    if (!document.hidden || !("Notification" in window) || Notification.permission !== "granted") return;
    const n = new Notification("Video ready", { body: video.prompt.slice(0, 120) });
    n.onclick = () => { window.focus(); location.hash = "video"; openViewer(state.videos.findIndex((v) => v.id === video.id)); n.close(); };
  }

  // ---------- viewer ----------

  function buildViewer() {
    const v = {};
    v.player = el("video", { controls: true, loop: true, playsInline: true, autoplay: true, muted: true });
    v.prev = el("button", { type: "button", className: "icon-btn nav prev", title: "Previous (←)" }, icon("left"));
    v.next = el("button", { type: "button", className: "icon-btn nav next", title: "Next (→)" }, icon("chev"));
    v.stage = el("div", { className: "stage" }, v.player, v.prev, v.next);
    v.when = el("span");
    v.close = el("button", { type: "button", className: "icon-btn small-icon", title: "Close (Esc)" }, icon("x"));
    v.prompt = el("p", { className: "prompt-text" });
    v.copyPrompt = el("button", { type: "button", className: "text-btn", textContent: "Copy" });
    v.neg = el("p", { className: "prompt-text" });
    v.negWrap = el("div", { className: "field" }, el("div", { className: "label", textContent: "Negative" }), v.neg);
    v.startImg = el("img", { alt: "" });
    v.startWrap = el("div", { className: "field" }, el("div", { className: "label", textContent: "Start frame" }), el("div", { className: "ref-strip" }, v.startImg));
    v.facts = el("dl", { className: "facts" });
    const action = (cls, name, text) => el("button", { type: "button", className: "action " + cls }, icon(name), el("span", { textContent: text }));
    v.vary = action("primary", "redo", "Vary");
    v.reuse = action("", "edit", "Reuse");
    v.frame = action("", "image", "Frame as start");
    v.download = el("a", { className: "action", download: "" }, icon("down"), el("span", { textContent: "Download" }));
    v.del = action("danger", "trash", "Delete");
    v.dialog = el("dialog", { className: "video-viewer" }, el("div", { className: "viewer" }, v.stage,
      el("aside", { className: "details" },
        el("div", { className: "details-head" }, v.when, v.close),
        el("div", { className: "field" }, el("div", { className: "label" }, el("span", { textContent: "Prompt" }), v.copyPrompt), v.prompt),
        v.negWrap, v.startWrap, v.facts,
        el("div", { className: "actions" }, v.vary, v.reuse, v.frame, v.download, v.del))));
    document.body.append(v.dialog);

    v.close.onclick = () => v.dialog.close();
    v.prev.onclick = () => step(-1);
    v.next.onclick = () => step(1);
    v.dialog.addEventListener("close", () => { state.viewing = -1; v.player.pause(); v.player.removeAttribute("src"); v.player.load(); });
    v.stage.addEventListener("click", (e) => { if (e.target === v.stage) v.dialog.close(); });
    v.copyPrompt.onclick = () => copy(state.videos[state.viewing].prompt, "Prompt copied");
    v.vary.onclick = () => { submit({ ...pick(state.videos[state.viewing]), seed: -1 }); v.dialog.close(); toast("Queued a variation"); };
    v.reuse.onclick = () => { reuse(state.videos[state.viewing]); v.dialog.close(); ui.prompt.focus(); };
    v.frame.onclick = () => frameAsStart();
    v.del.onclick = async () => {
      if (!v.del.classList.contains("armed")) { v.del.classList.add("armed"); v.del.lastChild.textContent = "Delete for good"; return; }
      const video = state.videos[state.viewing];
      await fetch(`/api/library/${video.id}`, { method: "DELETE" });
      state.videos.splice(state.viewing, 1);
      if (state.viewing >= state.videos.length) state.viewing = state.videos.length - 1;
      if (state.viewing < 0) v.dialog.close(); else refreshViewer();
    };
    ui.viewer = v;
  }

  function openViewer(index) {
    if (index < 0) return;
    state.viewing = index;
    refreshViewer();
    if (!ui.viewer.dialog.open) ui.viewer.dialog.showModal();
  }

  function refreshViewer() {
    const v = ui.viewer, video = state.videos[state.viewing];
    if (!video) { v.dialog.close(); return; }
    const src = `/files/library/${video.id}.webm`;
    if (v.player.getAttribute("src") !== src) { v.player.src = src; v.player.play().catch(() => {}); }
    v.prompt.textContent = video.prompt;
    v.neg.textContent = video.negative || "";
    v.negWrap.hidden = !video.negative;
    v.startWrap.hidden = !video.refs?.length;
    if (video.refs?.length) v.startImg.src = "/files/refs/" + video.refs[0];
    v.when.textContent = new Date(video.created).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
    const facts = [["Model", video.model], ["Size", `${video.width} × ${video.height}`], ["Length", `${fmtClip(video.frames || 1)} · ${video.frames} frames`], ["Steps", video.steps], ["Seed", video.seed]];
    if (video.seconds) facts.push(["Took", fmtDuration(video.seconds)]);
    v.facts.replaceChildren(...facts.flatMap(([k, val]) => [el("dt", { textContent: k }), el("dd", { textContent: String(val) })]));
    v.download.href = src;
    v.download.download = `${video.model}-${video.seed}.webm`;
    v.del.classList.remove("armed");
    v.del.lastChild.textContent = "Delete";
    v.prev.hidden = state.viewing <= 0;
    v.next.hidden = state.viewing >= state.videos.length - 1;
  }

  function step(delta) {
    const next = state.viewing + delta;
    if (next >= 0 && next < state.videos.length) { state.viewing = next; refreshViewer(); }
  }

  // Grabs the frame on screen, so the next clip can pick up where this one is.
  async function frameAsStart() {
    const player = ui.viewer.player;
    player.pause();
    const canvas = el("canvas", { width: player.videoWidth, height: player.videoHeight });
    canvas.getContext("2d").drawImage(player, 0, 0);
    const blob = await new Promise((resolve) => canvas.toBlob(resolve, "image/png"));
    await addStart(blob);
    ui.viewer.dialog.close();
    toast("Frame set as the start");
    ui.prompt.focus();
  }

  function reuse(video) {
    ui.prompt.value = video.prompt;
    ui.negative.value = video.negative || "";
    if (state.models.some((m) => m.id === video.model)) ui.model.value = video.model;
    ui.steps.value = video.steps;
    const ratio = RATIOS.find((r) => { const d = dims(r.id, state.tier); return Math.abs(d.width / d.height - video.width / video.height) < 0.05; });
    if (ratio) state.ratio = ratio.id;
    const tier = TIERS.find((t) => { const d = dims(state.ratio, t.id); return d.width === video.width && d.height === video.height; });
    if (tier) state.tier = tier.id;
    if (FRAMES.includes(video.frames)) state.frames = video.frames;
    state.seedLocked = true;
    ui.seed.value = video.seed;
    state.start = video.refs?.[0] || null;
    store.set(key("ratio"), state.ratio); store.set(key("tier"), state.tier); store.set(key("frames"), state.frames);
    store.set(key("seedLocked"), true); store.set(key("start"), state.start);
    renderSegments(); renderSeed(); renderStart(); saveDraft(); autosize();
    if (!ui.advanced.open && (video.negative || video.steps !== 20)) ui.advanced.open = true;
  }

  // ---------- wiring ----------

  function wire() {
    ui.prompt.value = store.get(key("prompt"), "");
    ui.negative.value = store.get(key("negative"), "");
    ui.steps.value = store.get(key("steps"), 20);
    ui.seed.value = store.get(key("seed"), "");
    ui.advanced.open = store.get(key("advancedOpen"), false);

    ui.prompt.addEventListener("input", () => { autosize(); saveDraft(); });
    ui.negative.addEventListener("input", saveDraft);
    ui.steps.addEventListener("input", () => { updateEstimate(); saveDraft(); });
    ui.model.addEventListener("change", () => { updateEstimate(); saveDraft(); });
    ui.seed.addEventListener("input", () => {
      ui.seed.value = ui.seed.value.replace(/\D/g, "");
      if (ui.seed.value && !state.seedLocked) { state.seedLocked = true; store.set(key("seedLocked"), true); renderSeed(); }
      saveDraft();
    });
    ui.seedLock.onclick = () => { state.seedLocked = !state.seedLocked; store.set(key("seedLocked"), state.seedLocked); if (!state.seedLocked) ui.seed.value = ""; renderSeed(); saveDraft(); };
    ui.seedRoll.onclick = () => { ui.seed.value = Math.floor(Math.random() * 2 ** 31); state.seedLocked = true; store.set(key("seedLocked"), true); renderSeed(); saveDraft(); };
    ui.generate.onclick = () => submit(readRequest());
    ui.startFile.addEventListener("change", (e) => { addStart(e.target.files[0]); e.target.value = ""; });
    ui.advanced.addEventListener("toggle", () => store.set(key("advancedOpen"), ui.advanced.open));

    document.addEventListener("keydown", (e) => {
      if (Studio.active !== "video") return;
      if ((e.metaKey || e.ctrlKey) && e.key === "Enter") { e.preventDefault(); submit(readRequest()); return; }
      if (!ui.viewer.dialog.open) return;
      if (e.key === "ArrowLeft") step(-1);
      if (e.key === "ArrowRight") step(1);
    });
    document.addEventListener("paste", (e) => {
      if (Studio.active !== "video") return;
      const file = [...(e.clipboardData?.files || [])].find((f) => f.type.startsWith("image/"));
      if (file) { e.preventDefault(); addStart(file); }
    });
    const c = ui.composer;
    c.addEventListener("dragover", (e) => { if ([...e.dataTransfer.types].includes("Files")) { e.preventDefault(); c.classList.add("dragging"); } });
    c.addEventListener("dragleave", (e) => { if (!c.contains(e.relatedTarget)) c.classList.remove("dragging"); });
    c.addEventListener("drop", (e) => { e.preventDefault(); c.classList.remove("dragging"); addStart(e.dataTransfer.files[0]); });

    setInterval(() => { if (Studio.active === "video" && state.jobs.some((j) => j.state === "running")) renderGrid(); }, 1000);

    if (state.start) {
      fetch("/files/refs/" + state.start, { method: "HEAD" }).then((r) => { if (!r.ok) setStart(null); }).catch(() => {});
    }
  }

  Studio.register("video", {
    mount(section) {
      ui.grid = el("div", { className: "grid clips" });
      ui.head = libraryHead("Videos", "video", "video", "videos");
      ui.empty = el("div");
      section.append(el("div", { className: "app" },
        buildComposer(),
        el("main", { className: "main" }, el("div", { className: "main-inner" }, ui.head.node, ui.grid, ui.empty))));
      buildViewer();
      wire();
      renderSegments();
      renderSeed();
      renderStart();
      autosize();
    },
    models: renderModels,
    snapshot(snap, libraryChanged) {
      state.jobs = snap.jobs.filter((j) => j.kind === "video");
      if (libraryChanged) loadVideos();
      else renderGrid();
    },
    show() { autosize(); renderGrid(); },
    hide() { for (const t of tiles.values()) t.node.querySelector("video")?.pause(); },
  });
})();
