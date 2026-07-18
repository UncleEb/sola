"use strict";

// The dashboard is a thin view over the collector's /api/status endpoint. It
// polls on an interval and repaints; all logic that matters lives in Go.

const REFRESH_MS = 1000;

// A reading older than this is treated as stale (the poller may be stuck or the
// device offline). It is deliberately a few poll intervals wide.
const STALE_MS = 20000;

// Flow-wave geometry. The SVG viewBox is 90x24, centred on y=12; amplitude is
// how far the peaks rise above and dip below that centre. Amplitude scales with
// charge current, so a stronger charge draws a taller squiggle and zero current
// draws a flat line.
const WAVE_CENTER = 48;
const WAVE_MAX_AMPLITUDE = 32;
// With no configured max_amperage we cannot scale to the rating, so any charge
// draws a fixed moderate wave instead of nothing.
const WAVE_FALLBACK_FRACTION = 0.6;

const els = {
    connection: document.getElementById("connection"),
    connectionText: document.getElementById("connection-text"),
    charger: document.getElementById("charger"),
    chargerFlow: document.getElementById("charger-flow"),
    aggregate: document.getElementById("aggregate"),
    banks: document.getElementById("banks"),
    lastUpdate: document.getElementById("last-update"),
};

// Demo mode (append ?demo to the URL) overlays synthetic charging data on top
// of the real feed so the charger card can be seen "producing" after dark. It
// never writes to the database and is clearly labelled, so it cannot be
// mistaken for real readings.
const DEMO = new URLSearchParams(location.search).has("demo");

// ---- formatting helpers -------------------------------------------------

// fmt renders a nullable number to a fixed precision, or an em dash when the
// value is null (never read yet).
function fmt(value, digits, unit) {
    if (value === null || value === undefined) {
        return "—";
    }
    return `${value.toFixed(digits)}${unit ? " " + unit : ""}`;
}

function reading(value, digits, unit, label, variant) {
    const cls = variant ? `reading reading--${variant}` : "reading";
    return `
        <div class="${cls}">
            <span class="reading__value">${fmt(value, digits, unit)}</span>
            <span class="reading__label">${label}</span>
        </div>`;
}

// newestTimestamp returns the most recent updated_at across all devices, in ms,
// or null if nothing has ever been read.
function newestTimestamp(data) {
    const stamps = [];
    for (const s of data.shunts || []) {
        if (s.updated_at) stamps.push(Date.parse(s.updated_at));
    }
    if (data.charger && data.charger.updated_at) {
        stamps.push(Date.parse(data.charger.updated_at));
    }
    return stamps.length ? Math.max(...stamps) : null;
}

// ---- rendering ----------------------------------------------------------

// wavePath returns an SVG path for a horizontal squiggle of the given
// amplitude. At amplitude 0 the control points sit on the centre line, so it
// renders as a flat line with no special case for "no flow".
function wavePath(amplitude) {
    let d = `M0 ${WAVE_CENTER} Q7.5 ${WAVE_CENTER - amplitude} 15 ${WAVE_CENTER}`;
    for (let x = 30; x <= 90; x += 15) {
        d += ` T${x} ${WAVE_CENTER}`;
    }
    return d;
}

// flowAmplitude maps the charger's delivered current to a wave amplitude,
// scaled against its rated max_amperage when that is configured.
function flowAmplitude(charger) {
    const current = charger.battery_current || 0;
    if (current <= 0) {
        return 0;
    }

    const max = charger.max_amperage;
    if (max && max > 0) {
        return Math.min(current / max, 1) * WAVE_MAX_AMPLITUDE;
    }

    return WAVE_FALLBACK_FRACTION * WAVE_MAX_AMPLITUDE;
}

// yieldReading renders the daily solar yield, dropping to Wh below 1 kWh so a
// small early-day yield reads as "340 Wh" rather than "0.34 kWh".
function yieldReading(kwh) {
    if (kwh !== null && kwh !== undefined && kwh < 1) {
        return reading(kwh * 1000, 0, "Wh", "Yield today", "battery");
    }

    return reading(kwh, 2, "kWh", "Yield today", "battery");
}

// flowNodes caches the flow indicator's DOM once it is built. Rebuilding it on
// every refresh would recreate the <path> element and restart the CSS
// animation, so the single pulse could never finish its trip; instead we build
// it once and mutate it in place.
let flowNodes = null;

// renderFlow updates the charging indicator (wave + caption) without replacing
// the animating element. Changing the path's "d" or the container class does
// not restart a running animation, so the pulse travels uninterrupted.
function renderFlow(charger) {
    if (!flowNodes) {
        els.chargerFlow.innerHTML = `
            <div class="flow" aria-hidden="true">
                <svg class="flow__wave" viewBox="0 0 90 96" preserveAspectRatio="none">
                    <path d="" />
                </svg>
            </div>`;
        flowNodes = {
            flow: els.chargerFlow.querySelector(".flow"),
            path: els.chargerFlow.querySelector(".flow__wave path"),
        };
    }

    const amplitude = flowAmplitude(charger);
    const flowing = amplitude > 0;

    // The wave itself conveys charge state: animated pulses when charging, a
    // still, dimmed line when not.
    flowNodes.path.setAttribute("d", wavePath(amplitude));
    flowNodes.flow.className = `flow ${flowing ? "flow--active" : "flow--idle"}`;
}

