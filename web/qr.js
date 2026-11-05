"use strict";
// QR poster page: shows the poster and can send it to the normal print flow.
(function () {
  const t = I18N.t;
  const $ = (s) => document.querySelector(s);
  const cfg = { name: "printer-web", location: "", publicURL: location.origin, lang: "auto" };
  let qrCanvas = null;

  function showError(msg) {
    const e = $("#err");
    e.textContent = msg;
    e.hidden = !msg;
  }

  // Draw the A4 poster (150 dpi, 1240x1754) and return it as a PNG blob.
  async function drawPoster() {
    const W = 1240, H = 1754;
    if (document.fonts && document.fonts.load) {
      try { await Promise.all([document.fonts.load('800 80px "Bricolage Grotesque"'), document.fonts.load('700 40px "Atkinson Hyperlegible"')]); } catch (e) { /* system fonts */ }
    }
    const c = document.createElement("canvas");
    c.width = W; c.height = H;
    const g = c.getContext("2d");
    g.fillStyle = "#ffffff"; g.fillRect(0, 0, W, H);
    g.textAlign = "center"; g.textBaseline = "alphabetic";

    // four toner dots
    ["#00a6c8", "#d6247a", "#f2c21b", "#1b1d22"].forEach((col, i) => {
      g.fillStyle = col; g.beginPath(); g.arc(W / 2 - 78 + i * 52, 150, 18, 0, Math.PI * 2); g.fill();
    });
    // name, shrunk to fit
    g.fillStyle = "#16191f";
    let size = 120;
    const fit = (txt, font, maxW, start) => { let s = start; do { g.font = font(s); s -= 4; } while (g.measureText(txt).width > maxW && s > 24); };
    fit(cfg.name, (s) => '800 ' + s + 'px "Bricolage Grotesque", system-ui, sans-serif', W - 200, size);
    g.fillText(cfg.name, W / 2, 330);
    if (cfg.location) {
      g.fillStyle = "#5b6270";
      fit(cfg.location, (s) => '400 ' + s + 'px "Atkinson Hyperlegible", system-ui, sans-serif', W - 240, 52);
      g.fillText(cfg.location, W / 2, 410);
    }
    // QR code
    const qs = 800, qx = (W - qs) / 2, qy = 480;
    g.imageSmoothingEnabled = false;
    g.drawImage(qrCanvas, qx, qy, qs, qs);
    g.imageSmoothingEnabled = true;
    // address, directly under the QR
    g.fillStyle = "#16191f";
    fit(cfg.publicURL, (s) => '700 ' + s + 'px ui-monospace, Menlo, Consolas, monospace', W - 160, 64);
    g.fillText(cfg.publicURL, W / 2, qy + qs + 100);
    // instruction
    fit(t("qr_scan"), (s) => '700 ' + s + 'px "Atkinson Hyperlegible", system-ui, sans-serif', W - 200, 56);
    g.fillText(t("qr_scan"), W / 2, qy + qs + 220);

    return new Promise((res, rej) => c.toBlob((b) => (b ? res(b) : rej(new Error("toBlob"))), "image/png"));
  }

  async function printPoster() {
    const btn = $("#posterBtn");
    btn.disabled = true;
    showError("");
    $("#msg").textContent = t("qr_making");
    try {
      const blob = await drawPoster();
      const fd = new FormData();
      fd.append("file", new File([blob], "affiche-qr.png", { type: "image/png" }));
      const r = await fetch("/api/uploads", { method: "POST", headers: { "X-Requested-With": "printer-web" }, body: fd });
      const j = await r.json().catch(() => ({}));
      if (!r.ok) throw { key: j.error || "internal" };
      // the main page picks the draft up and asks for the usual confirmation
      location.href = "./?upload=" + encodeURIComponent(j.id) + "&name=affiche-qr.png&kind=image";
    } catch (e) {
      showError(I18N.errMsg(e && e.key ? e.key : "network"));
      $("#msg").textContent = t("qr_hint");
      btn.disabled = false;
    }
  }

  async function init() {
    try {
      const r = await fetch("/api/config");
      if (r.ok) Object.assign(cfg, await r.json());
    } catch (e) { /* defaults */ }
    I18N.setLang(I18N.detect(cfg.lang));
    I18N.apply();
    document.title = t("qr_title") + " · " + cfg.name;
    $("#pname").textContent = cfg.name;
    $("#ploc").textContent = cfg.location || "";
    $("#ploc").hidden = !cfg.location;
    $("#addr").textContent = cfg.publicURL;
    try {
      new QRCode($("#qr"), { text: cfg.publicURL, width: 800, height: 800, colorDark: "#000000", colorLight: "#ffffff", correctLevel: QRCode.CorrectLevel.M });
      qrCanvas = $("#qr canvas");
    } catch (e) { qrCanvas = null; }
    if (!qrCanvas) $("#posterBtn").disabled = true;
    $("#posterBtn").onclick = printPoster;
    $("#browserBtn").onclick = () => window.print();
  }
  init();
})();
