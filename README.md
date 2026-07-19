<p align="center">
  <img src="assets/logo.svg" alt="Sola" width="220">
</p>

<p align="center">
  <em>Self-hosted solar telemetry for Victron systems — local-first, dependency-light, built to run quietly for years.</em>
</p>

<p align="center">
  <a href="#license"><img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="MIT License"></a>
  <img src="https://img.shields.io/badge/Go-1.26-00ADD8.svg" alt="Go 1.26">
  <img src="https://img.shields.io/badge/Android-client-3DDC84.svg" alt="Android client">
</p>

---

## What is Sola?

Sola is a lightweight Go service that polls telemetry from a [Victron Venus OS](https://www.victronenergy.com/) device over Modbus TCP, stores it in SQLite, and serves a live web dashboard plus a JSON API — all from a single static binary.

It exists to be **dependable infrastructure for an off-grid solar install**: install it once, point it at your Venus OS device, and let it run. There is no cloud, no external database, and no dependency on Home Assistant — your data stays on your hardware, and the dashboard keeps working even when the Modbus link drops.

The project is built incrementally. Every feature earns its place, and unnecessary complexity is treated as a bug.

## Features

- **Modbus TCP polling** of Venus OS on a configurable interval (default 5s).
- **Reads** aggregate ("All Banks") telemetry, individual battery banks, and solar charger data.
- **Live web dashboard** with real-time connection status and an animated power-flow view.
- **History graphs** — periodic snapshots (default every 15s) written to per-device-type SQLite tables and exposed as time-series charts.
- **JSON API** for current status and history, so Grafana or your own tools can consume Sola instead of talking to Modbus directly.
- **Device management from the UI** — add, edit, enable/disable, or remove devices; changes are written to `config.json` and applied live, no restart.
- **Android client** — a thin WebView wrapper for the dashboard (see [`clients/android`](clients/android)).
- **Resilient by design** — the Modbus link is non-fatal and self-healing, so a device reboot or network blip never takes the dashboard down.
- **Tiny deployment** — a single static binary with the UI embedded, or a ~13 MB distroless Docker image that runs as a non-root user.

## Quick start

### Docker (recommended)

```bash
# Point MODBUS_URL in docker-compose.yml at your Venus OS device, then:
docker compose up -d --build
```

The dashboard is then at **http://localhost:8088**.

`config.json` and the SQLite database live in a named volume (`sola-data`, mounted at `/data`) and persist across restarts and upgrades. On first run against an empty volume, Sola writes a default `config.json` you can edit directly or manage from the dashboard. The `MODBUS_URL` environment variable overrides `modbus_url`, so you can point the container at your device without editing the file.

The container runs as uid `65532`. A named volume gets writable ownership automatically; a bind mount (`-v /host/path:/data`) must be writable by uid `65532`. The image ships a `HEALTHCHECK` that runs `sola healthcheck` (which probes `/api/status` — the distroless image has no shell or curl).

Multi-arch build and push:

```bash
docker buildx build --platform linux/amd64,linux/arm64 \
    -t <registry>/sola:<tag> --push .
```

### From source

```bash
go build -o sola .
MODBUS_URL=tcp://<venus-ip>:502 ./sola
```

Then open **http://localhost:8088**.

## Configuration

Settings live in `config.json` and are **reloaded on every poll**, with a last-good fallback if the file is temporarily invalid. Most keys are also editable from the dashboard's settings and device pages.

| Key | Purpose |
|-----|---------|
| `http_addr` | Dashboard listen address (default `:8088`). |
| `modbus_url` | Venus OS Modbus TCP endpoint, e.g. `tcp://192.168.1.50:502`. |
| `database_path` | Path to the SQLite database file. |
| `poll_interval_seconds` | How often to poll Modbus (default 5). |
| `history_interval_seconds` | How often to snapshot history (default 15). |
| `debug` | When true, per-poll readings print to the terminal. |
| `max_amperage` | Full-scale reference used by the dashboard gauges. |
| `soc_low_percent` | State-of-charge threshold for low-battery styling. |
| `devices` | Device definitions (id, name, `modbus_unit`, `device_type`, enabled). |

Environment overrides:

| Variable | Effect |
|----------|--------|
| `MODBUS_URL` | Overrides `modbus_url` (handy for containers). |
| `SOLA_CONFIG_DIR` | Directory holding `config.json` and the database. |

## HTTP API

| Method & path | Description |
|---------------|-------------|
| `GET /api/status` | Latest reading per device + link health. |
| `GET /api/history` | Time-series history for the graphs. |
| `GET /api/devices` | List configured devices. |
| `POST /api/devices` | Add a device. |
| `PUT /api/devices/{id}` | Update a device. |
| `DELETE /api/devices/{id}` | Remove a device. |
| `GET /api/settings` | Read current settings. |
| `PUT /api/settings` | Update settings. |

## Android client

A native Android app in [`clients/android`](clients/android) wraps the dashboard in a full-screen WebView: it asks for the server's IP/port on first launch, verifies reachability, then loads the dashboard and remembers it. Build and sideloading instructions are in [`clients/android/README.md`](clients/android/README.md). An iOS sibling is planned for `clients/ios`.

## Current device layout

A human-readable summary of the current installation (the authoritative definitions live in `config.json`):

| Device | Unit ID |
|--------|---------|
| System | 100 |
| Solar Charger | 238 |
| Battery Shunt (Aggregate) | 239 |
| Battery Bank 3 | 235 |
| Battery Bank 4 | 233 |
| Battery Bank 5 | 236 |

Battery Banks 1 and 2 are currently disconnected.

## Architecture

```
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
        ├── Historical Storage (SQLite)
        ├── HTTP API + Web Dashboard
        └── Logging
```

Visualization tools (Grafana, Home Assistant, custom dashboards) should consume data from Sola rather than talking to Modbus directly whenever practical.

## Design philosophy

This project favors:

- Explicit code over clever code.
- Readability over abstraction.
- Small, understandable components.
- Long-term maintainability.
- Minimal dependencies.
- Local-first architecture.

If a future maintainer (or myself in five years) cannot quickly understand how a component works, it is probably too complicated.

## Coding guidelines

- Keep functions small.
- Prefer descriptive names.
- Prefer straightforward control flow.
- Avoid unnecessary interfaces, goroutines, and abstractions.
- Return errors rather than hiding them.
- Log operational failures with useful context.
- Comments should explain *why*, not *what*.

## Roadmap

Phases 1–5 (stable polling, SQLite storage, live-reloaded config, HTTP API, and the web UI) are substantially complete. Likely next:

- Retention / pruning for the history tables (currently keep-everything).
- Metrics and health endpoints for external monitoring.
- Support for non-Victron and additional data sources.
- Formal database migrations once the schema needs to change.
- iOS client.

## Long-term vision

This project is intended to become dependable infrastructure — software that can be installed, configured once, and then quietly run for years with minimal maintenance. Reliability, clarity, and simplicity are considered more important than feature count.

## License

Released under the [MIT License](LICENSE).
