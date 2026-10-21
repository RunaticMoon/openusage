---
title: Snapshots
description: The UsageSnapshot data model, what metrics it carries, refresh cadence, and how time-window filtering works.
---

A `UsageSnapshot` is the unit of data the TUI renders. Each provider produces one snapshot per account per fetch. Everything you see on screen — tiles, gauges, detail tables, status badges — comes from a snapshot plus the provider's static widget definition.

## What a snapshot carries

A snapshot is a normalized container. Not every provider populates every field; what's missing simply isn't shown.

### Identity

- account ID and provider ID
- timestamp of the fetch
- status (`OK`, `WARN`, `LIMIT`, `AUTH`, `ERR`, `UNKNOWN`)

### Spend

- total spend in the provider's reported currency
- monthly / cycle spend
- spend limits (hard, soft, plan-included, plan-bonus)
- credit balance breakdown (cash, voucher, granted)

Currencies vary: most providers report USD, Mistral reports EUR, DeepSeek defaults to CNY. The detail view shows the provider's native currency without conversion.

### Tokens

- input / output / cache-read / cache-create / reasoning tokens
- per-model token counts
- tool-call counts (for agents that report them)

### Daily token history

The telemetry read model includes UTC daily token series in `daily_series`:

| Key | Value |
|---|---|
| `analytics_tokens` | Canonical total tokens from deduplicated usage events |
| `analytics_work_tokens` | Total tokens minus cache reads and cache writes |
| `analytics_cached_tokens` | Cache reads plus cache writes |
| `analytics_token_split_partial` | `1` when a day's Work/Cached totals cover only the events with a known token breakdown |

Work and Cached use the same account, event deduplication, and date boundaries as
the total series. A recorded zero remains zero. An event without a cache
breakdown does not become a zero-cache observation, and a day without any known
breakdowns has no Work/Cached point. Partial days contain the measured subtotal;
consumers should mark it as incomplete.

These series also travel with exported snapshots. Each sending daemon must be
updated for a multi-machine dashboard to receive its daily breakdown. Restarting
the updated daemon rebuilds the series from retained usage events, including
existing history; no database migration is required. Older snapshots without
these keys remain valid, but cannot supply daily Work/Cached curves.

### Rate limits

Providers may expose any combination of:

- requests per minute (rpm)
- tokens per minute (tpm)
- requests per day (rpd)
- tokens per day (tpd)
- concurrency caps

For each, the snapshot can carry `limit`, `remaining`, and `reset` timestamps.

### Per-model breakdown

A list of per-model rows with input/output/cache tokens, request counts, and (where available) cost in the provider's currency.

### Provider-specific extras

A free-form key/value map for things that don't fit a standard field. Detail widgets can render these as their own sections (e.g. Claude Code billing blocks, Z.AI grants list).

## Refresh cadence

The daemon drives the poll loop and the TUI refreshes its read model on a tick.

- Default: **30 seconds** (`--interval` for the daemon, `ui.refresh_interval_seconds` for how often the TUI re-fetches the read model).
- Collectors run every interval; hooks deliver events between ticks for agents that emit them.
- Manual refresh: press `r` in the TUI to ask the daemon for a fresh read model.

There is no streaming — every snapshot is a fresh full state, not a delta.

## Time-window filtering

The TUI exposes a window selector with `w`:

| Token | Meaning |
|---|---|
| `1d` | Today since local midnight |
| `3d` | Rolling 72 hours |
| `7d` | Rolling 7 days |
| `30d` | Rolling 30 days (default) |
| `all` | No filter |

What the window changes:

- Aggregations in the detail view (total spend, total tokens) are restricted to the window.
- Per-day bar charts in the Analytics screen scale to the window.
- Live "current" values (rate-limit gauges, balances) are not affected — those are always the latest snapshot.

The window only applies to data the daemon has actually seen — everything within `data.retention_days` (default 30). See [telemetry](telemetry.md).

## Snapshot lifecycle

```
provider.Fetch()
   │
   ▼
UsageSnapshot
   │
   └─► telemetry.Store
              │
              ▼
         ReadModel
              │
              ▼
       UsageSnapshot
              │
              ▼
       UDS /v1/read-model ─► render
```

The snapshot returned to the TUI is rebuilt from stored events on each request. That means historical data persists across TUI restarts and daemon restarts.

## When fields go missing

If a provider can't reach its source, the snapshot still renders, but with reduced fields and a non-OK status:

- `AUTH` — the configured env var or local credentials are missing or invalid.
- `ERR` — fetch failed (network, parse error, unexpected payload). The detail panel shows the error message.
- `UNKNOWN` — provider is registered but no data has been collected yet.

Tiles never disappear because of a transient failure; they just badge themselves and keep retrying on the next tick.
