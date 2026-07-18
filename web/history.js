"use strict";

// History page: one line chart per metric the device reports (amperage,
// voltage, wattage, and SOC where applicable), all sharing a time range chosen
// with the five pickers. Data is bucketed server-side by the selected time unit.

const params = new URLSearchParams(location.search);
const deviceId = params.get("device");

const els = {
    title: document.getElementById("history-title"),
    unit: document.getElementById("unit"),
    startDate: document.getElementById("start-date"),
    startTime: document.getElementById("start-time"),
    endDate: document.getElementById("end-date"),
    endTime: document.getElementById("end-time"),
    tiles: document.getElementById("tiles"),
    charts: document.getElementById("charts"),
};

// Render the headline "max over range" tiles (yield / peak power at fine units).
function renderTiles(tiles) {
    els.tiles.textContent = "";
    if (!tiles || !tiles.length) {
        els.tiles.hidden = true;
        return;
    }
    els.tiles.hidden = false;
    for (const t of tiles) {
        const value =
            t.value === null || t.value === undefined
                ? "—"
                : `${Number.isInteger(t.value) ? t.value : t.value.toFixed(2)} ${t.unit}`;
        const tile = document.createElement("div");
        tile.className = "tile";
        const v = document.createElement("div");
        v.className = "tile__value";
        v.textContent = value;
        const label = document.createElement("div");
        label.className = "tile__label";
        label.textContent = t.label;
        tile.append(v, label);
        els.tiles.appendChild(tile);
    }
}

const rootStyle = getComputedStyle(document.documentElement);
const cssVar = (name, fallback) => rootStyle.getPropertyValue(name).trim() || fallback;
const COLOR_LINE = cssVar("--flow", "#4cc2ff");
const COLOR_TEXT = cssVar("--text", "#e6edf3");
const COLOR_MUTED = cssVar("--muted", "#8b98a9");
const COLOR_GRID = "rgba(255, 255, 255, 0.08)";

const charts = new Map(); // series key -> chart instance, in display order

// ---- date/time controls ---------------------------------------------------

const pad = (n) => String(n).padStart(2, "0");
const toDateValue = (d) => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
const toTimeValue = (d) => `${pad(d.getHours())}:${pad(d.getMinutes())}`;

function setDefaults() {
    const now = new Date();
    const start = new Date(now.getTime() - 24 * 3600 * 1000); // yesterday, this time
    els.unit.value = "hours";
    els.startDate.value = toDateValue(start);
    els.startTime.value = toTimeValue(start);
    els.endDate.value = toDateValue(now);
    els.endTime.value = toTimeValue(now);
}

// Build a local Date from a date input and a time input.
function localDateTime(dateStr, timeStr) {
    if (!dateStr || !timeStr) {
        return null;
    }
    const [y, mo, da] = dateStr.split("-").map(Number);
    const [h, mi] = timeStr.split(":").map(Number);
    return new Date(y, mo - 1, da, h, mi, 0, 0);
}

// Combine the (local) date and time inputs into an RFC3339 UTC string without
// fractional seconds, matching the stored timestamps.
function isoFrom(dateStr, timeStr) {
    const d = localDateTime(dateStr, timeStr);
    return d ? d.toISOString().replace(/\.\d{3}Z$/, "Z") : null;
}

// When the time unit changes, reset the start pickers to a sensible span before
// the end: a wider window for coarser units. Date arithmetic handles day/month
// rollover (e.g. Minutes crossing midnight) on its own.
function adjustStartForUnit() {
    const end = localDateTime(els.endDate.value, els.endTime.value);
    if (!end) {
        return;
    }
    const start = new Date(end);
    switch (els.unit.value) {
        case "months":
            start.setMonth(start.getMonth() - 6);
            break;
        case "weeks":
            start.setDate(start.getDate() - 28);
            break;
        case "days":
            start.setDate(start.getDate() - 7);
            break;
        case "hours":
            start.setDate(start.getDate() - 1);
            break;
        case "minutes":
            start.setMinutes(start.getMinutes() - 60);
            break;
    }
    els.startDate.value = toDateValue(start);
    els.startTime.value = toTimeValue(start);
}

function xLabel(ms, spanMs) {
    const d = new Date(ms);
    if (spanMs <= 36 * 3600 * 1000) {
        return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    }
    if (spanMs <= 60 * 24 * 3600 * 1000) {
        return d.toLocaleDateString([], { month: "2-digit", day: "2-digit" });
    }
    return d.toLocaleDateString([], { year: "2-digit", month: "2-digit" });
}

// niceNum / niceScale: round an axis range to human-friendly numbers (steps of
// 1, 2, or 5 × a power of ten) so ticks read as e.g. 24/26/28/30 rather than
// 25.56/23.4/… (Heckbert's "nice numbers for graph labels").
function niceNum(range, round) {
    if (range <= 0) {
        return 1;
    }
    const exp = Math.floor(Math.log10(range));
    const frac = range / Math.pow(10, exp);
    let nice;
    if (round) {
        nice = frac < 1.5 ? 1 : frac < 3 ? 2 : frac < 7 ? 5 : 10;
    } else {
        nice = frac <= 1 ? 1 : frac <= 2 ? 2 : frac <= 5 ? 5 : 10;
    }
    return nice * Math.pow(10, exp);
}

