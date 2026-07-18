"use strict";

// History page: a single-series line chart of a device's amperage over the last
// N minutes, fetched from /api/history. Single series → no legend (the title
// names it); recessive grid/axes; a crosshair + tooltip on hover.

const params = new URLSearchParams(location.search);
const deviceId = params.get("device");
const MINUTES = 24; // baseline window; will grow later
const REFRESH_MS = 15000; // matches the snapshot cadence

const canvas = document.getElementById("chart");
const ctx = canvas.getContext("2d");
const titleEl = document.getElementById("history-title");
const emptyEl = document.getElementById("chart-empty");

const rootStyle = getComputedStyle(document.documentElement);
const cssVar = (name, fallback) => rootStyle.getPropertyValue(name).trim() || fallback;
const COLOR_LINE = cssVar("--flow", "#4cc2ff");
const COLOR_TEXT = cssVar("--text", "#e6edf3");
const COLOR_MUTED = cssVar("--muted", "#8b98a9");
const COLOR_GRID = "rgba(255, 255, 255, 0.08)";

let data = null; // { name, unit, minutes, points: [{t, v}] }
let plotted = []; // screen coords of non-null points, for hover hit-testing
let geom = null;
let hoverIndex = -1;

const fmtHM = (ms) => new Date(ms).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
const fmtHMS = (ms) => new Date(ms).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });

async function load() {
    if (!deviceId) {
        titleEl.textContent = "History — no device selected";
        return;
    }

    try {
        const resp = await fetch(`/api/history?device=${encodeURIComponent(deviceId)}&minutes=${MINUTES}`, {
            cache: "no-store",
        });
        if (!resp.ok) {
            throw new Error(`HTTP ${resp.status}`);
        }
        data = await resp.json();
    } catch (err) {
        titleEl.textContent = `History — failed to load (${err.message})`;
        return;
    }

    titleEl.textContent = `${data.name} — Amperage (last ${data.minutes} min)`;
    draw();
}

function resize() {
    const dpr = window.devicePixelRatio || 1;
    const rect = canvas.getBoundingClientRect();
    canvas.width = Math.round(rect.width * dpr);
    canvas.height = Math.round(rect.height * dpr);
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    draw();
}

function roundRect(x, y, w, h, r) {
    ctx.beginPath();
    ctx.moveTo(x + r, y);
    ctx.arcTo(x + w, y, x + w, y + h, r);
    ctx.arcTo(x + w, y + h, x, y + h, r);
    ctx.arcTo(x, y + h, x, y, r);
    ctx.arcTo(x, y, x + w, y, r);
    ctx.closePath();
}

function draw() {
    if (!data) {
        return;
    }

    const rect = canvas.getBoundingClientRect();
    const W = rect.width;
    const H = rect.height;
    ctx.clearRect(0, 0, W, H);

    const pts = (data.points || []).map((p) => ({ t: Date.parse(p.t), v: p.v }));
    const hasData = pts.some((p) => p.v !== null && p.v !== undefined);
    emptyEl.hidden = hasData;
    if (!hasData) {
        plotted = [];
        return;
    }

    const m = { l: 52, r: 16, t: 14, b: 28 };
    const plotW = W - m.l - m.r;
    const plotH = H - m.t - m.b;

    // X domain: the requested window ending now (so it slides live).
    const t1 = Date.now();
    const t0 = t1 - data.minutes * 60 * 1000;

    // Y domain from the data, always including 0 as a baseline, with padding.
    let vmin = Infinity;
    let vmax = -Infinity;
    for (const p of pts) {
        if (p.v === null || p.v === undefined) {
            continue;
        }
        vmin = Math.min(vmin, p.v);
        vmax = Math.max(vmax, p.v);
    }
    vmin = Math.min(0, vmin);
    vmax = Math.max(0, vmax);
    if (vmin === vmax) {
        vmax = vmin + 1;
    }
    const pad = (vmax - vmin) * 0.1 || 1;
    vmax += pad;
    if (vmin < 0) {
        vmin -= pad;
    }

    const xOf = (t) => m.l + ((t - t0) / (t1 - t0)) * plotW;
    const yOf = (v) => m.t + (1 - (v - vmin) / (vmax - vmin)) * plotH;
    geom = { m, plotW, plotH, W, H };

    // Grid + Y labels.
    ctx.font = "11px system-ui, -apple-system, sans-serif";
    ctx.textBaseline = "middle";
    ctx.lineWidth = 1;
    const yTicks = 4;
    for (let i = 0; i <= yTicks; i++) {
        const v = vmin + ((vmax - vmin) * i) / yTicks;
        const y = yOf(v);
        ctx.strokeStyle = COLOR_GRID;
        ctx.beginPath();
        ctx.moveTo(m.l, y);
        ctx.lineTo(m.l + plotW, y);
        ctx.stroke();
        ctx.fillStyle = COLOR_MUTED;
        ctx.textAlign = "right";
        ctx.fillText(v.toFixed(1), m.l - 8, y);
    }

    // X labels.
    ctx.textBaseline = "top";
    ctx.textAlign = "center";
    ctx.fillStyle = COLOR_MUTED;
    const xTicks = 4;
    for (let i = 0; i <= xTicks; i++) {
        const t = t0 + ((t1 - t0) * i) / xTicks;
        ctx.fillText(fmtHM(t), xOf(t), m.t + plotH + 8);
    }

    // Zero baseline (only if the range crosses 0).
    if (vmin < 0 && vmax > 0) {
        ctx.strokeStyle = "rgba(255, 255, 255, 0.16)";
        ctx.beginPath();
        ctx.moveTo(m.l, yOf(0));
        ctx.lineTo(m.l + plotW, yOf(0));
        ctx.stroke();
    }

    // The line, broken at gaps (null = device offline at that snapshot).
    plotted = [];
    ctx.strokeStyle = COLOR_LINE;
    ctx.lineWidth = 2;
    ctx.lineJoin = "round";
    ctx.lineCap = "round";
    ctx.beginPath();
    let drawing = false;
    for (const p of pts) {
        if (p.v === null || p.v === undefined) {
            drawing = false;
            continue;
        }
        const x = xOf(p.t);
        const y = yOf(p.v);
        plotted.push({ x, y, t: p.t, v: p.v });
        if (drawing) {
            ctx.lineTo(x, y);
        } else {
            ctx.moveTo(x, y);
            drawing = true;
        }
    }
    ctx.stroke();

    if (hoverIndex >= 0 && hoverIndex < plotted.length) {
        drawHover(plotted[hoverIndex]);
    }
}

