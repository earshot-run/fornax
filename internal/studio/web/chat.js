"use strict";
// Chat mode: conversations with text, vision and audio models over the
// studio's warm model server. Conversations live in studio/chats; the
// sampling parameters and the system prompt are per viewer.

(() => {
const PARAM_DEFAULTS = { system: "", temperature: 0.7, top_p: 0.9, max_tokens: 2048 };
const SUGGESTIONS = [
  "Explain how a transformer model works, in plain words",
  "Write a haiku about a computer that thinks offline",
  "Give me a Python function that removes duplicates but keeps order",
];

const chat = {
  mounted: false,
  models: [],
  model: store.get("chatModel", null),
  list: [],
  query: "",
  searchList: null,
  listError: "",
  searching: false,
  conv: null,
  status: { state: "" },
  params: { ...PARAM_DEFAULTS, ...store.get("chatParams", {}) },
  think: store.get("chatThink", false),
  pending: [],
  stream: null,
  recording: null,
  editing: -1,
  ui: {},
};

const displayName = (id) => {
  const m = chat.models.find((x) => x.id === id);
  const name = (m && m.name) || id || "";
  return name.replace(/^(hf|ollama)-/, "");
};

function newId() {
  const bytes = crypto.getRandomValues(new Uint8Array(8));
  return [...bytes].map((b) => b.toString(16).padStart(2, "0")).join("");
}

function relTime(iso) {
  const t = Date.parse(iso);
  if (!t) return "";
  const s = (Date.now() - t) / 1000;
  if (s < 60) return "now";
  if (s < 3600) return Math.floor(s / 60) + "m";
  if (s < 86400) return Math.floor(s / 3600) + "h";
  if (s < 7 * 86400) return Math.floor(s / 86400) + "d";
  return new Date(t).toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

function dayGroup(iso) {
  const d = new Date(iso), now = new Date();
  const start = (x) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  const days = Math.round((start(now) - start(d)) / 86400000);
  if (days <= 0) return "Today";
  if (days === 1) return "Yesterday";
  if (days < 7) return "This week";
  return "Earlier";
}

// ---------- markdown, built as DOM nodes — model text never becomes HTML ----------

const INLINE = /(`+)([^`]|[^`][\s\S]*?[^`])\1(?!`)|\*\*(?=\S)([\s\S]*?\S)\*\*|__(?=\S)([\s\S]*?\S)__|~~(?=\S)([\s\S]*?\S)~~|\*(?=[^\s*])([^*\n]*?[^\s*])\*|(?<![\w\\])_(?=[^\s_])([^_\n]*?[^\s_])_(?!\w)|\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)|(https?:\/\/[^\s<>()]*[^\s<>().,;:!?'"*_])|\\\((.+?)\\\)|\$(?=\S)([^$\n]*?[\\^_{}=][^$\n]*?)(?<=\S)\$/g;

function inline(text) {
  const frag = document.createDocumentFragment();
  let last = 0;
  for (const m of text.matchAll(INLINE)) {
    if (m.index > last) frag.append(text.slice(last, m.index));
    last = m.index + m[0].length;
    if (m[1]) frag.append(el("code", { textContent: m[2].trim() || m[2] }));
    else if (m[3] || m[4]) frag.append(el("strong", {}, inline(m[3] || m[4])));
    else if (m[5]) frag.append(el("del", {}, inline(m[5])));
    else if (m[6] || m[7]) frag.append(el("em", {}, inline(m[6] || m[7])));
    else if (m[8]) frag.append(link(m[9], inline(m[8])));
    else if (m[10]) frag.append(link(m[10], m[10]));
    else if (m[11] || m[12]) frag.append(el("span", { className: "math-inline" }, tex(m[11] || m[12])));
  }
  if (last < text.length) frag.append(text.slice(last));
  return frag;
}

// Enough TeX to read what small models write: \boxed, \frac, \text, the
// common symbols, ^ and _ — as text, never markup.
const TEX_SYMBOLS = {
  times: "×", cdot: "·", div: "÷", pm: "±", le: "≤", leq: "≤", ge: "≥", geq: "≥", neq: "≠", ne: "≠", approx: "≈",
  to: "→", rightarrow: "→", leftarrow: "←", Rightarrow: "⇒", implies: "⇒", infty: "∞", sum: "∑", prod: "∏", int: "∫",
  pi: "π", alpha: "α", beta: "β", gamma: "γ", delta: "δ", theta: "θ", lambda: "λ", mu: "μ", sigma: "σ", phi: "φ", omega: "ω",
  Delta: "Δ", Sigma: "Σ", Omega: "Ω", in: "∈", notin: "∉", subset: "⊂", cup: "∪", cap: "∩", forall: "∀", exists: "∃",
  ldots: "…", cdots: "⋯", dots: "…", quad: " ", qquad: "  ", circ: "°", degree: "°", partial: "∂", nabla: "∇", equiv: "≡",
  sqrt: "√", mid: "|", lfloor: "⌊", rfloor: "⌋", lceil: "⌈", rceil: "⌉", langle: "⟨", rangle: "⟩",
};

function texGroup(src, i) {
  if (src[i] !== "{") return [src[i] || "", i + 1];
  let depth = 0;
  for (let j = i; j < src.length; j++) {
    if (src[j] === "{") depth++;
    else if (src[j] === "}" && --depth === 0) return [src.slice(i + 1, j), j + 1];
  }
  return [src.slice(i + 1), src.length];
}

function tex(src) {
  const frag = document.createDocumentFragment();
  let text = "";
  const flush = () => { if (text) { frag.append(text); text = ""; } };
  for (let i = 0; i < src.length;) {
    const c = src[i];
    if (c === "\\") {
      const name = (src.slice(i + 1).match(/^[A-Za-z]+/) || [""])[0];
      if (!name) { text += src[i + 1] === "," || src[i + 1] === ";" ? " " : src[i + 1] || ""; i += 2; continue; }
      i += 1 + name.length;
      if (name === "boxed" || name === "fbox") {
        const [inner, next] = texGroup(src, i);
        flush(); frag.append(el("span", { className: "math-box" }, tex(inner))); i = next; continue;
      }
      if (name === "frac" || name === "dfrac" || name === "tfrac") {
        const [a, n1] = texGroup(src, i);
        const [b, n2] = texGroup(src, n1);
        const wrap = (x) => (/^[\w.]+$/.test(x) ? x : `(${x})`);
        flush(); frag.append(tex(wrap(a)), "/", tex(wrap(b))); i = n2; continue;
      }
      if (name === "text" || name === "mathrm" || name === "textbf" || name === "mathbf" || name === "operatorname" || name === "mathit") {
        const [inner, next] = texGroup(src, i);
        flush(); frag.append(tex(inner)); i = next; continue;
      }
      if (name === "sqrt") {
        const [inner, next] = texGroup(src, i);
        flush(); frag.append("√", tex(/^[\w.]+$/.test(inner) ? inner : `(${inner})`)); i = next; continue;
      }
      if (name === "left" || name === "right" || name === "displaystyle" || name === "limits") continue;
      text += TEX_SYMBOLS[name] ?? name;
      continue;
    }
    if (c === "^" || c === "_") {
      const [inner, next] = texGroup(src, i + 1);
      flush(); frag.append(el(c === "^" ? "sup" : "sub", {}, tex(inner))); i = next; continue;
    }
    if (c === "{" || c === "}") { i++; continue; }
    text += c;
    i++;
  }
  flush();
  return frag;
}

function link(href, content) {
  const a = el("a", { href, target: "_blank", rel: "noopener noreferrer" }, content);
  return a;
}

function inlineLines(text) {
  const frag = document.createDocumentFragment();
  text.split("\n").forEach((line, i) => { if (i) frag.append(el("br")); frag.append(inline(line)); });
  return frag;
}

const FENCE = /^\s*(`{3,}|~{3,})\s*([\w+#.-]*)[^\n]*$/;
const LIST = /^(\s*)([-*+]|\d{1,9}[.)])\s+(.*)$/;
const HEADING = /^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$/;
const RULE = /^\s{0,3}([-*_])(\s*\1){2,}\s*$/;
const TABLE_SEP = /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$/;

function startsBlock(lines, i) {
  const line = lines[i];
  return FENCE.test(line) || HEADING.test(line) || RULE.test(line) || /^\s*>/.test(line) || LIST.test(line) ||
    (line.includes("|") && i + 1 < lines.length && TABLE_SEP.test(lines[i + 1]));
}

function markdown(src) {
  const frag = document.createDocumentFragment();
  const lines = src.replace(/\r\n?/g, "\n").split("\n");
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    const fence = line.match(FENCE);
    if (fence) {
      const code = [];
      i++;
      while (i < lines.length && !new RegExp("^\\s*" + fence[1][0] + "{" + fence[1].length + ",}\\s*$").test(lines[i])) code.push(lines[i++]);
      i++;
      frag.append(codeBlock(code.join("\n"), fence[2]));
      continue;
    }
    if (!line.trim()) { i++; continue; }
    const display = line.match(/^\s*(\$\$|\\\[)(.*)$/);
    if (display) {
      const close = display[1] === "$$" ? "$$" : "\\]";
      const body = [];
      let rest = display[2];
      for (;;) {
        const end = rest.indexOf(close);
        if (end >= 0) { body.push(rest.slice(0, end)); break; }
        body.push(rest);
        if (++i >= lines.length) break;
        rest = lines[i];
      }
      i++;
      frag.append(el("div", { className: "math" }, tex(body.join(" ").trim())));
      continue;
    }
    const heading = line.match(HEADING);
    if (heading) {
      frag.append(el("h" + Math.min(6, heading[1].length + 2), { className: "md-h" + heading[1].length }, inline(heading[2])));
      i++;
      continue;
    }
    if (RULE.test(line)) { frag.append(el("hr")); i++; continue; }
    if (/^\s*>/.test(line)) {
      const quoted = [];
      while (i < lines.length && /^\s*>/.test(lines[i])) quoted.push(lines[i++].replace(/^\s*>\s?/, ""));
      frag.append(el("blockquote", {}, markdown(quoted.join("\n"))));
      continue;
    }
    if (LIST.test(line)) {
      const [list, next] = parseList(lines, i, line.match(LIST)[1].length);
      frag.append(list);
      i = next;
      continue;
    }
    if (line.includes("|") && i + 1 < lines.length && TABLE_SEP.test(lines[i + 1])) {
      const rows = [line];
      i += 2;
      while (i < lines.length && lines[i].includes("|") && lines[i].trim()) rows.push(lines[i++]);
      frag.append(table(rows));
      continue;
    }
    const para = [line];
    i++;
    while (i < lines.length && lines[i].trim() && !startsBlock(lines, i)) para.push(lines[i++]);
    frag.append(el("p", {}, inlineLines(para.join("\n"))));
  }
  return frag;
}

function parseList(lines, i, indent) {
  const ordered = /\d/.test(lines[i].match(LIST)[2]);
  const list = el(ordered ? "ol" : "ul");
  if (ordered) list.start = parseInt(lines[i].match(LIST)[2], 10);
  let item = null;
  while (i < lines.length) {
    const line = lines[i];
    const m = line.match(LIST);
    if (!m) {
      if (!line.trim()) {
        let j = i + 1;
        while (j < lines.length && !lines[j].trim()) j++;
        const next = j < lines.length && lines[j].match(LIST);
        if (next && next[1].length >= indent) { i = j; continue; }
        break;
      }
      const lead = line.match(/^\s*/)[0].length;
      if (item && lead > indent && !FENCE.test(line)) { item.append(el("br"), inline(line.trim())); i++; continue; }
      if (item && lead > indent && FENCE.test(line)) {
        const fence = line.match(FENCE);
        const code = [];
        i++;
        while (i < lines.length && !new RegExp("^\\s*" + fence[1][0] + "{" + fence[1].length + ",}\\s*$").test(lines[i])) code.push(lines[i++].slice(lead));
        i++;
        item.append(codeBlock(code.join("\n"), fence[2]));
        continue;
      }
      break;
    }
    const lead = m[1].length;
    if (lead < indent) break;
    if (lead > indent && item) {
      const [sub, next] = parseList(lines, i, lead);
      item.append(sub);
      i = next;
      continue;
    }
    if (/\d/.test(m[2]) !== ordered) break;
    item = el("li", {}, inline(m[3]));
    list.append(item);
    i++;
  }
  return [list, i];
}

function table(rows) {
  const cells = (row) => row.trim().replace(/^\|/, "").replace(/\|$/, "").split("|").map((c) => c.trim());
  const head = el("tr", {}, ...cells(rows[0]).map((c) => el("th", {}, inline(c))));
  const body = rows.slice(1).map((r) => el("tr", {}, ...cells(r).map((c) => el("td", {}, inline(c)))));
  return el("div", { className: "md-table" }, el("table", {}, el("thead", {}, head), el("tbody", {}, ...body)));
}

function codeBlock(code, lang) {
  const copyBtn = el("button", { type: "button", className: "code-copy", title: "Copy code" }, icon("copy"), el("span", { textContent: "Copy" }));
  copyBtn.onclick = () => {
    navigator.clipboard.writeText(code).then(() => {
      copyBtn.classList.add("done");
      copyBtn.lastChild.textContent = "Copied";
      setTimeout(() => { copyBtn.classList.remove("done"); copyBtn.lastChild.textContent = "Copy"; }, 1400);
    }, () => toast("Copy failed — select the text instead"));
  };
  return el("div", { className: "code" },
    el("div", { className: "code-head" }, el("span", { textContent: lang || "code" }), copyBtn),
    el("pre", {}, el("code", { textContent: code })));
}

// ---------- layout ----------

function mount(section) {
  const u = chat.ui;
  u.list = el("div", { className: "chat-list" });
  u.newBtn = el("button", { type: "button", className: "icon-btn small-icon", title: "New chat" }, icon("plus"));
  u.newBtn.onclick = () => newChat(true);
  u.search = el("input", { type: "search", className: "input chat-search", placeholder: "Search conversations" });
  u.search.setAttribute("aria-label", "Search conversations");
  u.search.oninput = searchChats;
  u.side = el("aside", { className: "chat-side" },
    el("div", { className: "chat-side-head" }, el("span", { textContent: "Chats" }), u.newBtn),
    el("div", { className: "chat-search-wrap" }, u.search),
    u.list);

  u.menuBtn = el("button", { type: "button", className: "icon-btn small-icon chat-menu-btn", title: "Chats" }, icon("menu"));
  u.menuBtn.onclick = () => section.classList.toggle("side-open");
  u.title = el("div", { className: "chat-title" });
  u.saveState = el("button", { type: "button", className: "text-btn chat-save", hidden: true });
  u.saveState.onclick = () => saveConv();
  u.saveState.setAttribute("aria-live", "polite");
  u.exportBtn = el("button", { type: "button", className: "icon-btn small-icon", title: "Export chat", disabled: true }, icon("down"));
  u.exportPop = el("div", { className: "chat-pop export-pop", hidden: true });
  const exportOption = (format, label) => {
    const b = el("button", { type: "button", className: "text-btn", textContent: label });
    b.onclick = () => { exportChat(format); closePopovers(); };
    return b;
  };
  u.exportPop.append(exportOption("md", "Export Markdown"), exportOption("json", "Export JSON"));
  u.exportBtn.onclick = () => { const open = u.exportPop.hidden; closePopovers(); u.exportPop.hidden = !open; };
  u.statusDot = el("i", { className: "dot" });
  u.statusText = el("span");
  u.unload = el("button", { type: "button", className: "chat-unload", title: "Unload the model and free its memory" }, icon("power"), el("span", { textContent: "Unload" }));
  u.unload.onclick = unloadModel;
  u.status = el("div", { className: "chat-status" }, u.statusDot, u.statusText, u.unload);
  const head = el("header", { className: "chat-head" }, u.menuBtn, u.title, u.saveState, el("div", { className: "pop-anchor" }, u.exportBtn, u.exportPop), u.status);

  u.thread = el("div", { className: "chat-thread" });
  u.scroll = el("div", { className: "chat-scroll" }, u.thread);
  u.scroll.addEventListener("scroll", () => {
    const gap = u.scroll.scrollHeight - u.scroll.scrollTop - u.scroll.clientHeight;
    chat.stick = gap < 48;
    u.jump.hidden = chat.stick;
  });
  u.jump = el("button", { type: "button", className: "chat-jump", title: "Jump to latest", hidden: true }, icon("down"));
  u.jump.onclick = () => scrollToEnd(true);

  mountComposer();
  const main = el("main", { className: "chat-main" }, head, u.scroll, u.jump, u.composerWrap);
  const scrim = el("div", { className: "chat-scrim" });
  scrim.onclick = () => section.classList.remove("side-open");
  section.append(el("div", { className: "chat-app" }, u.side, scrim, main));
  u.section = section;
  chat.mounted = true;
  chat.stick = true;

  document.addEventListener("keydown", (e) => {
    if (Studio.active !== "chat") return;
    if (e.key === "Escape") {
      if (closePopovers()) return;
      if (chat.stream) { e.preventDefault(); stopStream(); }
    }
    if ((e.metaKey || e.ctrlKey) && e.shiftKey && e.key.toLowerCase() === "o") { e.preventDefault(); newChat(true); }
  });
  document.addEventListener("click", (e) => {
    for (const pop of [u.modelPop, u.paramPop, u.exportPop]) if (!pop.hidden && !pop.parentElement.contains(e.target)) pop.hidden = true;
  });
  document.addEventListener("paste", (e) => {
    if (Studio.active !== "chat") return;
    const files = [...(e.clipboardData?.files || [])];
    if (files.length) { e.preventDefault(); attachFiles(files); }
  });
  window.addEventListener("beforeunload", (e) => {
    if (!chat.stream && !chatDrafts.size) return;
    e.preventDefault(); e.returnValue = "";
  });

  loadList().then(() => {
    if (chat.conv) return; // A model opened from Models already started a new chat.
    const current = store.get("chatCurrent", null);
    if (current && chat.list.some((c) => c.id === current)) openChat(current);
    else newChat(false);
  });
  refreshStatus();
}

function mountComposer() {
  const u = chat.ui;
  u.input = el("textarea", { className: "chat-input", rows: 1, placeholder: "Message" });
  u.input.value = store.get("chatDraft", "");
  u.input.addEventListener("input", () => { autosize(); store.set("chatDraft", u.input.value); updateSend(); });
  u.input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); send(); }
  });

  u.attachments = el("div", { className: "chat-attachments" });
  u.file = el("input", { type: "file", multiple: true, hidden: true });
  u.file.addEventListener("change", () => { attachFiles([...u.file.files]); u.file.value = ""; });

  u.modelName = el("span", { className: "model-name" });
  u.modelBadges = el("span", { className: "badges" });
  u.modelBtn = el("button", { type: "button", className: "model-btn", title: "Model" }, el("i", { className: "dot" }), u.modelName, u.modelBadges, icon("updown"));
  u.modelPop = el("div", { className: "chat-pop model-pop", hidden: true });
  u.modelBtn.onclick = () => { const open = u.modelPop.hidden; closePopovers(); if (open) { renderModelMenu(); u.modelPop.hidden = false; } };

  u.attachBtn = el("button", { type: "button", className: "tool-btn", title: "Attach" }, icon("paperclip"));
  u.attachBtn.onclick = () => u.file.click();
  u.micBtn = el("button", { type: "button", className: "tool-btn", title: "Record audio" }, icon("mic"));
  u.micBtn.onclick = () => (chat.recording ? stopRecording() : startRecording());
  u.thinkBtn = el("button", { type: "button", className: "tool-btn think-btn", title: "Let the model think before it answers (Qwen3 and other reasoning models)" }, icon("bulb"), el("span", { textContent: "Think" }));
  u.thinkBtn.onclick = () => { chat.think = !chat.think; store.set("chatThink", chat.think); renderTools(); };

  u.paramBtn = el("button", { type: "button", className: "tool-btn", title: "Parameters" }, icon("sliders"));
  u.paramPop = el("div", { className: "chat-pop param-pop", hidden: true });
  u.paramBtn.onclick = () => { const open = u.paramPop.hidden; closePopovers(); if (open) { renderParams(); u.paramPop.hidden = false; } };

  u.send = el("button", { type: "button", className: "send-btn", title: "Send (Enter)" }, icon("send"));
  u.send.onclick = () => (chat.stream ? stopStream() : send());

  const tools = el("div", { className: "chat-tools" },
    el("div", { className: "pop-anchor" }, u.modelBtn, u.modelPop),
    u.attachBtn, u.micBtn, u.thinkBtn,
    el("div", { className: "pop-anchor" }, u.paramBtn, u.paramPop),
    el("span", { className: "grow" }),
    u.send);
  u.composer = el("div", { className: "chat-composer" }, u.attachments, u.input, tools, u.file);
  u.hint = el("div", { className: "chat-hint" });
  u.composerWrap = el("div", { className: "chat-composer-wrap" }, u.composer, u.hint);

  u.composer.addEventListener("dragover", (e) => { if ([...e.dataTransfer.types].includes("Files")) { e.preventDefault(); u.composer.classList.add("dragging"); } });
  u.composer.addEventListener("dragleave", (e) => { if (!u.composer.contains(e.relatedTarget)) u.composer.classList.remove("dragging"); });
  u.composer.addEventListener("drop", (e) => { e.preventDefault(); u.composer.classList.remove("dragging"); attachFiles([...e.dataTransfer.files]); });
}

