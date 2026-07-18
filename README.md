Sola
Project Overview

Sola is a lightweight Go service that polls telemetry from a Victron Venus OS device over Modbus TCP.

The long-term goal is to provide a simple, reliable telemetry collection service that stores historical data in SQLite and exposes the information for visualization, APIs, and automation without depending on Home Assistant.

This project is intentionally being built incrementally. Every feature should have a clear purpose, and unnecessary complexity should be avoided.

Current Status

Current functionality:

Connects to Venus OS using Modbus TCP.
Polls on a configurable interval (default five seconds).
Reads:
Aggregate ("All Banks") telemetry.
Individual battery banks.
Solar charger telemetry.
Stores the latest reading per device in SQLite (one current-status row each).
Serves a live web dashboard and a JSON status endpoint over HTTP.
Loads deployment settings from config.json, reloaded each poll.
Handles graceful shutdown (Ctrl+C / SIGTERM), including the HTTP server.
Uses structured logging (log/slog); per-poll readings print to the terminal
only when "debug" is enabled in config.json.
Captures periodic history snapshots (default every 15 seconds) into
per-device-type SQLite tables, exposed as time-series graphs on a History page.
Manages devices (add / edit / delete) from the web UI, written to config.json
and applied live without a restart.
Deploys as a single static binary or a small distroless Docker image; the
Modbus link is non-fatal and self-healing, so the dashboard stays up across
device reboots and network blips.

Running with Docker

Sola builds to a single static binary with the web UI embedded, so the image is
tiny (~13 MB), runs as a non-root user, and needs no runtime dependencies.

Quick start (docker compose):

    # Set MODBUS_URL in docker-compose.yml to your Venus OS device, then:
    docker compose up -d --build

The dashboard is then at http://localhost:8088.

config.json and the SQLite database live in a named volume (sola-data, mounted
at /data), so they persist across restarts and upgrades. On first run against an
empty volume, Sola writes a default config.json that you can edit or configure
from the dashboard. The MODBUS_URL environment variable overrides modbus_url so
you can point the container at your device without editing the file.

The Modbus link is not required for the dashboard to start: if the device is
unreachable, Sola serves the UI anyway and keeps retrying the connection, so a
device reboot or network blip never takes the service down.

The container runs as uid 65532. A named volume inherits writable ownership
automatically; a bind mount (-v /host/path:/data) must be writable by uid 65532.

Building a multi-arch image (amd64 + arm64) and pushing to a registry:

    docker buildx build --platform linux/amd64,linux/arm64 \
        -t <registry>/sola:<tag> --push .

The image defines a HEALTHCHECK that runs "sola healthcheck", which probes the
dashboard's /api/status endpoint (the distroless image has no shell or curl).

Design Philosophy

This project favors:

Explicit code over clever code.
Readability over abstraction.
Small, understandable components.
Long-term maintainability.
Minimal dependencies.
Local-first architecture.

If a future maintainer (or myself in five years) cannot quickly understand how a component works, it is probably too complicated.

Architecture Goals

The intended data flow is:

Victron Devices
        │
        ▼
   Venus OS
        │
   Modbus TCP
        │
        ▼
     Sola (Go)
        │
        ├── Current Status
        ├── Historical Storage
        ├── HTTP API
        └── Logging

Visualization (Grafana, Home Assistant, custom dashboards, etc.) should consume data from Sola rather than directly from Modbus whenever practical.

Current Device Layout

Current Venus OS services:

Device	Unit ID
System	100
Solar Charger	238
Battery Shunt (Aggregate)	239
Battery Bank 3	235
Battery Bank 4	233
Battery Bank 5	236

Battery Banks 1 and 2 are currently disconnected.

Device unit IDs, names, and enable/disable now live in config.json; this table
is a human-readable summary of the current installation.

Coding Guidelines

General coding style:

Keep functions small.
Prefer descriptive names.
Prefer straightforward control flow.
Avoid unnecessary interfaces.
Avoid unnecessary goroutines.
Avoid unnecessary abstractions.
Return errors rather than hiding them.
Log operational failures with useful context.

Comments should explain why, not what.

Roadmap

Phases 1–5 are substantially complete:

Phase 1 — Stable Modbus polling, solar charger and battery support, configuration cleanup. Done.
Phase 2 — SQLite: current-status tables, per-device-type history, automatic schema creation. Done (formal migrations not yet needed).
Phase 3 — Configuration: config.json with device definitions, poll interval, and per-device enable/disable, reloaded live each poll. Done (JSON rather than YAML).
Phase 4 — HTTP API: current status (/api/status), history (/api/history), plus device and settings endpoints. Done.
Phase 5 — Web UI: live dashboard, connection status, device management, and history graphs. Done.

Likely next:

Retention / pruning for the history tables (currently keep-everything).
Metrics and health endpoints for external monitoring.
Support for non-Victron and additional data sources.
Formal database migrations once the schema needs to change.

AI Assistant Notes

This repository is being developed collaboratively with AI assistance.

When suggesting changes:

Preserve the existing architecture unless there is a compelling reason to change it.
Prefer incremental improvements over rewrites.
Keep implementations simple.
Explain tradeoffs.
Avoid introducing frameworks unless they provide significant value.

Suggestions should optimize for maintainability rather than cleverness.

Long-Term Vision

This project is intended to become dependable infrastructure.

The desired outcome is software that can be installed, configured once, and then quietly run for years with minimal maintenance.

Reliability, clarity, and simplicity are considered more important than feature count.