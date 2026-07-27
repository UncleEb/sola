"use strict";

// Source form: adds a new data source (POST) or edits an existing one (PUT) when
// the URL carries ?name=NAME. It writes through the API, which rewrites
// config.json; the collector applies the change on its next poll. A source is
// either the Victron Modbus link (type "modbus") or a MadBus HTTP endpoint
// (type "madbus"); devices reference a source by name.

const params = new URLSearchParams(location.search);
const editName = params.get("name");
const editing = editName !== null && editName !== "";

const els = {
    form: document.getElementById("source-form"),
    title: document.getElementById("form-title"),
    type: document.getElementById("type"),
    name: document.getElementById("name"),
    url: document.getElementById("url"),
    typeHint: document.getElementById("type-hint"),
    urlHint: document.getElementById("url-hint"),
    error: document.getElementById("form-error"),
    submit: document.getElementById("submit-btn"),
};

const TYPE_HINTS = {
    modbus: "A Victron Venus OS device over Modbus TCP. Only one Modbus source is supported in this version.",
    madbus: "A MadBus instance serving normalized RS-485 device data over HTTP.",
};

const URL_HINTS = {
    modbus: "Modbus TCP address, e.g. tcp://192.168.1.100:502",
    madbus: "MadBus base URL, e.g. http://192.168.1.50:8090",
};

const URL_PLACEHOLDERS = {
    modbus: "tcp://192.168.1.100:502",
    madbus: "http://192.168.1.50:8090",
};

function syncTypeHints() {
    const type = els.type.value;
    els.typeHint.textContent = TYPE_HINTS[type] || "";
    els.urlHint.textContent = URL_HINTS[type] || "";
    els.url.placeholder = URL_PLACEHOLDERS[type] || "";
}

function showError(message) {
    els.error.textContent = message;
    els.error.hidden = false;
}

function hideError() {
    els.error.hidden = true;
}

els.type.addEventListener("change", syncTypeHints);

async function init() {
    if (editing) {
        els.title.textContent = "Edit Source";
        els.submit.textContent = "Save Changes";
        els.name.disabled = true; // a source's name is fixed on edit (devices reference it)

        try {
            const resp = await fetch("/api/sources", { cache: "no-store" });
            if (!resp.ok) {
                throw new Error(`HTTP ${resp.status}`);
            }
            const source = (await resp.json()).find((s) => s.name === editName);
            if (!source) {
                showError("Source not found — it may have been deleted.");
                els.submit.disabled = true;
                return;
            }

            els.type.value = source.type;
            els.name.value = source.name;
            els.url.value = source.url;
        } catch (err) {
            showError(`Failed to load source: ${err.message}`);
            els.submit.disabled = true;
        }
    }

    syncTypeHints();
}

els.form.addEventListener("submit", async (e) => {
    e.preventDefault();
    hideError();

    const source = {
        name: els.name.value.trim(),
        type: els.type.value,
        url: els.url.value.trim(),
    };

    const url = editing ? `/api/sources/${encodeURIComponent(editName)}` : "/api/sources";
    const method = editing ? "PUT" : "POST";

    els.submit.disabled = true;
    try {
        const resp = await fetch(url, {
            method,
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(source),
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
