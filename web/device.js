"use strict";

// Device form: adds a new device (POST) or edits an existing one (PUT) when the
// URL carries ?id=N. It writes through the API, which rewrites config.json; the
// collector applies the change on its next poll.
//
// Every device reads from a named data source (Settings → Data Sources). The
// device type decides which source type is compatible — Modbus for
// shunt/charge_controller/system, MadBus for energy_meter — so the Source
// dropdown is filtered to compatible sources, and the selected source's type
// (not the device type) decides whether we ask for a Modbus unit or a MadBus id.

const params = new URLSearchParams(location.search);
const editId = params.has("id") ? Number(params.get("id")) : null;
const editing = editId !== null && !Number.isNaN(editId);

const els = {
    form: document.getElementById("device-form"),
    title: document.getElementById("form-title"),
    type: document.getElementById("device_type"),
    name: document.getElementById("name"),
    source: document.getElementById("source"),
    sourceHint: document.getElementById("source-hint"),
    modbusUnit: document.getElementById("modbus_unit"),
    aggregate: document.getElementById("aggregate"),
    maxAmperage: document.getElementById("max_amperage"),
    madbusId: document.getElementById("madbus_id"),
    madbusHint: document.getElementById("madbus-hint"),
    testBtn: document.getElementById("test-btn"),
    testStatus: document.getElementById("test-status"),
    error: document.getElementById("form-error"),
    submit: document.getElementById("submit-btn"),
};

// All sources fetched from the API, and the type each device type requires.
let allSources = [];

// The MadBus device id we'd like selected once the picker is (re)loaded — the
// existing id in edit mode, cleared whenever the source changes.
let desiredMadbusId = "";

// Whether the current Modbus unit has passed a "Test Connection". Cleared on any
// change to the unit, source, or type. MadBus devices don't use this — their
// live device picker is the verification.
let modbusVerified = false;

function isModbusSource() {
    const src = selectedSource();
    return Boolean(src && src.type === "modbus");
}

function modbusUnitBlank() {
    return els.modbusUnit.value.trim() === "";
}

// Decide whether Save is allowed. MadBus: a device must be picked. Modbus: the
// unit must pass a connection test — no blank/"disconnected" devices, so the
// user never has to wonder whether a device is misconfigured or just offline.
function updateSubmitState() {
    const src = selectedSource();
    if (!src) {
        els.submit.disabled = true;
        return;
    }
    if (src.type === "madbus") {
        els.submit.disabled = !els.madbusId.value;
        return;
    }
    els.submit.disabled = !modbusVerified;
}

function setTestStatus(message, ok) {
    els.testStatus.textContent = message;
    els.testStatus.className = "test-status" +
        (ok === true ? " test-status--ok" : ok === false ? " test-status--fail" : "");
}

// Invalidate any prior Modbus test and recompute Save. Called whenever the unit,
// source, or type changes.
function invalidateModbusTest() {
    modbusVerified = false;
    if (!isModbusSource()) {
        setTestStatus("", null);
    } else if (modbusUnitBlank()) {
        setTestStatus("Enter a Modbus unit ID, then test the connection to enable saving.", null);
    } else {
        setTestStatus("Test the connection to enable saving.", null);
    }
    updateSubmitState();
}

function compatibleSourceType(deviceType) {
    return deviceType === "energy_meter" ? "madbus" : "modbus";
}

function typeDefaultName() {
    switch (els.type.value) {
        case "charge_controller":
            return "PV Charger";
        case "system":
            return "System";
        case "energy_meter":
            return "Energy Meter";
        default:
            return "New Bank";
    }
}

// Fill the Source dropdown with the sources compatible with the current device
// type. When none exist, disable the form and point the user at Settings.
function populateSources(selected) {
    const want = compatibleSourceType(els.type.value);
    const compatible = allSources.filter((s) => s.type === want);

    els.source.innerHTML = compatible
        .map((s) => `<option value="${escapeAttr(s.name)}">${escapeAttr(s.name)}</option>`)
        .join("");

    if (compatible.length === 0) {
        const label = want === "madbus" ? "MadBus" : "Modbus";
        els.sourceHint.textContent = `No ${label} data source configured. Add one under Settings → Data Sources first.`;
        els.source.disabled = true;
        els.submit.disabled = true;
        return;
    }

    els.source.disabled = false;
    // Save stays gated by the connection test (Modbus) or device picker (MadBus);
    // syncSourceFields sets the correct state right after this.
    els.sourceHint.textContent = "The source Sola reads this device from. Manage sources under Settings → Data Sources.";

    if (selected && compatible.some((s) => s.name === selected)) {
        els.source.value = selected;
    }
}

function selectedSource() {
    return allSources.find((s) => s.name === els.source.value);
}

function escapeAttr(value) {
    return String(value).replace(/[&<>"']/g, (c) => ({
        "&": "&amp;",
        "<": "&lt;",
        ">": "&gt;",
        '"': "&quot;",
        "'": "&#39;",
    }[c]));
}

