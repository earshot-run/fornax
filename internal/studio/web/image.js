"use strict";
// Image mode: composer, queue tiles, gallery and the viewer.

const RATIOS = [
  { id: "1:1", w: 1, h: 1, label: "Square" },
  { id: "4:5", w: 4, h: 5, label: "Portrait" },
  { id: "3:2", w: 3, h: 2, label: "Photo" },
  { id: "16:9", w: 16, h: 9, label: "Wide" },
  { id: "9:16", w: 9, h: 16, label: "Tall" },
];
const TIERS = [
  { id: "draft", label: "Draft", side: 512 },
  { id: "standard", label: "Standard", side: 768 },
  { id: "high", label: "High", side: 1024 },
];

const state = {
  models: [],
  images: [],
  jobs: [],
  gallery: -1,
  refs: store.get("refs", []),
  ratio: store.get("ratio", "1:1"),
  tier: store.get("tier", "standard"),
  seedLocked: store.get("seedLocked", false),
  viewing: -1,
  fresh: new Set(),
  seen: null,
};

function dims(ratioId = state.ratio, tierId = state.tier) {
  const ratio = RATIOS.find((r) => r.id === ratioId) || RATIOS[0];
  const tier = TIERS.find((t) => t.id === tierId) || TIERS[1];
  const area = tier.side * tier.side;
  const snap = (v) => Math.max(256, Math.round(v / 64) * 64);
  return { width: snap(Math.sqrt(area * ratio.w / ratio.h)), height: snap(Math.sqrt(area * ratio.h / ratio.w)) };
}

// What the last run of this model on this machine says the next one costs.
// Step cost grows a little faster than pixel count (attention), decode about linearly.
// Reference images slow every step, so runs with and without them are measured apart.
function estimate(modelId, width, height, steps, withRefs) {
  const measured = state.images.filter((i) => i.model === modelId && i.stepSeconds > 0);
  const past = measured.find((i) => (i.refs?.length > 0) === withRefs) || measured[0];
  if (!past) return null;
  const scale = (width * height) / (past.width * past.height);
  const step = past.stepSeconds * Math.pow(scale, 1.15);
  const decode = (past.decodeSeconds || 0) * scale;
  const overhead = Math.max(0, (past.seconds || 0) - past.stepSeconds * past.steps - (past.decodeSeconds || 0));
  return { total: overhead + step * steps + decode, step, decode };
}

// ---------- composer ----------

function renderSegments() {
  const ratios = $("ratios");
  ratios.replaceChildren(...RATIOS.map((r) => {
    const big = 14, w = r.w >= r.h ? big : Math.round(big * r.w / r.h), h = r.h >= r.w ? big : Math.round(big * r.h / r.w);
    const glyph = el("span", { className: "ratio-glyph" });
    glyph.style.width = w + "px"; glyph.style.height = h + "px";
    const b = el("button", { type: "button", title: r.id }, el("span", { className: "ratio-box" }, glyph), r.label);
    b.setAttribute("aria-pressed", String(r.id === state.ratio));
    b.onclick = () => { state.ratio = r.id; store.set("ratio", r.id); renderSegments(); updateEstimate(); };
    return b;
  }));
  const tiers = $("tiers");
  tiers.replaceChildren(...TIERS.map((t) => {
    const b = el("button", { type: "button" }, t.label);
    b.setAttribute("aria-pressed", String(t.id === state.tier));
    b.onclick = () => { state.tier = t.id; store.set("tier", t.id); renderSegments(); updateEstimate(); };
    return b;
  }));
  const { width, height } = dims();
  $("size-out").textContent = `${width} × ${height}`;
}

function renderModels() {
  state.models = Studio.modelsOf("image");
  const select = $("model");
  const wanted = store.get("model", null);
  select.replaceChildren(...state.models.map((m) => el("option", { value: m.id, textContent: m.name && m.name !== m.id ? `${m.name}` : m.id })));
  if (state.models.some((m) => m.id === wanted)) select.value = wanted;
  select.disabled = state.models.length === 0;
  applyModelSteps();
  updateEstimate();
}

