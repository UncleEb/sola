"use strict";

// Source form: adds a new data source (POST) or edits an existing one (PUT) when
// the URL carries ?name=NAME. It writes through the API, which rewrites
// config.json; the collector applies the change on its next poll (Modbus URL
// changes apply on restart). A source is either the Victron Modbus link
// (type "modbus") or a MadBus HTTP endpoint (type "madbus").
//
// Connection details never require a scheme. Both types take a host + port; the
// scheme is implied by the type — Modbus assumes tcp:// (default port 502),
// MadBus assumes http:// (default port 8090) — and assembled on save.
//
// A source may not be saved until "Test Connection" succeeds for its current
// parameters — we don't persist endpoints we can't reach. Editing any
// connection field invalidates a prior successful test and re-disables Save.

const params = new URLSearchParams(location.search);
const editName = params.get("name");
const editing = editName !== null && editName !== "";

// Per-type connection assumptions.
const TYPE_SPEC = {
    modbus: { scheme: "tcp", defaultPort: "502" },
    madbus: { scheme: "http", defaultPort: "8090" },
};

const els = {
    form: document.getElementById("source-form"),
    title: document.getElementById("form-title"),
    type: document.getElementById("type"),
    name: document.getElementById("name"),
    host: document.getElementById("host"),
    port: document.getElementById("port"),
    typeHint: document.getElementById("type-hint"),
    hostHint: document.getElementById("host-hint"),
    portHint: document.getElementById("port-hint"),
    testBtn: document.getElementById("test-btn"),
    testStatus: document.getElementById("test-status"),
    error: document.getElementById("form-error"),
    submit: document.getElementById("submit-btn"),
};

const TYPE_HINTS = {
    modbus: "A Victron Venus OS device over Modbus TCP. Only one Modbus source is supported in this version.",
    madbus: "A MadBus instance serving normalized RS-485 device data over HTTP.",
};

const HOST_HINTS = {
    modbus: "Your Venus OS device's IP address or hostname. TCP is assumed.",
    madbus: "The MadBus host's IP address or hostname. HTTP is assumed.",
};

const PORT_HINTS = {
    modbus: "Modbus-TCP port. Default 502 (the Venus OS standard).",
    madbus: "MadBus HTTP port. Default 8090.",
};

// Whether the current connection parameters have passed a Test Connection. Any
// edit to a connection field clears this, which re-disables Save.
let connectionVerified = false;

// Assemble the canonical source URL from the form, applying the type's implied
// scheme and default port.
function currentURL() {
    const host = els.host.value.trim();
    if (!host) {
        return "";
    }
    const spec = TYPE_SPEC[els.type.value];
    const port = els.port.value.trim() || spec.defaultPort;
    return `${spec.scheme}://${host}:${port}`;
}

function syncTypeHints() {
    const type = els.type.value;
    els.typeHint.textContent = TYPE_HINTS[type] || "";
    els.hostHint.textContent = HOST_HINTS[type] || "";
    els.portHint.textContent = PORT_HINTS[type] || "";
    els.port.placeholder = TYPE_SPEC[type].defaultPort;
}

// Invalidate any prior successful test — call whenever a connection field or the
// type changes. Save stays disabled until the (new) parameters test clean.
function invalidateTest() {
    connectionVerified = false;
    els.submit.disabled = true;
    setTestStatus("", null);
}

function setTestStatus(message, ok) {
    els.testStatus.textContent = message;
    els.testStatus.className = "test-status" +
        (ok === true ? " test-status--ok" : ok === false ? " test-status--fail" : "");
}

function showError(message) {
    els.error.textContent = message;
    els.error.hidden = false;
}

function hideError() {
    els.error.hidden = true;
}

els.type.addEventListener("change", () => {
    // Switching type is a deliberate reconfiguration: reset the port to the new
    // type's default so a stale 502/8090 doesn't carry over.
    els.port.value = TYPE_SPEC[els.type.value].defaultPort;
    syncTypeHints();
    invalidateTest();
});
// Any change to a connection field forces a re-test before saving.
[els.host, els.port].forEach((el) => el.addEventListener("input", invalidateTest));

els.testBtn.addEventListener("click", async () => {
    hideError();

    const url = currentURL();
    if (!url) {
        setTestStatus("Enter a host/IP first.", false);
        return;
    }

    els.testBtn.disabled = true;
    setTestStatus("Testing connection…", null);
    try {
        const resp = await fetch("/api/sources/test", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ type: els.type.value, url }),
        });
        const result = await resp.json();
        if (result.ok) {
            connectionVerified = true;
            els.submit.disabled = false;
            setTestStatus("Connection successful — you can save.", true);
        } else {
            connectionVerified = false;
            els.submit.disabled = true;
            setTestStatus(`Connection failed: ${result.error || "unreachable"}`, false);
        }
    } catch (err) {
        connectionVerified = false;
        els.submit.disabled = true;
        setTestStatus(`Connection test failed: ${err.message}`, false);
    } finally {
        els.testBtn.disabled = false;
    }
});

// Split a stored URL back into host + port, dropping the scheme.
function loadURLIntoFields(url) {
    const bare = url.replace(/^[a-z]+:\/\//i, "");
    const idx = bare.lastIndexOf(":");
    if (idx > -1) {
        els.host.value = bare.slice(0, idx);
        els.port.value = bare.slice(idx + 1);
    } else {
        els.host.value = bare;
        els.port.value = TYPE_SPEC[els.type.value].defaultPort;
    }
}

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
                els.testBtn.disabled = true;
                return;
            }

            els.type.value = source.type;
            els.name.value = source.name;
            loadURLIntoFields(source.url);
        } catch (err) {
            showError(`Failed to load source: ${err.message}`);
            els.testBtn.disabled = true;
            return;
        }
    } else {
        els.port.value = TYPE_SPEC[els.type.value].defaultPort;
    }

    syncTypeHints();
    // Whether adding or editing, saving requires a fresh successful test.
    invalidateTest();
    if (editing) {
        setTestStatus("Test the connection to enable saving.", null);
    }
}

els.form.addEventListener("submit", async (e) => {
    e.preventDefault();
    hideError();

    if (!connectionVerified) {
        showError("Test the connection successfully before saving.");
        return;
    }

    const source = {
        name: els.name.value.trim(),
        type: els.type.value,
        url: currentURL(),
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