function closePopovers() {
  let closed = false;
  for (const pop of [chat.ui.modelPop, chat.ui.paramPop, chat.ui.exportPop]) if (pop && !pop.hidden) { pop.hidden = true; closed = true; }
  return closed;
}

function autosize() {
  const t = chat.ui.input;
  t.style.height = "auto";
  t.style.height = Math.min(t.scrollHeight, 240) + "px";
}

// ---------- models, capabilities, status ----------

function setModels(list) {
  chat.models = Studio.modelsOf("chat");
  if (!chat.mounted) return;
  if (!chat.models.some((m) => m.id === chat.model)) chat.model = chat.models[0]?.id || null;
  renderTools();
  renderThread();
}

// What the chosen model takes besides text: what its loaded server says, else its kind.
function caps(id = chat.model) {
  const m = chat.models.find((x) => x.id === id);
  if (!m) return { vision: false, audio: false };
  if (chat.status.model === id && chat.status.modalities) return chat.status.modalities;
  return { vision: m.kind === "vision", audio: m.kind === "audio" };
}

function kindBadges(id) {
  const c = caps(id);
  const out = [];
  if (c.vision) out.push(el("span", { className: "badge", textContent: "Vision" }));
  if (c.audio) out.push(el("span", { className: "badge", textContent: "Audio" }));
  return out;
}

