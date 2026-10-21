// Synthetic contract/lifecycle events only. No provider calls or host config.
import assert from "node:assert/strict"
import { pathToFileURL } from "node:url"
import { readdirSync, readFileSync } from "node:fs"
const [pluginPath, mode] = process.argv.slice(2)
const module = await import(pathToFileURL(pluginPath).href)
const wait = ms => new Promise(resolve => setTimeout(resolve, ms))
const until = async predicate => {
  const end = Date.now() + 6000
  while (!predicate()) {
    assert(Date.now() < end, "runtime condition timed out")
    await wait(10)
  }
}
const SECRET = "SYNTHETIC_CONTENT_MUST_NOT_LEAVE_HOST"
const usage = { input: 100, output: 30, reasoning: 5, cache: { read: 20, write: 2 } }
const event = (type, data) => ({
  id: "evt_synthetic", type, created: 1792540800000,
  location: { directory: "/synthetic/project" },
  data: { sessionID: "ses_test", assistantMessageID: "msg_test", ...data },
})
if (mode === "legacy") {
  // Official OpenCode v1.2.0 loader calls every distinct module export.
  const seen = new Set()
  let initialized = 0
  for (const factory of Object.values(module)) {
    if (seen.has(factory)) continue
    seen.add(factory)
    assert.equal(typeof factory, "function")
    const hooks = await factory({})
    initialized++
    await hooks["chat.message"](
      { sessionID: "ses_legacy", messageID: "msg_legacy", model: { providerID: "openai", modelID: "legacy-model" } },
      { message: { id: "msg_legacy", sessionID: "ses_legacy" }, model: { providerID: "openai", modelID: "legacy-model" }, usage: { input_tokens: 9, output_tokens: 4, total_tokens: 13 } },
    )
  }
  assert.equal(initialized, 1)
  await wait(100)
  console.log("legacy contract and hook execution passed")
} else {
const plugin = module.default
assert.equal(typeof plugin, "object")
assert.equal(plugin.id, "openusage-telemetry")
assert.equal(typeof plugin.setup, "function")
// OpenCode 2.0.32 and 2.1.1 apply this same Effect schema at load time.
if (process.env.OPENUSAGE_TEST_EFFECT_SCHEMA) {
  const Schema = await import(pathToFileURL(process.env.OPENUSAGE_TEST_EFFECT_SCHEMA).href)
  Schema.decodeUnknownSync(Schema.Struct({ default: Schema.Struct({
    id: Schema.String, setup: Schema.declare(value => typeof value === "function"),
  }) }))(module)
}
assert.equal(await plugin.setup({}), undefined)
process.env.OPENUSAGE_TELEMETRY_ENABLED = "false"
assert.equal(await plugin.setup({ event: { subscribe() { throw new Error("disabled subscription") } } }), undefined)
process.env.OPENUSAGE_TELEMETRY_ENABLED = "true"
let subscriptions = 0
let secondBatchDone = false
let aborted = false
const cleanup = await plugin.setup({
  location: { directory: "/synthetic/wrong-context" },
  event: {
    async *subscribe({ signal }) {
      subscriptions++
      try {
        if (["rejected", "offline", "ingest-failed", "malformed-ack"].includes(mode)) {
          yield event("session.step.started", { agent: "build", model: { providerID: "anthropic", id: "test-model" } })
          yield event("session.step.ended", { tokens: usage, cost: 0.12, native: SECRET, files: [SECRET] })
          secondBatchDone = true
        } else if (subscriptions === 1) {
          yield event("session.step.started", { agent: "build", model: { providerID: "anthropic", id: "test-model" }, started: 1792540799000, prompt: SECRET })
          yield event("session.text.delta", { text: SECRET })
          yield event("session.usage.updated", { tokens: { input: 999999 }, cost: 999 })
          yield event("session.step.ended", { tokens: usage, cost: 0.12, native: SECRET, providerState: SECRET, files: [SECRET] })
          throw new Error("synthetic stream interruption")
        } else {
          // Duplicate delivery must deduplicate against the stable message ID.
          yield event("session.step.ended", { tokens: usage, cost: 0.12 })
          // Missed start: retain usage; DB reconciliation supplies attribution.
          yield event("session.step.ended", { assistantMessageID: "msg_missed_start", tokens: usage, cost: 0.12 })
          yield event("session.step.failed", { assistantMessageID: "msg_no_usage", error: SECRET })
          yield event("session.step.started", { assistantMessageID: "msg_failed_usage", model: { providerID: "openai", id: "failed-model" } })
          yield event("session.step.failed", { assistantMessageID: "msg_failed_usage", tokens: usage, cost: 0.12, error: SECRET })
          secondBatchDone = true
        }
        if (!signal.aborted) await new Promise(resolve => signal.addEventListener("abort", resolve, { once: true }))
      } finally { if (signal.aborted) aborted = true }
    },
  },
})
assert.equal(typeof cleanup, "function")
await until(() => secondBatchDone)
const start = Date.now()
await cleanup()
assert(Date.now() - start < 1000, "cleanup did not release event subscription")
assert(aborted)
if (["rejected", "offline", "ingest-failed", "malformed-ack"].includes(mode)) {
  const files = readdirSync(process.env.OPENUSAGE_HOOK_SPOOL).filter(file => file.endsWith(".json"))
  assert.equal(files.length, 1, "failed delivery must spool once")
  assert(!readFileSync(`${process.env.OPENUSAGE_HOOK_SPOOL}/${files[0]}`, "utf8").includes(SECRET))
} else { assert.equal(subscriptions, 2) }
console.log(`v2 ${mode}: usage, privacy, reconnect, disabled and cleanup passed`)

}
