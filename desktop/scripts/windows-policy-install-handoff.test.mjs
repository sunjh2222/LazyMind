import assert from "node:assert/strict";
import { createRequire } from "node:module";
import os from "node:os";
import path from "node:path";
import test from "node:test";

const require = createRequire(import.meta.url);
const { createUpdateInstallFlow } = require("../electron/src/update-install-flow");
const settle = () => new Promise(resolve => setImmediate(resolve));

const sourceVersion = "1.2.3";
const releaseId = "windows-amd64-1.2.4";
const profile = "format-v2-" + "f".repeat(64);
const base = "https://modelscope.cn/datasets/fixture/updates/resolve/master/windows/stable/releases/" + releaseId;
const installedManifest = { url: base + "/installed-manifest.json", size: 71, sha256: "c".repeat(64) };

function fixture(t, { initial = "complete", final = installedManifest } = {}) {
  const release = {
    releaseId, version: "1.2.4", platform: "windows", arch: "amd64", signingIdentity: "LazyMind Windows Release",
    package: { url: base + "/package.exe", size: 128, sha256: "a".repeat(64) },
    components: { url: base + "/components.json", size: 64, sha256: "b".repeat(64) },
  };
  if (initial !== "missing offer manifest") release.installedManifest = { ...installedManifest };
  const local = name => path.join(os.tmpdir(), "windows-policy-install-handoff-fixture", name);
  const receipt = {
    status: "downloaded", releaseId, version: release.version, dataProfile: profile,
    packagePath: local("package.exe"), componentsPath: local("components.json"),
  };
  if (initial !== "missing receipt manifest") receipt.installedManifestPath = local("installed-manifest.json");
  const offer = { status: "direct", next: release, route: [releaseId] };
  const calls = [];
  let intent, pausedEpoch, state = {};
  const runtime = {
    async prepareComponents() {
      calls.push("prepare-components");
      return {
        status: "prepared", transactionId: "d".repeat(32), dataProfile: profile,
        sourceHadRAG: false, componentMode: "absent",
      };
    },
    async status() { return { active: 0 }; },
    async pause(epoch) {
      calls.push("pause");
      pausedEpoch = epoch;
      return { active: 0, paused: true, epoch };
    },
    async resume(epoch) {
      calls.push("resume");
      assert.equal(epoch, pausedEpoch);
      pausedEpoch = undefined;
    },
    async verifyComponents() {
      calls.push("verify-components");
      return { dataProfile: profile };
    },
    async stop(epoch) {
      calls.push("stop");
      assert.equal(epoch, pausedEpoch);
      assert.equal(intent.phase, "installing");
      assert.equal(intent.epoch, epoch);
    },
  };
  const flow = createUpdateInstallFlow({
    currentVersion: sourceVersion,
    store: { read: () => intent, write: record => { intent = record; } },
    runtime,
    context: () => ({
      configuration: { mode: "windows-policy", updaterProtocol: 2, platform: "windows", arch: "amd64" },
      offer, receipt,
    }),
    download: async () => {},
    async rediscover() {
      calls.push("rediscover");
      const decision = structuredClone(offer);
      if (final === null) delete decision.next.installedManifest;
      else decision.next.installedManifest = { ...final };
      return { decision, local: { dirty: false, dataProfile: profile } };
    },
    nativeInstaller: {
      capability: () => ({ supported: true }),
      dispose() {},
      async prepare() {
        calls.push("prepare-native");
        return { async install() { calls.push("install"); return { handoff: true }; } };
      },
    },
    clock: { setTimeout: setImmediate, clearTimeout: clearImmediate },
    publish: change => { state = { ...state, ...change }; },
    getState: () => state,
  });
  t.after(() => flow.dispose());
  return {
    calls, paused: () => pausedEpoch !== undefined, view: () => state,
    async install() {
      try { await flow.begin(releaseId); }
      catch (error) { return error; }
      for (let tick = 0; tick < 30; tick++) {
        await settle();
        if (calls.includes("install") || ["error", "blocked", "repair"].includes(state.phase)) return;
      }
      assert.fail("installation neither handed off nor rejected the offer");
    },
  };
}

test("an unchanged Windows policy manifest reaches the guarded handoff", async t => {
  const f = fixture(t);
  assert.equal(await f.install(), undefined);
  assert.deepEqual(f.calls.filter(call => call !== "pause"), [
    "prepare-components", "prepare-native", "rediscover", "verify-components", "stop", "install",
  ]);
  assert.equal(f.view().phase, "installing");
});

for (const [change, final] of [
  ["SHA-256", { ...installedManifest, sha256: "e".repeat(64) }],
  ["size", { ...installedManifest, size: 72 }],
  ["URL", { ...installedManifest, url: base + "/other/installed-manifest.json" }],
  ["removal", null],
]) {
  test(`a final Windows policy manifest change (${change}) rejects handoff and resumes admission`, async t => {
    const f = fixture(t, { final });
    assert.equal(await f.install(), undefined);
    assert.deepEqual(f.calls.filter(call => ["rediscover", "resume", "stop", "install"].includes(call)), ["rediscover", "resume"]);
    assert.equal(f.view().phase, "error");
    assert.equal(f.view().reason, "offer_changed");
    assert.equal(f.paused(), false);
  });
}

for (const initial of ["missing offer manifest", "missing receipt manifest"]) {
  test(`Windows policy rejects ${initial} before any preparation`, async t => {
    const f = fixture(t, { initial });
    const rejected = await f.install();
    assert.deepEqual(f.calls.filter(call => ["prepare-components", "prepare-native", "stop", "install"].includes(call)), []);
    assert.ok(rejected instanceof Error || ["error", "blocked"].includes(f.view().phase));
    assert.equal(f.paused(), false);
  });
}