function renderTools() {
  const u = chat.ui;
  const c = caps();
  const has = chat.models.length > 0;
  u.modelName.textContent = has ? displayName(chat.model) : "No chat models";
  u.modelBadges.replaceChildren(...kindBadges(chat.model));
  u.modelBtn.dataset.state = chat.status.model === chat.model ? chat.status.state : "";
  u.modelBtn.disabled = !has;
  u.attachBtn.hidden = !(c.vision || c.audio);
  u.attachBtn.title = c.vision && c.audio ? "Attach an image or audio clip" : c.vision ? "Attach an image" : "Attach an audio clip";
  u.file.accept = [c.vision ? "image/png,image/jpeg,image/webp" : "", c.audio ? "audio/wav,audio/x-wav,audio/mpeg" : ""].filter(Boolean).join(",");
  u.micBtn.hidden = !c.audio;
  u.micBtn.classList.toggle("recording", !!chat.recording);
  u.micBtn.title = chat.recording ? "Stop recording" : "Record audio";
  u.micBtn.replaceChildren(icon(chat.recording ? "stop" : "mic"));
  u.thinkBtn.setAttribute("aria-pressed", String(chat.think));
  u.paramBtn.classList.toggle("tuned", paramsTuned());
  u.input.placeholder = !has ? "Download a chat model to start" : c.audio ? "Message, or record a question" : c.vision ? "Message, or drop an image" : "Message " + displayName(chat.model);
  u.input.disabled = !has;
  u.hint.replaceChildren(
    el("span", {}, el("kbd", { textContent: "Enter" }), " to send, ", el("kbd", { textContent: "Shift Enter" }), " for a new line"),
  );
  renderStatus();
  updateSend();
}

