"use strict";
// printer-web front end: upload, preview, options, confirm, progress, queue, history.
(function () {
  const t = I18N.t, tp = I18N.tp;
  const $ = (s) => document.querySelector(s);
  const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

  const S = {
    cfg: { name: "printer-web", location: "", publicURL: location.origin, lang: "auto", showNames: false },
    printer: null, // last /api/printer answer
    caps: {}, // last known good capabilities
    doc: null, // {id, name, kind, pages, pdf, landscape, orientKnown, previewFailed, file}
    uploading: false,
    notice: "",
    o: { color: true, sides: "one", orient: "auto", quality: "standard", copies: 1, pages: "all", spec: "", paper: "", tray: "" },
    est: null, // {key, sheets, cost, currency}
    jobs: { queue: [], history: [] },
    qOpen: false
  };
  const pending = new Set();
  const thumbs = new Map(); // page number -> canvas | false (failed)
  let docToken = 0;
  let keyFor = { id: null, est: "", key: "" }; // idempotency key per draft and options
  let submitting = false;
  let lastPrint = null; // {file, name, pages, options, job} of the last submitted job, for "Try again"

  // ---------- helpers ----------
  const API_TIMEOUT = 8000;
  async function api(method, url, body) {
    const init = { method: method, headers: {} };
    if (method !== "GET") init.headers["X-Requested-With"] = "printer-web";
    let timer = 0;
    if (body instanceof FormData) init.body = body; // uploads: no timeout
    else {
      const ctl = new AbortController();
      init.signal = ctl.signal;
      timer = setTimeout(() => ctl.abort(), API_TIMEOUT);
      if (body !== undefined) { init.headers["Content-Type"] = "application/json"; init.body = JSON.stringify(body); }
    }
    let r, data = null;
    try {
      r = await fetch(url, init);
      if (r.status !== 204) { try { data = await r.json(); } catch (e) { data = null; } }
    } catch (e) { throw { key: "network" }; }
    finally { clearTimeout(timer); }
    if (!r.ok) throw { key: (data && data.error) || "internal", status: r.status };
    return data;
  }

  // crypto.randomUUID only exists in secure contexts; the app is served over plain http.
  function newKey() {
    if (window.crypto && typeof crypto.randomUUID === "function") return crypto.randomUUID();
    const b = new Uint8Array(16);
    crypto.getRandomValues(b);
    return Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
  }

  function fmtCost(cost, cur) {
    if (typeof cost !== "number") return "";
    let s;
    if (/^[A-Za-z]{3}$/.test(cur || "")) {
      try { s = new Intl.NumberFormat(I18N.lang, { style: "currency", currency: cur.toUpperCase() }).format(cost); } catch (e) { s = ""; }
    }
    if (!s) s = new Intl.NumberFormat(I18N.lang, { minimumFractionDigits: 2, maximumFractionDigits: 2 }).format(cost) + (cur ? " " + cur : "");
    return "≈ " + s;
  }

  function sheetsFor(pages, copies, sides) {
    const per = sides === "one" ? pages : Math.ceil(pages / 2);
    return per * copies;
  }

  function fmtWhen(iso) {
    const d = new Date(iso);
    if (isNaN(d)) return "";
    const now = new Date(), day = 86400000;
    const hm = d.toLocaleTimeString(I18N.lang, { hour: "2-digit", minute: "2-digit" });
    const startOf = (x) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
    const diff = Math.round((startOf(now) - startOf(d)) / day);
    if (diff === 0) return t("today") + ", " + hm;
    if (diff === 1) return t("yesterday") + ", " + hm;
    if (diff > 1 && diff < 7) return d.toLocaleDateString(I18N.lang, { weekday: "long" });
    return d.toLocaleDateString(I18N.lang, { day: "numeric", month: "short" });
  }

  function toast(msg) {
    const el = $("#toast");
    el.textContent = msg;
    el.hidden = false;
    clearTimeout(toast.t);
    toast.t = setTimeout(() => { el.hidden = true; }, 5000);
  }

  // ---------- printer status ----------
  const CRITICAL = ["media-jam", "door-open", "media-empty", "media-needed", "toner-empty", "output-area-full", "input-tray-missing", "offline", "paused", "shutdown"];
  const BLOCKING = ["media-empty", "media-jam", "door-open"];
  const baseReason = (r) => String(r).replace(/-(error|warning|report)$/, "");
  const known = (r) => t("reason." + r) !== "reason." + r;
  function reasons() {
    return ((S.printer && S.printer.reasons) || []).map(baseReason).filter(known);
  }
  function criticalReason() { const rs = reasons(); return CRITICAL.find((c) => rs.includes(c)) || ""; }
  function blockingReason() { const rs = reasons(); return BLOCKING.find((c) => rs.includes(c)) || ""; }
  // help text for a reason, with the paper in use (e.g. "Add A4 sheets")
  function reasonHelp(r) { return t("reason." + r + ".help", { paper: S.o.paper || (S.caps.papers || [])[0] || "A4" }); }

  let printerBusy = false, printerAgain = false, printerSeq = 0;
  async function refreshPrinter() {
    if (printerBusy) { printerAgain = true; return; }
    printerBusy = true;
    const seq = ++printerSeq;
    try {
      await refreshPrinterOnce(seq);
    } finally {
      printerBusy = false;
      if (printerAgain) { printerAgain = false; refreshPrinter(); }
    }
  }
  async function refreshPrinterOnce(seq) {
    try {
      const p = await api("GET", "/api/printer");
      if (seq !== printerSeq) return; // stale
      S.printer = p;
      // an unreachable printer reports empty capabilities: keep the last good ones
      if (p.state !== "unreachable") S.caps = p.caps || {};
    } catch (e) {
      if (seq !== printerSeq) return;
      S.printer = { state: "unreachable", reasons: [], supplies: [], caps: S.caps };
    }
    renderPrinter();
    applyCaps();
    render();
  }

  function renderPrinter() {
    const p = S.printer, st = $("#status");
    $("#pname").textContent = S.cfg.name || "printer-web";
    document.title = S.cfg.name || "printer-web";
    const model = [S.caps.model, S.cfg.location].filter(Boolean).join(" · ");
    $("#pmodel").textContent = model;
    if (!p) { st.dataset.state = "idle"; $("#statusTxt").textContent = t("st_loading"); $("#statusSub").textContent = ""; return; }
    const crit = criticalReason();
    const trayTxt = [(S.caps.trays || [])[0], (S.caps.papers || [])[0]].filter(Boolean).join(" · ");
    let state = "ok", txt = t("st_ready"), sub = trayTxt;
    if (p.state === "unreachable") { state = "bad"; txt = t("st_unreachable"); sub = ""; }
    else if (crit) { state = "warn"; txt = t("reason." + crit); sub = trayTxt; }
    else if (p.state === "stopped") { state = "warn"; txt = t("st_stopped"); }
    else if (p.state === "printing") { state = "busy"; txt = t("st_printing"); }
    else if (reasons().includes("toner-low")) { state = "warn"; sub = t("reason.toner-low"); }
    st.dataset.state = state;
    $("#statusTxt").textContent = txt;
    $("#statusSub").textContent = sub;

    // toner levels in the share card
    const sup = (p.supplies || []).filter((s) => s.level >= 0);
    $("#tonerBox").hidden = sup.length === 0;
    $("#toner").innerHTML = sup.map((s) => {
      const col = supplyColor(s);
      return '<div><span>' + esc(s.name) + '</span><span class="t"><i style="width:' + level(s) + "%;background:" + col + '"></i></span><span>' + level(s) + " %</span></div>";
    }).join("");
  }

  const level = (s) => Math.max(0, Math.min(100, Number(s.level) || 0));
  function supplyColor(s) {
    const c = String(s.color || "").toLowerCase();
    if (/^#[0-9a-f]{3,8}$/.test(c)) return c;
    const n = (c + " " + String(s.name).toLowerCase());
    if (/cyan/.test(n)) return "var(--c)";
    if (/magenta/.test(n)) return "var(--m)";
    if (/yellow|jaune/.test(n)) return "var(--y)";
    if (/black|noir/.test(n)) return "var(--k)";
    return "var(--accent)";
  }

  // show only the options the printer supports; fill papers and trays
  let capsKey = "", prevColor = false;
  function applyCaps() {
    const c = S.caps;
    const key = JSON.stringify([c.color, c.duplex, c.quality, c.papers, c.trays]);
    if (key === capsKey) return;
    capsKey = key;
    if (c.color && !prevColor) S.o.color = true;
    prevColor = !!c.color;
    $("#optColor").hidden = !c.color;
    if (!c.color) S.o.color = false;
    $("#optSides").hidden = !c.duplex;
    if (!c.duplex) S.o.sides = "one";
    $("#optQuality").hidden = !c.quality;
    const papers = c.papers || [], trays = c.trays || [];
    const fill = (sel, items, withAuto) => {
      const cur = sel.value;
      sel.innerHTML = (withAuto ? '<option value="">' + esc(t("tray_auto")) + "</option>" : "") + items.map((x) => '<option value="' + esc(x) + '">' + esc(x) + "</option>").join("");
      if ([...sel.options].some((o) => o.value === cur)) sel.value = cur;
    };
    fill($("#paper"), papers, false);
    fill($("#tray"), trays, true);
    S.o.paper = $("#paper").value || papers[0] || "";
    S.o.tray = $("#tray").value;
    $("#optPaper").hidden = papers.length < 2;
    $("#optTray").hidden = trays.length === 0;
    $("#more").hidden = !c.quality && papers.length < 2 && trays.length === 0;
  }

  // ---------- options and summary ----------
  function selectedPages() {
    const n = S.doc ? S.doc.pages : 0;
    if (S.o.pages === "all") return { list: [...Array(n).keys()].map((i) => i + 1) };
    const spec = S.o.spec.trim();
    if (!spec) return { err: t("pages_empty") };
    const out = new Set();
    for (const raw of spec.split(",")) {
      const part = raw.trim();
      const m = part.match(/^(\d+)(?:\s*-\s*(\d*))?$/);
      if (!m) return { err: t("pages_unknown", { p: part }) };
      const a = +m[1], b = m[2] === undefined ? a : (m[2] === "" ? n : +m[2]);
      if (a < 1 || b < a) return { err: t("pages_reversed", { p: part }) };
      if (b > n) return { err: t("pages_toomany", { n: n }) };
      for (let i = a; i <= b; i++) out.add(i);
    }
    return { list: [...out].sort((x, y) => x - y) };
  }
  const pagesParam = () => (S.o.pages === "all" ? "" : S.o.spec.trim());
  // "auto" becomes the first page's orientation when the preview could read it
  function orientJSON() {
    if (S.o.orient !== "auto" || !S.doc || !S.doc.orientKnown) return S.o.orient;
    return S.doc.landscape ? "landscape" : "portrait";
  }
  function optsJSON() {
    const o = S.o, c = S.caps;
    return { copies: o.copies, sides: c.duplex ? o.sides : "one", color: c.color ? o.color : false, paper: o.paper || "", orientation: orientJSON(), quality: c.quality ? o.quality : "", tray: o.tray || "" };
  }
  function effectiveLandscape() {
    if (S.o.orient === "landscape") return true;
    if (S.o.orient === "portrait") return false;
    return !!(S.doc && S.doc.landscape);
  }
  const estKey = () => JSON.stringify([S.doc && S.doc.id, pagesParam(), optsJSON()]);
  const mono = () => !(S.caps.color && S.o.color);

  let estTimer = 0, estSeq = 0;
  function scheduleEstimate() {
    clearTimeout(estTimer);
    if (!S.doc || selectedPages().err) return;
    estTimer = setTimeout(doEstimate, 400);
  }
  async function doEstimate() {
    clearTimeout(estTimer);
    if (!S.doc || selectedPages().err) return;
    const seq = ++estSeq, key = estKey(), id = S.doc.id;
    try {
      const r = await api("POST", "/api/estimate", { upload: id, pages: pagesParam(), options: optsJSON() });
      if (seq !== estSeq || !S.doc || S.doc.id !== id) return;
      S.est = { key: key, sheets: r.sheets, cost: r.cost, currency: r.currency };
      render();
    } catch (e) {
      if (e.key === "expired" || e.key === "not_found") { if (seq === estSeq && S.doc && S.doc.id === id) handleExpired(e.key); }
    }
  }

  function handleExpired(key) {
    closeSheet();
    dropDoc();
    S.notice = I18N.errMsg(key || "expired");
    render();
  }
  function dropDoc() {
    docToken++;
    estSeq++;
    clearTimeout(estTimer);
    if (S.doc && S.doc.pdf) { try { S.doc.pdf.destroy(); } catch (e) { /* ignore */ } }
    S.doc = null; S.est = null;
    thumbs.clear();
    pending.clear();
    keyFor = { id: null, est: "", key: "" };
    S.o.pages = "all"; S.o.spec = ""; $("#pagesInput").value = "";
  }

  // ---------- render ----------
  function render() {
    // segmented controls
    document.querySelectorAll(".seg").forEach((seg) => {
      const k = seg.dataset.k;
      seg.querySelectorAll("button").forEach((b) => b.setAttribute("aria-pressed", String(b.dataset.v === (k === "color" ? (S.o.color ? "color" : "mono") : S.o[k]))));
    });
    $("#file").disabled = S.uploading;
    $("#copies").textContent = S.o.copies;
    $("#allPages").setAttribute("aria-pressed", String(S.o.pages === "all"));
    $("#somePages").setAttribute("aria-pressed", String(S.o.pages === "some"));
    $("#pagesInput").hidden = S.o.pages !== "some";

    const d = S.doc;
    $("#fname").textContent = d ? d.name : "";
    $("#fsub").textContent = d ? t("kind." + (["pdf", "ps", "image", "heic", "text"].includes(d.kind) ? d.kind : "unknown")) + " · " + tp("pages_count", d.pages) : "";
    $("#chooseLbl").textContent = d ? t("change_file") : t("choose_file");
    const nt = $("#notice");
    nt.hidden = !S.notice;
    nt.textContent = S.notice;
    $("#hint").textContent = d && d.previewFailed ? t("preview_failed") : t("hint");

    const sel = selectedPages();
    const err = $("#pagesErr");
    err.hidden = !d || !sel.err;
    err.textContent = d && sel.err ? sel.err : "";
    renderStage(sel);

    const bar = $("#printBtn");
    const list = sel.list || [];
    if (!d) {
      $("#sumTitle").textContent = t("sum_none");
      $("#sumSub").textContent = t("sum_none_sub");
      bar.disabled = true;
      return;
    }
    const est = S.est && S.est.key === estKey() ? S.est : null;
    const sh = est ? est.sheets : sheetsFor(list.length, S.o.copies, S.o.sides);
    const cost = est && est.cost !== undefined ? fmtCost(est.cost, est.currency) : "";
    $("#sumTitle").textContent = sel.err ? t("sum_fix") : tp("sheets", sh) + (cost ? " · " + cost : "");
    const parts = [mono() ? t("mono_l") : t("color_l"), t("side_" + S.o.sides), effectiveLandscape() ? t("landscape_l") : t("portrait_l")];
    if (S.o.copies > 1) parts.push(tp("copies_count", S.o.copies));
    $("#sumSub").textContent = parts.join(" · ");
    bar.disabled = !!sel.err || S.uploading;
  }

  function renderStage(sel) {
    const box = $("#pages"), stage = $("#stage");
    box.textContent = "";
    if (S.uploading) {
      box.innerHTML = '<div class="busy" role="status"><span class="spinner" aria-hidden="true"></span>' + esc(t("uploading")) + "</div>";
      return;
    }
    if (!S.doc) {
      box.innerHTML = '<div class="empty"><div class="glyph" aria-hidden="true">+</div><h2>' + esc(t("empty_title")) + "</h2><p>" + esc(t("empty_sub")) +
        '</p><label class="btn primary" for="file">' + esc(t("choose_file")) + "</label></div>";
      return;
    }
    const land = effectiveLandscape(), m = mono(), duplex = S.o.sides !== "one";
    const list = sel.list && sel.list.length ? sel.list : [1];
    const show = list.slice(0, duplex ? 4 : 3);
    show.forEach((p, i) => {
      const el = document.createElement("div");
      el.className = "page " + (land ? "landscape" : "portrait") + (m ? " mono" : "") + (duplex && i % 2 ? " back" : "");
      el.dataset.back = t("back");
      const c = thumbs.get(p);
      if (c) { c.setAttribute("role", "img"); c.setAttribute("aria-label", t("page_alt", { n: p })); el.appendChild(c); }
      else { const ph = document.createElement("span"); ph.className = "ph"; ph.textContent = c === false ? "" : "…"; el.appendChild(ph); if (c === undefined) loadThumb(p); }
      const n = document.createElement("span");
      n.className = "num"; n.textContent = p; el.appendChild(n);
      box.appendChild(el);
    });
    if (list.length > show.length) {
      const more = document.createElement("div");
      more.className = "hint"; more.textContent = t("more_pages", { n: list.length - show.length });
      box.appendChild(more);
    }
  }

  async function loadThumb(p) {
    const d = S.doc;
    if (!d || !d.pdf || pending.has(p)) return;
    pending.add(p);
    const tok = docToken;
    try {
      const pg = await d.pdf.getPage(p);
      const vp0 = pg.getViewport({ scale: 1 });
      const vp = pg.getViewport({ scale: Math.min(3, 420 / Math.max(vp0.width, 1)) });
      const c = document.createElement("canvas");
      c.width = Math.ceil(vp.width); c.height = Math.ceil(vp.height);
      await pg.render({ canvasContext: c.getContext("2d"), viewport: vp }).promise;
      pg.cleanup();
      if (tok !== docToken) return;
      thumbs.set(p, c);
    } catch (e) {
      if (tok !== docToken) return;
      thumbs.set(p, false);
      d.previewFailed = true;
    } finally { pending.delete(p); }
    if (tok === docToken) render();
  }

  // ---------- upload / draft ----------
  async function openPdf(url) {
    const lib = window.pdfjsLib;
    if (!lib) throw new Error("pdf.js missing");
    lib.GlobalWorkerOptions.workerSrc = "vendor/pdf.worker.min.js";
    return lib.getDocument({ url: url, isEvalSupported: false }).promise;
  }

  async function adoptDraft(info) {
    const old = S.doc;
    dropDoc();
    if (old && old.id !== info.id) api("DELETE", "/api/uploads/" + encodeURIComponent(old.id)).catch(() => {});
    const tok = docToken;
    const d = { id: info.id, name: info.name, kind: info.kind, pages: info.pages || 1, pdf: null, landscape: false, orientKnown: false, previewFailed: false, file: info.file || null };
    S.doc = d;
    try {
      const pdf = await openPdf("/api/uploads/" + encodeURIComponent(info.id) + "/pdf");
      if (tok !== docToken) { pdf.destroy(); return false; }
      d.pdf = pdf;
      if (!info.pages) d.pages = pdf.numPages;
      const first = await pdf.getPage(1), vp = first.getViewport({ scale: 1 });
      d.landscape = vp.width > vp.height;
      d.orientKnown = vp.width !== vp.height;
      first.cleanup();
    } catch (e) {
      if (tok !== docToken) return false;
      if (!info.pages) { dropDoc(); S.notice = I18N.errMsg("expired"); return false; }
      d.previewFailed = true;
    }
    S.notice = "";
    render();
    scheduleEstimate();
    return true;
  }

  async function chooseFile(f) {
    if (!f || S.uploading) return;
    S.notice = ""; S.uploading = true; render();
    const fd = new FormData();
    fd.append("file", f, f.name);
    try {
      const r = await api("POST", "/api/uploads", fd);
      S.uploading = false;
      await adoptDraft(Object.assign({}, r, { file: f }));
    } catch (e) {
      S.notice = I18N.errMsg(e.key);
    }
    S.uploading = false;
    render();
  }

  // ---------- sheets ----------
  let opener = null;
  const scrim = $("#scrim"), dlg = $("#dialog");
  let progress = null; // {id, sheets, timer}
  function showSheet(html) {
    if (scrim.hidden) opener = document.activeElement;
    dlg.innerHTML = html;
    scrim.hidden = false;
    const f = dlg.querySelector("button");
    if (f) f.focus();
  }
  function closeSheet() {
    if (progress) { clearTimeout(progress.timer); progress = null; }
    if (scrim.hidden) return;
    scrim.hidden = true;
    dlg.innerHTML = "";
    if (opener && document.contains(opener)) opener.focus();
    refreshJobs();
  }
  scrim.addEventListener("click", (e) => { if (e.target === scrim && !submitting) closeSheet(); });
  document.addEventListener("keydown", (e) => {
    if (scrim.hidden) return;
    if (e.key === "Escape") { if (!submitting) closeSheet(); return; }
    if (e.key === "Tab") { // keep focus inside the sheet
      const fs = [...dlg.querySelectorAll("button:not([disabled]):not([hidden]),a[href],select,input")];
      if (!fs.length) { e.preventDefault(); return; }
      const first = fs[0], last = fs[fs.length - 1];
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
    }
  });
  const sheetClose = (id) => { const b = $("#" + id); if (b) b.onclick = closeSheet; };

  function openPrinter() {
    const p = S.printer || { state: "unreachable", reasons: [], supplies: [] }, c = S.caps;
    let title = t("ps_ready");
    if (p.state === "unreachable") title = t("ps_unreachable");
    else if (p.state === "stopped") title = t("ps_stopped");
    else if (p.state === "printing") title = t("ps_printing");
    const crit = criticalReason();
    if (crit && p.state !== "unreachable") title = t("reason." + crit);
    const rows = [];
    const add = (k, v) => { if (v) rows.push("<dt>" + esc(k) + "</dt><dd>" + esc(v) + "</dd>"); };
    add(t("ps_colormode"), c.color ? t("ps_color_yes") : t("ps_color_no"));
    add(t("ps_duplex"), c.duplex ? t("ps_yes") : t("ps_no"));
    add(t("ps_papers"), (c.papers || []).join(", "));
    add(t("ps_trays"), (c.trays || []).join(", "));
    add(t("ps_dpi"), c.dpi ? c.dpi + " dpi" : "");
    if (c.ppm) add(t("ps_speed"), t("ps_speed_v", { n: c.ppm }) + (c.ppmColor ? " (" + t("ps_speed_c", { n: c.ppmColor }) + ")" : ""));
    const sup = (p.supplies || []);
    const supHtml = sup.length ? '<div class="label">' + esc(t("supplies")) + '</div><div class="sup">' + sup.map((s) =>
      "<div><span>" + esc(s.name) + '</span><span class="t"><i style="width:' + level(s) + "%;background:" + supplyColor(s) + '"></i></span><span>' + (s.level >= 0 ? level(s) + " %" : "?") + "</span></div>").join("") + "</div>" : "";
    const where = [c.model, S.cfg.location].filter(Boolean).join(", ");
    const help = crit ? '<div class="alert warn">' + esc(reasonHelp(crit)) + "</div>" : (p.state === "unreachable" ? '<div class="alert">' + esc(t("err.printer_unreachable")) + "</div>" : "");
    showSheet('<h2 id="dlgTitle">' + esc(title) + "</h2><p>" + esc(where || S.cfg.name) + "</p>" +
      '<dl class="recap">' + rows.join("") + '<dt>' + esc(t("ps_address")) + '</dt><dd class="addr">' + esc(S.cfg.publicURL) + "</dd></dl>" + supHtml + help +
      '<div class="actions" style="margin-top:14px"><button type="button" class="btn" id="ok">' + esc(t("close")) + "</button></div>");
    sheetClose("ok");
  }

  function showBlocked(r) {
    showSheet('<h2 id="dlgTitle">' + esc(t("reason." + r)) + "</h2><p>" + esc(t("reason_blocked")) + '</p><div class="alert">' + esc(reasonHelp(r)) +
      '</div><div class="actions" style="margin-top:16px"><button type="button" class="btn" id="ok">' + esc(t("understood")) + "</button></div>");
    sheetClose("ok");
  }

  async function onPrint() {
    if (!S.doc || selectedPages().err || S.uploading) return;
    $("#printBtn").disabled = true;
    await doEstimate();
    render();
    if (!S.doc) return; // expired meanwhile
    const b = blockingReason();
    if (b) { showBlocked(b); return; }
    openConfirm();
  }

  function openConfirm() {
    const d = S.doc, sel = selectedPages(), o = optsJSON();
    const est = S.est && S.est.key === estKey() ? S.est : null;
    const sh = est ? est.sheets : sheetsFor(sel.list.length, S.o.copies, S.o.sides);
    const cost = est && est.cost !== undefined ? fmtCost(est.cost, est.currency) : "";
    const total = tp("sheets", sh) + (cost ? " · " + cost : "");
    // a new key when the draft, the pages or the options change
    if (keyFor.id !== d.id || keyFor.est !== estKey()) keyFor = { id: d.id, est: estKey(), key: newKey() };
    const key = keyFor.key;
    const rows = [
      [t("r_file"), d.name],
      [t("r_pages"), S.o.pages === "all" ? t("r_pages_all", { n: sel.list.length }) : S.o.spec.trim()],
      [t("r_color"), mono() ? t("mono") : t("color")],
      [t("r_sides"), o.sides === "one" ? t("one") : t("sides_" + o.sides)],
      [t("r_orient"), effectiveLandscape() ? t("orient_landscape") : t("orient_portrait")],
      [t("r_copies"), String(S.o.copies)]
    ];
    if (S.caps.papers && S.caps.papers.length > 1 && o.paper) rows.push([t("r_paper"), o.paper]);
    rows.push([t("r_total"), total]);
    const warn = S.printer && S.printer.state === "unreachable" ? '<div class="alert">' + esc(t("printer_warn")) + "</div>" : "";
    showSheet('<h2 id="dlgTitle">' + esc(t("confirm_title", { sheets: tp("sheets", sh) })) + "</h2><p>" + esc(t("confirm_sub")) + "</p>" +
      '<dl class="recap">' + rows.map((r) => "<dt>" + esc(r[0]) + '</dt><dd style="overflow-wrap:anywhere">' + esc(r[1]) + "</dd>").join("") + "</dl>" + warn +
      '<div class="alert bad" id="cerr" role="alert" hidden></div>' +
      '<div class="actions"><button type="button" class="btn" id="cancel">' + esc(t("modify")) + '</button><button type="button" class="print" id="go">' + esc(t("go")) + "</button></div>");
    sheetClose("cancel");
    const go = $("#go");
    go.onclick = async () => {
      if (submitting) return;
      submitting = true;
      go.disabled = true; // one tap only
      $("#cancel").disabled = true;
      go.textContent = t("sending");
      $("#cerr").hidden = true;
      try {
        const pages = pagesParam();
        const job = await api("POST", "/api/jobs", { upload: d.id, key: key, pages: pages, options: o });
        if (!job || typeof job.id !== "string" || !job.id) throw { key: "internal" };
        submitting = false;
        lastPrint = d.file ? { file: d.file, name: d.name, pages: pages, options: o, job: job.id } : null;
        // the draft now belongs to the job: back to the empty state
        dropDoc(); S.notice = ""; render();
        openProgress(job);
      } catch (e) {
        submitting = false;
        if (e.key === "expired" || e.key === "not_found") { handleExpired(e.key); return; }
        const ce = $("#cerr");
        if (!ce) return;
        ce.textContent = I18N.errMsg(e.key); ce.hidden = false;
        go.disabled = false; go.textContent = t("retry");
        $("#cancel").disabled = false;
        go.focus();
      }
    };
  }

  // ---------- progress sheet ----------
  const STEP_OF = { queued: 0, preparing: 0, sending: 1, printing: 2 };
  function openProgress(job) {
    showSheet('<div class="live" id="prog" aria-live="polite" aria-atomic="true"></div><div class="actions"><button type="button" class="btn" id="ok"></button>' +
      '<button type="button" class="print" id="again" hidden></button></div>');
    sheetClose("ok");
    $("#again").onclick = retryPrint;
    progress = { id: job.id, sheets: job.sheets, timer: 0, misses: 0, done: false };
    paintProgress(job);
    pollJob(1500);
  }
  function pollJob(delay) {
    const cur = progress;
    if (!cur || cur.done) return;
    clearTimeout(cur.timer);
    cur.timer = setTimeout(async () => {
      if (progress !== cur) return;
      if (document.hidden) { pollJob(1500); return; } // resumes on visibilitychange
      try {
        const j = await api("GET", "/api/jobs/" + encodeURIComponent(cur.id));
        if (progress !== cur) return;
        cur.misses = 0;
        paintProgress(j);
        if (["done", "sent", "failed", "canceled"].includes(j.state)) { cur.done = true; refreshJobs(); return; }
      } catch (e) {
        if (progress !== cur) return;
        // the job may not be visible for a moment: retry twice before giving up
        if (e.key === "not_found" && ++cur.misses > 2) { cur.done = true; paintProgress(null); return; }
      }
      pollJob(1500);
    }, delay);
  }
  function paintProgress(j) {
    const prog = $("#prog"), ok = $("#ok");
    if (!prog || !ok) return;
    let title = "";
    const sheets = tp("sheets", (j && j.sheets) || (progress && progress.sheets) || 1);
    const steps = [t("step_prepare"), t("step_send"), t("step_print")];
    const stepList = (i) => '<div class="progress">' + steps.map((s, k) => '<div class="stepl ' + (k < i ? "done" : k === i ? "on" : "") + '"><i aria-hidden="true">' + (k < i ? "✓" : "") + "</i>" + esc(s) + "</div>").join("") + "</div>";
    let h = "", okLabel = t("close"), okCls = "btn";
    if (!j) {
      title = t("failed_title");
      h = '<div class="alert bad">' + esc(t("lost_job")) + "</div>";
    } else if (j.state in STEP_OF) {
      title = j.state === "queued" ? t("prog_wait_title") : t("prog_title");
      h = "<p>" + esc(t("prog_sub")) + "</p>" + (j.state === "queued" && j.position > 1 ? "<p>" + esc(t("prog_queued", { n: j.position - 1 })) + "</p>" : "") + stepList(STEP_OF[j.state]);
    } else if (j.state === "done" || j.state === "sent") {
      title = j.state === "done" ? t("done_title") : t("sent_title");
      h = "<p>" + esc(t(j.state === "done" ? "done_sub" : "sent_sub", { sheets: sheets })) + '</p><div class="tray"><div class="out"></div></div>';
      okLabel = t("finish"); okCls = "print";
    } else if (j.state === "canceled") {
      title = t("canceled_title");
      h = "<p>" + esc(j.error ? I18N.errMsg(j.error) : t("canceled_sub")) + "</p>";
    } else { // failed
      title = t("failed_title");
      const b = criticalReason();
      let msg = I18N.errMsg(j.error || "internal");
      if (b && (j.error === "printer_error" || j.error === "printer_unreachable" || !j.error)) msg = t("reason." + b) + ". " + reasonHelp(b);
      h = '<div class="alert bad">' + esc(msg) + "</div>";
      if (canRetry(j)) h += "<p>" + esc(t("retry_hint")) + "</p>";
    }
    const again = $("#again");
    if (again && !again.dataset.busy) {
      again.hidden = !canRetry(j);
      if (again.textContent !== t("retry_print")) again.textContent = t("retry_print");
    }
    // the title lives inside the live region so screen readers announce it; skip no-op writes
    const html = '<h2 id="dlgTitle">' + esc(title) + "</h2>" + h;
    if (prog.dataset.html !== html) { prog.innerHTML = html; prog.dataset.html = html; }
    if (ok.textContent !== okLabel) ok.textContent = okLabel;
    if (ok.className !== okCls) ok.className = okCls;
  }

  // a printer problem can be retried while we still have the file
  function canRetry(j) {
    return !!(j && j.state === "failed" && lastPrint && progress && lastPrint.job === progress.id &&
      (j.error === "printer_unreachable" || j.error === "printer_error"));
  }

  // "Try again": upload the same file again and print it with the same
  // options and a new key. The tap on the button is the confirmation.
  async function retryPrint() {
    const lp = lastPrint, again = $("#again");
    if (!lp || !again || submitting) return;
    submitting = true;
    again.disabled = true; again.dataset.busy = "1"; again.textContent = t("retrying");
    try {
      const fd = new FormData();
      fd.append("file", lp.file, lp.name);
      const up = await api("POST", "/api/uploads", fd);
      const job = await api("POST", "/api/jobs", { upload: up.id, key: newKey(), pages: lp.pages, options: lp.options });
      if (!job || typeof job.id !== "string" || !job.id) throw { key: "internal" };
      submitting = false;
      lp.job = job.id;
      openProgress(job);
    } catch (e) {
      submitting = false;
      if (!document.contains(again)) return;
      delete again.dataset.busy;
      again.disabled = false; again.textContent = t("retry_print");
      toast(I18N.errMsg(e.key));
      again.focus();
    }
  }

  // ---------- queue and history ----------
  let lastQ = "", lastH = "";
  let jobsBusy = false, jobsAgain = false, jobsSeq = 0;
  async function refreshJobs() {
    if (jobsBusy) { jobsAgain = true; return; }
    jobsBusy = true;
    const seq = ++jobsSeq;
    try {
      const r = await api("GET", "/api/jobs");
      if (seq === jobsSeq) {
        S.jobs = { queue: r.queue || [], history: r.history || [] };
        renderQueue();
        renderHistory();
      }
    } catch (e) { /* keep the last list */ }
    finally {
      jobsBusy = false;
      if (jobsAgain) { jobsAgain = false; refreshJobs(); }
    }
  }

  function renderQueue() {
    const q = S.jobs.queue, mine = q.filter((j) => j.mine);
    let sum;
    if (mine.length) {
      const m = mine[0], before = Math.max(0, (m.position || 1) - 1);
      if (m.state !== "queued") sum = t("queue_yours_now");
      else sum = before > 0 ? tp("queue_before", before) : t("queue_yours_next");
    } else sum = q.length ? tp("queue_others", q.length) : t("queue_none");
    if ($("#qsum").textContent !== sum) $("#qsum").textContent = sum;
    const tg = $("#qToggle");
    tg.hidden = q.length === 0;
    tg.textContent = S.qOpen ? t("queue_hide") : t("queue_all");
    tg.setAttribute("aria-expanded", String(S.qOpen && q.length > 0));
    const ul = $("#qlist");
    ul.hidden = !(S.qOpen && q.length > 0);
    const html = q.map((j) => {
      const name = j.name || t("document");
      return '<li><span class="pos" aria-hidden="true">' + (j.position || "") + '</span><span class="t"><b>' + esc(name) + (j.mine ? ' <span class="badge">' + esc(t("mine")) + "</span>" : "") + "</b><span>" +
        esc(tp("sheets", j.sheets) + " · " + t("jobstate." + j.state)) + (j.position ? " · " + esc(t("position", { n: j.position })) : "") + "</span></span>" +
        (j.mine && j.state === "queued" ? '<button type="button" class="btn small" data-cancel="' + esc(j.id) + '">' + esc(t("cancel_job")) + "</button>" : "") + "</li>";
    }).join("");
    if (html !== lastQ) { ul.innerHTML = html; lastQ = html; }
  }

  function renderHistory() {
    const h = S.jobs.history.slice(0, 10);
    $("#histEmpty").hidden = h.length > 0;
    const html = h.map((j) => {
      const name = j.name || t("document");
      const ext = (name.includes(".") ? name.split(".").pop() : "").slice(0, 4).toUpperCase();
      const ok = j.state === "done" || j.state === "sent";
      return '<li><span class="ic" aria-hidden="true">' + esc(ext || "–") + '</span><span class="t"><b>' + esc(name) + "</b><span>" +
        esc(fmtWhen(j.finished || j.created) + " · " + tp("sheets", j.sheets)) + '</span></span><span class="pill ' + (ok ? "" : "bad") + '">' + esc(t("pill." + (j.state === "done" || j.state === "sent" || j.state === "failed" || j.state === "canceled" ? j.state : "active"))) + "</span></li>";
    }).join("");
    if (html !== lastH) { $("#hist").innerHTML = html; lastH = html; }
  }

  async function cancelJob(id, btn) {
    btn.disabled = true;
    try { await api("DELETE", "/api/jobs/" + encodeURIComponent(id)); toast(t("canceled_ok")); }
    catch (e) { toast(e.key === "not_active" ? t("cancel_failed") : I18N.errMsg(e.key)); }
    refreshJobs();
  }

  // ---------- events ----------
  document.querySelectorAll(".seg").forEach((seg) => seg.addEventListener("click", (e) => {
    const b = e.target.closest("button");
    if (!b) return;
    const k = seg.dataset.k;
    if (k === "color") S.o.color = b.dataset.v === "color";
    else S.o[k] = b.dataset.v;
    render(); scheduleEstimate();
  }));
  $("#minus").onclick = () => { S.o.copies = Math.max(1, S.o.copies - 1); render(); scheduleEstimate(); };
  $("#plus").onclick = () => { S.o.copies = Math.min(99, S.o.copies + 1); render(); scheduleEstimate(); };
  $("#allPages").onclick = () => { S.o.pages = "all"; render(); scheduleEstimate(); };
  $("#somePages").onclick = () => { S.o.pages = "some"; render(); $("#pagesInput").focus(); scheduleEstimate(); };
  $("#pagesInput").addEventListener("input", (e) => { S.o.spec = e.target.value; render(); scheduleEstimate(); });
  $("#paper").addEventListener("change", (e) => { S.o.paper = e.target.value; scheduleEstimate(); });
  $("#tray").addEventListener("change", (e) => { S.o.tray = e.target.value; scheduleEstimate(); });
  $("#status").onclick = openPrinter;
  $("#printBtn").onclick = onPrint;
  $("#qToggle").onclick = () => { S.qOpen = !S.qOpen; lastQ = ""; renderQueue(); };
  $("#qlist").addEventListener("click", (e) => {
    const b = e.target.closest("button[data-cancel]");
    if (b) cancelJob(b.dataset.cancel, b);
  });
  $("#file").addEventListener("change", (e) => { const f = e.target.files[0]; e.target.value = ""; chooseFile(f); });
  const stage = $("#stage");
  ["dragenter", "dragover"].forEach((ev) => stage.addEventListener(ev, (e) => { e.preventDefault(); stage.classList.add("dragging"); }));
  ["dragleave", "drop"].forEach((ev) => stage.addEventListener(ev, (e) => { e.preventDefault(); stage.classList.remove("dragging"); }));
  stage.addEventListener("drop", (e) => chooseFile(e.dataTransfer.files[0]));

  // ---------- start ----------
  async function init() {
    try { S.cfg = Object.assign(S.cfg, await api("GET", "/api/config")); } catch (e) { /* defaults */ }
    I18N.setLang(I18N.detect(S.cfg.lang));
    I18N.apply();
    $("#addr").textContent = S.cfg.publicURL;
    try {
      new QRCode($("#qr"), { text: S.cfg.publicURL, width: 116, height: 116, colorDark: "#16191f", colorLight: "#ffffff" });
    } catch (e) { $("#qr").textContent = "QR"; }
    renderPrinter();
    render();
    await refreshPrinter();
    refreshJobs();

    const qp = new URLSearchParams(location.search);
    if (qp.get("upload")) {
      const id = qp.get("upload"), name = qp.get("name") || "affiche-qr.png";
      history.replaceState(null, "", location.pathname);
      if (await adoptDraft({ id: id, name: name, kind: qp.get("kind") || "image", pages: 0 })) onPrint();
      else render();
    }

    setInterval(() => { if (!document.hidden) refreshPrinter(); }, 10000);
    setInterval(() => { if (!document.hidden) refreshJobs(); }, 5000);
    document.addEventListener("visibilitychange", () => {
      if (document.hidden) return;
      refreshPrinter(); refreshJobs();
      if (progress && !progress.done) pollJob(0);
    });
  }
  init();
})();