// A model that only works at its own step count (SDXL Lightning: 4) brings
// it along; the slider follows the pick.
function applyModelSteps() {
  const m = state.models.find((x) => x.id === $("model").value);
  if (m?.steps) { $("steps").value = m.steps; $("steps-out").textContent = m.steps; }
}

function renderRefs() {
  const wrap = $("refs");
  const tiles = state.refs.map((name, i) => {
    const remove = el("button", { type: "button", title: "Remove" }, icon("x"));
    remove.onclick = () => { state.refs.splice(i, 1); store.set("refs", state.refs); renderRefs(); };
    return el("div", { className: "ref" }, el("img", { src: "/files/refs/" + name, alt: "" }), remove);
  });
  if (state.refs.length < 10) {
    const add = el("button", { type: "button", className: "ref-add", title: "Add a reference — or drop / paste an image" }, icon("plus"));
    add.onclick = () => $("ref-file").click();
    tiles.push(add);
  }
  wrap.replaceChildren(...tiles);
  updateEstimate();
}

async function addRefBlobs(blobs) {
  for (const blob of blobs) {
    if (state.refs.length >= 10) { toast("Ten references is the most a run takes"); break; }
    if (!/^image\/(png|jpeg|webp)$/.test(blob.type)) { toast("References must be PNG, JPEG or WebP"); continue; }
    const name = await uploadRef(blob);
    if (name) state.refs.push(name);
  }
  store.set("refs", state.refs);
  renderRefs();
}

function renderSeed() {
  const locked = state.seedLocked;
  $("seed-lock").setAttribute("aria-pressed", String(locked));
  $("seed-hint").textContent = locked ? "kept between runs" : "random each run";
  $("seed").placeholder = locked ? "Seed" : "Random";
}

function updateEstimate() {
  const { width, height } = dims();
  const steps = Number($("steps").value);
  $("steps-out").textContent = steps;
  const est = estimate($("model").value, width, height, steps, state.refs.length > 0);
  const hint = $("tier-hint");
  if (!state.models.length) { $("estimate").textContent = ""; hint.textContent = ""; return; }
  $("estimate").textContent = est ? `About ${fmtDuration(est.total)} per image` : "The first run measures this machine";
  hint.textContent = est ? `${fmtDuration(est.step)} a step` : "";
}

function readRequest() {
  const seedText = $("seed").value.trim();
  const { width, height } = dims();
  return {
    model: $("model").value,
    prompt: $("prompt").value.trim(),
    negative: $("negative").value.trim(),
    width, height,
    steps: Number($("steps").value),
    seed: state.seedLocked && /^\d+$/.test(seedText) ? Number(seedText) : -1,
    refs: [...state.refs],
  };
}

async function submit(request) {
  if (!request.model) { toast("Add an image model first"); return; }
  if (!request.prompt) { $("prompt").focus(); toast("Describe the image first"); return; }
  askNotify();
  const res = await fetch("/api/jobs", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ kind: "image", ...request }) });
  if (!res.ok) { toast((await res.text()).trim()); return; }
  const job = await res.json();
  if (state.seedLocked && !$("seed").value) $("seed").value = job.seed;
}

function saveDraft() {
  store.set("prompt", $("prompt").value);
  store.set("negative", $("negative").value);
  store.set("steps", Number($("steps").value));
  store.set("seed", $("seed").value);
  store.set("model", $("model").value);
}

// ---------- queue + gallery ----------

const PHASES = { preparing: "Getting ready", loading: "Loading model", decoding: "Decoding", saving: "Saving" };

// Job tiles are keyed and updated in place: the page re-renders every second
// while a job runs, and rebuilding would restart the bar and reload the preview.
const tiles = new Map();

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
    const img = el("img", { src: `/files/previews/${job.id}.png?v=${job.preview}`, alt: "" });
    img.onload = () => t.canvas.replaceChildren(img);
  }
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

function imageTile(image, index) {
  let t = tiles.get("img:" + image.id);
  if (!t) {
    const img = el("img", { src: `/files/library/${image.id}.png`, alt: image.prompt, loading: "lazy", decoding: "async" });
    img.width = image.width; img.height = image.height;
    const node = el("button", { type: "button", className: "tile" }, img, el("span", { className: "caption", textContent: image.prompt }));
    if (state.fresh.has(image.id)) {
      node.classList.add("fresh");
      node.addEventListener("animationend", () => node.classList.remove("fresh"), { once: true });
    }
    t = { node };
    tiles.set("img:" + image.id, t);
  }
  t.node.onclick = () => openViewer(index);
  return t.node;
}