function updateSend() {
  const u = chat.ui;
  const streaming = !!chat.stream;
  u.send.classList.toggle("stop", streaming);
  u.send.replaceChildren(icon(streaming ? "stop" : "send"));
  u.send.title = streaming ? "Stop (Esc)" : "Send (Enter)";
  u.send.disabled = !streaming && (!chat.models.length || (!u.input.value.trim() && !chat.pending.length) || !!chat.recording || chat.pending.some((p) => p.uploading));
}

function renderStatus() {
  const u = chat.ui;
  const st = chat.status;
  u.status.dataset.state = st.state || "idle";
  if (st.state === "loading") u.statusText.textContent = `Loading ${displayName(st.model)}`;
  else if (st.state === "ready") u.statusText.textContent = `${displayName(st.model)} ready`;
  else u.statusText.textContent = "No model loaded";
  u.unload.hidden = st.state !== "ready" || !!chat.stream;
}

async function refreshStatus() {
  try {
    const res = await fetch("/api/chat/status");
    if (res.ok) applyStatus(await res.json());
  } catch {}
}

function applyStatus(st) {
  chat.status = st || { state: "" };
  if (!chat.mounted) return;
  renderTools();
  if (!chat.conv?.messages.length) renderThread();
  clearTimeout(chat.statusTimer);
  chat.statusTimer = setTimeout(refreshStatus, st && st.state === "loading" ? 1500 : 20000);
}

async function warm(id) {
  try {
    const res = await fetch("/api/chat/load", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ model: id }) });
    if (res.ok) applyStatus(await res.json());
    else toast((await res.text()).trim());
  } catch {}
}

async function unloadModel() {
  chat.ui.unload.disabled = true;
  try { await fetch("/api/chat/unload", { method: "POST" }); } catch {}
  chat.ui.unload.disabled = false;
  refreshStatus();
  toast("Model unloaded");
}

function renderModelMenu() {
  const u = chat.ui;
  const items = chat.models.map((m) => {
    const loaded = chat.status.model === m.id ? chat.status.state : "";
    const meta = [m.kind, m.params, m.quant].filter(Boolean).join(" · ");
    const b = el("button", { type: "button", className: "model-item" },
      el("span", { className: "model-item-main" },
        el("span", { className: "model-item-name" }, el("span", { textContent: displayName(m.id) }), ...kindBadges(m.id)),
        el("span", { className: "model-item-meta", textContent: meta + (loaded === "ready" ? " · loaded" : loaded === "loading" ? " · loading" : "") })),
      m.id === chat.model ? icon("check") : el("span", { className: "check-space" }));
    b.onclick = () => pickModel(m.id);
    return b;
  });
  u.modelPop.replaceChildren(el("div", { className: "pop-title", textContent: "Chat models" }), ...items);
}

function pickModel(id) {
  chat.ui.modelPop.hidden = true;
  if (chat.stream) { toast("Stop the reply first"); return; }
  chat.model = id;
  store.set("chatModel", id);
  renderTools();
  renderThread();
  if (!(chat.status.model === id && chat.status.state)) warm(id);
}

// ---------- parameters ----------

function paramsTuned() {
  const p = chat.params;
  return p.system.trim() !== "" || p.temperature !== PARAM_DEFAULTS.temperature || p.top_p !== PARAM_DEFAULTS.top_p || p.max_tokens !== PARAM_DEFAULTS.max_tokens;
}

function saveParams() {
  store.set("chatParams", chat.params);
  chat.ui.paramBtn.classList.toggle("tuned", paramsTuned());
}

function renderParams() {
  const p = chat.params;
  const system = el("textarea", { className: "small", placeholder: "You are a concise, friendly assistant.", value: p.system });
  system.addEventListener("input", () => { p.system = system.value; saveParams(); });
  const slider = (label, key, min, max, step, hint) => {
    const out = el("output", { textContent: String(p[key]) });
    const input = el("input", { type: "range", min, max, step, value: p[key] });
    input.addEventListener("input", () => { p[key] = Number(input.value); out.textContent = input.value; saveParams(); });
    return el("div", { className: "field" }, el("div", { className: "label" }, el("span", { textContent: label, title: hint }), out), input);
  };
  const maxTokens = el("input", { className: "input", type: "number", min: 16, max: 32768, step: 16, value: p.max_tokens });
  maxTokens.addEventListener("input", () => {
    const n = Math.round(Number(maxTokens.value));
    if (n >= 16 && n <= 32768) { p.max_tokens = n; saveParams(); }
  });
  const reset = el("button", { type: "button", className: "text-btn", textContent: "Reset" });
  reset.onclick = () => { chat.params = { ...PARAM_DEFAULTS }; saveParams(); renderParams(); };
  chat.ui.paramPop.replaceChildren(
    el("div", { className: "pop-title" }, el("span", { textContent: "Parameters" }), reset),
    el("div", { className: "field" }, el("label", { className: "label", textContent: "System prompt" }), system),
    slider("Temperature", "temperature", 0, 2, 0.05, "Higher is more varied, lower more predictable"),
    slider("Top P", "top_p", 0.05, 1, 0.05, "Samples only from the most likely tokens that add up to this share"),
    el("div", { className: "field" }, el("div", { className: "label" }, el("span", { textContent: "Max tokens" }), el("span", { className: "hint", textContent: "reply length cap" })), maxTokens),
  );
}

// ---------- attachments and recording ----------

async function attachFiles(files) {
  const c = caps();
  for (const file of files) {
    const isImage = /^image\/(png|jpeg|webp)$/.test(file.type);
    const isAudio = /^audio\/(wav|x-wav|wave|mpeg|mp3)$/.test(file.type);
    if (!isImage && !isAudio) { toast("Attach PNG, JPEG or WebP images, or WAV or MP3 audio"); continue; }
    if (isImage && !c.vision) { toast(`${displayName(chat.model)} can't see images — pick a vision model`); continue; }
    if (isAudio && !c.audio) { toast(`${displayName(chat.model)} can't hear audio — pick an audio model`); continue; }
    await addAttachment(file, isImage ? "image" : "audio");
  }
}

async function addAttachment(blob, kind) {
  if (chat.pending.length >= 8) { toast("Eight attachments is the most one message takes"); return; }
  const item = { kind, uploading: true, preview: URL.createObjectURL(blob) };
  chat.pending.push(item);
  renderAttachments();
  const name = await uploadRef(blob);
  if (!chat.pending.includes(item)) return; // Removed while the upload was in flight.
  item.uploading = false;
  if (!name) { chat.pending.splice(chat.pending.indexOf(item), 1); URL.revokeObjectURL(item.preview); }
  else item.name = name;
  renderAttachments();
}

function renderAttachments() {
  const u = chat.ui;
  const nodes = chat.pending.map((p, i) => {
    const remove = el("button", { type: "button", className: "att-x", title: "Remove" }, icon("x"));
    remove.onclick = () => { chat.pending.splice(i, 1); URL.revokeObjectURL(p.preview); renderAttachments(); };
    const body = p.kind === "image"
      ? el("img", { src: p.preview, alt: "" })
      : el("div", { className: "att-audio" }, icon("voice"), el("span", { textContent: p.label || "Audio" }));
    return el("div", { className: `att ${p.kind}${p.uploading ? " uploading" : ""}` }, body, remove);
  });
  if (chat.recording) {
    chat.ui.recTime = el("span", { className: "rec-time", textContent: "0:00" });
    const stop = el("button", { type: "button", className: "text-btn", textContent: "Stop" });
    stop.onclick = stopRecording;
    const cancel = el("button", { type: "button", className: "text-btn", textContent: "Discard" });
    cancel.onclick = () => stopRecording(true);
    chat.ui.recLevel = el("i", { className: "rec-level" });
    nodes.push(el("div", { className: "att recording-pill" }, el("i", { className: "rec-dot" }), el("span", { textContent: "Recording" }), chat.ui.recTime, el("span", { className: "rec-meter" }, chat.ui.recLevel), stop, cancel));
  }
  u.attachments.replaceChildren(...nodes);
  u.attachments.hidden = nodes.length === 0;
  updateSend();
}