function niceScale(lo, hi, maxTicks) {
    const step = niceNum(niceNum(hi - lo, false) / maxTicks, true);
    return {
        min: Math.floor(lo / step) * step,
        max: Math.ceil(hi / step) * step,
        step,
    };
}

// ---- chart instance -------------------------------------------------------

function makeChart() {
    const block = document.createElement("div");
    block.className = "chart-block";
    const heading = document.createElement("h3");
    heading.className = "chart-block__title";
    const wrap = document.createElement("div");
    wrap.className = "chart-wrap";
    const canvas = document.createElement("canvas");
    const empty = document.createElement("p");
    empty.className = "empty";
    empty.hidden = true;
    empty.textContent = "No data in this range.";
    wrap.append(canvas, empty);
    block.append(heading, wrap);

    const ctx = canvas.getContext("2d");

    // Match the canvas backing store to its displayed size × DPR, so the browser
    // doesn't stretch a low-res bitmap (which blurs the text and lines). Drawing
    // then happens in CSS pixels via the DPR transform.
    function sizeCanvas() {
        const dpr = window.devicePixelRatio || 1;
        const rect = canvas.getBoundingClientRect();
        if (!rect.width || !rect.height) {
            return;
        }
        canvas.width = Math.round(rect.width * dpr);
        canvas.height = Math.round(rect.height * dpr);
        ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    }

    let points = []; // {t (ms), v}
    let unitLabel = "";
    let labels = null; // enum code(string) -> name; null for numeric series
    let start = 0;
    let end = 0;
    let plotted = [];
    let hoverIndex = -1;
    let geom = null;
    let fmtValue = (v) => String(v); // tooltip/label formatter, set per draw

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
        const rect = canvas.getBoundingClientRect();
        const W = rect.width;
        const H = rect.height;
        ctx.clearRect(0, 0, W, H);

        const hasData = points.some((p) => p.v !== null && p.v !== undefined);
        empty.hidden = hasData;
        if (!hasData) {
            plotted = [];
            return;
        }

        const t0 = start;
        const t1 = end;
        const span = Math.max(1, t1 - t0);

        // Build the Y scale. Enum series use a categorical axis (named ticks,
        // even spacing, stepped line); numeric series use a nice-number scale.
        // yTicks hold a fraction (0 = bottom, 1 = top) and text; fracOf maps a
        // data value to that fraction.
        let yTicks;
        let fracOf;
        let stepped = false;
        let crossesZero = false;

        if (labels) {
            const codes = Object.keys(labels)
                .map(Number)
                .sort((a, b) => a - b);
            const lo = -0.5;
            const hi = codes.length - 1 + 0.5;
            fracOf = (code) => (codes.indexOf(code) - lo) / (hi - lo);
            yTicks = codes.map((c) => ({ frac: fracOf(c), text: labels[String(c)] || String(c) }));
            stepped = true;
            fmtValue = (code) => labels[String(code)] || String(code);
        } else {
            let dataMin = Infinity;
            let dataMax = -Infinity;
            for (const p of points) {
                if (p.v === null || p.v === undefined) {
                    continue;
                }
                dataMin = Math.min(dataMin, p.v);
                dataMax = Math.max(dataMax, p.v);
            }
            // Frame to the data (not forced to zero) with headroom, then round to
            // nice numbers so the axis reads cleanly and a flat line isn't pinned.
            let lo;
            let hi;
            if (dataMin === dataMax) {
                const p = Math.max(Math.abs(dataMin) * 0.1, 0.5);
                lo = dataMin - p;
                hi = dataMax + p;
            } else {
                const p = (dataMax - dataMin) * 0.1;
                lo = dataMin - p;
                hi = dataMax + p;
            }
            const scale = niceScale(lo, hi, 4);
            const vmin = scale.min;
            const vmax = scale.max;
            const vdecimals = Math.max(0, -Math.floor(Math.log10(scale.step)));
            fracOf = (v) => (v - vmin) / (vmax - vmin);
            yTicks = [];
            const nt = Math.round((vmax - vmin) / scale.step);
            for (let i = 0; i <= nt; i++) {
                const v = vmin + i * scale.step;
                yTicks.push({ frac: fracOf(v), text: v.toFixed(vdecimals) });
            }
            crossesZero = vmin < 0 && vmax > 0;
            fmtValue = (v) => (unitLabel ? `${v.toFixed(2)} ${unitLabel}` : `${v}`);
        }

        // Left margin sized to the widest Y label (enum names can be long).
        ctx.font = "11px system-ui, -apple-system, sans-serif";
        let maxLabelW = 0;
        for (const tk of yTicks) {
            maxLabelW = Math.max(maxLabelW, ctx.measureText(tk.text).width);
        }
        const m = { l: Math.max(44, Math.ceil(maxLabelW) + 14), r: 16, t: 12, b: 26 };
        const plotW = W - m.l - m.r;
        const plotH = H - m.t - m.b;

        const xOf = (t) => m.l + ((t - t0) / span) * plotW;
        const yOf = (frac) => m.t + (1 - frac) * plotH;
        geom = { m, plotW, plotH, W };

        // Y grid + labels.
        ctx.lineWidth = 1;
        ctx.textBaseline = "middle";
        ctx.textAlign = "right";
        for (const tk of yTicks) {
            const y = yOf(tk.frac);
            ctx.strokeStyle = COLOR_GRID;
            ctx.beginPath();
            ctx.moveTo(m.l, y);
            ctx.lineTo(m.l + plotW, y);
            ctx.stroke();
            ctx.fillStyle = COLOR_MUTED;
            ctx.fillText(tk.text, m.l - 8, y);
        }

        // X labels.
        ctx.textBaseline = "top";
        ctx.textAlign = "center";
        ctx.fillStyle = COLOR_MUTED;
        for (let i = 0; i <= 4; i++) {
            const t = t0 + (span * i) / 4;
            ctx.fillText(xLabel(t, span), xOf(t), m.t + plotH + 7);
        }

        // Zero baseline (numeric only, when the range crosses it).
        if (crossesZero) {
            ctx.strokeStyle = "rgba(255, 255, 255, 0.16)";
            ctx.beginPath();
            ctx.moveTo(m.l, yOf(fracOf(0)));
            ctx.lineTo(m.l + plotW, yOf(fracOf(0)));
            ctx.stroke();
        }

        // Line, broken at gaps; stepped for enums (a category holds until it
        // changes) and straight for numeric series.
        plotted = [];
        ctx.strokeStyle = COLOR_LINE;
        ctx.lineWidth = 2;
        ctx.lineJoin = "round";
        ctx.lineCap = "round";
        ctx.beginPath();
        let drawing = false;
        let prevY = 0;
        for (const p of points) {
            if (p.v === null || p.v === undefined) {
                drawing = false;
                continue;
            }
            const x = xOf(p.t);
            const y = yOf(fracOf(p.v));
            plotted.push({ x, y, t: p.t, v: p.v });
            if (!drawing) {
                ctx.moveTo(x, y);
                drawing = true;
            } else if (stepped) {
                ctx.lineTo(x, prevY);
                ctx.lineTo(x, y);
            } else {
                ctx.lineTo(x, y);
            }
            prevY = y;
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

        const valueText = fmtValue(pt.v);
        const timeText = new Date(pt.t).toLocaleString([], {
            month: "short",
            day: "numeric",
            hour: "2-digit",
            minute: "2-digit",
        });
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

    function render() {
        sizeCanvas();
        draw();
    }

    return {
        el: block,
        resize: render,
        setData(newPoints, meta) {
            points = newPoints;
            unitLabel = meta.unit;
            labels = meta.labels || null;
            start = meta.start;
            end = meta.end;
            heading.textContent = meta.unit ? `${meta.label} (${meta.unit})` : meta.label;
            hoverIndex = -1;
            render();
        },
    };
}

// Create/remove/reorder chart blocks to match the returned series.
function syncCharts(series) {
    const keys = new Set(series.map((s) => s.key));
    for (const [key, chart] of charts) {
        if (!keys.has(key)) {
            chart.el.remove();
            charts.delete(key);
        }
    }
    for (const s of series) {
        if (!charts.has(s.key)) {
            charts.set(s.key, makeChart());
        }
        els.charts.appendChild(charts.get(s.key).el); // (re)append in series order
    }
}

async function refresh() {
    if (!deviceId) {
        els.title.textContent = "History — no device selected";
        return;
    }

    const url =
        `/api/history?device=${encodeURIComponent(deviceId)}` +
        `&unit=${encodeURIComponent(els.unit.value)}` +
        `&start=${encodeURIComponent(isoFrom(els.startDate.value, els.startTime.value) || "")}` +
        `&end=${encodeURIComponent(isoFrom(els.endDate.value, els.endTime.value) || "")}`;

    let data;
    try {
        const resp = await fetch(url, { cache: "no-store" });
        if (!resp.ok) {
            throw new Error((await resp.text()).trim() || `HTTP ${resp.status}`);
        }
        data = await resp.json();
    } catch (err) {
        els.title.textContent = `History — failed to load (${err.message})`;
        return;
    }

    els.title.textContent = `${data.name} — History`;
    renderTiles(data.tiles);
    syncCharts(data.series);

    const startMs = Date.parse(data.start);
    const endMs = Date.parse(data.end);
    for (const s of data.series) {
        const pts = data.buckets.map((b) => ({ t: Date.parse(b.t), v: b[s.key] }));
        charts.get(s.key).setData(pts, {
            label: s.label,
            unit: s.unit,
            start: startMs,
            end: endMs,
            labels: s.labels,
        });
    }
}

els.unit.addEventListener("change", () => {
    adjustStartForUnit();
    refresh();
});
for (const el of [els.startDate, els.startTime, els.endDate, els.endTime]) {
    el.addEventListener("change", refresh);
}
window.addEventListener("resize", () => {
    for (const chart of charts.values()) {
        chart.resize();
    }
});

setDefaults();
refresh();
