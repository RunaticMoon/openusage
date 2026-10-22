---
title: Jev
description: Host-local Paseo Jev usage metadata and reference cost estimates.
sidebar_label: Jev
---

# Jev

The `jev` provider reads Paseo's existing accounting metadata from
`$PASEO_HOME/usage-limits/jev`, or `~/.paseo/usage-limits/jev` when unset.
It requires no authentication and makes no model or balance API requests.
The normal telemetry daemon provider poll sends its snapshot to the configured
Hub, including the originating machine/account identity.

Enable an account on each host that records Jev usage. With automatic detection
enabled, an existing metadata directory is detected. Otherwise configure it:

```json
{
  "id": "m1-jev-personal",
  "provider": "jev",
  "auth": "local",
  "provider_paths": { "usage_dir": "~/.paseo/usage-limits/jev" }
}
```

Use a distinct account ID on another host, for example `oci-jev-personal`.
Restart the collector through its normal service workflow after installing a
build containing this provider and updating settings. Existing credentials and
other provider accounts do not need to change.

## Accounting and corrections

Only regular `YYYY-MM-DD.jsonl` files are read. Version 1 records contain `id`,
`timestamp`, `model`, `inputTokens`, `outputTokens`, and optional positive
`requests` (default 1). A `source: "typesafe-console-csv"` record may list
`supersedes` IDs. Each poll rebuilds the metadata snapshot, counts an ID once,
and excludes superseded rows across files. Conflicting duplicate IDs, invalid
records and unsafe files produce an error rather than a partial total.

Snapshots include UTC daily tokens, requests, and model/day rows. The reference
rate already verified by Paseo on 2026-09-25 is used only for the exact model
`jev-1.13.0`: $0.042 per million input tokens, with zero output-token cost.
These are estimates from [TypeSafe model pricing](https://docs.typesafe.ai/models),
not invoices or an account balance. Unknown models retain their tokens and
requests but have no cost value. Mixed days and totals retain the known cost
subtotal and explicitly mark its incomplete coverage. Unknown-only days have
no cost value; they are never represented as zero. A known zero-cost observation
remains zero.

The wire contract uses `cost_basis=provider-reference-estimate`, the snapshot
attribute `cost_partial=true`, daily `analytics_cost_partial` points with value
`1`, and model-row `dimensions.cost_partial=true` where prices are missing.
Daily `analytics_cost` contains only known subtotals. These fields preserve the
same coverage semantics as the panel's local Jev reader after Hub serialization.

The provider reads only the accounting fields. Prompts, permission requests,
credentials and arbitrary extra fields are neither retained nor exported.
It exports `jev_usage_source=paseo-ledger-v1`, `jev_usage_scope=host-local`,
and a `history_path_identity` SHA-256 of the resolved metadata-directory path.
Consumers can use the host and this identity to avoid adding the same local
ledger to its Hub copy. The raw directory path and individual record IDs are
not exported.

The source is a complete provider snapshot rather than an incremental usage
event adapter. No duplicate native usage events are appended to telemetry
SQLite, so later CSV replacements also correct the next exported snapshot.
Jev exposes no response-time data and does not estimate speed from token counts.