// In-browser recording straight to 16-bit PCM WAV at 16 kHz: MediaRecorder
// only makes webm/ogg, which llama-server's audio path does not decode.
async function startRecording() {
  if (!navigator.mediaDevices?.getUserMedia) { toast("This browser can't record audio here"); return; }
  let stream;
  try {
    stream = await navigator.mediaDevices.getUserMedia({ audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true } });
  } catch {
    toast("Microphone access was refused");
    return;
  }
  const ctx = new AudioContext();
  const source = ctx.createMediaStreamSource(stream);
  const proc = ctx.createScriptProcessor(4096, 1, 1);
  const rec = { stream, ctx, source, proc, chunks: [], started: Date.now(), level: 0 };
  proc.onaudioprocess = (e) => {
    const data = e.inputBuffer.getChannelData(0);
    rec.chunks.push(new Float32Array(data));
    let peak = 0;
    for (let i = 0; i < data.length; i += 16) peak = Math.max(peak, Math.abs(data[i]));
    rec.level = rec.level * 0.6 + peak * 0.4;
  };
  source.connect(proc);
  proc.connect(ctx.destination);
  chat.recording = rec;
  rec.timer = setInterval(() => {
    const s = Math.floor((Date.now() - rec.started) / 1000);
    if (chat.ui.recTime) chat.ui.recTime.textContent = `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
    if (chat.ui.recLevel) chat.ui.recLevel.style.transform = `scaleX(${Math.min(1, rec.level * 2.5).toFixed(2)})`;
    if (s >= 300) stopRecording();
  }, 100);
  renderTools();
  renderAttachments();
}

async function stopRecording(discard) {
  const rec = chat.recording;
  if (!rec) return;
  chat.recording = null;
  clearInterval(rec.timer);
  rec.source.disconnect();
  rec.proc.disconnect();
  rec.stream.getTracks().forEach((t) => t.stop());
  const rate = rec.ctx.sampleRate;
  rec.ctx.close();
  renderTools();
  renderAttachments();
  if (discard === true) return;
  const seconds = (Date.now() - rec.started) / 1000;
  if (seconds < 0.4) { toast("That was too short to hear"); return; }
  const wav = encodeWav(resample(concat(rec.chunks), rate, 16000), 16000);
  const s = Math.round(seconds);
  await addAttachment(wav, "audio");
  const item = chat.pending[chat.pending.length - 1];
  if (item) { item.label = `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`; renderAttachments(); }
}

function concat(chunks) {
  const out = new Float32Array(chunks.reduce((n, c) => n + c.length, 0));
  let offset = 0;
  for (const c of chunks) { out.set(c, offset); offset += c.length; }
  return out;
}

// Box-filtered decimation: averages the source samples each output sample
// covers, which keeps the aliasing of a plain pick-every-nth out.
function resample(input, from, to) {
  if (from === to) return input;
  const ratio = from / to;
  const out = new Float32Array(Math.floor(input.length / ratio));
  for (let i = 0; i < out.length; i++) {
    const start = Math.floor(i * ratio), end = Math.min(input.length, Math.floor((i + 1) * ratio));
    let sum = 0;
    for (let j = start; j < end; j++) sum += input[j];
    out[i] = end > start ? sum / (end - start) : input[start] || 0;
  }
  return out;
}

function encodeWav(samples, rate) {
  const buffer = new ArrayBuffer(44 + samples.length * 2);
  const view = new DataView(buffer);
  const text = (offset, s) => { for (let i = 0; i < s.length; i++) view.setUint8(offset + i, s.charCodeAt(i)); };
  text(0, "RIFF"); view.setUint32(4, 36 + samples.length * 2, true); text(8, "WAVE");
  text(12, "fmt "); view.setUint32(16, 16, true); view.setUint16(20, 1, true); view.setUint16(22, 1, true);
  view.setUint32(24, rate, true); view.setUint32(28, rate * 2, true); view.setUint16(32, 2, true); view.setUint16(34, 16, true);
  text(36, "data"); view.setUint32(40, samples.length * 2, true);
  for (let i = 0; i < samples.length; i++) {
    const s = Math.max(-1, Math.min(1, samples[i]));
    view.setInt16(44 + i * 2, s < 0 ? s * 0x8000 : s * 0x7fff, true);
  }
  return new Blob([buffer], { type: "audio/wav" });
}

// ---------- conversations ----------

async function loadList() {
  const mutation = listMutation;
  try {
    const res = await fetch("/api/chats");
    if (!res.ok) throw new Error((await res.text()).trim() || "Couldn't load conversations");
    const list = await res.json();
    if (mutation === listMutation) chat.list = list;
    chat.listError = "";
  } catch (err) { chat.listError = err.message || "Couldn't load conversations"; }
  renderList();
}

let chatSearchSeq = 0, chatSearchTimer, chatSearchController;
function searchChats() {
  const seq = ++chatSearchSeq;
  const query = chat.query = chat.ui.search.value.trim();
  clearTimeout(chatSearchTimer);
  chatSearchController?.abort();
  chat.searchList = null;
  chat.searchError = "";
  chat.searching = !!query;
  renderList();
  if (!query) return;
  chatSearchTimer = setTimeout(async () => {
    const controller = chatSearchController = new AbortController();
    try {
      const res = await fetch("/api/chats?" + new URLSearchParams({ q: query }), { signal: controller.signal });
      if (!res.ok) throw new Error((await res.text()).trim() || "Couldn't search conversations");
      const matches = await res.json();
      if (seq === chatSearchSeq) chat.searchList = matches;
    } catch (err) {
      if (seq === chatSearchSeq && !controller.signal.aborted) chat.searchError = err.message || "Couldn't search conversations";
    } finally {
      if (seq === chatSearchSeq) { chat.searching = false; renderList(); }
    }
  }, 200);
}

function renderList() {
  const u = chat.ui;
  if (u.list.querySelector(".item-rename")) return;
  const error = chat.query ? chat.searchError : chat.listError;
  let notice;
  if (error) {
    const retry = el("button", { type: "button", className: "text-btn", textContent: "Try again" });
    retry.onclick = chat.query ? searchChats : loadList;
    notice = el("div", {}, el("p", { className: "chat-list-empty", textContent: error }), retry);
  }
  const base = chat.query ? chat.searchList || [] : chat.list;
  const drafts = [...chatDrafts.values()].filter((conv) => !deletingChats.has(conv.id) && (!chat.query ||
    [conv.title, conv.model, ...conv.messages.flatMap((m) => [m.content, m.reasoning])].filter(Boolean).join("\n").toLowerCase().includes(chat.query.toLowerCase())));
  const list = [...drafts.map((conv) => ({ id: conv.id, title: conv.title, model: conv.model, updated: conv.updated || new Date().toISOString(), unsaved: conv._saveState === "failed" })),
    ...base.filter((c) => !drafts.some((conv) => conv.id === c.id))];
  if (chat.searching || !list.length) {
    u.list.replaceChildren(...(notice ? [notice] : [el("p", { className: "chat-list-empty", role: "status", textContent: chat.searching ? "Searching conversations…" : chat.query ? "No conversations match." : "Your conversations show up here." })]));
    return;
  }
  const nodes = notice ? [notice] : [];
  let group = "";
  for (const c of list) {
    const g = dayGroup(c.updated);
    if (g !== group) { group = g; nodes.push(el("div", { className: "chat-group", textContent: g })); }
    nodes.push(listItem(c));
  }
  u.list.replaceChildren(...nodes);
}

function listItem(c) {
  const active = chat.conv && chat.conv.id === c.id;
  const title = el("span", { className: "item-title", textContent: c.title || "New chat" });
  const time = el("span", { className: "item-time" + (c.unsaved ? " unsaved" : ""), textContent: c.unsaved ? "Unsaved" : relTime(c.updated) });
  const rename = el("button", { type: "button", className: "item-act", title: "Rename" }, icon("edit"));
  const del = el("button", { type: "button", className: "item-act danger", title: "Delete" }, icon("trash"));
  const row = el("div", { className: "chat-item" + (active ? " active" : ""), tabIndex: 0, role: "button" }, title, time, el("span", { className: "item-acts" }, rename, del));
  row.onclick = (e) => { if (!e.target.closest(".item-acts") && !e.target.closest("input")) { openChat(c.id); chat.ui.section.classList.remove("side-open"); } };
  row.onkeydown = (e) => { if (e.key === "Enter" && e.target === row) openChat(c.id); };
  rename.onclick = () => {
    const input = el("input", { className: "item-rename", value: c.title || "" });
    title.replaceWith(input);
    row.classList.add("renaming");
    input.focus();
    input.select();
    let done = false;
    const finish = async (save) => {
      if (done) return;
      done = true;
      const next = input.value.trim();
      input.remove();
      if (save && next && next !== c.title) {
        const conv = chat.conv?.id === c.id ? chat.conv : chatDrafts.get(c.id);
        const previous = conv?.title;
        if (conv) conv.title = next;
        try {
          await chatSaves.get(c.id);
          const res = await fetch(`/api/chats/${c.id}`, { method: "PATCH", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ title: next }) });
          if (!res.ok) throw new Error((await res.text()).trim() || "Couldn't rename this chat");
          ++listMutation;
          for (const list of [chat.list, chat.searchList || []]) for (const item of list) if (item.id === c.id) item.title = next;
          if (chat.query) searchChats();
        } catch (err) { if (conv) conv.title = previous; toast(err.message || "Couldn't rename this chat"); }
        if (conv === chat.conv) renderTitle();
      }
      renderList();
    };
    input.onkeydown = (e) => { if (e.key === "Enter") finish(true); if (e.key === "Escape") { e.stopPropagation(); finish(false); } };
    input.onblur = () => finish(true);
  };
  del.onclick = async () => {
    if (!del.classList.contains("armed")) {
      del.classList.add("armed");
      del.title = "Click again to delete";
      row.classList.add("arming");
      setTimeout(() => { del.classList.remove("armed"); row.classList.remove("arming"); del.title = "Delete"; }, 2400);
      return;
    }
    if (deletingChats.has(c.id)) return;
    deletingChats.add(c.id);
    del.disabled = true;
    let removed = false;
    const conv = chat.conv?.id === c.id ? chat.conv : chatDrafts.get(c.id);
    if (chat.stream?.conv.id === c.id) stopStream();
    try {
      await chatSaves.get(c.id);
      const res = await fetch(`/api/chats/${c.id}`, { method: "DELETE" });
      if (!res.ok) throw new Error((await res.text()).trim() || "Couldn't delete this chat");
      removed = true;
      ++listMutation;
      chatDrafts.delete(c.id);
      chat.list = chat.list.filter((x) => x.id !== c.id);
      if (chat.conv?.id === c.id) newChat(false);
      if (chat.query) searchChats();
      renderList();
    } catch (err) { toast(err.message || "Couldn't delete this chat"); }
    // Keep a tombstone so late stream finalizers cannot recreate the file.
    finally {
      if (!removed) { deletingChats.delete(c.id); if (conv) saveConv(conv); }
      del.disabled = false;
      renderList();
    }
  };
  return row;
}

let conversationSeq = 0;
let listMutation = 0;

function newChat(focus) {
  ++conversationSeq;
  if (chat.stream) stopStream();
  chat.conv = { id: newId(), title: "", model: chat.model, messages: [], fresh: true };
  chat.editing = -1;
  store.set("chatCurrent", null);
  renderTitle();
  renderThread();
  renderList();
  if (focus) chat.ui.input.focus();
}

async function openChat(id) {
  const seq = ++conversationSeq;
  const stopped = chat.stream ? stopStream() : null;
  let conv;
  try {
    await stopped;
    await chatSaves.get(id);
    conv = chatDrafts.get(id);
    if (!conv) {
      const res = await fetch(`/api/chats/${id}`);
      if (seq !== conversationSeq) return;
      if (!res.ok) throw new Error(res.status === 404 ? "That conversation is gone" : "Couldn't open this conversation. Try again.");
      conv = await res.json();
    }
  } catch (err) {
    if (seq === conversationSeq) toast(err.message || "Couldn't open this conversation. Try again.");
    return;
  }
  if (seq !== conversationSeq) return;
  chat.conv = conv;
  chat.editing = -1;
  store.set("chatCurrent", id);
  if (chat.conv.model && chat.models.some((m) => m.id === chat.conv.model) && chat.conv.model !== chat.model) {
    chat.model = chat.conv.model;
    store.set("chatModel", chat.model);
  }
  renderTitle();
  renderTools();
  renderThread();
  renderList();
  scrollToEnd(true);
}

const chatSaves = new Map(), chatDrafts = new Map(), deletingChats = new Set();
function saveConv(conv = chat.conv) {
  if (!conv?.messages.length || deletingChats.has(conv.id)) return Promise.resolve(false);
  // Capture now and serialize writes per conversation, including after navigation.
  const messages = conv.messages.map((m) => ({ ...cleanMessage(m), ...(chat.stream?.msg === m ? { stopped: true } : {}) }));
  const body = { title: conv.title, model: conv.model, created: conv.created, messages };
  const raw = JSON.stringify(body);
  const seq = conv._saveSeq = (conv._saveSeq || 0) + 1;
  conv._saveState = "saving";
  chatDrafts.set(conv.id, conv);
  renderList();
  if (chat.conv === conv) renderTitle();
  const save = (chatSaves.get(conv.id) || Promise.resolve()).then(async () => {
    if (deletingChats.has(conv.id)) return false;
    try {
      const res = await fetch(`/api/chats/${conv.id}`, { method: "PUT", headers: { "Content-Type": "application/json" }, body: raw });
      if (!res.ok) throw new Error((await res.text()).trim() || "Couldn't save this chat");
      const saved = await res.json();
      conv.created = saved.created;
      conv.updated = saved.updated;
      delete conv.fresh;
      if (seq === conv._saveSeq) { conv._saveState = "saved"; chatDrafts.delete(conv.id); }
      if (chat.conv === conv) store.set("chatCurrent", conv.id);
      const summary = { id: conv.id, title: body.title, model: body.model, created: saved.created, updated: saved.updated, count: messages.length };
      ++listMutation;
      chat.list = [summary, ...chat.list.filter((c) => c.id !== conv.id)];
      renderList();
      return true;
    } catch (err) {
      if (seq === conv._saveSeq) {
        conv._saveState = "failed";
        conv._saveError = err.message || "Couldn't save this chat";
      }
      return false;
    } finally { renderList(); if (chat.conv === conv) renderTitle(); }
  });
  chatSaves.set(conv.id, save);
  save.then(() => { if (chatSaves.get(conv.id) === save) chatSaves.delete(conv.id); });
  return save;
}

function cleanMessage(m) {
  const out = { role: m.role, content: m.content || "" };
  for (const k of ["refs", "reasoning", "model", "tokens", "perSecond", "thought", "stopped", "error"]) if (m[k]) out[k] = m[k];
  return out;
}

function titleFrom(text, refs) {
  const t = (text || "").replace(/\s+/g, " ").trim();
  if (t) return t.length > 60 ? t.slice(0, 57).replace(/\s+\S*$/, "") + "…" : t;
  if (refs?.some((r) => /\.(wav|mp3|ogg)$/.test(r))) return "Voice message";
  return "Image";
}

function renderTitle() {
  const conv = chat.conv, u = chat.ui;
  u.title.textContent = conv?.title || "New chat";
  u.exportBtn.disabled = !conv?.messages.length;
  u.saveState.hidden = !conv?._saveState;
  u.saveState.disabled = conv?._saveState !== "failed";
  u.saveState.textContent = conv?._saveState === "failed" ? "Retry save" : conv?._saveState === "saving" ? "Saving…" : "Saved";
  u.saveState.classList.toggle("failed", conv?._saveState === "failed");
  u.saveState.title = conv?._saveState === "failed" ? conv._saveError : "Saved privately on this machine";
}

function exportChat(format) {
  const conv = chat.conv;
  if (!conv?.messages.length) return;
  const messages = conv.messages.map((m) => ({ ...cleanMessage(m), ...(chat.stream?.msg === m ? { stopped: true } : {}) }));
  const body = { id: conv.id, title: conv.title, model: conv.model, created: conv.created, updated: conv.updated, messages };
  const content = format === "json" ? JSON.stringify(body, null, 2) + "\n" :
    `# ${conv.title || "Conversation"}\n\nModel: ${conv.model || "Unknown"}\n\n` + messages.map((m) =>
      `## ${m.role === "user" ? "You" : m.role === "assistant" ? "Assistant" : "System"}\n\n` +
      (m.reasoning ? `### Reasoning\n\n${m.reasoning}\n\n### Reply\n\n` : "") + m.content +
      (m.refs?.length ? "\n\nAttachments (files not included): " + m.refs.join(", ") : "") +
      (m.error ? `\n\nReply error: ${m.error}` : m.stopped ? "\n\nReply stopped before completion." : "")
    ).join("\n\n") + "\n";
  const url = URL.createObjectURL(new Blob([content], { type: format === "json" ? "application/json" : "text/markdown;charset=utf-8" }));
  const file = (conv.title || "conversation").replace(/[^\p{L}\p{N}._-]+/gu, "-").slice(0, 80) || "conversation";
  const a = el("a", { href: url, download: `${file}.${format}` });
  document.body.append(a); a.click(); a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

// ---------- thread ----------

function renderThread() {
  const u = chat.ui;
  if (!u.thread) return;
  const conv = chat.conv;
  if (!conv || !conv.messages.length) { u.thread.replaceChildren(emptyState()); u.thread.classList.add("is-empty"); return; }
  u.thread.classList.remove("is-empty");
  u.thread.replaceChildren(...conv.messages.map((m, i) => messageNode(m, i)));
}

function emptyState() {
  if (!chat.models.length) {
    return getModel("chat", "Your next idea starts here", "Choose a model that fits your machine, then chat, write or explore. Your conversations stay here.", "empty chat-empty");
  }
  const c = caps();
  const sub = c.audio && c.vision ? "It can look at images and listen to audio too. Everything runs on this machine."
    : c.audio ? "It can listen too: record a question or attach a clip. Everything runs on this machine."
    : c.vision ? "It can see too: drop or paste an image. Everything runs on this machine."
    : "Everything runs on this machine. Nothing leaves it.";
  const chips = SUGGESTIONS.map((s) => {
    const b = el("button", { type: "button", className: "suggestion", textContent: s });
    b.onclick = () => { chat.ui.input.value = s; autosize(); updateSend(); send(); };
    return b;
  });
  return el("div", { className: "chat-empty" },
    el("img", { className: "brand-mark big", src: "/assets/mark.svg", alt: "" }),
    el("h2", { textContent: `Chat with ${displayName(chat.model)}` }),
    el("p", { textContent: sub }),
    el("div", { className: "suggestions" }, ...chips));
}

function attachmentsNode(refs) {
  if (!refs?.length) return null;
  return el("div", { className: "msg-atts" }, ...refs.map((name) => /\.(png|jpg|webp)$/.test(name)
    ? el("a", { href: "/files/refs/" + name, target: "_blank", className: "msg-img" }, el("img", { src: "/files/refs/" + name, alt: "", loading: "lazy" }))
    : el("audio", { controls: true, src: "/files/refs/" + name, preload: "metadata" })));
}

function actionBtn(name, title, fn) {
  const b = el("button", { type: "button", className: "msg-act", title }, icon(name));
  b.onclick = fn;
  return b;
}

function messageNode(m, i) {
  const conv = chat.conv;
  const last = i === conv.messages.length - 1;
  if (m.role === "user") {
    if (chat.editing === i) return editNode(m, i);
    const bubble = el("div", { className: "bubble" }, attachmentsNode(m.refs), m.content ? el("div", { className: "bubble-text", textContent: m.content }) : null);
    const acts = el("div", { className: "msg-acts" },
      actionBtn("copy", "Copy", () => copy(m.content, "Copied")),
      actionBtn("edit", "Edit and resend", () => { if (chat.stream) return; chat.editing = i; renderThread(); }));
    return el("div", { className: "msg user" }, bubble, acts);
  }
  const node = el("div", { className: "msg assistant" });
  node.dataset.index = i;
  fillAssistant(node, m, last && chat.stream && chat.stream.msg === m);
  return node;
}

function fillAssistant(node, m, live) {
  const parts = [];
  if (m.reasoning) {
    const thinking = live && !m.content;
    const label = thinking ? "Thinking" : m.thought ? `Thought for ${fmtDuration(m.thought)}` : "Thoughts";
    const details = el("details", { className: "think" + (thinking ? " live" : "") },
      el("summary", {}, icon("bulb"), el("span", { textContent: label }), icon("chev")),
      el("div", { className: "think-body", textContent: m.reasoning }));
    details.open = thinking || !!openThoughts.get(m);
    if (!live) details.addEventListener("toggle", () => openThoughts.set(m, details.open));
    parts.push(details);
  }
  const body = el("div", { className: "md" });
  if (m.content) body.append(markdown(m.content));
  if (live && m.content) placeCaret(body);
  if (live && !m.content && !m.reasoning) {
    body.append(el("div", { className: "pending" }, el("i"), el("i"), el("i"),
      chat.stream.loading ? el("span", { textContent: `Loading ${displayName(m.model)}…` }) : null));
  }
  parts.push(body);
  if (m.error) {
    const retry = el("button", { type: "button", className: "text-btn", textContent: "Try again" });
    retry.onclick = regenerate;
    const copyErr = el("button", { type: "button", className: "text-btn", textContent: "Copy error" });
    copyErr.onclick = () => copy(m.error, "Error copied");
    parts.push(el("div", { className: "msg-error" }, el("div", { className: "msg-error-head" }, icon("alert"), el("span", { textContent: m.content || m.reasoning ? "Reply interrupted" : "The model didn't answer" })),
      el("pre", { className: "mono", textContent: m.error }), el("div", { className: "job-actions" }, retry, copyErr)));
  }
  if (!live) {
    const conv = chat.conv;
    const isLast = conv.messages[conv.messages.length - 1] === m;
    const meta = [];
    if (m.stopped) meta.push("Stopped");
    if (m.perSecond) meta.push(`${m.perSecond.toFixed(1)} tok/s`);
    if (m.tokens) meta.push(`${m.tokens} tokens`);
    if (m.model) meta.push(displayName(m.model));
    const acts = el("div", { className: "msg-acts" });
    if (m.content) acts.append(actionBtn("copy", "Copy", () => copy(m.content, "Copied")));
    if (isLast && !m.error) acts.append(actionBtn("redo", "Regenerate", regenerate));
    parts.push(el("div", { className: "msg-foot" }, el("span", { className: "msg-meta", textContent: meta.join(" · ") }), acts));
  }
  node.replaceChildren(...parts);
}

const openThoughts = new WeakMap();

function placeCaret(body) {
  let target = body.lastElementChild;
  while (target && (target.tagName === "UL" || target.tagName === "OL" || target.tagName === "BLOCKQUOTE") && target.lastElementChild) target = target.lastElementChild;
  const caret = el("span", { className: "caret" });
  if (target && /^(P|LI|H\d)$/.test(target.tagName)) target.append(caret);
  else body.append(caret);
}

function editNode(m, i) {
  const input = el("textarea", { className: "edit-input", value: m.content });
  const cancel = el("button", { type: "button", className: "text-btn", textContent: "Cancel" });
  const save = el("button", { type: "button", className: "text-btn primary", textContent: "Send" });
  cancel.onclick = () => { chat.editing = -1; renderThread(); };
  save.onclick = () => {
    const text = input.value.trim();
    if (!text && !m.refs?.length) return;
    chat.editing = -1;
    chat.conv.messages = chat.conv.messages.slice(0, i);
    chat.conv.messages.push({ role: "user", content: text, refs: m.refs });
    if (i === 0) chat.conv.title = titleFrom(text, m.refs);
    renderTitle();
    run();
  };
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); save.click(); }
    if (e.key === "Escape") { e.stopPropagation(); cancel.click(); }
  });
  requestAnimationFrame(() => {
    input.style.height = "auto";
    input.style.height = Math.min(input.scrollHeight + 2, 320) + "px";
    input.focus();
    input.setSelectionRange(input.value.length, input.value.length);
  });
  input.addEventListener("input", () => { input.style.height = "auto"; input.style.height = Math.min(input.scrollHeight + 2, 320) + "px"; });
  return el("div", { className: "msg user editing" }, attachmentsNode(m.refs), input, el("div", { className: "edit-acts" }, cancel, save));
}

