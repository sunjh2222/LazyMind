import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { EventEmitter } from "node:events";
import { existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import path from "node:path";
import { PassThrough } from "node:stream";
import test from "node:test";
import { fileURLToPath } from "node:url";

const require = createRequire(import.meta.url);
const repoSource = new URL("../electron/src/", import.meta.url);
const source = existsSync(repoSource) ? repoSource : new URL("../../../../../LazyMind/desktop/electron/src/", import.meta.url);
const { createNativeUpdateInstaller } = require(fileURLToPath(new URL("native-update-installer.js", source)));
const { createUpdateInstallFlow } = require(fileURLToPath(new URL("update-install-flow.js", source)));
const settle = () => new Promise(resolve => setImmediate(resolve));
const hash = bytes => createHash("sha256").update(bytes).digest("hex");
const transactionId = "a".repeat(32);
const timeout = 1000;

async function waitFor(predicate, message) {
  const deadline = Date.now() + 3000;
  while (!predicate()) {
    assert.ok(Date.now() < deadline, message);
    await settle();
  }
}

function nativeFixture(t, { host = {}, config = {} } = {}) {
  const root = mkdtempSync(path.join(tmpdir(), "lazymind-windows-policy-handoff-"));
  const resourcesPath = path.join(root, "resources");
  const helperPath = path.join(resourcesPath, "runtime", "bin", "lazymind-update-helper.exe");
  mkdirSync(path.dirname(helperPath), { recursive: true });
  writeFileSync(helperPath, "synthetic helper, never executed");
  const configuration = { mode: "windows-policy", platform: "windows", arch: "amd64", audience: "official", channel: "stable",
    trustDomain: "fixture", releaseId: "source", version: "1.0.0", signingIdentity: "Fixture Windows Release", updaterProtocol: 2, ...config };
  const metadata = { updaterCacheDirName: `LazyMind-${configuration.trustDomain}-${configuration.audience}-${configuration.channel}`,
    lazymind: { protocol: configuration.updaterProtocol, releaseId: configuration.releaseId, version: configuration.version,
      mode: configuration.mode, platform: configuration.platform, arch: configuration.arch, signingIdentity: configuration.signingIdentity } };
  const metadataPath = path.join(resourcesPath, "app-update.yml");
  writeFileSync(metadataPath, JSON.stringify(metadata));
  const fixtureArtifact = (name, contents) => {
    const bytes = Buffer.from(contents), filename = path.join(root, name);
    writeFileSync(filename, bytes);
    return { filename, descriptor: { url: `https://updates.example.invalid/${name}`, size: bytes.length, sha256: hash(bytes) } };
  };
  const pkg = fixtureArtifact("package.exe", "synthetic installer, never executed");
  const catalog = fixtureArtifact("components.json", "synthetic component catalog");
  const manifest = fixtureArtifact("installed-manifest.json", "synthetic installed manifest");
  const release = { releaseId: "target", version: "1.1.0", platform: configuration.platform, arch: configuration.arch,
    signingIdentity: configuration.signingIdentity, package: { ...pkg.descriptor, unpackedBytes: 4096 }, components: catalog.descriptor,
    installedManifest: manifest.descriptor };
  const receipt = { releaseId: release.releaseId, version: release.version, dataProfile: "fixture-data-profile",
    packagePath: pkg.filename, componentsPath: catalog.filename, installedManifestPath: manifest.filename };
  const child = new EventEmitter();
  child.stdout = new PassThrough();
  child.stderr = new PassThrough();
  child.unref = () => {};
  child.kill = () => true;
  const spawns = [], quitting = [], quits = [];
  const installer = createNativeUpdateInstaller({ platform: "win32", arch: "x64", currentVersion: configuration.version,
    isPackaged: true, resourcesPath, handoffTimeoutMs: timeout, ...host,
    createUpdater: () => assert.fail("Windows policy handoff created electron-updater"),
    spawn(file, args, options) { spawns.push({ file, args, options }); return child; },
    setQuitting: value => quitting.push(value), quit: () => quits.push("quit"),
  });
  t.after(() => { installer.dispose(); child.stdout.destroy(); child.stderr.destroy(); rmSync(root, { recursive: true, force: true }); });
  return { installer, input: { configuration, release, receipt, transactionId }, metadata, metadataPath, child, helperPath, spawns, quitting, quits };
}

function acknowledge(child, id = transactionId) {
  child.stdout.write(JSON.stringify({ status: "handoff_accepted", transactionId: id }) + "\n");
}

async function startHandoff(f, stage) {
  let settled = false;
  const outcome = stage.install().then(value => { settled = true; return { value }; }, error => { settled = true; return { error }; });
  await waitFor(() => f.spawns.length || settled, "handoff did not start");
  assert.equal(f.spawns.length, 1, "handoff must spawn exactly one trusted helper");
  return { outcome, settled: () => settled };
}

test("Windows policy eligibility is limited to packaged Windows x64 official stable V2", async t => {
  const valid = nativeFixture(t);
  assert.equal(valid.installer.capability(valid.input.configuration).supported, true);
  for (const options of [
    { host: { isPackaged: false } }, { host: { platform: "darwin" }, config: { platform: "darwin" } },
    { host: { arch: "arm64" }, config: { arch: "arm64" } }, { config: { audience: "fixture-fork" } },
    { config: { channel: "canary" } }, { config: { channel: "beta" } }, { config: { updaterProtocol: 1 } },
    { config: { version: "1.0.0-rc.1" } },
  ]) {
    const invalid = nativeFixture(t, options);
    assert.equal(invalid.installer.capability(invalid.input.configuration).supported, false, JSON.stringify(options));
    await assert.rejects(invalid.installer.prepare(invalid.input));
    assert.deepEqual(invalid.spawns, []);
  }
  await assert.rejects(valid.installer.prepare({ ...valid.input, transactionId: undefined }));
  await assert.rejects(valid.installer.prepare({ ...valid.input, transactionId: "../foreign" }));
  assert.deepEqual(valid.spawns, []);
});

test("Windows policy starts only the bound helper and quits A after the matching full acknowledgment", async t => {
  const f = nativeFixture(t);
  rmSync(f.metadataPath);
  const stage = await f.installer.prepare(f.input);
  assert.deepEqual(f.spawns, [], "prepare must not start a helper or installer");
  const handoff = await startHandoff(f, stage);
  const [{ file, args, options }] = f.spawns;
  assert.equal(file, f.helperPath);
  assert.deepEqual(args, ["windows-handoff", transactionId]);
  assert.equal(options.shell, false);
  assert.equal(options.detached, true);
  assert.ok(options.stdio === "pipe" || options.stdio?.[1] === "pipe", "the helper acknowledgment must have a stdout pipe");
  f.child.emit("spawn");
  const line = JSON.stringify({ status: "handoff_accepted", transactionId });
  f.child.stdout.write(line.slice(0, 17));
  await settle();
  assert.equal(handoff.settled(), false);
  assert.deepEqual(f.quitting, []);
  assert.deepEqual(f.quits, []);
  f.child.stdout.write(line.slice(17));
  await settle();
  assert.equal(handoff.settled(), false, "the acknowledgment is incomplete until its newline");
  assert.deepEqual(f.quitting, []);
  assert.deepEqual(f.quits, []);
  f.child.stdout.write("\n");
  const result = await handoff.outcome;
  assert.equal(result.error, undefined);
  assert.equal(result.value.handoff, true);
  assert.deepEqual(f.quitting, [true]);
  assert.deepEqual(f.quits, ["quit"]);
  assert.equal(f.spawns.length, 1);
});

function exitHelper(child, code) {
  child.stdout.end();
  child.emit("exit", code, null);
  child.emit("close", code, null);
}

for (const [name, finish] of [
  ["spawn failure", child => child.emit("error", new Error("fixture spawn failure"))],
  ["empty acknowledgment", child => exitHelper(child, 0)],
  ["malformed acknowledgment", child => {
    const incomplete = JSON.stringify({ status: "handoff_accepted", transactionId }).slice(0, -1);
    child.stdout.write(incomplete + "\n");
    exitHelper(child, 0);
  }],
  ["wrong transaction", child => { acknowledge(child, "b".repeat(32)); exitHelper(child, 0); }],
  ["wrong acknowledgment status", child => {
    child.stdout.write(JSON.stringify({ status: "blocked", transactionId }) + "\n");
    exitHelper(child, 0);
  }],
  ["failed helper exit", child => exitHelper(child, 7)],
]) {
  test(`Windows policy keeps A alive after ${name}`, async t => {
    const f = nativeFixture(t);
    const handoff = await startHandoff(f, await f.installer.prepare(f.input));
    if (name !== "spawn failure") f.child.emit("spawn");
    finish(f.child);
    const result = await handoff.outcome;
    assert.ok(result.error instanceof Error);
    assert.ok(!f.quitting.includes(true));
    assert.deepEqual(f.quits, []);
  });
}

test("Windows policy timeout cannot quit A on a late matching acknowledgment", async t => {
  const f = nativeFixture(t);
  const stage = await f.installer.prepare(f.input);
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const handoff = await startHandoff(f, stage);
  f.child.emit("spawn");
  t.mock.timers.tick(timeout - 1);
  await settle();
  assert.equal(handoff.settled(), false);
  t.mock.timers.tick(1);
  assert.ok((await handoff.outcome).error instanceof Error);
  acknowledge(f.child);
  await settle();
  assert.ok(!f.quitting.includes(true));
  assert.deepEqual(f.quits, []);
});

test("legacy signed Windows still requires matching publisher metadata", t => {
  const f = nativeFixture(t, { config: { mode: "official", updaterProtocol: 1 } });
  assert.equal(f.installer.capability(f.input.configuration).supported, false);
  writeFileSync(f.metadataPath, JSON.stringify({ ...f.metadata, publisherName: [f.input.configuration.signingIdentity] }));
  assert.equal(f.installer.capability(f.input.configuration).supported, true);
});

test("installation flow forwards the prepared transaction and waits for actual runtime stop", async t => {
  const f = nativeFixture(t);
  const { configuration, release, receipt } = f.input;
  const offer = { status: "direct", next: release, route: [release.releaseId] };
  let resumeStop, state = {}, saved, nativeInput;
  let stopEntered = false, stopCompleted = false, handoffStarted = false;
  const stop = new Promise(resolve => { resumeStop = resolve; });
  const flow = createUpdateInstallFlow({
    currentVersion: configuration.version,
    store: { read: () => saved, write: record => { saved = record; } },
    context: () => ({ configuration, offer, receipt }), download: async () => {},
    rediscover: async () => ({ decision: offer, local: { dirty: false, dataProfile: receipt.dataProfile } }),
    runtime: {
      prepareComponents: async () => ({ status: "prepared", transactionId, dataProfile: receipt.dataProfile, sourceHadRAG: false, componentMode: "absent" }),
      verifyComponents: async () => ({ dataProfile: receipt.dataProfile }),
      status: async () => ({ active: 0 }), pause: async epoch => ({ active: 0, paused: true, epoch }), resume: async () => {},
      async stop() { stopEntered = true; await stop; stopCompleted = true; },
    },
    nativeInstaller: {
      capability: () => ({ supported: true }), dispose() {},
      async prepare(input) { nativeInput = input; return { async install() { handoffStarted = true; return { handoff: true }; } }; },
    },
    clock: { setTimeout: setImmediate, clearTimeout: clearImmediate },
    publish: change => { state = { ...state, ...change }; }, getState: () => state,
  });
  t.after(() => { flow.dispose(); resumeStop(); });
  await flow.begin(release.releaseId);
  await waitFor(() => stopEntered, "installation did not reach runtime stop");
  assert.equal(saved.phase, "installing");
  assert.equal(saved.transactionId, transactionId);
  assert.equal(stopCompleted, false);
  assert.equal(handoffStarted, false);
  resumeStop();
  await waitFor(() => handoffStarted, "handoff did not start after runtime stop");
  assert.equal(stopCompleted, true);
  assert.equal(nativeInput.transactionId, transactionId);
});
