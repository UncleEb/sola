"use strict";

// Settings page. Currently just the background selector: it saves to the server
// (which persists it to config.json) and previews the change live via the
// background module's window.applyBackground.

const select = document.getElementById("background-select");
const status = document.getElementById("settings-status");

async function load() {
    try {
        const resp = await fetch("/api/settings", { cache: "no-store" });
        if (!resp.ok) {
            throw new Error(`HTTP ${resp.status}`);
        }
        const settings = await resp.json();
        select.value = settings.background;
    } catch (err) {
        status.textContent = `Failed to load settings: ${err.message}`;
    }
}

select.addEventListener("change", async () => {
    const background = select.value;
    status.textContent = "Saving…";

    try {
        const resp = await fetch("/api/settings", {
            method: "PUT",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ background }),
        });
        if (!resp.ok) {
            throw new Error((await resp.text()).trim() || `HTTP ${resp.status}`);
        }

        // Preview immediately without a page reload.
        if (typeof window.applyBackground === "function") {
            window.applyBackground(background);
        }
        status.textContent = "Saved.";
    } catch (err) {
        status.textContent = `Save failed: ${err.message}`;
    }
});

load();
