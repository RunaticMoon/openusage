# OpenCode account projection

OpenCode records identify the model vendor in `provider_id` (for example
Anthropic, OpenAI or a configured custom vendor). The upstream OpenCode tile
selected `provider_id = opencode`, so successful canonical collection could
still leave its usage missing from the local snapshot and Hub.

An explicit OpenCode snapshot now selects `usage_raw_events.source_system =
opencode` and its exact canonical `account_id`, independently of the model
vendor. The cached, incremental and SQL aggregation paths use the same source
and account boundary. Provider and model values remain unchanged, so their
breakdowns still identify the original vendors. Quota snapshots retain their
existing provider/account hydration path.

An empty OpenCode account does not fall back to a source-wide or provider-wide
aggregate. This intentionally avoids attributing another profile's usage to an
empty account. Configure a distinct account ID for each OpenCode profile and
use that same ID for its collector and hook. Unattributed older records are not
automatically reassigned. Other providers and the existing global
`telemetry.provider_links` policy are unchanged; this patch does not solve
unrelated provider fallback behavior.

No database migration, event rewrite, new adapter or Hub protocol change is
introduced. Tests cover multiple model vendors, separate personal/company
fixtures, another source with the same account ID, an empty sole account,
vendor preservation, cache separation and new-vendor incremental ingestion.
