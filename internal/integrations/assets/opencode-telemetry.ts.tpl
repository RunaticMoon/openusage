import type { Plugin } from "@opencode-ai/plugin"
// openusage-integration-version: __OPENUSAGE_INTEGRATION_VERSION__

import { request as httpRequest } from "node:http"
import { existsSync, mkdirSync, writeFileSync, renameSync, readdirSync } from "node:fs"

type RuntimeConfig = {
  enabled: boolean
  accountID?: string
  verbose: boolean
}

type AnyRecord = Record<string, unknown>

function parseBool(value: string | undefined, defaultValue: boolean): boolean {
  if (value === undefined) {
    return defaultValue
  }
  const normalized = value.trim().toLowerCase()
  if (normalized === "" || normalized === "1" || normalized === "true" || normalized === "yes" || normalized === "on") {
    return true
  }
  if (normalized === "0" || normalized === "false" || normalized === "no" || normalized === "off") {
    return false
  }
  return defaultValue
}

function asRecord(value: unknown): AnyRecord | undefined {
  if (value && typeof value === "object") {
    return value as AnyRecord
  }
  return undefined
}

function pickString(...values: unknown[]): string {
  for (const value of values) {
    if (typeof value === "string") {
      const trimmed = value.trim()
      if (trimmed !== "") {
        return trimmed
      }
    }
  }
  return ""
}

function pickInt(...values: unknown[]): number {
  for (const value of values) {
    if (typeof value === "number" && Number.isFinite(value)) {
      return Math.trunc(value)
    }
    if (typeof value === "string") {
      const parsed = Number.parseInt(value, 10)
      if (Number.isFinite(parsed)) {
        return parsed
      }
    }
  }
  return 0
}

function pickPathString(root: unknown, ...paths: string[][]): string {
  for (const path of paths) {
    let current: unknown = root
    let found = true
    for (const key of path) {
      const rec = asRecord(current)
      if (!rec || !(key in rec)) {
        found = false
        break
      }
      current = rec[key]
    }
    if (!found) {
      continue
    }
    const resolved = pickString(current)
    if (resolved !== "") {
      return resolved
    }
  }
  return ""
}

function sanitizeUpstreamProvider(value: string): string {
  const trimmed = value.trim()
  if (trimmed === "") {
    return ""
  }
  const normalized = trimmed.toLowerCase()
  if (normalized === "openrouter" || normalized === "openusage" || normalized === "opencode" || normalized === "unknown") {
    return ""
  }
  return trimmed
}

function normalizeAgentName(value: unknown): string {
  if (typeof value === "string" && value.trim() !== "") {
    return value.trim()
  }
  const rec = asRecord(value)
  if (!rec) {
    return ""
  }
  return pickString(rec.name, rec.id, rec.type)
}

function normalizeModel(value: unknown): { providerID?: string; modelID?: string } {
  const rec = asRecord(value)
  if (!rec) {
    return {}
  }
  const providerID = pickString(rec.providerID, rec.provider_id, rec.provider)
  const modelID = pickString(rec.modelID, rec.model_id, rec.id, rec.model)
  const out: { providerID?: string; modelID?: string } = {}
  if (providerID) {
    out.providerID = providerID
  }
  if (modelID) {
    out.modelID = modelID
  }
  return out
}

function loadConfig(): RuntimeConfig {
  const accountID = process.env.OPENUSAGE_TELEMETRY_ACCOUNT_ID?.trim()
  return {
    enabled: parseBool(process.env.OPENUSAGE_TELEMETRY_ENABLED, true),
    accountID: accountID && accountID !== "" ? accountID : undefined,
    verbose: parseBool(process.env.OPENUSAGE_TELEMETRY_VERBOSE, false),
  }
}

function summarizeParts(parts: unknown): Record<string, number> {
  if (!Array.isArray(parts)) {
    return {}
  }

  const summary: Record<string, number> = {}
  for (const part of parts) {
    const typeValue = (part as { type?: unknown })?.type
    const key = typeof typeValue === "string" && typeValue.trim() !== ""
      ? typeValue.trim()
      : "unknown"
    summary[key] = (summary[key] || 0) + 1
  }
  return summary
}