function renderCharger(charger) {
    if (!charger) {
        els.chargerFlow.innerHTML = "";
        flowNodes = null;
        els.charger.className = "charger charger--empty";
        els.charger.innerHTML = `<p class="empty">No charge controller registered.</p>`;
        return;
    }

    renderFlow(charger);

    els.charger.className = "charger";

    els.charger.innerHTML = `
        <div class="flow-side">
            <h3>Solar (PV)</h3>
            <div class="reading-grid">
                ${reading(charger.pv_power, 0, "W", "Power", "pv")}
                ${reading(charger.pv_voltage, 1, "V", "Voltage", "pv")}
                ${reading(charger.pv_current, 1, "A", "Current", "pv")}
            </div>
        </div>
        <div class="flow-side">
            <h3>Battery</h3>
            <div class="reading-grid">
                ${reading(charger.battery_voltage, 2, "V", "Voltage", "battery")}
                ${reading(charger.battery_current, 1, "A", "Current", "battery")}
                ${yieldReading(charger.yield_today)}
            </div>
        </div>
        <div class="charger__state">
            <span>State: <strong>${charger.charge_state_name ?? "—"}</strong></span>
            <span>MPPT: <strong>${charger.mpp_mode_name ?? "—"}</strong></span>
            <span>Peak today: <strong>${charger.max_power_today ?? "—"} W</strong></span>
            <span>Error: <strong>${charger.error_name ?? "—"}</strong></span>
        </div>`;
}

function renderAggregate(shunts) {
    const agg = (shunts || []).find((s) => s.aggregate);
    if (!agg) {
        els.aggregate.innerHTML = `<p class="empty">No aggregate shunt registered.</p>`;
        return;
    }

    const soc = agg.soc;
    const socText = soc === null || soc === undefined ? "—" : `${soc}%`;

    els.aggregate.innerHTML = `
        <div class="soc-ring" style="--soc: ${soc ?? 0}">
            <div style="text-align:center">
                <div class="soc-ring__value">${socText}</div>
                <div class="soc-ring__label">State of charge</div>
            </div>
        </div>
        <div class="reading-grid" style="flex:1">
            ${reading(agg.voltage, 1, "V", "Voltage")}
            ${reading(agg.current, 1, "A", "Current")}
            ${reading(agg.wattage, 0, "W", "Power")}
        </div>`;
}

function renderBanks(shunts) {
    const banks = (shunts || []).filter((s) => !s.aggregate);
    if (banks.length === 0) {
        els.banks.innerHTML = `<p class="empty">No banks registered.</p>`;
        return;
    }

    els.banks.innerHTML = banks.map(bankCard).join("");
}

function bankCard(bank) {
    // A bank with no Modbus mapping is a deliberately disconnected bank, not a
    // failed read — label it distinctly from an offline device.
    let state = bank.status === "online" ? "online" : "offline";
    let stateText = state === "online" ? "Online" : "Offline";
    if (bank.modbus_id === null || bank.modbus_id === undefined) {
        state = "disconnected";
        stateText = "Disconnected";
    }

    return `
        <div class="bank bank--${state}">
            <div class="bank__head">
                <span class="bank__name">${bank.name}</span>
                <span class="status-dot status-dot--${state}">${stateText}</span>
            </div>
            <div class="bank__readings">
                ${reading(bank.voltage, 2, "V", "Volts")}
                ${reading(bank.current, 1, "A", "Amps")}
                ${reading(bank.wattage, 0, "W", "Watts")}
            </div>
        </div>`;
}

function setConnection(kind, text) {
    els.connection.className = `pill pill--${kind}`;
    els.connectionText.textContent = text;
}

// ---- polling loop -------------------------------------------------------

// applyDemo replaces the charger reading with a gently oscillating synthetic
// charge so the wave animates at full height and the readouts look alive. The
// battery current stays well above any sane max_amperage, so the wave sits near
// full amplitude. The battery pool and banks are left as real data.
let demoTick = 0;
function applyDemo(data) {
    demoTick++;
    const wave = (Math.sin(demoTick / 8) + 1) / 2; // slow 0..1 sweep

    const c = data.charger || { id: 7, name: "PV Charger", modbus_id: 238 };
    c.status = "online";
    c.pv_voltage = 58 + wave * 8;
    c.pv_current = 4 + wave * 4;
    c.pv_power = Math.round(c.pv_voltage * c.pv_current);
    c.battery_voltage = 27.4 + wave * 0.6;
    c.battery_current = 8 + wave * 8;
    c.yield_today = 3.2;
    c.max_power_today = 480;
    c.charge_state_name = "Bulk";
    c.mpp_mode_name = "Active";
    c.error_name = "None";

    data.charger = c;
    return data;
}

async function refresh() {
    let data;
    try {
        const resp = await fetch("api/status", { cache: "no-store" });
        if (!resp.ok) {
            throw new Error(`HTTP ${resp.status}`);
        }
        data = await resp.json();
    } catch (err) {
        setConnection("stale", "Collector unreachable");
        els.lastUpdate.textContent = `Last fetch failed: ${err.message}`;
        return;
    }

    if (DEMO) {
        data = applyDemo(data);
    }

    renderCharger(data.charger);
    renderAggregate(data.shunts);
    renderBanks(data.shunts);

    if (DEMO) {
        setConnection("live", "Demo");
        els.lastUpdate.textContent = "DEMO MODE — synthetic charge data, not real readings";
        return;
    }

    const newest = newestTimestamp(data);
    if (newest === null) {
        setConnection("unknown", "No readings yet");
        els.lastUpdate.textContent = "Awaiting first reading";
        return;
    }

    const ageMs = Date.now() - newest;
    if (ageMs > STALE_MS) {
        setConnection("stale", "Data stale");
    } else {
        setConnection("live", "Live");
    }

    els.lastUpdate.textContent = `Updated ${new Date(newest).toLocaleTimeString()}`;
}

refresh();
setInterval(refresh, REFRESH_MS);
