---
title: Antigravity CLI
description: Track local Antigravity CLI transcript usage and optional TUI context and quota snapshots.
sidebar_label: Antigravity CLI
keywords: [antigravity cli usage tracker, agy quota tracking, antigravity token usage, antigravity status line]
---

# Antigravity CLI

The `antigravity` provider reads local CLI transcript usage and, when available, the latest custom status-line observation. It does not log in, switch accounts, read credentials, or make model or usage API requests.

## Two separate data sources

Native transcript collection reads `brain/*/.system_generated/logs/transcript.jsonl` beneath the configured CLI profile. Completed model response steps with a stable conversation/step identity, timestamp and numeric usage supply request history. Message text, prompts, tool arguments and responses are not retained in telemetry. Re-reading an unchanged transcript must not create additional requests. History covers only steps that reported all supported counters: a completed step with no counters remains unknown and is skipped, never converted to zero usage. Partially reported or invalid counters fail collection without importing a partial batch.

The optional status-line feed supplies the most recently observed context window, model label, session counters, current token components, quota remaining/reset times, plan tier and agent state. Its contract has a conversation ID but no request ID. State changes, repeated counts and token revisions therefore create **no usage-history or raw-envelope events**. Session totals are not attributed to whichever model happens to be active when the status line runs.

Transcript token components are normalized as input, output and cache-read traffic. A derived component sum is not an independently reported billing total; no dollar cost is inferred. Status-line current components remain separate because that contract does not establish their overlap. There is no fabricated `current_tokens` sum.

## Configuration and profile isolation

OpenUsage detects the default CLI installation when `agy` is on `PATH` and `~/.gemini/antigravity-cli/` contains `settings.json` or `brain/`. Configure additional profiles explicitly, with a unique account ID and native transcript root for each profile:

```json
{
  "accounts": [
    {
      "id": "agy-profile-one",
      "provider": "antigravity",
      "auth": "local",
      "binary": "agy",
      "provider_paths": {
        "transcripts_dir": "/path/to/profile-one/.gemini/antigravity-cli/brain",
        "status_file": "/path/to/private-state/profile-one-status.json"
      }
    }
  ]
}
```

The default transcript root is `~/.gemini/antigravity-cli/brain`. The default optional status file is `${XDG_STATE_HOME:-~/.local/state}/openusage/antigravity-status.json`. An explicit account never falls back to another account's transcript usage, including an empty profile configured on its own.

Event identity includes an explicit account namespace. If native history was copied between profiles, choose one source for that copied history; the reader does not infer cross-profile lineage. Renaming an account changes its namespace and requires a reviewed migration to avoid importing the same history again.

Distinct profile directories do not necessarily mean distinct billing accounts. Do not add their quota fractions together. Profiles that share a symlinked `.local` directory must use separate explicit status files. The collector's `provider_paths.status_file` and the status-line command's `--state-file` must name the same file for that profile.

## Optional TUI status-line capture

```bash
openusage integrations install antigravity
```

The native installer backs up the selected CLI `settings.json` to `settings.json.bak`, registers the current OpenUsage binary and enables `stack_with_default`. In that profile's `~/.gemini/antigravity-cli/settings.json`, a separate state file can be configured as follows:

```json
{
  "statusLine": {
    "type": "command",
    "command": "\"/path/to/openusage\" antigravity statusline --state-file \"/path/to/private-state/profile-one-status.json\"",
    "enabled": true,
    "stack_with_default": true
  }
}
```

Use literal absolute paths. Native install/upgrade preserves an existing explicit `--state-file`, including quoted paths with spaces. Commands containing shell expansion, pipelines or unrecognized options are refused for review rather than rewritten. Unrelated configuration is preserved. A different custom status-line command is not replaced.

The [official status-line contract](https://antigravity.google/docs/cli/statusline/) describes callbacks from the terminal UI when agent state changes. [Headless JSON and stream-json output](https://antigravity.google/docs/cli/headless/) are separate contracts. Installing this bridge does not demonstrate that a headless, ACP or Paseo session invokes it. Native transcript history does not depend on that callback, but TUI quota/context remains unavailable until an actual status-line payload is captured.

The command reads status-line JSON from stdin and prints a compact line such as `AGY · Gemini Pro · quota 94% · context 14%`. A synthetic input test proves parsing and storage only; it is not evidence of real usage or quota collection.

## Freshness and privacy

Status capture atomically replaces a `0600` file with a typed field allowlist. Email, transcript paths, arbitrary extra fields and credential fields are omitted; workspace values become short labels. Snapshot exports also omit those fields when reading older unsanitized state files.

The snapshot timestamp is the original capture time, with file modification time as a fallback for legacy files. Polling never makes an old observation fresh or extends a relative quota reset. Expired quota windows are omitted. If all reported windows have expired, the provider waits for fresh data. Missing status-line data is an unknown local snapshot state, not evidence that authentication failed. Collector health and transcript usage must be checked separately.

Completed native steps are treated as settled records. Conflicting duplicates within one file fail collection, but a later rewrite of an already imported completed step has no native revision authority: existing store deduplication can retain the original positive counters. OpenUsage does not invent revision authority from file modification or polling time. Such corrections require separately reviewed reconciliation.

Existing rows produced by an older status-line revision implementation are not automatically deleted or rewritten. Any historical correction requires a separately reviewed backup and cleanup operation.

## Limits and troubleshooting

- **No recent usage:** check that the account points to its own native `brain` directory and that completed model-response rows contain supported numeric counters. A readable source or installed bridge alone is not successful usage collection.
- **No quota/context:** confirm a natural TUI callback reached the matching status file. Headless history may be available while quota is still unavailable.
- **Stale snapshot:** the timestamp reflects the last observation. The collector does not call the model to refresh it.
- **Binary moved:** `openusage integrations list` reports an unavailable native command as outdated. `openusage integrations upgrade antigravity` updates the binary path while preserving the explicit state file.
- **Unsupported data:** model attribution absent from a transcript remains unknown. No dollar spend, token-per-second metric, account-manager proxy history, or credential extraction is added.
- **Local format changes:** the transcript reader supports the inspected CLI 1.4.3 schema. Unknown usage-bearing formats and partially reported counters fail closed. A trailing incomplete line waits for its newline; a changing file is retried. This is distinct from the documented status-line contract.
- **Collection bounds:** the reader accepts at most 10,000 root entries, 64 MiB per file, 4 MiB per line, 256 MiB per collection and 100,000 events per collection. It refuses symlinked transcript components. Exceeding a bound reports a collection error without importing a partial batch.

[Gemini CLI](./gemini-cli.md) is a separate provider with a different local data format.
