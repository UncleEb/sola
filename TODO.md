# Sola — Project TODO

The living list of planned and outstanding work. When you pick something up, note
progress inline; when you finish, delete the item (git history is the archive).
Keep entries actionable and include the *why* so future-you (or a new
contributor) doesn't have to reconstruct the reasoning.

Rough priority order within each section, most impactful first. This file is the
canonical backlog — prefer it over scattered `TODO(...)` code comments and
one-off notes.

---

## Data sources & devices

- **Phase 2 — multiple concurrent Modbus sources.** Today `config.validate()`
  allows *at most one* `modbus` source (guardrail in [config.go](config.go),
  `at most one modbus source is supported in this version`). Lift it: per-source
  Modbus clients + per-source connection health, and rework the poll loop in
  [main.go](main.go) (currently a single `client` built from `modbusSource(cfg)`).
  The `sources[]` schema, migration, and UI already support many sources — this
  is the poll-loop/runtime half of the sources refactor.
- **More MadBus device types.** MadBus will serve shunts, BMSs, inverters, and
  charge controllers in addition to energy meters. Source/type are already
  orthogonal, so each slots in by adding a per-type MadBus→Sola mapping (like the
  existing `energy_meter` path: status/history tables, `statusTable` routing,
  poll mapping, dashboard pane). Until each type is wired, `validate()` only
  permits a MadBus source on `energy_meter` so nothing half-built is
  configurable — extend that allow-list as each type lands.
- **Per-source health in the UI.** Surface each source's reachability (last poll
  ok/failed, last error) in Settings → Data Sources so a misconfigured or
  unreachable source is obvious. Pairs naturally with Phase 2's per-source health.

## Dashboard / UI

- **Movable / reorderable panes.** Let users drag panes (or set an order) so a
  meter can sit where it matters for their install, instead of the fixed order.
  Flagged inline at [solar_dashboard.html](web/solar_dashboard.html) (the
  `#meters` container). Larger effort — persist the order to config and apply it
  across pages.

## Connectivity

- **Embedded WireGuard tunnel (headline feature).** In-app WireGuard on *both*
  platforms for a "cloud feel without a cloud": opt-*out* (on by default),
  seamless auto-switching between direct LAN and tunnel. iOS split-tunnel solves
  the all-or-nothing VPN problem. Not "optional" — a core differentiator.

## Clients

- **iOS client.** A sibling to the Android WebView app, planned for
  `clients/ios`.

## Data & operations

- **History retention / pruning.** The per-device-type history tables are
  keep-everything today; add a retention policy (age- and/or size-based) with
  pruning so the SQLite DB doesn't grow unbounded.
- **Formal database migrations.** Introduce a real migration mechanism before the
  next schema change (currently schema is created idempotently, no versioning).
- **Metrics / health endpoints for external monitoring.** Beyond the existing
  `sola healthcheck`, expose metrics (Prometheus-style or similar) so Sola can be
  watched by external tooling.
