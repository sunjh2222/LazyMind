import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { generateKeyPairSync } from "node:crypto";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { prepareUpdateBuildConfig } from "./prepare-update-build-config.mjs";

const publicKey = generateKeyPairSync("ed25519").publicKey
  .export({ type: "spki", format: "der" }).subarray(-32).toString("base64");
const configuration = {
  trustDomain: "windows-policy-fixture",
  audience: "official",
  channel: "stable",
  mode: "windows-policy",
  signingIdentity: "LazyMind Windows Release",
  updaterProtocol: 2,
  policyURL: "https://updates.example.invalid/windows/stable/policy.json",
  allowedOrigins: ["https://updates.example.invalid"],
  publicKeys: { release: publicKey },
};
const build = { configuration, packageVersion: "1.2.3", platform: "windows", arch: "amd64", signingMode: "none" };
const embedded = {
  ...configuration, version: "1.2.3", releaseId: "windows-amd64-1.2.3", platform: "windows", arch: "amd64",
};
const decode = encoded => JSON.parse(Buffer.from(encoded, "base64").toString("utf8"));

test("the Windows policy build embeds the supplied public trust and binds the stable release identity", () => {
  assert.deepEqual(decode(prepareUpdateBuildConfig(build)), embedded);
  const packageVersion = "1.2.3+windows-build.1";
  const candidate = decode(prepareUpdateBuildConfig({ ...build, packageVersion }));
  assert.equal(candidate.version, packageVersion);
  assert.equal(candidate.releaseId, "windows-amd64-1.2.3_windows-build.1");
});

test("the Windows policy build rejects inputs outside its platform, release, and trust contract", () => {
  for (const [reason, overrides] of [
    ["macOS target", { platform: "darwin", arch: "arm64" }],
    ["Windows ARM64 target", { arch: "arm64" }],
    ["Authenticode signing", { signingMode: "authenticode" }],
    ["ad-hoc signing", { signingMode: "adhoc" }],
    ["missing signing mode", { signingMode: undefined }],
    ["isolated test build", { isolatedTestBuild: true }],
    ["prerelease package", { packageVersion: "1.2.3-rc.1" }],
    ["invalid release version", { packageVersion: "1.2" }],
  ]) {
    assert.throws(() => prepareUpdateBuildConfig({ ...build, ...overrides }), undefined, reason);
  }
  for (const [reason, overrides] of [
    ["personal audience", { audience: "personal" }],
    ["canary channel", { channel: "canary" }],
    ["beta channel", { channel: "beta" }],
    ["v1 protocol", { updaterProtocol: 1 }],
    ["missing public keys", { publicKeys: undefined }],
    ["non-Ed25519 public key bytes", { publicKeys: { release: Buffer.alloc(31, 1).toString("base64") } }],
  ]) {
    const invalid = { ...build, configuration: { ...configuration, ...overrides } };
    assert.throws(() => prepareUpdateBuildConfig(invalid), undefined, reason);
  }
  const withoutMode = { ...configuration };
  delete withoutMode.mode;
  assert.throws(() => prepareUpdateBuildConfig({ ...build, configuration: withoutMode }));
});

test("existing official and QA update builds still require their native signing modes", () => {
  for (const [platform, arch, channel, signingMode] of [
    ["windows", "amd64", "stable", "authenticode"],
    ["windows", "amd64", "canary", "authenticode"],
    ["darwin", "arm64", "stable", "developer-id"],
  ]) {
    const official = { ...configuration, mode: "official", channel };
    const input = { ...build, configuration: official, platform, arch };
    assert.throws(() => prepareUpdateBuildConfig(input));
    assert.equal(decode(prepareUpdateBuildConfig({ ...input, signingMode })).mode, "official");
  }
});

test("the packaged-build CLI emits the same explicit Windows policy configuration", t => {
  const dir = mkdtempSync(path.join(os.tmpdir(), "windows-policy-build-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const packagePath = path.join(dir, "package.json");
  writeFileSync(packagePath, JSON.stringify({ version: build.packageVersion }));
  const script = fileURLToPath(new URL("./prepare-update-build-config.mjs", import.meta.url));
  const output = execFileSync(process.execPath, [
    script, "--platform", build.platform, "--arch", build.arch,
    "--signing-mode", build.signingMode, "--package", packagePath,
  ], {
    encoding: "utf8",
    env: { ...process.env, LAZYMIND_DESKTOP_UPDATE_CONFIG: "", LAZYMIND_DESKTOP_UPDATE_CONFIG_JSON: JSON.stringify(configuration) },
  });
  assert.deepEqual(decode(output.trim()), embedded);
});
