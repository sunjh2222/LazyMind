import assert from "node:assert/strict";
import { createRequire } from "node:module";
import path from "node:path";
import test from "node:test";

const require = createRequire(import.meta.url);
const { createDesktopUpdater } = require("../electron/src/update-controller");
const { createUpdateRuntimeClient } = require("../electron/src/update-runtime-client");

const root = path.resolve("windows-policy-download-fixture");
const release = {
  releaseId: "windows-amd64-1.1.0",
  version: "1.1.0",
  package: { size: 37, sha256: "a".repeat(64) },
  components: { size: 19, sha256: "b".repeat(64) },
  installedManifest: {
    url: "https://updates.example.invalid/releases/windows-amd64-1.1.0/installed-manifest.json",
    size: 11,
    sha256: "c".repeat(64),
  },
};
const receipt = {
  status: "downloaded",
  releaseId: release.releaseId,
  version: release.version,
  packagePath: path.join(root, "package.exe"),
  componentsPath: path.join(root, "components.json"),
  installedManifestPath: path.join(root, "installed-manifest.json"),
};

function createUpdater(t, downloadReceipt) {
  const updater = createDesktopUpdater({
    currentVersion: "1.0.0",
    readLocalState: async () => ({ systemVersion: "10.0.22631", dataProfile: "format-v2-fixture", dirty: false }),
    async invokeHelper(action) {
      if (action === "config") return { status: "enabled", config: { mode: "windows-policy", version: "1.0.0", updaterProtocol: 2 } };
      if (action === "discover") return { status: "direct", next: release, route: [release.releaseId] };
      if (action === "download") return downloadReceipt;
      throw new Error(`unexpected helper action: ${action}`);
    },
  });
  t.after(() => updater.dispose());
  return updater;
}

test("Windows policy shows the manifest bytes and accepts the complete verified receipt", async t => {
  const updater = createUpdater(t, receipt);
  const available = await updater.check();
  assert.equal(available.phase, "available");
  assert.equal(available.totalBytes, release.package.size + release.components.size + release.installedManifest.size);
  const downloaded = await updater.download(release.releaseId);
  assert.equal(downloaded.phase, "downloaded");
  assert.equal(downloaded.downloadedBytes, available.totalBytes);
});

test("Windows policy does not report a successful download without an absolute manifest path", async t => {
  for (const installedManifestPath of [undefined, "", "relative/installed-manifest.json", `${root}\0manifest.json`]) {
    await t.test(JSON.stringify(installedManifestPath) ?? "absent", async t => {
      const updater = createUpdater(t, { ...receipt, installedManifestPath });
      await updater.check();
      const result = await updater.download(release.releaseId);
      assert.equal(result.phase, "error");
      assert.equal(result.reason, "helper_protocol");
    });
  }
});

function createRuntime(calls) {
  return createUpdateRuntimeClient({
    async runSidecar(command, args) {
      assert.equal(command, "update-bootstrap");
      calls.push(args);
      if (args[1] === "prepare-components") return JSON.stringify({ status: "prepared", transactionId: "a".repeat(32) });
      assert.equal(args[1], "verify-components");
      return JSON.stringify({ status: "compatible", dataProfile: "format-v2-fixture" });
    },
  });
}

test("preparation gives the sidecar the verified manifest path and follow-up uses its transaction", async () => {
  const calls = [];
  const runtime = createRuntime(calls);
  await runtime.prepareComponents(release, receipt);
  const manifest = calls[0].indexOf("--installed-manifest");
  assert.ok(manifest >= 0);
  assert.equal(calls[0][manifest + 1], receipt.installedManifestPath);
  await runtime.verifyComponents(release, receipt);
  assert.deepEqual(calls[1], ["--action", "verify-components", "--transaction-id", "a".repeat(32)]);
});

test("preparation does not ask the sidecar to prepare a manifest-bearing release without a valid path", async t => {
  for (const installedManifestPath of [undefined, "relative/installed-manifest.json", `${root}\0manifest.json`]) {
    await t.test(JSON.stringify(installedManifestPath) ?? "absent", async () => {
      const calls = [];
      const runtime = createRuntime(calls);
      await assert.rejects(runtime.prepareComponents(release, { ...receipt, installedManifestPath }), { code: "installed_manifest_invalid" });
      assert.deepEqual(calls, []);
    });
  }
});

test("existing V2 preparation keeps its original arguments for a release without a manifest", async () => {
  const calls = [];
  const runtime = createRuntime(calls);
  const existingRelease = { releaseId: release.releaseId, package: release.package, components: release.components };
  await runtime.prepareComponents(existingRelease, { packagePath: receipt.packagePath, componentsPath: receipt.componentsPath });
  assert.deepEqual(calls, [["--action", "prepare-components", "--release-id", release.releaseId,
    "--package", receipt.packagePath, "--components", receipt.componentsPath, "--control-stdin"]]);
});
