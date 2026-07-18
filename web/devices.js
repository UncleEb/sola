"use strict";

// Devices page: lists the registered devices from config.json (via the API),
// links each to its edit form, and deletes on request. All mutations go through
// the API, which rewrites config.json; the collector applies them on its next
// poll.

const listEl = document.getElementById("devices");

function escapeHtml(value) {
    return String(value).replace(/[&<>"']/g, (c) => ({
        "&": "&amp;",
        "<": "&lt;",
        ">": "&gt;",
        '"': "&quot;",
        "'": "&#39;",
    }[c]));
}

const TYPE_LABELS = {
    charge_controller: "Charge Controller",
    system: "System",
    shunt: "Battery Shunt",
};

function deviceRow(d) {
    const type = TYPE_LABELS[d.device_type] || d.device_type;
    const unit = d.modbus_unit === null || d.modbus_unit === undefined
        ? "No Modbus port"
        : `Unit ${d.modbus_unit}`;

    const tags = [];
    if (d.aggregate || d.device_type === "system") {
        tags.push("Aggregate");
    }
    if (d.max_amperage !== null && d.max_amperage !== undefined) {
        tags.push(`${d.max_amperage} A max`);
    }

    const meta = [type, unit, ...tags].join(" · ");

    return `
        <div class="device-row" data-id="${d.id}" role="button" tabindex="0">
            <div class="device-row__main">
                <span class="device-row__name">${escapeHtml(d.name)}</span>
                <span class="device-row__meta">${escapeHtml(meta)}</span>
            </div>
            <button type="button" class="icon-btn icon-btn--danger device-row__delete"
                    data-id="${d.id}" aria-label="Delete ${escapeHtml(d.name)}" title="Delete">
                <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor"
                     stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                    <path d="M3 6h18"/>
                    <path d="M8 6V4a1 1 0 0 1 1-1h6a1 1 0 0 1 1 1v2"/>
                    <path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6"/>
                    <path d="M10 11v6"/>
                    <path d="M14 11v6"/>
                </svg>
            </button>
        </div>`;
}

function render(devices) {
    if (!devices || devices.length === 0) {
        listEl.innerHTML = `<p class="empty">No devices registered yet. Use “Add Device” to create one.</p>`;
        return;
    }

    listEl.innerHTML = devices.map(deviceRow).join("");
}

async function load() {
    try {
        const resp = await fetch("/api/devices", { cache: "no-store" });
        if (!resp.ok) {
            throw new Error(`HTTP ${resp.status}`);
        }
        render(await resp.json());
    } catch (err) {
        listEl.innerHTML = `<p class="empty">Failed to load devices: ${escapeHtml(err.message)}</p>`;
    }
}

async function remove(id) {
    if (!confirm("Delete this device? It will be removed from the configuration and the dashboard.")) {
        return;
    }

    try {
        const resp = await fetch(`/api/devices/${id}`, { method: "DELETE" });
        if (!resp.ok) {
            alert(`Delete failed: ${(await resp.text()).trim() || `HTTP ${resp.status}`}`);
            return;
        }
    } catch (err) {
        alert(`Delete failed: ${err.message}`);
        return;
    }

    load();
}

function editFromRow(row) {
    location.href = `/device?id=${row.dataset.id}`;
}

listEl.addEventListener("click", (e) => {
    const del = e.target.closest(".device-row__delete");
    if (del) {
        e.stopPropagation();
        remove(Number(del.dataset.id));
        return;
    }

    const row = e.target.closest(".device-row");
    if (row) {
        editFromRow(row);
    }
});

listEl.addEventListener("keydown", (e) => {
    if (e.key !== "Enter" && e.key !== " ") {
        return;
    }
    const row = e.target.closest(".device-row");
    if (row) {
        e.preventDefault();
        editFromRow(row);
    }
});

load();
