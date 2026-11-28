import assert from "node:assert/strict";
import { createHash, generateKeyPairSync, sign, verify } from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { prepareRelease } from "./prepare-update-release.mjs";

const origin = "https://modelscope.cn";
const base = `${origin}/datasets/fixture/updates/resolve/master/updates/windows/stable/`;
const keyId = "windows-release";
const manifestFile = "resources/installed-manifest.json";
const pythonArchive = "resources/runtime/python-runtime.zip";
const componentCatalog = "resources/runtime/config/python-components.json";
const hashed = raw => createHash("sha256").update(raw).digest("hex");

function fixture(t, mode = "windows-policy") {
  const keys = generateKeyPairSync("ed25519");
  const privateKey = keys.privateKey.export({ type: "pkcs8", format: "pem" });
  const publicKey = keys.publicKey.export({ type: "spki", format: "der" }).subarray(-32).toString("base64");
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "windows-policy-release-"));
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  const appRoot = path.join(directory, "win-unpacked");
  const resources = path.join(appRoot, "resources");
  const packagePath = path.join(directory, "LazyMind-fixture.exe");
  const output = path.join(directory, "publication");
  const configuration = {
    trustDomain: "windows-official-fixture", audience: "official", channel: "stable", mode,
    releaseId: "windows-amd64-1.2.3", version: "1.2.3", platform: "windows", arch: "amd64",
    signingIdentity: "LazyMind Windows Release", updaterProtocol: 2,
    policyURL: base + "policy.json", allowedOrigins: [origin], publicKeys: { [keyId]: publicKey },
  };
  const files = new Map([
    ["LazyMind.exe", Buffer.from("public launcher after rcedit")],
    ["LazyMindApp.exe", Buffer.from("Electron application executable")],
    ["resources/app.asar", Buffer.from("compiled desktop app")],
    ["resources/runtime/bin/lazymind-update-helper.exe", Buffer.from("configured update helper")],
    ["resources/runtime/bin/local-runtime-manager.exe", Buffer.from("local runtime manager")],
    ["resources/runtime/bin/local-proxy.exe", Buffer.from("other packaged program")],
    [pythonArchive, Buffer.from("deferred packaged Python zip bytes")],
    ["resources/runtime/manifest.json", Buffer.from('{"version":1}')],
    [componentCatalog, Buffer.from(JSON.stringify({
      schemaVersion: 1, platform: "windows", arch: "amd64", components: {
        rag: {
          id: "rag", platform: "windows", arch: "amd64", pythonAbi: "cp311", baseFingerprint: "b".repeat(64),
          revision: "a".repeat(64), tag: "c".repeat(64), packages: { fixture: "1" },
          filename: "rag.zip", sizeBytes: 32, unpackedBytes: 64, sha256: "d".repeat(64),
          url: `${origin}/datasets/fixture/components/resolve/master/RAG/tags/${"c".repeat(64)}/rag.zip`,
          catalogUrl: `${origin}/datasets/fixture/components/resolve/master/RAG/tags/${"c".repeat(64)}/python-components.json`,
        },
      },
    }))],
    ["resources/app-update.yml", Buffer.from(JSON.stringify({ lazymind: {
      protocol: configuration.updaterProtocol, releaseId: configuration.releaseId, version: configuration.version,
      mode, platform: configuration.platform, arch: configuration.arch, signingIdentity: configuration.signingIdentity,
    } }))],
    ["resources/runtime/config/update-format-v2.json",
      fs.readFileSync(new URL("../../local/local-runtime-manager/update-format-v2.json", import.meta.url))],
  ]);
  const write = (relative, bytes) => {
    const filename = path.join(appRoot, relative);
    fs.mkdirSync(path.dirname(filename), { recursive: true });
    fs.writeFileSync(filename, bytes);
  };
  for (const [filename, raw] of files) write(filename, raw);
  fs.writeFileSync(packagePath, "fixture final NSIS EXE bytes");
  return { appRoot, resources, packagePath, output, configuration, keys, privateKey, files, write };
}

function addReleaseHelperFixture(f) {
  // Only the Windows PE helper's config command is substituted on non-Windows.
  const helper = path.join(f.resources, "runtime", "bin", "lazymind-update-helper");
  const config = JSON.stringify({ status: "enabled", config: f.configuration });
  fs.writeFileSync(helper, `#!${process.execPath}\nconsole.log(${JSON.stringify(config)});\n`, { mode: 0o755 });
}

function signedManifestFixture(f) {
  const { trustDomain, audience, channel, releaseId, version, platform, arch } = f.configuration;
  const files = [...f.files].map(([name, raw]) => ({ path: name, size: raw.length, sha256: hashed(raw) }));
  // Independently sign fixture bytes so the release stager tests reach the
  // existing entry point even when the manifest generator is absent.
  const payload = Buffer.from(JSON.stringify({
    schemaVersion: 1, kind: "windows-installed-manifest", trustDomain, audience, channel, releaseId, version, platform, arch, files,
  }));
  return Buffer.from(JSON.stringify({
    keyId, payload: payload.toString("base64"), signature: sign(null, payload, f.keys.privateKey).toString("base64"),
  }));
}

