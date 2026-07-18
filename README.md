Victron Collector
Project Overview

Victron Collector is a lightweight Go service that polls telemetry from a Victron Venus OS device over Modbus TCP.

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

Historical storage does not exist yet: only the most recent reading per device
is kept, so the dashboard is a live "now" view rather than a time-series.

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
Victron Collector (Go)
        │
        ├── Current Status
        ├── Historical Storage
        ├── HTTP API
        └── Logging

Visualization (Grafana, Home Assistant, custom dashboards, etc.) should consume data from the collector rather than directly from Modbus whenever practical.

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

Future Roadmap

The rough order of development is expected to be:

Phase 1
Stable Modbus polling
Solar charger support
Battery support
Configuration cleanup
Phase 2

SQLite

Historical measurements
Current status table
Automatic schema creation
Database migrations
Phase 3

Configuration

YAML configuration file
Device definitions
Poll intervals
Optional device enable/disable
Phase 4

HTTP API

Potential endpoints:

Current status
Historical data
Health
Metrics
Phase 5

Web UI

A lightweight operational interface for:

Connection status
Device health
Diagnostics
Configuration verification

This is not intended to become a dashboard replacement.

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