function normalizeToolPayload(input: unknown, output: unknown): { input: AnyRecord; output: AnyRecord } {
  const inputRec = asRecord(input) || {}
  const outputRec = asRecord(output) || {}
  const outputData = asRecord(outputRec.output) || {}
  const normalizedInput: AnyRecord = {
    tool: pickString(inputRec.tool, inputRec.name, outputRec.tool, outputData.tool),
    sessionID: pickString(inputRec.sessionID, inputRec.sessionId, outputRec.sessionID, outputRec.sessionId),
    callID: pickString(
      inputRec.callID,
      inputRec.callId,
      inputRec.toolCallID,
      inputRec.tool_call_id,
      outputRec.callID,
      outputRec.callId,
      outputData.callID,
      outputData.callId,
    ),
  }
  const normalizedOutput: AnyRecord = {
    title: pickString(outputRec.title, outputData.title, outputRec.name),
  }
  return { input: normalizedInput, output: normalizedOutput }
}

function normalizeChatPayload(input: unknown, output: unknown): { input: AnyRecord; output: AnyRecord } {
  const inputRec = asRecord(input) || {}
  const outputRec = asRecord(output) || {}
  const inputMessage = asRecord(inputRec.message)
  const outputMessage = asRecord(outputRec.message)

  const outputModel = normalizeModel(outputRec.model || outputMessage?.model)
  const inputModel = normalizeModel(inputRec.model || inputMessage?.model)

  const sessionID = pickString(
    inputRec.sessionID,
    inputRec.sessionId,
    inputMessage?.sessionID,
    inputMessage?.sessionId,
    outputMessage?.sessionID,
    outputMessage?.sessionId,
  )
  const messageID = pickString(
    inputRec.messageID,
    inputRec.messageId,
    inputMessage?.id,
    outputMessage?.id,
  )

  const normalizedInput: AnyRecord = {
    sessionID,
    agent: normalizeAgentName(inputRec.agent),
    messageID,
    variant: pickString(inputRec.variant, asRecord(inputRec.agent)?.variant),
    model: {
      providerID: pickString(outputModel.providerID, inputModel.providerID),
      modelID: pickString(outputModel.modelID, inputModel.modelID),
    },
  }

  const outputUsage = asRecord(outputRec.usage) || asRecord(outputMessage?.usage) || {}
  const partsCount = pickInt(outputRec.parts_count, Array.isArray(outputRec.parts) ? outputRec.parts.length : 0)
  const upstreamProvider = sanitizeUpstreamProvider(pickString(
    pickPathString(outputRec,
      ["upstream_provider"],
      ["upstreamProvider"],
      ["route", "provider_name"],
      ["route", "providerName"],
      ["route", "provider"],
      ["routing", "provider_name"],
      ["routing", "providerName"],
      ["routing", "provider"],
      ["router", "provider_name"],
      ["router", "providerName"],
      ["router", "provider"],
      ["endpoint", "provider_name"],
      ["endpoint", "providerName"],
      ["endpoint", "provider"],
      ["provider_name"],
      ["providerName"],
      ["provider"],
    ),
    pickPathString(outputMessage,
      ["upstream_provider"],
      ["upstreamProvider"],
      ["provider_name"],
      ["providerName"],
      ["provider"],
      ["info", "provider_name"],
      ["info", "providerName"],
      ["info", "provider"],
    ),
    pickPathString(outputRec,
      ["model", "provider"],
      ["model", "provider_name"],
      ["model", "providerName"],
    ),
  ))

  const normalizedOutput: AnyRecord = {
    message: {
      id: pickString(outputMessage?.id, messageID),
      sessionID,
      role: pickString(outputMessage?.role, "assistant"),
    },
    model: {
      providerID: pickString(outputModel.providerID, inputModel.providerID),
      modelID: pickString(outputModel.modelID, inputModel.modelID),
    },
    usage: outputUsage,
    context: {
      parts_total: Array.isArray(outputRec.parts) ? outputRec.parts.length : 0,
      parts_by_type: summarizeParts(outputRec.parts),
    },
    parts_count: partsCount,
  }
  if (upstreamProvider !== "") {
    normalizedOutput.upstream_provider = upstreamProvider
  }

  return { input: normalizedInput, output: normalizedOutput }
}

function safeJSONStringify(value: unknown): string | undefined {
  try {
    const seen = new WeakSet<object>()
    return JSON.stringify(value, (_key, current) => {
      if (typeof current === "bigint") {
        return Number(current)
      }
      if (typeof current === "object" && current !== null) {
        if (seen.has(current)) {
          return undefined
        }
        seen.add(current)
      }
      return current
    })
  } catch {
    return undefined
  }
}