function pick(i) {
  return { model: i.model, prompt: i.prompt, negative: i.negative || "", width: i.width, height: i.height, steps: i.steps, seed: i.seed, refs: i.refs || [] };
}

function progressOf(job) {
  const est = estimate(job.model, job.width, job.height, job.total, job.refs?.length > 0);
  const stepCost = job.stepSeconds || (est && est.step) || 0;
  const decode = (est && est.decode) || stepCost * 3;
  const sampleTotal = stepCost * job.total;
  if (!sampleTotal) return job.phase === "sampling" ? job.step / job.total * 0.85 : 0.02;
  const whole = sampleTotal + decode;
  if (job.phase === "sampling") return (job.step * stepCost) / whole;
  if (job.phase === "decoding" || job.phase === "saving") return Math.min(0.99, (sampleTotal + (job.phase === "saving" ? decode : decode / 3)) / whole);
  return 0.02;
}

function remaining(job) {
  const est = estimate(job.model, job.width, job.height, job.total, job.refs?.length > 0);
  const stepCost = job.stepSeconds || (est && est.step);
  if (!stepCost) return est ? est.total : null;
  const decode = (est && est.decode) || 0;
  if (job.phase === "sampling") return stepCost * (job.total - job.step) + decode;
  if (job.phase === "decoding") return decode || null;
  return est ? est.total : null;
}

const imageHead = libraryHead("Images", "image", "image", "images");
$("grid").before(imageHead.node);

function renderGrid() {
  const active = state.jobs.filter((j) => j.state !== "canceled");
  const nodes = [...active.map(jobTile), ...state.images.map(imageTile)];
  const live = new Set([...active.map((j) => "job:" + j.id), ...state.images.map((i) => "img:" + i.id)]);
  for (const key of tiles.keys()) if (!live.has(key)) tiles.delete(key);
  state.fresh.clear();
  const grid = $("grid");
  if (nodes.length !== grid.children.length || nodes.some((n, i) => grid.children[i] !== n)) grid.replaceChildren(...nodes);
  imageHead.update(state.images.length);
  renderEmpty(nodes.length === 0);

  const running = state.jobs.find((j) => j.state === "running");
  const queued = state.jobs.filter((j) => j.state === "queued").length;
  $("queue-note").textContent = queued ? `${queued} queued` : "";
  if (Studio.active === "image") document.title = running ? `${Math.round(progressOf(running) * 100)}% · fornax studio` : "fornax studio";
}

function renderEmpty(show) {
  const empty = $("empty");
  if (!show) { empty.replaceChildren(); return; }
  if (!state.models.length) {
    empty.replaceChildren(getModel("image", "No image models yet", "Download one to start. SDXL Lightning is quick on any machine."));
    return;
  }
  empty.replaceChildren(el("div", { className: "empty" },
    el("h2", { textContent: "Nothing here yet" }),
    el("p", { textContent: "Describe an image on the left. Everything you make stays in ~/.fornax/studio." })));
}

async function loadImages() {
  const res = await fetch("/api/library?kind=image");
  if (!res.ok) return;
  const images = await res.json();
  if (state.seen) for (const i of images) if (!state.seen.has(i.id)) state.fresh.add(i.id);
  const added = state.seen ? images.filter((i) => !state.seen.has(i.id)) : [];
  state.seen = new Set(images.map((i) => i.id));
  state.images = images;
  renderGrid();
  updateEstimate();
  if (added.length) notifyDone(added[0]);
  if (state.viewing >= 0) refreshViewer();
}

// Re-render once a second while something runs, so elapsed time moves.
setInterval(() => { if (state.jobs.some((j) => j.state === "running")) renderGrid(); }, 1000);

// ---------- viewer ----------

function openViewer(index) {
  state.viewing = index;
  refreshViewer();
  if (!$("viewer").open) $("viewer").showModal();
}