async function writeInstalledManifest(f, privateKey = f.privateKey) {
  const { writeWindowsInstalledManifest } = await import("./windows-policy-release.mjs");
  return writeWindowsInstalledManifest({ appRoot: f.appRoot, configuration: f.configuration, keyId, privateKey });
}

function stage(f) {
  return prepareRelease({
    resources: f.resources, packagePath: f.packagePath, output: f.output, baseURL: base, minimumSystemVersion: "10.0.19045",
  });
}

test("the final Windows installed manifest binds the staged executable, Python archive and component bytes", async t => {
  const f = fixture(t);
  await writeInstalledManifest(f);
  const envelope = JSON.parse(fs.readFileSync(path.join(f.appRoot, manifestFile), "utf8"));
  assert.equal(envelope.keyId, keyId);
  const payload = Buffer.from(envelope.payload, "base64");
  assert.equal(verify(null, payload, f.keys.publicKey, Buffer.from(envelope.signature, "base64")), true);
  const { files, ...identity } = JSON.parse(payload);
  const { trustDomain, audience, channel, releaseId, version, platform, arch } = f.configuration;
  assert.deepEqual(identity, { schemaVersion: 1, kind: "windows-installed-manifest", trustDomain, audience, channel, releaseId, version, platform, arch });
  const entries = new Map(files.map(file => [file.path, file]));
  assert.equal(entries.size, files.length, "no duplicate paths can be signed");
  assert.equal(entries.has(manifestFile), false, "the manifest cannot contain its own hash");
  for (const [name, raw] of f.files) {
    assert.deepEqual(entries.get(name), { path: name, size: raw.length, sha256: hashed(raw) });
  }
});

test("manifest generation rejects the wrong Ed25519 key and missing Python archive", async t => {
  const f = fixture(t);
  const wrongKey = generateKeyPairSync("ed25519").privateKey.export({ type: "pkcs8", format: "pem" });
  // Load the production entry point before checking expected failures.
  const { writeWindowsInstalledManifest } = await import("./windows-policy-release.mjs");
  const options = { appRoot: f.appRoot, configuration: f.configuration, keyId, privateKey: f.privateKey };
  await assert.rejects(async () => writeWindowsInstalledManifest({ ...options, privateKey: wrongKey }));
  assert.equal(fs.existsSync(path.join(f.appRoot, manifestFile)), false);
  fs.rmSync(path.join(f.appRoot, pythonArchive));
  await assert.rejects(async () => writeWindowsInstalledManifest(options));
  assert.equal(fs.existsSync(path.join(f.appRoot, manifestFile)), false);
});

test("the real release stager describes one final EXE and its exact embedded manifest", { skip: process.platform === "win32" }, async t => {
  const f = fixture(t);
  addReleaseHelperFixture(f);
  const manifest = signedManifestFixture(f);
  f.write(manifestFile, manifest);
  const record = await stage(f);
  const { releaseId } = f.configuration;
  const output = path.join(f.output, releaseId);
  const exe = fs.readFileSync(f.packagePath);
  assert.deepEqual(fs.readFileSync(path.join(output, "package.exe")), exe);
  assert.deepEqual(fs.readFileSync(path.join(output, "installed-manifest.json")), manifest);
  const staged = JSON.parse(fs.readFileSync(path.join(output, "release.json"), "utf8"));
  assert.deepEqual(staged, record);
  assert.deepEqual(staged.config, f.configuration);
  assert.equal(staged.release.releaseId, releaseId);
  const expected = (filename, raw) => ({ url: `${base}releases/${releaseId}/${filename}`, size: raw.length, sha256: hashed(raw) });
  const { unpackedBytes, ...packageArtifact } = staged.release.package;
  assert.ok(unpackedBytes > 0);
  assert.deepEqual(packageArtifact, expected("package.exe", exe));
  assert.deepEqual(staged.release.installedManifest, expected("installed-manifest.json", manifest));
  assert.deepEqual(staged.release.components, expected("components.json", f.files.get(componentCatalog)));
});

test("the release stager preserves the existing Authenticode record layout", { skip: process.platform === "win32" }, async t => {
  const f = fixture(t, "official");
  addReleaseHelperFixture(f);
  const record = await stage(f);
  const output = path.join(f.output, f.configuration.releaseId);
  assert.equal(record.config.mode, "official");
  assert.equal(Object.hasOwn(record.release, "installedManifest"), false);
  assert.deepEqual(fs.readdirSync(output).sort(), ["components.json", "feed.json", "package.exe", "release.json"]);
});
