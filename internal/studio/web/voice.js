"use strict";
// Voice mode: text to speech, optionally in a voice cloned from a clip the
// viewer records or uploads. Its own scope: image.js owns the page-level names.

(() => {
  const LANGS = [
    ["en", "English"], ["zh", "Chinese"], ["de", "German"], ["it", "Italian"], ["pt", "Portuguese"],
    ["es", "Spanish"], ["ja", "Japanese"], ["ko", "Korean"], ["fr", "French"], ["ru", "Russian"],
  ];
  const MAX_CHARS = 2000;
  // Qwen3-TTS clones from a few seconds; longer clips only slow the encoder.
  const CLIP_RATE = 24000, CLIP_MAX = 20;
  const PHASES = { preparing: "Getting ready", loading: "Loading model", speaking: "Speaking", saving: "Saving" };

  const key = (k) => "voice." + k;
  const state = {
    models: [],
    clips: [],
    jobs: [],
    voices: store.get(key("voices"), []),
    voice: store.get(key("voice"), ""),
    fresh: new Set(),
    seen: null,
  };
  const ui = {};
  const rows = new Map();
  const audioCache = new Map();

  const langName = (code) => (LANGS.find(([c]) => c === code) || [code, code])[1];
  const fmtTime = (s) => (!isFinite(s) ? "0:00" : `${Math.floor(s / 60)}:${String(Math.floor(s % 60)).padStart(2, "0")}`);
  const cssVar = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();

  // ---------- audio: decode, peaks, WAV ----------

  // Decoded once per URL: duration plus a peak envelope for the waveform.
  function analyze(url) {
    if (!audioCache.has(url)) {
      audioCache.set(url, (async () => {
        const raw = await (await fetch(url)).arrayBuffer();
        const ctx = new OfflineAudioContext(1, 1, 44100);
        const buffer = await ctx.decodeAudioData(raw);
        const data = buffer.getChannelData(0), buckets = 480, size = Math.max(1, Math.floor(data.length / buckets));
        const peaks = new Float32Array(buckets);
        let top = 0;
        for (let b = 0; b < buckets; b++) {
          let sum = 0;
          for (let i = b * size; i < Math.min(data.length, (b + 1) * size); i++) sum += data[i] * data[i];
          peaks[b] = Math.sqrt(sum / size);
          top = Math.max(top, peaks[b]);
        }
        if (top > 0) for (let b = 0; b < buckets; b++) peaks[b] /= top;
        return { duration: buffer.duration, peaks };
      })().catch(() => ({ duration: 0, peaks: new Float32Array(0) })));
    }
    return audioCache.get(url);
  }

  // Mono, resampled, silence trimmed off both ends, peak-normalized.
  async function toClip(source, rate = CLIP_RATE) {
    const buffer = source instanceof AudioBuffer ? source : await new OfflineAudioContext(1, 1, rate).decodeAudioData(await source.arrayBuffer());
    const length = Math.max(1, Math.ceil(buffer.duration * rate));
    const offline = new OfflineAudioContext(1, length, rate);
    const node = offline.createBufferSource();
    node.buffer = buffer;
    node.connect(offline.destination);
    node.start();
    let samples = (await offline.startRendering()).getChannelData(0);
    let top = 0;
    for (const s of samples) top = Math.max(top, Math.abs(s));
    const floor = top * 0.04, pad = Math.floor(rate * 0.15);
    let from = 0, to = samples.length;
    while (from < to && Math.abs(samples[from]) < floor) from++;
    while (to > from && Math.abs(samples[to - 1]) < floor) to--;
    samples = samples.slice(Math.max(0, from - pad), Math.min(samples.length, to + pad, Math.max(0, from - pad) + rate * CLIP_MAX));
    const gain = top > 0 ? 0.89 / top : 1;
    return { samples: samples.map((s) => s * gain), rate, seconds: samples.length / rate };
  }

  function encodeWav({ samples, rate }) {
    const view = new DataView(new ArrayBuffer(44 + samples.length * 2));
    const text = (at, s) => { for (let i = 0; i < s.length; i++) view.setUint8(at + i, s.charCodeAt(i)); };
    text(0, "RIFF"); view.setUint32(4, 36 + samples.length * 2, true); text(8, "WAVE");
    text(12, "fmt "); view.setUint32(16, 16, true); view.setUint16(20, 1, true); view.setUint16(22, 1, true);
    view.setUint32(24, rate, true); view.setUint32(28, rate * 2, true); view.setUint16(32, 2, true); view.setUint16(34, 16, true);
    text(36, "data"); view.setUint32(40, samples.length * 2, true);
    for (let i = 0; i < samples.length; i++) view.setInt16(44 + i * 2, Math.max(-1, Math.min(1, samples[i])) * 0x7fff, true);
    return new Blob([view], { type: "audio/wav" });
  }

  function drawWave(canvas, peaks, played = 0, hover = -1) {
    const dpr = window.devicePixelRatio || 1, w = canvas.clientWidth, h = canvas.clientHeight;
    if (!w || !h) return;
    if (canvas.width !== Math.round(w * dpr) || canvas.height !== Math.round(h * dpr)) { canvas.width = Math.round(w * dpr); canvas.height = Math.round(h * dpr); }
    const g = canvas.getContext("2d");
    g.setTransform(dpr, 0, 0, dpr, 0, 0);
    g.clearRect(0, 0, w, h);
    const bar = 2, gap = 2, count = Math.max(1, Math.floor((w + gap) / (bar + gap)));
    const on = cssVar("--accent"), off = cssVar("--wave-off") || cssVar("--border"), ghost = cssVar("--wave-hover") || off;
    for (let i = 0; i < count; i++) {
      const at = i / count;
      let v = 0.08;
      if (peaks.length) {
        const a = Math.floor(at * peaks.length), b = Math.max(a + 1, Math.floor((i + 1) / count * peaks.length));
        let m = 0;
        for (let j = a; j < b; j++) m = Math.max(m, peaks[j]);
        v = Math.max(0.08, m);
      }
      const bh = Math.max(2, Math.round(v * (h - 2)));
      g.fillStyle = at < played ? on : (hover >= 0 && at < hover ? ghost : off);
      g.beginPath();
      g.roundRect(i * (bar + gap), Math.round((h - bh) / 2), bar, bh, 1);
      g.fill();
    }
  }

  // ---------- player: one <audio> shared by every clip ----------

  const player = new Audio();
  player.preload = "auto";
  let playing = null;
  let frame = 0;

  function play(row) {
    if (playing === row && !player.paused) { player.pause(); return; }
    if (playing !== row) {
      if (playing) setPlaying(playing, false);
      playing = row;
      player.src = row.url;
    }
    player.play().catch(() => {});
  }

  function setPlaying(row, on) {
    row.node.classList.toggle("playing", on);
    row.playBtn.replaceChildren(icon(on ? "pause" : "play"));
    row.playBtn.title = on ? "Pause" : "Play";
    if (!on) paint(row);
  }

  function paint(row) {
    const at = playing === row && player.duration ? player.currentTime / player.duration : 0;
    drawWave(row.canvas, row.peaks || new Float32Array(0), at, row.hover ?? -1);
    row.time.textContent = playing === row && player.currentTime > 0 ? `${fmtTime(player.currentTime)} / ${fmtTime(row.duration)}` : fmtTime(row.duration);
  }

  function tick() {
    if (playing) paint(playing);
    if (!player.paused) frame = requestAnimationFrame(tick);
  }

  player.addEventListener("play", () => { if (playing) setPlaying(playing, true); cancelAnimationFrame(frame); frame = requestAnimationFrame(tick); });
  player.addEventListener("pause", () => { if (playing) setPlaying(playing, false); });
  player.addEventListener("ended", () => { if (!playing) return; const row = playing; player.currentTime = 0; setPlaying(row, false); });

  // ---------- composer ----------

  function buildComposer() {
    ui.model = el("select", { id: "voice-model" });
    ui.text = el("textarea", { id: "voice-text", placeholder: "Welcome to fornax. Everything you hear was made on this machine, and nothing left it.", maxLength: MAX_CHARS });
    ui.count = el("span", { className: "hint" });
    ui.lang = el("select", { id: "voice-lang" }, ...LANGS.map(([code, name]) => el("option", { value: code, textContent: name })));
    ui.voices = el("div", { className: "voices", role: "radiogroup", ariaLabel: "Voice" });
    ui.recorder = el("div", { className: "recorder" });
    ui.file = el("input", { type: "file", accept: "audio/*", hidden: true });
    ui.generate = el("button", { className: "generate", type: "button" }, el("span", { textContent: "Speak" }), el("kbd", { textContent: /Mac|iPhone|iPad/.test(navigator.platform) ? "⌘ ↵" : "Ctrl ↵" }));
    ui.estimate = el("span");
    ui.queueNote = el("span");
    const label = (text, forId, extra) => el("div", { className: "label" }, el("label", { htmlFor: forId, textContent: text }), extra || null);
    ui.composer = el("aside", { className: "composer" },
      el("div", { className: "composer-scroll" },
        el("div", { className: "field" }, label("Model", "voice-model"), ui.model),
        el("div", { className: "field" }, label("Text", "voice-text", ui.count), ui.text),
        el("div", { className: "field" }, label("Language", "voice-lang"), ui.lang),
        el("div", { className: "field" }, el("div", { className: "label" }, el("span", { textContent: "Voice" }), el("span", { className: "hint", textContent: "record or upload one to clone it" })), ui.voices, ui.recorder, ui.file)),
      el("div", { className: "composer-foot" }, ui.generate, el("div", { className: "foot-note" }, ui.estimate, ui.queueNote)));
    return ui.composer;
  }

  function renderModels() {
    if (!ui.model) return;
    state.models = Studio.modelsOf("speech");
    const wanted = store.get(key("model"), null);
    ui.model.replaceChildren(...state.models.map((m) => el("option", { value: m.id, textContent: m.name || m.id })));
    if (state.models.some((m) => m.id === wanted)) ui.model.value = wanted;
    ui.model.disabled = state.models.length === 0;
    ui.generate.disabled = state.models.length === 0;
    updateFoot();
    renderList();
  }

  let preview = null;
  function previewVoice(v, btn) {
    if (preview && !preview.paused && preview.dataset.name === v.name) { preview.pause(); return; }
    preview?.pause();
    player.pause();
    preview = new Audio("/files/refs/" + v.name);
    preview.dataset.name = v.name;
    const set = (on) => btn.replaceChildren(icon(on ? "pause" : "play"));
    preview.onplay = () => set(true);
    preview.onpause = preview.onended = () => set(false);
    preview.play().catch(() => {});
  }

  function renderVoices() {
    const chip = (v) => {
      const selected = state.voice === (v ? v.name : "");
      const node = el("div", { className: "voice-chip" + (selected ? " selected" : ""), role: "radio", tabIndex: 0 });
      node.setAttribute("aria-checked", String(selected));
      const choose = () => { state.voice = v ? v.name : ""; store.set(key("voice"), state.voice); renderVoices(); };
      node.onclick = choose;
      node.onkeydown = (e) => { if (e.key === " " || e.key === "Enter") { e.preventDefault(); choose(); } };
      if (!v) {
        node.append(el("span", { className: "voice-glyph" }, icon("voice")), el("span", { className: "voice-text" }, el("strong", { textContent: "Default" }), el("span", { textContent: "the model's own voice" })));
        return node;
      }
      const listen = el("button", { type: "button", className: "voice-glyph", title: "Listen" }, icon("play"));
      listen.onclick = (e) => { e.stopPropagation(); previewVoice(v, listen); };
      const remove = el("button", { type: "button", className: "voice-remove", title: "Forget this voice" }, icon("x"));
      remove.onclick = (e) => {
        e.stopPropagation();
        state.voices = state.voices.filter((x) => x.name !== v.name);
        if (state.voice === v.name) state.voice = "";
        store.set(key("voices"), state.voices); store.set(key("voice"), state.voice);
        renderVoices();
      };
      node.append(listen, el("span", { className: "voice-text" }, el("strong", { textContent: v.label }), el("span", { textContent: `${v.seconds.toFixed(1)}s clip` })), remove);
      return node;
    };
    const record = el("button", { type: "button", className: "voice-add" }, icon("mic"), "Record");
    record.onclick = startRecording;
    const upload = el("button", { type: "button", className: "voice-add" }, icon("upload"), "Upload");
    upload.onclick = () => ui.file.click();
    ui.voices.replaceChildren(chip(null), ...state.voices.map(chip), el("div", { className: "voice-adds" }, record, upload));
    ui.voices.hidden = !!recording;
    updateFoot();
  }

  async function addVoice(blob, label) {
    try {
      const clip = await toClip(blob);
      if (clip.seconds < 1) { toast("That clip is almost silent — try again closer to the mic"); return; }
      const name = await uploadRef(encodeWav(clip));
      if (!name) return;
      state.voices = [{ name, label, seconds: clip.seconds }, ...state.voices].slice(0, 8);
      state.voice = name;
      store.set(key("voices"), state.voices); store.set(key("voice"), name);
      renderVoices();
      toast(`Cloning from “${label}”`);
    } catch {
      toast("Couldn't read that audio file");
    }
  }

  // ---------- recording: raw PCM from getUserMedia, WAV encoded here ----------

  let recording = null;

  async function startRecording() {
    if (recording) return;
    let stream;
    try {
      stream = await navigator.mediaDevices.getUserMedia({ audio: { channelCount: 1, echoCancellation: false, noiseSuppression: true, autoGainControl: true } });
    } catch {
      toast("Microphone access was blocked");
      return;
    }
    const ctx = new AudioContext();
    const source = ctx.createMediaStreamSource(stream);
    const analyser = ctx.createAnalyser();
    analyser.fftSize = 1024;
    // ScriptProcessor is deprecated but needs no worklet module, and a clip is seconds long.
    const tap = ctx.createScriptProcessor(4096, 1, 1);
    const chunks = [];
    tap.onaudioprocess = (e) => chunks.push(new Float32Array(e.inputBuffer.getChannelData(0)));
    source.connect(analyser);
    source.connect(tap);
    tap.connect(ctx.destination);
    recording = { stream, ctx, chunks, analyser, started: performance.now(), levels: new Array(40).fill(0) };
    renderRecorder();
    renderVoices();
    meter();
  }

  function meter() {
    if (!recording) return;
    const r = recording, data = new Float32Array(r.analyser.fftSize);
    r.analyser.getFloatTimeDomainData(data);
    let sum = 0;
    for (const s of data) sum += s * s;
    r.levels.push(Math.min(1, Math.sqrt(sum / data.length) * 5));
    r.levels.shift();
    const elapsed = (performance.now() - r.started) / 1000;
    r.timeEl.textContent = `${fmtTime(elapsed)} / ${fmtTime(CLIP_MAX)}`;
    r.bars.forEach((bar, i) => { bar.style.transform = `scaleY(${Math.max(0.08, r.levels[i])})`; });
    if (elapsed >= CLIP_MAX) { stopRecording(true); return; }
    r.raf = requestAnimationFrame(meter);
  }

  async function stopRecording(keep) {
    const r = recording;
    if (!r) return;
    recording = null;
    cancelAnimationFrame(r.raf);
    r.stream.getTracks().forEach((t) => t.stop());
    const rate = r.ctx.sampleRate;
    await r.ctx.close();
    renderRecorder();
    renderVoices();
    if (!keep) return;
    const total = r.chunks.reduce((n, c) => n + c.length, 0);
    if (total < rate) { toast("Too short — record at least a couple of seconds"); return; }
    const buffer = new AudioBuffer({ length: total, sampleRate: rate, numberOfChannels: 1 });
    let at = 0;
    for (const c of r.chunks) { buffer.copyToChannel(c, 0, at); at += c.length; }
    const n = state.voices.filter((v) => v.label.startsWith("Recording")).length + 1;
    await addVoice(buffer, n === 1 ? "Recording" : `Recording ${n}`);
  }

  function renderRecorder() {
    if (!recording) { ui.recorder.replaceChildren(); ui.recorder.hidden = true; return; }
    const r = recording;
    r.bars = r.levels.map(() => el("i"));
    r.timeEl = el("span", { className: "rec-time" });
    const stop = el("button", { type: "button", className: "rec-stop" }, icon("stop"), "Use clip");
    stop.onclick = () => stopRecording(true);
    const cancel = el("button", { type: "button", className: "icon-btn small-icon", title: "Discard" }, icon("x"));
    cancel.onclick = () => stopRecording(false);
    ui.recorder.hidden = false;
    ui.recorder.replaceChildren(
      el("div", { className: "rec-head" }, el("span", { className: "rec-dot" }), r.timeEl, cancel),
      el("div", { className: "rec-meter" }, ...r.bars),
      el("p", { className: "rec-tip", textContent: "Read a sentence or two in your normal voice. Five to fifteen seconds in a quiet room clones best." }),
      stop);
  }

  // ---------- estimate + submit ----------

  // Runs here are short and mostly fixed cost (load, then decode per character).
  function estimate(modelId, chars, cloned) {
    const past = state.clips.filter((c) => c.model === modelId && c.seconds > 0);
    const same = past.filter((c) => !!c.voice === cloned);
    const pool = (same.length ? same : past).slice(0, 6);
    if (!pool.length) return null;
    const perChar = pool.reduce((n, c) => n + c.seconds / (c.prompt.length + 60), 0) / pool.length;
    return perChar * (chars + 60);
  }

  function updateFoot() {
    if (!ui.text) return;
    const chars = ui.text.value.trim().length;
    ui.count.textContent = chars > MAX_CHARS * 0.8 ? `${chars} / ${MAX_CHARS}` : chars ? `${chars} characters` : "";
    if (!state.models.length) { ui.estimate.textContent = ""; return; }
    const est = estimate(ui.model.value, Math.max(chars, 40), !!state.voice);
    ui.estimate.textContent = est ? `About ${fmtDuration(est)} per clip` : "The first run measures this machine";
  }

  async function submit(request) {
    if (!request.model) { toast("Add a speech model first"); return; }
    if (!request.prompt) { ui.text.focus(); toast("Type something to say first"); return; }
    askNotify();
    const res = await fetch("/api/jobs", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ kind: "speech", ...request }) });
    if (res.ok) return;
    const message = (await res.text()).trim();
    toast(message);
    if (request.voice && message.includes(request.voice)) forgetVoice(request.voice);
  }

  function forgetVoice(name) {
    state.voices = state.voices.filter((v) => v.name !== name);
    if (state.voice === name) state.voice = "";
    store.set(key("voices"), state.voices); store.set(key("voice"), state.voice);
    renderVoices();
  }

  function readRequest() {
    return { model: ui.model.value, prompt: ui.text.value.trim(), lang: ui.lang.value, voice: state.voice };
  }

  function saveDraft() {
    store.set(key("text"), ui.text.value);
    store.set(key("lang"), ui.lang.value);
    store.set(key("model"), ui.model.value);
  }

  function autosize() {
    ui.text.style.height = "auto";
    ui.text.style.height = Math.min(ui.text.scrollHeight + 2, 360) + "px";
  }

  // ---------- queue + clips ----------

  function voiceLabel(name) {
    if (!name) return "Default voice";
    const v = state.voices.find((x) => x.name === name);
    return v ? `${v.label} voice` : "Cloned voice";
  }

  function jobRow(job) {
    let r = rows.get("job:" + job.id);
    if (!r || r.state !== job.state) {
      const phase = el("span", { className: "job-phase" });
      const cancel = el("button", { type: "button", className: "icon-btn small-icon" }, icon("x"));
      cancel.onclick = () => fetch(`/api/jobs/${job.id}`, { method: "DELETE" });
      r = { state: job.state, phase, cancel };
      const body = [el("div", { className: "job-top" }, phase, cancel)];
      if (job.state === "failed") {
        const retry = el("button", { type: "button", className: "text-btn", textContent: "Try again" });
        retry.onclick = () => { fetch(`/api/jobs/${job.id}`, { method: "DELETE" }); submit({ model: job.model, prompt: job.prompt, lang: job.lang, voice: job.voice || "" }); };
        const log = el("button", { type: "button", className: "text-btn", textContent: "Copy error" });
        log.onclick = () => copy(job.error, "Error copied");
        body.push(el("pre", { className: "job-error mono", textContent: job.error }), el("div", { className: "job-actions" }, retry, log));
      } else {
        r.bar = el("i");
        r.elapsed = el("span");
        r.left = el("span");
        if (job.state === "running") body.push(el("div", { className: "bar" }, r.bar), el("div", { className: "job-meta" }, r.elapsed, r.left));
        body.push(el("p", { className: "clip-text", textContent: job.prompt }));
      }
      r.node = el("div", { className: `clip-card job ${job.state}` }, ...body);
      rows.set("job:" + job.id, r);
    }
    r.cancel.title = job.state === "failed" ? "Dismiss" : "Cancel";
    if (job.state === "failed") {
      r.phase.textContent = "Didn't finish";
    } else if (job.state === "queued") {
      const ahead = state.jobs.filter((j) => j.state === "queued" || j.state === "running").indexOf(job);
      r.phase.textContent = ahead > 0 ? `Queued · ${ahead} ahead` : "Queued";
    } else {
      const elapsed = (Date.now() - Date.parse(job.started)) / 1000;
      const est = estimate(job.model, job.prompt.length, !!job.voice);
      const byPhase = { preparing: 0.08, loading: 0.2, speaking: 0.45, saving: 0.97 }[job.phase] || 0.04;
      const fraction = est ? Math.max(byPhase, Math.min(0.97, elapsed / est)) : byPhase;
      r.phase.textContent = PHASES[job.phase] || "Starting";
      r.bar.style.width = (fraction * 100).toFixed(1) + "%";
      r.elapsed.textContent = fmtDuration(elapsed) || "0s";
      r.left.textContent = est && est > elapsed ? `~${fmtDuration(est - elapsed)} left` : "";
    }
    return r.node;
  }

  function clipRow(clip) {
    let r = rows.get("clip:" + clip.id);
    if (r) return r.node;
    const url = `/files/library/${clip.id}.wav`;
    r = { url, duration: 0, peaks: null };
    r.playBtn = el("button", { type: "button", className: "clip-play-btn", title: "Play" }, icon("play"));
    r.playBtn.onclick = () => play(r);
    r.canvas = el("canvas", { className: "wave" });
    r.time = el("span", { className: "clip-time", textContent: "0:00" });
    const seek = (e) => {
      const rect = r.canvas.getBoundingClientRect();
      const at = Math.min(1, Math.max(0, (e.clientX - rect.left) / rect.width));
      if (playing !== r) { play(r); player.addEventListener("loadedmetadata", () => { player.currentTime = at * player.duration; }, { once: true }); }
      else { player.currentTime = at * (player.duration || 0); paint(r); }
    };
    r.canvas.addEventListener("click", seek);
    r.canvas.addEventListener("mousemove", (e) => { const rect = r.canvas.getBoundingClientRect(); r.hover = (e.clientX - rect.left) / rect.width; paint(r); });
    r.canvas.addEventListener("mouseleave", () => { r.hover = -1; paint(r); });

    const reuseBtn = el("button", { type: "button", className: "icon-btn small-icon", title: "Reuse text and voice" }, icon("edit"));
    reuseBtn.onclick = () => reuse(clip);
    const download = el("a", { className: "icon-btn small-icon", title: "Download", href: url, download: `${clip.model}-${clip.id}.wav` }, icon("down"));
    const del = el("button", { type: "button", className: "icon-btn small-icon del", title: "Delete" }, icon("trash"));
    del.onclick = async () => {
      if (!del.classList.contains("armed")) {
        del.classList.add("armed");
        del.title = "Click again to delete";
        setTimeout(() => { del.classList.remove("armed"); del.title = "Delete"; }, 3000);
        return;
      }
      if (playing === r) { player.pause(); playing = null; }
      await fetch(`/api/library/${clip.id}`, { method: "DELETE" });
    };
    const when = new Date(clip.created);
    const meta = el("div", { className: "clip-meta" },
      el("span", { textContent: langName(clip.lang || "en") }),
      el("span", { textContent: voiceLabel(clip.voice) }),
      el("span", { title: when.toLocaleString(), textContent: when.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" }) }),
      clip.seconds ? el("span", { textContent: `made in ${fmtDuration(clip.seconds)}` }) : null);
    r.node = el("article", { className: "clip-card" },
      el("div", { className: "clip-player" }, r.playBtn, r.canvas, r.time),
      el("p", { className: "clip-text", textContent: clip.prompt }),
      el("div", { className: "clip-foot" }, meta, el("div", { className: "clip-actions" }, reuseBtn, download, del)));
    if (state.fresh.has(clip.id)) {
      r.node.classList.add("fresh");
      r.node.addEventListener("animationend", () => r.node.classList.remove("fresh"), { once: true });
    }
    rows.set("clip:" + clip.id, r);
    analyze(url).then(({ duration, peaks }) => { r.duration = duration; r.peaks = peaks; paint(r); });
    requestAnimationFrame(() => paint(r));
    return r.node;
  }

  function renderList() {
    if (!ui.list) return;
    const active = state.jobs.filter((j) => j.state !== "canceled");
    const nodes = [...active.map(jobRow), ...state.clips.map(clipRow)];
    const live = new Set([...active.map((j) => "job:" + j.id), ...state.clips.map((c) => "clip:" + c.id)]);
    for (const k of rows.keys()) if (!live.has(k)) rows.delete(k);
    state.fresh.clear();
    if (nodes.length !== ui.list.children.length || nodes.some((n, i) => ui.list.children[i] !== n)) ui.list.replaceChildren(...nodes);
    ui.head.update(state.clips.length);
    renderEmpty(nodes.length === 0);
    const queued = state.jobs.filter((j) => j.state === "queued").length;
    ui.queueNote.textContent = queued ? `${queued} queued` : "";
    const running = state.jobs.find((j) => j.state === "running");
    if (Studio.active === "voice") document.title = running ? `${PHASES[running.phase] || "Starting"} · fornax studio` : "fornax studio";
  }

  function renderEmpty(show) {
    if (!show) { ui.empty.replaceChildren(); return; }
    if (!state.models.length) {
      ui.empty.replaceChildren(getModel("speech", "No speech models yet", "Download one to start. Qwen3 TTS speaks ten languages and can clone a voice."));
      return;
    }
    ui.empty.replaceChildren(el("div", { className: "empty" },
      el("h2", { textContent: "Nothing said yet" }),
      el("p", { textContent: "Type something on the left. Record a few seconds of your voice first and it speaks as you." })));
  }

  async function loadClips() {
    const res = await fetch("/api/library?kind=speech");
    if (!res.ok) return;
    const clips = await res.json();
    if (state.seen) for (const c of clips) if (!state.seen.has(c.id)) state.fresh.add(c.id);
    const added = state.seen ? clips.filter((c) => !state.seen.has(c.id)) : [];
    state.seen = new Set(clips.map((c) => c.id));
    state.clips = clips;
    renderList();
    updateFoot();
    if (added.length) notifyDone(added[0]);
  }

  function notifyDone(clip) {
    if (!document.hidden || !("Notification" in window) || Notification.permission !== "granted") return;
    const n = new Notification("Speech ready", { body: clip.prompt.slice(0, 120) });
    n.onclick = () => { window.focus(); location.hash = "voice"; n.close(); };
  }

  function reuse(clip) {
    ui.text.value = clip.prompt;
    ui.lang.value = clip.lang || "en";
    if (state.models.some((m) => m.id === clip.model)) ui.model.value = clip.model;
    if (clip.voice && !state.voices.some((v) => v.name === clip.voice)) {
      state.voices = [{ name: clip.voice, label: "From a clip", seconds: 0 }, ...state.voices].slice(0, 8);
      store.set(key("voices"), state.voices);
      analyze("/files/refs/" + clip.voice).then(({ duration }) => {
        const v = state.voices.find((x) => x.name === clip.voice);
        if (v) { v.seconds = duration; store.set(key("voices"), state.voices); renderVoices(); }
      });
    }
    state.voice = clip.voice || "";
    store.set(key("voice"), state.voice);
    saveDraft(); autosize(); renderVoices();
    ui.text.focus();
  }

  // ---------- wiring ----------

  function wire() {
    ui.text.value = store.get(key("text"), "");
    ui.lang.value = store.get(key("lang"), "en");
    ui.text.addEventListener("input", () => { autosize(); saveDraft(); updateFoot(); });
    ui.lang.addEventListener("change", saveDraft);
    ui.model.addEventListener("change", () => { saveDraft(); updateFoot(); });
    ui.generate.onclick = () => submit(readRequest());
    ui.file.addEventListener("change", (e) => {
      const file = e.target.files[0];
      e.target.value = "";
      if (file) addVoice(file, file.name.replace(/\.[^.]+$/, "").slice(0, 24) || "Upload");
    });
    document.addEventListener("keydown", (e) => {
      if (Studio.active !== "voice") return;
      if ((e.metaKey || e.ctrlKey) && e.key === "Enter") { e.preventDefault(); submit(readRequest()); }
    });
    const c = ui.composer;
    c.addEventListener("dragover", (e) => { if ([...e.dataTransfer.types].includes("Files")) { e.preventDefault(); c.classList.add("dragging"); } });
    c.addEventListener("dragleave", (e) => { if (!c.contains(e.relatedTarget)) c.classList.remove("dragging"); });
    c.addEventListener("drop", (e) => {
      e.preventDefault();
      c.classList.remove("dragging");
      const file = [...e.dataTransfer.files].find((f) => f.type.startsWith("audio/") || /\.(wav|mp3|m4a|ogg|flac|aac|webm)$/i.test(f.name));
      if (file) addVoice(file, file.name.replace(/\.[^.]+$/, "").slice(0, 24));
      else toast("Drop an audio clip to clone its voice");
    });
    new ResizeObserver(() => { for (const r of rows.values()) if (r.canvas) paint(r); }).observe(ui.list);
    matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => { for (const r of rows.values()) if (r.canvas) paint(r); });
    setInterval(() => { if (Studio.active === "voice" && state.jobs.some((j) => j.state === "running")) renderList(); }, 500);

    // A clip uploaded in another session may be gone; forget the ones that don't load.
    Promise.all(state.voices.map((v) => fetch("/files/refs/" + v.name, { method: "HEAD" }).then((r) => (r.ok ? v : null)).catch(() => v)))
      .then((kept) => {
        state.voices = kept.filter(Boolean);
        if (state.voice && !state.voices.some((v) => v.name === state.voice)) state.voice = "";
        store.set(key("voices"), state.voices); store.set(key("voice"), state.voice);
        renderVoices();
      });
  }

  Studio.register("voice", {
    mount(section) {
      ui.list = el("div", { className: "clip-list" });
      ui.head = libraryHead("Clips", "speech", "clip", "clips");
      ui.empty = el("div");
      section.append(el("div", { className: "app voice-app" },
        buildComposer(),
        el("main", { className: "main" }, el("div", { className: "main-inner voice-inner" }, ui.head.node, ui.list, ui.empty))));
      wire();
      renderVoices();
      renderRecorder();
      autosize();
    },
    models: renderModels,
    snapshot(snap, libraryChanged) {
      state.jobs = snap.jobs.filter((j) => j.kind === "speech");
      if (libraryChanged) loadClips();
      else renderList();
    },
    show() { autosize(); renderList(); },
    hide() { player.pause(); preview?.pause(); if (recording) stopRecording(false); },
  });
})();