function refreshViewer() {
  const image = state.images[state.viewing];
  if (!image) { $("viewer").close(); return; }
  $("viewer-img").src = `/files/library/${image.id}.png`;
  $("viewer-img").alt = image.prompt;
  $("viewer-prompt").textContent = image.prompt;
  $("viewer-neg").textContent = image.negative || "";
  $("viewer-neg-wrap").hidden = !image.negative;
  $("viewer-refs-wrap").hidden = !(image.refs && image.refs.length);
  $("viewer-refs").replaceChildren(...(image.refs || []).map((r) => el("img", { src: "/files/refs/" + r, alt: "" })));
  $("viewer-when").textContent = new Date(image.created).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
  const facts = [["Model", image.model], ["Size", `${image.width} × ${image.height}`], ["Steps", image.steps], ["Seed", image.seed]];
  if (image.seconds) facts.push(["Took", fmtDuration(image.seconds)]);
  $("viewer-facts").replaceChildren(...facts.flatMap(([k, v]) => [el("dt", { textContent: k }), el("dd", { textContent: String(v) })]));
  $("act-download").href = `/files/library/${image.id}.png`;
  $("act-download").download = `${image.model}-${image.seed}.png`;
  disarmDelete();
  $("prev").hidden = state.viewing <= 0;
  $("next").hidden = state.viewing >= state.images.length - 1;
}

function step(delta) {
  const next = state.viewing + delta;
  if (next >= 0 && next < state.images.length) { state.viewing = next; refreshViewer(); }
}

function disarmDelete() {
  const btn = $("act-delete");
  btn.classList.remove("armed");
  btn.lastChild.textContent = "Delete";
}

function reuse(image) {
  $("prompt").value = image.prompt;
  $("negative").value = image.negative || "";
  if (state.models.some((m) => m.id === image.model)) $("model").value = image.model;
  $("steps").value = image.steps;
  const ratio = RATIOS.find((r) => { const d = dims(r.id, state.tier); return Math.abs(d.width / d.height - image.width / image.height) < 0.02; });
  if (ratio) state.ratio = ratio.id;
  const tier = TIERS.find((t) => { const d = dims(state.ratio, t.id); return d.width === image.width && d.height === image.height; });
  if (tier) state.tier = tier.id;
  state.seedLocked = true;
  $("seed").value = image.seed;
  state.refs = [...(image.refs || [])];
  store.set("ratio", state.ratio); store.set("tier", state.tier); store.set("seedLocked", true); store.set("refs", state.refs);
  renderSegments(); renderSeed(); renderRefs(); updateEstimate(); saveDraft(); autosize();
  if (!$("advanced").open && (image.negative || image.steps !== 20)) $("advanced").open = true;
}

$("close").onclick = () => $("viewer").close();
$("prev").onclick = () => step(-1);
$("next").onclick = () => step(1);
$("viewer").addEventListener("close", () => { state.viewing = -1; });
$("stage").addEventListener("click", (e) => { if (e.target === $("stage")) $("viewer").close(); });
$("copy-prompt").onclick = () => copy(state.images[state.viewing].prompt, "Prompt copied");
$("act-vary").onclick = () => {
  const image = state.images[state.viewing];
  submit({ ...pick(image), seed: -1 });
  $("viewer").close();
  toast("Queued a variation");
};
$("act-reuse").onclick = () => {
  reuse(state.images[state.viewing]);
  $("viewer").close();
  $("prompt").focus();
};
$("act-ref").onclick = async () => {
  const image = state.images[state.viewing];
  const blob = await (await fetch(`/files/library/${image.id}.png`)).blob();
  await addRefBlobs([blob]);
  $("viewer").close();
  toast("Added as a reference");
};
$("act-delete").onclick = async () => {
  const btn = $("act-delete");
  if (!btn.classList.contains("armed")) {
    btn.classList.add("armed");
    btn.lastChild.textContent = "Delete for good";
    return;
  }
  const image = state.images[state.viewing];
  await fetch(`/api/library/${image.id}`, { method: "DELETE" });
  state.images.splice(state.viewing, 1);
  if (state.viewing >= state.images.length) state.viewing = state.images.length - 1;
  if (state.viewing < 0) $("viewer").close(); else refreshViewer();
};