function scrollToEnd(force) {
  const s = chat.ui.scroll;
  if (force || chat.stick) {
    s.scrollTop = s.scrollHeight;
    chat.stick = true;
    chat.ui.jump.hidden = true;
  }
}

// ---------- sending and streaming ----------

function send() {
  if (chat.stream || chat.recording) return;
  const u = chat.ui;
  const text = u.input.value.trim();
  if (chat.pending.some((p) => p.uploading)) { toast("Still uploading the attachment"); return; }
  const refs = chat.pending.map((p) => p.name).filter(Boolean);
  if (!text && !refs.length) return;
  if (!chat.model) { toast("Download a chat model first"); return; }
  const conv = chat.conv;
  conv.messages.push(refs.length ? { role: "user", content: text, refs } : { role: "user", content: text });
  if (!conv.title) conv.title = titleFrom(text, refs);
  u.input.value = "";
  store.set("chatDraft", "");
  chat.pending.forEach((p) => URL.revokeObjectURL(p.preview));
  chat.pending = [];
  renderAttachments();
  autosize();
  renderTitle();
  run();
}

function regenerate() {
  if (chat.stream) return;
  const msgs = chat.conv.messages;
  while (msgs.length && msgs[msgs.length - 1].role === "assistant") msgs.pop();
  if (!msgs.length) return;
  run();
}

