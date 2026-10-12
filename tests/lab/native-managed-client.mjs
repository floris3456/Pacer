// Invoked by native-managed-client.py. All prompts, caller keys, and provider keys are dummy.
import assert from "node:assert/strict"
import { createHash } from "node:crypto"
import fs from "node:fs"
import path from "node:path"
import { pathToFileURL } from "node:url"

const [root, phase] = process.argv.slice(2)
const config = JSON.parse(fs.readFileSync(path.join(root, "manifest.json"), "utf8"))
const { PacerClient, ManagedCacheController, HttpError } = await import(pathToFileURL(config.sdk).href)
assert.equal(typeof ManagedCacheController, "function", "standalone SDK exports the durable lifecycle controller")
const recordsDir = path.join(root, "client-records")
const hostDir = path.join(root, "host-records")
fs.mkdirSync(recordsDir, { mode: 0o700, recursive: true })
fs.mkdirSync(hostDir, { mode: 0o700, recursive: true })
const checks = phase === "initial" ? [] : readJSON("checks.json")

function readFile(file) {
  try { return JSON.parse(fs.readFileSync(file, "utf8")) }
  catch (error) { if (error.code === "ENOENT") return undefined; throw error }
}
function readJSON(name) { return readFile(path.join(root, name)) }
function durable(file, data) {
  const temporary = file + ".tmp"
  const descriptor = fs.openSync(temporary, "w", 0o600)
  try { fs.writeFileSync(descriptor, JSON.stringify(data, null, 2) + "\n"); fs.fsyncSync(descriptor) }
  finally { fs.closeSync(descriptor) }
  fs.renameSync(temporary, file)
  const directory = fs.openSync(path.dirname(file), "r")
  try { fs.fsyncSync(directory) } finally { fs.closeSync(directory) }
}
function saveJSON(name, value) { durable(path.join(root, name), value) }
function checked(name) { checks.push(name); saveJSON("checks.json", checks); console.log("PASS", name) }
function record(scope, caller) {
  const matches = fs.readdirSync(recordsDir).map(name => readFile(path.join(recordsDir, name)))
    .filter(value => value?.scope.session_id === scope.session_id && value.connectionID === connectionID(caller))
  assert.equal(matches.length, 1, `exactly one durable record for ${scope.session_id}`)
  return matches[0]
}

// The fixture runs one Node process at a time. Synchronous CAS, file fsync and rename
// provide atomic durable writes across the explicit process restart in the Python driver.
const storage = {
  async read(key) { return readFile(path.join(recordsDir, createHash("sha256").update(key).digest("hex") + ".json")) },
  async compareAndSwap(key, expectedRevision, next) {
    const file = path.join(recordsDir, createHash("sha256").update(key).digest("hex") + ".json")
    const previous = readFile(file)
    if ((previous?.revision ?? null) !== expectedRevision) return false
    durable(file, next)
    return true
  },
}
function hostFile(scope) { return path.join(hostDir, scope.session_id + ".json") }
function seed(scope, empty, checkpoint) {
  assert.equal(readFile(hostFile(scope)), undefined)
  durable(hostFile(scope), { empty, ...(checkpoint ? { checkpoint } : {}), jobs: {} })
}
function saveHostJob(job, status) {
  const file = hostFile(job.scope)
  const state = readFile(file)
  state.jobs[job.id] = { transitionID: job.transitionID, previousCheckpoint: job.previousCheckpoint, ...status }
  if (status.status === "committed") { state.empty = false; state.checkpoint = status.checkpoint }
  durable(file, state)
}
const host = {
  async inspect(scope, signal) { signal.throwIfAborted(); const state = readFile(hostFile(scope)); return { empty: state.empty, checkpoint: state.checkpoint } },
  async drain(_scope, signal) { signal.throwIfAborted() }, // All fixture primary requests settle before mutation.
  async compaction(job, signal) {
    signal.throwIfAborted()
    const outcome = readFile(hostFile(job.scope)).jobs[job.id]
    if (!outcome) return { status: "not-started" }
    assert.equal(outcome.transitionID, job.transitionID)
    assert.equal(outcome.previousCheckpoint, job.previousCheckpoint)
    return { status: outcome.status, ...(outcome.checkpoint ? { checkpoint: outcome.checkpoint } : {}) }
  },
}