// On Windows the openusage daemon resolves its state dir to
// %APPDATA%\openusage\state (see telemetry.DefaultStateDir). XDG_STATE_HOME
// still wins when explicitly set, matching the Go side.
function windowsStateDir(): string {
  const stateHome = (process.env.XDG_STATE_HOME || "").trim()
  if (stateHome !== "") {
    return `${stateHome}\\openusage`
  }
  const appData = (process.env.APPDATA || "").trim()
  return `${appData}\\openusage\\state`
}

function resolveSocketPath(): string {
  const explicit = (process.env.OPENUSAGE_SOCKET || "").trim()
  if (explicit !== "") {
    return explicit
  }
  if (process.platform === "win32") {
    return `${windowsStateDir()}\\telemetry.sock`
  }
  const stateHome = (process.env.XDG_STATE_HOME || "").trim()
  const base = stateHome !== "" ? stateHome : `${process.env.HOME}/.local/state`
  return `${base}/openusage/telemetry.sock`
}

function resolveHookSpoolDir(): string {
  const explicit = (process.env.OPENUSAGE_HOOK_SPOOL || "").trim()
  if (explicit !== "") {
    return explicit
  }
  if (process.platform === "win32") {
    return `${windowsStateDir()}\\hook-spool`
  }
  const stateHome = (process.env.XDG_STATE_HOME || "").trim()
  const base = stateHome !== "" ? stateHome : `${process.env.HOME}/.local/state`
  return `${base}/openusage/hook-spool`
}

async function postToSocket(socketPath: string, path: string, body: string): Promise<boolean> {
  return new Promise((resolve) => {
    let resolved = false
    let timer: ReturnType<typeof setTimeout> | undefined
    const done = (ok: boolean) => {
      if (!resolved) { resolved = true; clearTimeout(timer); req.destroy(); resolve(ok) }
    }
    const req = httpRequest({
      socketPath, path, method: "POST", agent: false,
      headers: { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(body) },
    }, response => {
      if (!response.statusCode || response.statusCode < 200 || response.statusCode >= 300) { done(false); return }
      let text = ""
      response.setEncoding("utf8")
      response.on("data", chunk => {
        text += chunk
        if (text.length > 65536) done(false)
      })
      response.on("error", () => done(false))
      response.on("end", () => {
        try {
          // Native daemon acknowledges ingest failures with HTTP 200 too.
          // Replay is safe because usage records carry stable dedup IDs.
          const ack = asRecord(JSON.parse(text))
          done(!!ack && typeof ack.processed === "number" && typeof ack.failed === "number" && ack.failed === 0)
        } catch { done(false) }
      })
    })
    timer = setTimeout(() => done(false), 2000)
    req.on("error", () => done(false))
    req.end(body)
  })
}

async function spoolToDisk(source: string, accountID: string, payloadJSON: string, verbose: boolean): Promise<void> {
  const dir = resolveHookSpoolDir()
  try {
    if (!existsSync(dir)) { mkdirSync(dir, { recursive: true, mode: 0o700 }) }
    const files = readdirSync(dir).filter(f => f.endsWith(".json"))
    if (files.length >= 500) {
      if (verbose) { console.error("[openusage-telemetry] hook spool full (500 files)") }
      return
    }
    const ts = Math.floor(Date.now() / 1000)
    const rnd = Math.random().toString(16).slice(2, 10)
    const tmp = `${dir}/${ts}_${rnd}.json.tmp`
    const dst = `${dir}/${ts}_${rnd}.json`
    // Build record via interpolation — avoids JSON.parse round-trip on payload
    const record = `{"source":${JSON.stringify(source)},"account_id":${JSON.stringify(accountID)},"payload":${payloadJSON}}`
    writeFileSync(tmp, record + "\n", { mode: 0o600 })
    renameSync(tmp, dst)
  } catch (err) {
    if (verbose) { console.error(`[openusage-telemetry] spool write failed: ${err}`) }
  }
}

async function sendPayload(cfg: RuntimeConfig, payload: unknown): Promise<void> {
  const payloadJSON = safeJSONStringify(payload)
  if (!payloadJSON) {
    if (cfg.verbose) {
      console.error("[openusage-telemetry] payload serialization failed")
    }
    return
  }

  // Primary: POST to daemon unix socket (no process spawn).
  const socketPath = resolveSocketPath()
  let path = "/v1/hook/opencode"
  if (cfg.accountID) {
    path += `?account_id=${encodeURIComponent(cfg.accountID)}`
  }

  try {
    const ok = await postToSocket(socketPath, path, payloadJSON)
    if (ok) return
  } catch {
    // socket failed, fall through to spool
  }

  // Fallback: spool raw payload to disk for daemon pickup.
  await spoolToDisk("opencode", cfg.accountID || "", payloadJSON, cfg.verbose)
}