function stopStream() {
  if (!chat.stream) return;
  chat.stream.stopped = true;
  chat.stream.controller.abort();
  return chat.stream.finished;
}

async function run() {
  const conv = chat.conv;
  conv.model = chat.model;
  const history = conv.messages
    .filter((m) => !(m.role === "assistant" && (m.error || !m.content)))
    .map((m) => (m.refs?.length ? { role: m.role, content: m.content || "", refs: m.refs } : { role: m.role, content: m.content || "" }));
  const p = chat.params;
  if (p.system.trim()) history.unshift({ role: "system", content: p.system.trim() });

  const msg = { role: "assistant", content: "", reasoning: "", model: chat.model };
  conv.messages.push(msg);
  const controller = new AbortController();
  const stream = { controller, conv, msg, loading: !(chat.status.model === chat.model && chat.status.state === "ready"), started: performance.now(), raw: "" };
  let finish;
  stream.finished = new Promise((resolve) => { finish = resolve; });
  chat.stream = stream;
  chat.stick = true;
  renderThread();
  renderTools();
  scrollToEnd(true);
  saveConv();
  const autosave = setInterval(() => { if (!chatSaves.has(conv.id)) saveConv(conv); }, 3000);

  let frame = 0;
  const node = () => chat.ui.thread.querySelector(`.msg.assistant[data-index="${conv.messages.indexOf(msg)}"]`);
  const paint = () => {
    frame = 0;
    if (chat.conv !== conv) return;
    const n = node();
    if (n) fillAssistant(n, msg, true);
    scrollToEnd(false);
  };
  const schedule = () => { if (!frame) frame = requestAnimationFrame(paint); };

  let firstToken = 0, thinkStart = 0, deltas = 0;
  const onChunk = (chunk) => {
    if (chunk.timings?.predicted_per_second) msg.perSecond = chunk.timings.predicted_per_second;
    if (chunk.usage?.completion_tokens) msg.tokens = chunk.usage.completion_tokens;
    for (const choice of chunk.choices || []) {
      const d = choice.delta || {};
      if (d.reasoning_content) {
        if (!thinkStart) thinkStart = performance.now();
        msg.reasoning += d.reasoning_content;
        deltas++;
      }
      if (d.content) {
        if (!firstToken) firstToken = performance.now();
        if (thinkStart && !msg.thought) msg.thought = (performance.now() - thinkStart) / 1000;
        stream.raw += d.content;
        deltas++;
        splitThink(msg, stream.raw, !!thinkStart);
      }
    }
    schedule();
  };

  let reader;
  try {
    const body = { model: chat.model, messages: history, temperature: p.temperature, top_p: p.top_p, max_tokens: p.max_tokens, think: chat.think };
    const res = await fetch("/api/chat", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body), signal: controller.signal });
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
    let buf = "", completed = false;
    while (!completed) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += value;
      if (buf.length > 16 * 1024 * 1024) throw new Error("The reply stream sent an oversized event.");
      let separator;
      while ((separator = buf.match(/\r?\n\r?\n/))) {
        const block = buf.slice(0, separator.index);
        buf = buf.slice(separator.index + separator[0].length);
        let event = "message";
        const lines = [];
        for (const line of block.split(/\r?\n/)) {
          if (line.startsWith("event:")) event = line.slice(6).trim();
          else if (line.startsWith("data:")) lines.push(line.slice(5).replace(/^ /, ""));
        }
        const data = lines.join("\n");
        if (!data) continue;
        if (event === "status") {
          const st = JSON.parse(data);
          stream.loading = st.state === "loading";
          applyStatus(st);
          schedule();
        } else if (event === "error") {
          throw new Error(JSON.parse(data).message);
        } else if (data === "[DONE]") { completed = true; break; }
        else onChunk(JSON.parse(data));
      }
    }
    if (!completed) throw new Error("The reply stream ended before completion. Retry to generate a complete reply.");
    if (!msg.content && !msg.reasoning) throw new Error("The model returned an empty reply.");
  } catch (err) {
    if (stream.stopped) msg.stopped = true;
    else msg.error = err.message || String(err);
  } finally {
    clearInterval(autosave);
    if (reader) { try { await reader.cancel(); } catch {} reader.releaseLock(); }
    if (frame) cancelAnimationFrame(frame);
    if (thinkStart && !msg.thought) msg.thought = (performance.now() - thinkStart) / 1000;
    if (!msg.perSecond && firstToken && deltas > 4) {
      const seconds = (performance.now() - (thinkStart || firstToken)) / 1000;
      if (seconds > 0) msg.perSecond = deltas / seconds;
    }
    if (!msg.tokens && deltas && msg.stopped) msg.tokens = deltas;
    if (!msg.reasoning) delete msg.reasoning;
    if (chat.stream === stream) chat.stream = null;
    if (chat.conv === conv) { renderThread(); renderTools(); scrollToEnd(false); }
    refreshStatus();
    saveConv(conv);
    finish();
  }
}

// Models whose server does not split out thinking send it inline as <think>…</think>.
function splitThink(msg, raw, split) {
  const m = split ? null : raw.match(/^\s*<think>([\s\S]*?)(<\/think>\s*|$)/);
  if (!m) { msg.content = raw; return; }
  msg.reasoning = m[1].trim();
  msg.content = m[2] ? raw.slice(m[0].length) : "";
}

// ---------- wiring ----------

Studio.register("chat", {
  mount,
  models: setModels,
  selectModel(id) {
    if (chat.stream) { toast("Stop the reply first"); return; }
    pickModel(id);
    newChat(true);
  },
  show() {
    if (!chat.mounted) return;
    refreshStatus();
    autosize();
    if (!chat.stream) setTimeout(() => chat.ui.input.focus(), 0);
    document.title = "fornax studio";
  },
  hide() {
    if (chat.recording) stopRecording(true);
    closePopovers();
  },
});
})();