// Show only the fields that apply. The aggregate checkbox is shunt-only and max
// amperage is charger-only (both driven by device type). The Modbus unit vs.
// MadBus id field is driven by the *selected source's* type.
function syncTypeFields() {
    const type = els.type.value;
    document.querySelectorAll(".field--shunt").forEach((e) => (e.hidden = type !== "shunt"));
    document.querySelectorAll(".field--charger").forEach((e) => (e.hidden = type !== "charge_controller"));
}

function syncSourceFields() {
    const src = selectedSource();
    const isMadbus = src && src.type === "madbus";
    document.querySelectorAll(".field--modbus").forEach((e) => (e.hidden = isMadbus));
    document.querySelectorAll(".field--madbus").forEach((e) => (e.hidden = !isMadbus));
    if (isMadbus) {
        setTestStatus("", null);
        loadMadbusDevices();
    } else {
        // Modbus: gate Save on a per-unit connection test.
        invalidateModbusTest();
    }
}

// Populate the MadBus device picker from the selected source. Sola polls the
// source server-side (GET /api/sources/{name}/devices), so the browser never
// talks to MadBus directly. When the source can't be reached we degrade
// gracefully: in edit mode we keep the existing id so a save still works; in add
// mode we disable submit and explain why.
async function loadMadbusDevices() {
    const src = selectedSource();
    if (!src || src.type !== "madbus") {
        return;
    }

    els.madbusId.innerHTML = `<option value="">Loading…</option>`;
    els.madbusId.disabled = true;
    els.submit.disabled = true;

    try {
        const resp = await fetch(`/api/sources/${encodeURIComponent(src.name)}/devices`, { cache: "no-store" });
        if (!resp.ok) {
            throw new Error((await resp.text()).trim() || `HTTP ${resp.status}`);
        }
        renderMadbusOptions(await resp.json(), null);
    } catch (err) {
        renderMadbusOptions(null, err.message);
    }
}

function renderMadbusOptions(devices, errMsg) {
    const want = desiredMadbusId;

    // Unreachable source: keep the existing id (edit) so the form still saves,
    // otherwise there's nothing to pick.
    if (errMsg !== null) {
        if (want) {
            els.madbusId.innerHTML = `<option value="${escapeAttr(want)}">${escapeAttr(want)} (source unreachable)</option>`;
            els.madbusId.disabled = false;
            els.submit.disabled = false;
        } else {
            els.madbusId.innerHTML = `<option value="">No devices — source unreachable</option>`;
            els.madbusId.disabled = true;
            els.submit.disabled = true;
        }
        els.madbusHint.textContent = `Couldn't list devices from this source: ${errMsg}`;
        return;
    }

    const list = devices || [];
    const options = list.map((d) => {
        const label = d.name ? `${d.name} — ${d.id}` : d.id;
        const suffix = d.online ? "" : " (offline)";
        return `<option value="${escapeAttr(d.id)}">${escapeAttr(label)}${suffix}</option>`;
    });

    // Preserve a configured id that the source isn't currently reporting so an
    // edit doesn't silently drop it.
    if (want && !list.some((d) => d.id === want)) {
        options.unshift(`<option value="${escapeAttr(want)}">${escapeAttr(want)} (not currently reported)</option>`);
    }

    if (options.length === 0) {
        els.madbusId.innerHTML = `<option value="">No devices reported</option>`;
        els.madbusId.disabled = true;
        els.submit.disabled = true;
        els.madbusHint.textContent = "This MadBus source isn't reporting any devices yet.";
        return;
    }

    els.madbusId.innerHTML = options.join("");
    els.madbusId.disabled = false;
    els.submit.disabled = false;
    if (want) {
        els.madbusId.value = want;
    }
    els.madbusHint.textContent = "Choose a device reported by the selected MadBus source.";
}

function showError(message) {
    els.error.textContent = message;
    els.error.hidden = false;
}

function hideError() {
    els.error.hidden = true;
}

let nameEdited = false;
els.name.addEventListener("input", () => (nameEdited = true));

// Changing the unit invalidates a prior test (and recomputes Save).
els.modbusUnit.addEventListener("input", invalidateModbusTest);

els.testBtn.addEventListener("click", async () => {
    hideError();
    const src = selectedSource();
    if (!src || src.type !== "modbus") {
        return;
    }
    if (modbusUnitBlank()) {
        setTestStatus("Enter a Modbus unit ID to test.", false);
        return;
    }

    els.testBtn.disabled = true;
    setTestStatus("Testing connection…", null);
    try {
        const resp = await fetch("/api/devices/test", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
                source: src.name,
                device_type: els.type.value,
                modbus_unit: Number(els.modbusUnit.value),
            }),
        });
        const result = await resp.json();
        if (result.ok) {
            modbusVerified = true;
            setTestStatus(`Unit ${els.modbusUnit.value.trim()} responded — you can save.`, true);
        } else {
            modbusVerified = false;
            setTestStatus(`Test failed: ${result.error || "no response"}`, false);
        }
    } catch (err) {
        modbusVerified = false;
        setTestStatus(`Test failed: ${err.message}`, false);
    } finally {
        els.testBtn.disabled = false;
        updateSubmitState();
    }
});