const OpenUsageTelemetry: Plugin = async () => {
  const cfg = loadConfig()
  if (!cfg.enabled) {
    return {}
  }

  let queue: Promise<void> = Promise.resolve()
  const enqueue = (payload: unknown): Promise<void> => {
    queue = queue
      .catch(() => undefined)
      .then(() => sendPayload(cfg, payload))
    return queue
  }

  return {
    async event(input) {
      enqueue({
        event: input.event,
      })
    },

    async "tool.execute.after"(input, output) {
      const normalized = normalizeToolPayload(input, output)
      enqueue({
        hook: "tool.execute.after",
        timestamp: Date.now(),
        input: normalized.input,
        output: normalized.output,
      })
    },

    async "chat.message"(input, output) {
      const normalized = normalizeChatPayload(input, output)
      enqueue({
        hook: "chat.message",
        timestamp: Date.now(),
        input: normalized.input,
        output: normalized.output,
      })
    },
  }
}


// Native v2 events are global and contain prompts, responses and tool results.
// Keep only completed per-message usage. Session usage.updated is cumulative
// and must never be added to message totals. DB reconciliation fills gaps in
// the explicitly volatile event stream (including starts missed at attach).
async function setup(ctx: any): Promise<(() => Promise<void>) | undefined> {
  const cfg = loadConfig()
  if (!cfg.enabled || typeof ctx?.event?.subscribe !== "function") return
  const controller = new AbortController()
  const starts = new Map<string, AnyRecord>()
  const delay = (ms: number) => new Promise<void>(resolve => {
    if (controller.signal.aborted) { resolve(); return }
    const done = () => { clearTimeout(timer); controller.signal.removeEventListener("abort", done); resolve() }
    const timer = setTimeout(done, ms)
    controller.signal.addEventListener("abort", done, { once: true })
  })
  const consume = (async () => {
    let retryMS = 250
    while (!controller.signal.aborted) {
      try {
        for await (const input of ctx.event.subscribe({ signal: controller.signal })) {
          if (controller.signal.aborted) break
          const event = asRecord(input)
          const data = asRecord(event?.data)
          const sessionID = pickString(data?.sessionID)
          const messageID = pickString(data?.assistantMessageID)
          if (!event || !data || !sessionID || !messageID) continue
          const key = `${sessionID}:${messageID}`
          if (event.type === "session.step.started") {
            starts.set(key, {
              model: normalizeModel(data.model), agent: pickString(data.agent),
              started: pickInt(data.started, event.created),
            })
            if (starts.size > 2048) starts.delete(starts.keys().next().value!)
            continue
          }
          if (event.type !== "session.step.ended" && event.type !== "session.step.failed") continue
          const start = starts.get(key)
          starts.delete(key)
          const tokens = asRecord(data.tokens)
          // A failed request may carry no usage. Do not turn it into a zero-
          // usage "successful" request or synthesize counters from estimates.
          if (!tokens) continue
          const cache = asRecord(tokens.cache)
          const clean: AnyRecord = {
            sessionID, assistantMessageID: messageID,
            model: start?.model || {}, agent: start?.agent || "opencode",
            tokens: {
              input: pickInt(tokens.input), output: pickInt(tokens.output),
              reasoning: pickInt(tokens.reasoning),
              cache: { read: pickInt(cache?.read), write: pickInt(cache?.write) },
            },
          }
          if (typeof data.cost === "number" && Number.isFinite(data.cost)) clean.cost = data.cost
          await sendPayload(cfg, { event: {
            type: event.type, created: pickInt(event.created),
            location: { directory: pickString(asRecord(event.location)?.directory, asRecord(ctx.location)?.directory) },
            data: clean,
          } })
          retryMS = 250
        }
      } catch {
        if (cfg.verbose && !controller.signal.aborted) console.error("[openusage-telemetry] event subscription interrupted; reconnecting")
      }
      if (!controller.signal.aborted) {
        await delay(retryMS)
        retryMS = Math.min(retryMS * 2, 5000)
      }
    }
  })().catch(() => {
    // Telemetry must not reject OpenCode's host lifecycle.
  })
  return async () => {
    controller.abort()
    await consume
    starts.clear()
  }
}

// OpenCode 1.2 calls every module export as a function; OpenCode 2 validates
// an object with id/setup. The installer selects one default, never both.
__OPENUSAGE_OPENCODE_EXPORT__
