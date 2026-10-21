# Follow-up scope

This inventory is based on the v0.26.1 source tree. Registered support is not a
claim that a specific host has authenticated or collected usage successfully.
No extra provider adapters are implemented by the maintenance work.

## Named follow-up products

| Name | Source evidence and next verification |
| --- | --- |
| Command Code | Already registered as `command_code` for commandcode.ai. The provider uses its API key/configured key environment or the CLI's local auth file, then the existing billing/limits/summary endpoints. It exposes credit balance, quota windows and aggregate requests/tokens/spend. Verify the installed CLI/product, personal account mapping and authorized API behavior; do not describe aggregate billing totals as request history. |
| Antigravity | Registered as `antigravity`; the maintenance change collects completed CLI 1.4.3 native transcript steps only when counters are reported, with explicit profile/account isolation. Status-line observations remain optional latest quota/context snapshots and create no request history. Read-only native source comparisons passed; runtime deployment and natural TUI capture are separate evidence. Headless/Paseo status-line callbacks remain unverified, older responses without counters are unknown, and rewritten settled records need reviewed reconciliation. No independent authentication/network calls or account-manager proxy collection. See the [provider contract](../site/docs/providers/antigravity.md) and [patch registry](patches.md). |
| Jev | No matching registered provider was found in this source inventory. Product/CLI identity and an official URL or actual local usage source remain unconfirmed. Determine whether it is a client, router or billing provider before proposing integration. |
| B.ai | No matching registered provider was found. Resolve the exact product, account/profile boundaries and supported export/API/local-log contract first; the name alone is insufficient. |
| TheHive | No matching registered provider was found. Resolve the exact product and source contract first; do not assume it is a model provider or reuse another product's credentials. |

Relevant source:
[`registry.go`](../../internal/providers/registry.go),
[`command_code.go`](../../internal/providers/command_code/command_code.go),
[`antigravity.go`](../../internal/providers/antigravity/antigravity.go).

The baseline registry constructs these provider packages:

```text
openai anthropic azure_openai alibaba_cloud openrouter perplexity groq mistral
moonshot deepseek xai zai opencode gemini_api gemini_cli antigravity ollama
copilot cursor claude_code codex command_code amp goose hermes mux droid crush
roocode kilocode kiro zed codebuff kimi_cli openclaw pi qwen_cli muse_code
```

For each future source, record product/version, supported official API/Hook/local
log, authentication location, actual payer/provider, personal versus company
account, available usage/quota fields, time range and maintenance risk. Reuse an
already configured OpenAI-compatible endpoint only after verifying the identity
and fields it actually returns. Keep credentials on their originating machine.

## Remaining operational validation

- Verify quota freshness separately from local request history and process/Hub
  liveness. A previously observed Mac snapshot had an older timestamp.
- Validate macOS Keychain access in the actual LaunchAgent context after the
  approved canary; interactive-shell authentication is not equivalent evidence.
- Inspect any missing historical rows/backfill range and compare the same period
  and session against OpenCode's own output. Do not erase checkpoints or replay
  into production until the preservation/idempotency plan is reviewed.
- Complete native CI and final integrated-SHA cross-version Hub/Collector checks,
  then request the separate first-machine canary approval.
- Revisit upstream fixes and retire only redundant patch differences after
  behavior/diff comparison; do not mechanically revert a matching upstream fix.

## Future UI and performance work

Future web/Paseo clients may consume only the documented existing
[latest-snapshot Hub contract](compatibility.md#hub-contract-retained-for-future-clients)
until a separate project changes it. They need to handle authentication,
collection/export/view polling delays, evicted machines and empty state after a
Hub restart. No persistent central history, web dashboard, Paseo plugin,
provider-wide credential collection, tok/s, TTFT or model benchmarks are added
here. These are independent follow-up projects, not hidden maintenance scope.