els.source.addEventListener("change", () => {
    // A different source reports different devices, so drop any remembered id.
    desiredMadbusId = "";
    syncSourceFields();
});

els.type.addEventListener("change", () => {
    syncTypeFields();
    populateSources();
    syncSourceFields();
    if (editing) {
        return;
    }
    // Keep the prepopulated defaults sensible for the chosen type until the
    // user takes over the fields.
    if (!nameEdited) {
        els.name.value = typeDefaultName();
    }
    if (els.type.value === "charge_controller" && !els.maxAmperage.value) {
        els.maxAmperage.value = "30";
    }
    if (els.type.value === "system" && !els.modbusUnit.value) {
        els.modbusUnit.value = "100"; // Venus System service default unit
    }
    // Prefilling the unit programmatically doesn't fire an input event, so
    // recompute the test gate for the new default.
    if (isModbusSource()) {
        invalidateModbusTest();
    }
});

async function fetchSources() {
    try {
        const resp = await fetch("/api/sources", { cache: "no-store" });
        if (!resp.ok) {
            throw new Error(`HTTP ${resp.status}`);
        }
        allSources = await resp.json();
    } catch (err) {
        showError(`Failed to load data sources: ${err.message}`);
        allSources = [];
    }
}

async function init() {
    await fetchSources();

    if (editing) {
        els.title.textContent = "Edit Device";
        els.submit.textContent = "Save Changes";
        els.type.disabled = true; // a device's fundamental type is fixed on edit

        try {
            const resp = await fetch("/api/devices", { cache: "no-store" });
            if (!resp.ok) {
                throw new Error(`HTTP ${resp.status}`);
            }
            const device = (await resp.json()).find((d) => d.id === editId);
            if (!device) {
                showError("Device not found — it may have been deleted.");
                els.submit.disabled = true;
                return;
            }

            els.type.value = device.device_type;
            els.name.value = device.name;
            els.modbusUnit.value =
                device.modbus_unit === null || device.modbus_unit === undefined ? "" : device.modbus_unit;
            els.aggregate.checked = Boolean(device.aggregate);
            els.maxAmperage.value =
                device.max_amperage === null || device.max_amperage === undefined ? "" : device.max_amperage;
            desiredMadbusId = device.madbus_id || ""; // preselected once the picker loads

            syncTypeFields();
            populateSources(device.source);
            syncSourceFields();
            return;
        } catch (err) {
            showError(`Failed to load device: ${err.message}`);
            els.submit.disabled = true;
            return;
        }
    }

    els.name.value = typeDefaultName();
    syncTypeFields();
    populateSources();
    syncSourceFields();
}

els.form.addEventListener("submit", async (e) => {
    e.preventDefault();
    hideError();

    const type = els.type.value;
    const src = selectedSource();
    if (!src) {
        showError("Select a data source. Add one under Settings → Data Sources if none are listed.");
        return;
    }

    const device = {
        name: els.name.value.trim(),
        device_type: type,
        source: src.name,
    };

    if (src.type === "madbus") {
        // MadBus-sourced: no Modbus unit; carries a madbus_id instead.
        device.modbus_unit = null;
        device.madbus_id = els.madbusId.value.trim();
        if (!device.madbus_id) {
            showError("Select a MadBus device. If none are listed, check that the source is reachable under Settings → Data Sources.");
            return;
        }
    } else {
        device.modbus_unit = els.modbusUnit.value === "" ? null : Number(els.modbusUnit.value);
        if (device.modbus_unit === null) {
            showError("Enter a Modbus unit ID and test the connection before saving.");
            return;
        }
        // The unit must have passed a connection test.
        if (!modbusVerified) {
            showError("Test the Modbus connection before saving.");
            return;
        }
    }

    if (type === "shunt") {
        device.aggregate = els.aggregate.checked;
    } else if (type === "charge_controller" && els.maxAmperage.value !== "") {
        device.max_amperage = Number(els.maxAmperage.value);
    }
    // A system device carries neither an aggregate flag nor max amperage.

    const url = editing ? `/api/devices/${editId}` : "/api/devices";
    const method = editing ? "PUT" : "POST";

    els.submit.disabled = true;
    try {
        const resp = await fetch(url, {
            method,
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(device),
        });

        if (!resp.ok) {
            showError((await resp.text()).trim() || `Request failed (HTTP ${resp.status})`);
            els.submit.disabled = false;
            return;
        }

        location.href = "/settings";
    } catch (err) {
        showError(`Request failed: ${err.message}`);
        els.submit.disabled = false;
    }
});

init();