function drawHover(pt) {
    const { m, plotH, W } = geom;

    ctx.strokeStyle = "rgba(255, 255, 255, 0.25)";
    ctx.lineWidth = 1;
    ctx.beginPath();
    ctx.moveTo(pt.x, m.t);
    ctx.lineTo(pt.x, m.t + plotH);
    ctx.stroke();

    ctx.fillStyle = COLOR_LINE;
    ctx.beginPath();
    ctx.arc(pt.x, pt.y, 4, 0, Math.PI * 2);
    ctx.fill();
    ctx.strokeStyle = "rgba(0, 0, 0, 0.6)";
    ctx.lineWidth = 2;
    ctx.stroke();

    const valueText = `${pt.v.toFixed(2)} ${data.unit}`;
    const timeText = fmtHMS(pt.t);
    ctx.font = "12px system-ui, -apple-system, sans-serif";
    const boxW = Math.max(ctx.measureText(valueText).width, ctx.measureText(timeText).width) + 16;
    const boxH = 38;
    let bx = pt.x + 12;
    if (bx + boxW > W - 4) {
        bx = pt.x - 12 - boxW;
    }
    let by = pt.y - boxH - 10;
    if (by < m.t) {
        by = pt.y + 10;
    }

    ctx.fillStyle = "rgba(14, 17, 22, 0.92)";
    ctx.strokeStyle = "rgba(255, 255, 255, 0.12)";
    ctx.lineWidth = 1;
    roundRect(bx, by, boxW, boxH, 6);
    ctx.fill();
    ctx.stroke();

    ctx.textAlign = "left";
    ctx.textBaseline = "top";
    ctx.fillStyle = COLOR_TEXT;
    ctx.fillText(valueText, bx + 8, by + 7);
    ctx.fillStyle = COLOR_MUTED;
    ctx.fillText(timeText, bx + 8, by + 21);
}

canvas.addEventListener("mousemove", (e) => {
    if (!plotted.length) {
        return;
    }
    const rect = canvas.getBoundingClientRect();
    const mx = e.clientX - rect.left;
    let best = -1;
    let bestDist = Infinity;
    for (let i = 0; i < plotted.length; i++) {
        const d = Math.abs(plotted[i].x - mx);
        if (d < bestDist) {
            bestDist = d;
            best = i;
        }
    }
    if (best !== hoverIndex) {
        hoverIndex = best;
        draw();
    }
});

canvas.addEventListener("mouseleave", () => {
    if (hoverIndex !== -1) {
        hoverIndex = -1;
        draw();
    }
});

window.addEventListener("resize", resize);
resize(); // size the canvas
load();
setInterval(load, REFRESH_MS);