const losses = new Map()
async function transport(url, init) {
  const command = init?.method === "POST" && new URL(url).pathname === "/v1/plugins/pacer/cache-policy"
    ? JSON.parse(init.body) : undefined
  if (command) {
    const events = readJSON("wire-commands.json") ?? []
    // Control commands contain no key or private capability. Keep stable operation IDs as evidence.
    events.push({ phase, command })
    saveJSON("wire-commands.json", events)
  }
  const response = await fetch(url, init)
  const marker = command && `${command.session_id}:${command.operation}`
  if (response.ok && marker && losses.delete(marker)) {
    await response.arrayBuffer()
    throw new TypeError("intentional loss after the server accepted this control command")
  }
  return response
}
const clients = config.callers.map(key => new PacerClient({ baseURL: config.base, key, fetch: transport, timeoutMs: 5000 }))
const connectionID = caller => createHash("sha256").update(config.base + "\0" + config.callers[caller]).digest("hex")
const controller = caller => new ManagedCacheController({ client: clients[caller], storage, host, connectionID: connectionID(caller) })
const scope = session => ({ installation_id: config.installation, session_id: session, provider_route: config.route })
const sessions = [scope("caller-one"), scope("caller-two"), scope("caller-three")]
const failure = scope("helper-failure")
const stopped = scope("helper-stopped")

async function fixture(endpoint, body) {
  const response = await fetch(config.upstream + endpoint, body === undefined ? undefined : {
    method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body),
  })
  assert.equal(response.status, 200)
  return response.json()
}
const marker = { type: "ephemeral", ttl: "5m" }
function body(caller, text, auxiliary = false) {
  const content = [{ type: "text", text: `PACER_LAB_CALLER_${caller} ${text}`, cache_control: marker }]
  if (auxiliary) content.push({ type: "mcp_tool_result", tool_use_id: "local-fixture-tool",
    content: [{ type: "text", text: "Nested dummy tool result", cache_control: marker }], cache_control: marker })
  return {
    model: config.model, max_tokens: 64, stream: false,
    system: [{ type: "text", text: "Dummy native fixture context", cache_control: marker }],
    tools: [{ name: "local_unused_tool", description: "Dummy tool, never invoked", input_schema: { type: "object", properties: {} }, cache_control: marker }],
    messages: [{ role: "user", content }],
    ...(auxiliary ? { cache_control: marker, prompt_cache_options: { mode: "automatic" } } : {}),
  }
}
async function inference(caller, headers, text, signal, auxiliary = false) {
  const response = await fetch(config.base + "/v1/messages", {
    method: "POST", headers: { "Content-Type": "application/json", "Authorization": "Bearer " + config.callers[caller], ...headers },
    body: JSON.stringify(body(caller, text, auxiliary)), signal,
  })
  return { status: response.status, body: await response.json() }
}
function markers(value) {
  if (Array.isArray(value)) return value.flatMap(markers)
  if (!value || typeof value !== "object") return []
  return [...(value.cache_control ? [value.cache_control] : []),
    ...Object.entries(value).filter(([key]) => key !== "cache_control").flatMap(([, item]) => markers(item))]
}
function noLeakedCredentials(attempts) {
  const capabilities = fs.readdirSync(recordsDir).map(name => readFile(path.join(recordsDir, name))?.capability).filter(Boolean)
  for (const attempt of attempts) {
    assert.equal(attempt.coordination_leaked, false)
    const headers = JSON.stringify(attempt.headers)
    for (const privateValue of [...config.callers, ...capabilities]) assert.ok(!headers.includes(privateValue), "private client authority leaked to provider headers")
    assert.ok(!Object.keys(attempt.headers).some(name => /^(x-pacer-|x-quota-)/i.test(name)))
  }
}
async function waitFor(read, predicate, description) {
  const deadline = Date.now() + 10_000
  while (Date.now() < deadline) {
    const value = await read()
    if (predicate(value)) return value
    await new Promise(resolve => setTimeout(resolve, 100))
  }
  assert.fail(`Timed out waiting for ${description}`)
}
async function settled(caller, ownedScope) {
  const saved = record(ownedScope, caller)
  return waitFor(() => clients[caller].cachePolicy(ownedScope, saved.capability),
    state => state.active_primary === 0 && state.active_auxiliary === 0 && state.active_attempts === 0,
    `${ownedScope.session_id} native request completion`)
}
async function helper(control, caller, job, text = "PACER_LAB_SUMMARY") {
  const started = await control.beginCompaction(job)
  saveHostJob(started, { status: "pending" })
  const reply = await inference(caller, await control.auxiliaryHeaders(started), text, started.signal, true)
  assert.equal(reply.status, 200, JSON.stringify(reply.body))
  await settled(caller, started.scope)
  saveHostJob(started, { status: "committed", checkpoint: "checkpoint-" + started.id })
  return started
}
function ready(result) { assert.equal(result.status, "ready", JSON.stringify(result)); return result.lease }
function pending(result) { assert.equal(result.status, "compaction-required", JSON.stringify(result)); return result.job }

