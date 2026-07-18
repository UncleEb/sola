"use strict";

// Device form: adds a new device (POST) or edits an existing one (PUT) when the
// URL carries ?id=N. It writes through the API, which rewrites config.json; the
// collector applies the change on its next poll. The type dropdown reveals the
// fields relevant to that device type.

const params = new URLSearchParams(location.search);
const editId = params.has("id") ? Number(params.get("id")) : null;
const editing = editId !== null && !Number.isNaN(editId);

const els = {
    form: document.getElementById("device-form"),
    title: document.getElementById("form-title"),
    type: document.getElementById("device_type"),
    name: document.getElementById("name"),
    modbusUnit: document.getElementById("modbus_unit"),
    aggregate: document.getElementById("aggregate"),
    maxAmperage: document.getElementById("max_amperage"),
    error: document.getElementById("form-error"),
    submit: document.getElementById("submit-btn"),
};

function typeDefaultName() {
    switch (els.type.value) {
        case "charge_controller":
            return "PV Charger";
        case "system":
            return "System";
        default:
            return "New Bank";
    }
}

// Show only the fields that apply to the selected device type: the aggregate
// checkbox is shunt-only, max amperage is charger-only, and a System device
// takes neither (it is implicitly the aggregate).
function syncTypeFields() {
    const type = els.type.value;
    document.querySelectorAll(".field--shunt").forEach((e) => (e.hidden = type !== "shunt"));
    document.querySelectorAll(".field--charger").forEach((e) => (e.hidden = type !== "charge_controller"));
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

els.type.addEventListener("change", () => {
    syncTypeFields();
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
});

async function init() {
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
        } catch (err) {
            showError(`Failed to load device: ${err.message}`);
            els.submit.disabled = true;
        }
    } else {
        els.name.value = typeDefaultName();
    }

    syncTypeFields();
}

els.form.addEventListener("submit", async (e) => {
    e.preventDefault();
    hideError();

    const type = els.type.value;
    const device = {
        name: els.name.value.trim(),
        device_type: type,
        modbus_unit: els.modbusUnit.value === "" ? null : Number(els.modbusUnit.value),
    };

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

        location.href = "/devices";
    } catch (err) {
        showError(`Request failed: ${err.message}`);
        els.submit.disabled = false;
    }
});

init();