// ---------- misc ----------

function notifyDone(image) {
  if (!document.hidden || !("Notification" in window) || Notification.permission !== "granted") return;
  const n = new Notification("Image ready", { body: image.prompt.slice(0, 120), icon: `/files/library/${image.id}.png`, silent: false });
  n.onclick = () => { window.focus(); openViewer(state.images.findIndex((i) => i.id === image.id)); n.close(); };
}

function autosize() {
  const t = $("prompt");
  t.style.height = "auto";
  t.style.height = Math.min(t.scrollHeight + 2, 320) + "px";
}

// ---------- wiring ----------

$("prompt").value = store.get("prompt", "");
$("negative").value = store.get("negative", "");
$("steps").value = store.get("steps", 20);
$("seed").value = store.get("seed", "");
if (!/Mac|iPhone|iPad/.test(navigator.platform)) $("kbd").textContent = "Ctrl ↵";

$("prompt").addEventListener("input", () => { autosize(); saveDraft(); });
$("negative").addEventListener("input", saveDraft);
$("steps").addEventListener("input", () => { updateEstimate(); saveDraft(); });
$("model").addEventListener("change", () => { applyModelSteps(); updateEstimate(); saveDraft(); });
$("seed").addEventListener("input", () => {
  $("seed").value = $("seed").value.replace(/\D/g, "");
  if ($("seed").value && !state.seedLocked) { state.seedLocked = true; store.set("seedLocked", true); renderSeed(); }
  saveDraft();
});
$("seed-lock").onclick = () => { state.seedLocked = !state.seedLocked; store.set("seedLocked", state.seedLocked); if (!state.seedLocked) $("seed").value = ""; renderSeed(); saveDraft(); };
$("seed-roll").onclick = () => { $("seed").value = Math.floor(Math.random() * 2 ** 31); state.seedLocked = true; store.set("seedLocked", true); renderSeed(); saveDraft(); };
$("generate").onclick = () => submit(readRequest());
$("ref-file").addEventListener("change", (e) => { addRefBlobs([...e.target.files]); e.target.value = ""; });
$("advanced").open = store.get("advancedOpen", false);
$("advanced").addEventListener("toggle", () => store.set("advancedOpen", $("advanced").open));

document.addEventListener("keydown", (e) => {
  if (Studio.active !== "image") return;
  if ((e.metaKey || e.ctrlKey) && e.key === "Enter") { e.preventDefault(); submit(readRequest()); return; }
  if (!$("viewer").open) return;
  if (e.key === "ArrowLeft") step(-1);
  if (e.key === "ArrowRight") step(1);
});
document.addEventListener("paste", (e) => {
  if (Studio.active !== "image") return;
  const files = [...(e.clipboardData?.files || [])].filter((f) => f.type.startsWith("image/"));
  if (files.length) { e.preventDefault(); addRefBlobs(files); }
});
const composer = $("composer");
composer.addEventListener("dragover", (e) => { if ([...e.dataTransfer.types].includes("Files")) { e.preventDefault(); composer.classList.add("dragging"); } });
composer.addEventListener("dragleave", (e) => { if (!composer.contains(e.relatedTarget)) composer.classList.remove("dragging"); });
composer.addEventListener("drop", (e) => {
  e.preventDefault();
  composer.classList.remove("dragging");
  addRefBlobs([...e.dataTransfer.files]);
});

// A reference uploaded in another session may be gone; drop the ones that don't load.
Promise.all(state.refs.map((name) => fetch("/files/refs/" + name, { method: "HEAD" }).then((r) => r.ok ? name : null).catch(() => null)))
  .then((names) => { state.refs = names.filter(Boolean); store.set("refs", state.refs); renderRefs(); });

Studio.register("image", {
  mount() {
    renderSegments();
    renderSeed();
    renderRefs();
    autosize();
  },
  models: renderModels,
  snapshot(snap, libraryChanged) {
    state.jobs = snap.jobs.filter((j) => j.kind === "image");
    if (libraryChanged) loadImages();
    else renderGrid();
  },
  show() { autosize(); renderGrid(); },
});