if (phase === "initial") {
  const api = await clients[0].capabilities()
  assert.equal(api.features.managed_auxiliary.enabled, true)
  assert.ok(api.features.cache_policy.contract_revision >= 3)
  assert.deepEqual(api.access, { mode: "granted", enforcement: "required-interceptors" })
  assert.ok(api.requirements.cache_routes.some(route => route.provider_route === config.route && route.models.includes(config.model)))
  checked("native-host-attested-managed-auxiliary-discovery")
  const denied = new PacerClient({ baseURL: config.base, key: "local-caller-no-grant" })
  await assert.rejects(denied.capabilities(), error => error instanceof HttpError && error.status === 403)
  const before = (await fixture("/_test/attempts")).length
  const unmanaged = await inference(0, {}, "PACER_LAB_UNMANAGED")
  assert.equal(unmanaged.status, 409)
  assert.equal((await fixture("/_test/attempts")).length, before)
  checked("ungranted-caller-and-unenrolled-primary-refused-before-provider")

  seed(sessions[0], true)
  seed(sessions[1], false, "checkpoint-before-existing-history")
  seed(sessions[2], true)
  let controls = [controller(0), controller(1), controller(2)]
  const leases = [ready(await controls[0].ensure(sessions[0], { ttl: "1h" })), undefined,
    ready(await controls[2].ensure(sessions[2], { ttl: "1h" }))]
  losses.set(`${sessions[1].session_id}:prepare`, true)
  await assert.rejects(controls[1].ensure(sessions[1], { ttl: "5m" }))
  const retainedPrepare = record(sessions[1], 1).command
  assert.equal(retainedPrepare.operation, "prepare")
  controls[1].dispose()
  controls[1] = controller(1)
  const enrollment = pending(await controls[1].ensure(sessions[1], { ttl: "5m" }))
  const preparePosts = readJSON("wire-commands.json").filter(event => event.command.session_id === sessions[1].session_id && event.command.operation === "prepare")
  assert.equal(preparePosts.length, 2)
  assert.deepEqual(preparePosts[0].command, preparePosts[1].command)
  const guarded = await clients[1].cachePolicy(sessions[1], record(sessions[1], 1).capability)
  assert.equal(guarded.enrollment_required, true)
  assert.equal(guarded.active.ttl, "")
  const blockedPrimary = await inference(1, {
    "X-Pacer-Cache-Protocol": "1", "X-Pacer-Cache-Installation": config.installation,
    "X-Pacer-Cache-Session": sessions[1].session_id, "X-Pacer-Cache-Route": config.route,
    "X-Pacer-Cache-Epoch": guarded.active.context_epoch, "X-Pacer-Cache-Revision": String(guarded.revision),
    "X-Pacer-Cache-Kind": "primary", "X-Pacer-Cache-Capability": record(sessions[1], 1).capability,
  }, "PACER_LAB_BEFORE_CHECKPOINT")
  assert.equal(blockedPrimary.status, 409)
  assert.equal((await fixture("/_test/attempts")).length, before)
  leases[1] = ready(await controls[1].completeCompaction(await helper(controls[1], 1, enrollment)))
  checked("lost-prepare-replays-stable-operation-after-sdk-reconnect")
  checked("existing-history-enrolls-only-after-correlated-durable-compaction")

  const replies = await Promise.all(controls.map(async (control, caller) => inference(caller,
    await control.headers(leases[caller], "primary"), "PACER_LAB_CONCURRENT", leases[caller].signal)))
  replies.forEach((reply, caller) => {
    assert.equal(reply.status, 200, JSON.stringify(reply.body))
    assert.equal(reply.body.content[0].text, `Local dummy answer from account ${caller}`)
  })
  await Promise.all(sessions.map((ownedScope, caller) => settled(caller, ownedScope)))
  assert.equal((await fixture("/_test/activity")).max_active, 3)
  checked("three-concurrent-callers-disjoint-grants-select-only-their-provider-account")
  const concurrent = (await fixture("/_test/attempts")).filter(attempt => attempt.mode === "CONCURRENT")
  for (const attempt of concurrent) {
    const controlsOnWire = markers(attempt.body)
    assert.ok(controlsOnWire.length > 0)
    assert.ok(controlsOnWire.every(value => value.ttl === (attempt.account === 1 ? "5m" : "1h")))
  }
  for (let caller = 0; caller < clients.length; caller++) {
    const usage = await clients[caller].usage()
    const own = config.authIDs[caller]
    assert.deepEqual(usage.credentials.map(credential => credential.id), [own])
    assert.ok((usage.active ?? []).every(id => id === own))
    for (const foreign of config.authIDs.filter(id => id !== own)) assert.ok(!JSON.stringify(usage).includes(foreign))
  }
  await assert.rejects(clients[1].cachePolicy(sessions[0], record(sessions[0], 0).capability),
    error => error instanceof HttpError && [403, 409].includes(error.status))
  checked("primary-cache-markers-use-acknowledged-ttl-and-caller-views-hide-foreign-ids")

  // Deliberately leave a durable summary before the first policy-commit command.
  const interruptedJob = pending(await controls[0].ensure(sessions[0], { ttl: "5m" }))
  const interrupted = await helper(controls[0], 0, interruptedJob, "PACER_LAB_SUMMARY_BEFORE_RESTART")
  assert.ok(record(sessions[0], 0).compaction)
  assert.equal(record(sessions[0], 0).command, undefined)
  assert.equal((await clients[0].cachePolicy(sessions[0], record(sessions[0], 0).capability)).active.ttl, "1h")

  // The server accepts this commit, but the client process never observes its response.
  const uncertainJob = pending(await controls[2].ensure(sessions[2], { ttl: "5m" }))
  const uncertain = await helper(controls[2], 2, uncertainJob, "PACER_LAB_SUMMARY_LOST_COMMIT")
  losses.set(`${sessions[2].session_id}:commit`, true)
  await assert.rejects(controls[2].completeCompaction(uncertain))
  const retainedCommit = record(sessions[2], 2).command
  assert.equal(retainedCommit.operation, "commit")
  assert.equal(retainedCommit.checkpoint_receipt.committed, true)
  assert.equal(retainedCommit.checkpoint_receipt.transition_id, uncertain.transitionID)
  saveJSON("restart-expectations.json", {
    firstCheckpoint: "checkpoint-" + interrupted.id, thirdCheckpoint: "checkpoint-" + uncertain.id,
    retainedCommit, attemptsBeforeRestart: 0,
  })

  seed(failure, false, "failed-helper-old-history")
  const failedControl = controller(1)
  const failedJob = await failedControl.beginCompaction(pending(await failedControl.ensure(failure, { ttl: "5m" })))
  saveHostJob(failedJob, { status: "pending" })
  const failedReply = await inference(1, await failedControl.auxiliaryHeaders(failedJob), "PACER_LAB_FAIL", failedJob.signal, true)
  assert.equal(failedReply.status, 400)
  await settled(1, failure)
  saveHostJob(failedJob, { status: "failed" })
  const failedResult = await failedControl.completeCompaction(failedJob)
  assert.equal(failedResult.status, "blocked")
  assert.equal(failedResult.reason, "compaction-failed")
  const failedState = await clients[1].cachePolicy(failure, record(failure, 1).capability)
  assert.equal(failedState.enrollment_required, true)
  assert.equal(failedState.last_commit, undefined)
  checked("failed-native-compaction-does-not-commit-or-implicitly-enroll")

  seed(stopped, false, "stopped-helper-old-history")
  const stopControl = controller(2)
  const stopJob = await stopControl.beginCompaction(pending(await stopControl.ensure(stopped, { ttl: "5m" })))
  saveHostJob(stopJob, { status: "pending" })
  // This intentionally uncooperative host helper ignores cancellation. Stop must
  // invalidate its old completion even if it later creates a durable checkpoint.
  const lateReply = inference(2, await stopControl.auxiliaryHeaders(stopJob), "PACER_LAB_STOP", undefined, true)
  await waitFor(() => fixture("/_test/attempts"), attempts => attempts.some(attempt => attempt.mode === "STOP"), "held helper dispatch")
  await stopControl.stop(stopped)
  assert.equal(stopJob.signal.aborted, true)
  await fixture("/_test/release", {})
  assert.equal((await lateReply).status, 200)
  await settled(2, stopped)
  saveHostJob(stopJob, { status: "committed", checkpoint: "checkpoint-" + stopJob.id })
  await assert.rejects(async () => stopControl.completeCompaction(stopJob))
  const stoppedState = await clients[2].cachePolicy(stopped, record(stopped, 2).capability)
  assert.equal(stoppedState.enrollment_required, true)
  assert.equal(stoppedState.last_commit, undefined)
  checked("stop-invalidates-late-completion-even-when-native-helper-ignores-abort")

  const attempts = await fixture("/_test/attempts")
  noLeakedCredentials(attempts)
  for (const attempt of attempts) {
    assert.ok(JSON.stringify(attempt.body).includes(`PACER_LAB_CALLER_${attempt.account}`))
    if (attempt.mode !== "CONCURRENT") assert.deepEqual(markers(attempt.body), [], "managed auxiliary has no cache markers on final provider wire")
  }
  checked("managed-auxiliary-strips-caller-and-automatic-cache-markers-on-final-wire")
  checked("final-provider-headers-contain-no-caller-key-private-capability-or-coordination-header")
  const expectations = readJSON("restart-expectations.json")
  expectations.attemptsBeforeRestart = attempts.length
  expectations.stoppedCheckpoint = "checkpoint-" + stopJob.id
  saveJSON("restart-expectations.json", expectations)
  for (const control of [...controls, failedControl, stopControl]) control.dispose()
} else if (phase === "restart") {
  const expectations = readJSON("restart-expectations.json")
  const controls = [controller(0), controller(1), controller(2)]
  const first = ready(await controls[0].ensure(sessions[0], { ttl: "5m" }))
  assert.equal(first.policy.active.checkpoint, expectations.firstCheckpoint)
  assert.equal(first.policy.active.ttl, "5m")
  const third = ready(await controls[2].ensure(sessions[2], { ttl: "5m" }))
  assert.equal(third.policy.active.checkpoint, expectations.thirdCheckpoint)
  const replayed = readJSON("wire-commands.json").filter(event => event.command.operation_id === expectations.retainedCommit.operation_id)
  assert.equal(replayed.length, 2)
  assert.deepEqual(replayed[0].command, replayed[1].command)
  const stoppedResult = ready(await controls[2].ensure(stopped, { ttl: "5m" }))
  assert.equal(stoppedResult.policy.active.checkpoint, expectations.stoppedCheckpoint)
  const failed = await controls[1].ensure(failure, { ttl: "5m" })
  assert.equal(failed.status, "blocked")
  assert.equal(failed.reason, "compaction-failed")
  assert.equal((await fixture("/_test/attempts")).length, expectations.attemptsBeforeRestart)
  checked("cpa-and-sdk-process-restart-reconciles-durable-checkpoints-with-zero-helper-replay")
  checked("lost-commit-replays-identical-durable-operation-after-process-restart")
  const reply = await inference(0, await controls[0].headers(first, "primary"), "PACER_LAB_AFTER_RESTART", first.signal)
  assert.equal(reply.status, 200, JSON.stringify(reply.body))
  assert.equal(reply.body.content[0].text, "Local dummy answer from account 0")
  await settled(0, sessions[0])
  const attempts = await fixture("/_test/attempts")
  noLeakedCredentials(attempts)
  assert.ok(markers(attempts.at(-1).body).every(value => value.ttl === "5m"))
  checked("managed-primary-resumes-on-own-account-with-new-ttl-after-restart")
  saveJSON("result.json", { passed: checks.length, checks, provider_calls: attempts.length,
    real_provider_calls: 0, native_process_restarts: 1 })
  for (const control of controls) control.dispose()
} else {
  assert.fail(`Unknown phase ${phase}`)
}
