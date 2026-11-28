import { createHash, createPrivateKey, createPublicKey, sign, verify } from "node:crypto";
import { promises as fs } from "node:fs";
import { execFileSync } from "node:child_process";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { isDeepStrictEqual } from "node:util";
import { prepareUpdateBuildConfig } from "./prepare-update-build-config.mjs";

const manifestName = "resources/installed-manifest.json";
const maxEnvelope = 1024 * 1024;
const ed25519Prefix = Buffer.from("302a300506032b6570032100", "hex");
const requiredFiles = [
  "LazyMind.exe", "LazyMindApp.exe", "resources/app.asar", "resources/app-update.yml",
  "resources/runtime/bin/lazymind-update-helper.exe", "resources/runtime/bin/local-runtime-manager.exe",
  "resources/runtime/config/python-components.json", "resources/runtime/python-runtime.zip",
];

function manifestIdentity(configuration) {
  if (configuration?.mode !== "windows-policy" || !configuration.releaseId) {
    throw new Error("Expected the built Windows policy configuration");
  }
  prepareUpdateBuildConfig({ configuration, packageVersion: configuration.version, platform: "windows", arch: "amd64", signingMode: "none" });
  const { trustDomain, audience, channel, releaseId, version, platform, arch } = configuration;
  return { schemaVersion: 1, kind: "windows-installed-manifest", trustDomain, audience, channel, releaseId, version, platform, arch };
}

function validateFileName(name) {
  if (typeof name !== "string" || name.split("/").some(part => !part || part === "." || part === ".." || /[. ]$/.test(part))
    || /[\\:<>"|?*\x00]/.test(name)) throw new Error("Invalid installed file path");
}

async function fileDigest(appRoot, name) {
  validateFileName(name);
  const file = path.join(appRoot, name);
  const expected = await fs.lstat(file);
  if (!expected.isFile() || expected.nlink !== 1) throw new Error("Expected an unaliased installed file");
  const stream = await fs.open(file, "r");
  try {
    const before = await stream.stat();
    if (before.dev !== expected.dev || before.ino !== expected.ino || before.size !== expected.size || before.nlink !== 1) {
      throw new Error("Installed file changed before verification");
    }
    const hash = createHash("sha256");
    let size = 0;
    for await (const chunk of stream.createReadStream({ autoClose: false })) {
      size += chunk.length;
      hash.update(chunk);
    }
    const after = await stream.stat();
    if (size !== before.size || after.size !== size || after.mtimeMs !== before.mtimeMs || after.nlink !== 1) {
      throw new Error("Installed file changed during verification");
    }
    return { path: name, size, sha256: hash.digest("hex") };
  } finally {
    await stream.close();
  }
}

async function collectFiles(appRoot, parent = "") {
  const files = [];
  const names = await fs.readdir(path.join(appRoot, parent));
  names.sort();
  for (const name of names) {
    const relative = parent ? `${parent}/${name}` : name;
    validateFileName(relative);
    const entry = await fs.lstat(path.join(appRoot, relative));
    if (relative === manifestName) continue;
    if (entry.isDirectory()) files.push(...await collectFiles(appRoot, relative));
    else files.push(await fileDigest(appRoot, relative));
  }
  return files;
}

function checkFileList(files) {
  if (!Array.isArray(files)) throw new Error("Expected a signed installed file list");
  const paths = new Map();
  for (const file of files) {
    validateFileName(file.path);
    if (!Number.isSafeInteger(file.size) || file.size < 0 || !/^[a-f0-9]{64}$/.test(file.sha256 ?? "")) {
      throw new Error("Invalid installed file digest");
    }
    const key = file.path.toLowerCase();
    if (key === manifestName || paths.has(key)) throw new Error("Duplicate or self-referencing installed file");
    paths.set(key, file);
  }
  for (const file of requiredFiles) {
    if (!(paths.get(file.toLowerCase())?.size > 0)) throw new Error(`Required installed file is missing: ${file}`);
  }
}

async function checkAppRoot(appRoot, configuration) {
  if (!path.isAbsolute(appRoot) || !(await fs.lstat(appRoot)).isDirectory()) {
    throw new Error("Expected the final Windows application directory");
  }
  const metadata = JSON.parse(await fs.readFile(path.join(appRoot, "resources", "app-update.yml"), "utf8"));
  const { releaseId, version, mode, platform, arch, signingIdentity, updaterProtocol: protocol } = configuration;
  if (!isDeepStrictEqual(metadata.lazymind, { releaseId, version, mode, platform, arch, signingIdentity, protocol })) {
    throw new Error("Packaged update metadata does not match the built helper");
  }
}

export async function writeWindowsInstalledManifest({ appRoot, configuration, keyId, privateKey }) {
  const identity = manifestIdentity(configuration);
  await checkAppRoot(appRoot, configuration);
  const key = createPrivateKey(privateKey);
  const expected = configuration.publicKeys[keyId];
  const publicBytes = createPublicKey(key).export({ type: "spki", format: "der" });
  if (key.asymmetricKeyType !== "ed25519" || !expected ||
    !publicBytes.equals(Buffer.concat([ed25519Prefix, Buffer.from(expected, "base64")]))) {
    throw new Error("The installed manifest key does not match the embedded Windows policy key");
  }
  const files = await collectFiles(appRoot);
  checkFileList(files);
  const raw = Buffer.from(JSON.stringify({ ...identity, files }));
  const envelope = Buffer.from(JSON.stringify({
    keyId, payload: raw.toString("base64"), signature: sign(null, raw, key).toString("base64"),
  }));
  if (envelope.length > maxEnvelope) throw new Error("The installed manifest exceeds the updater size limit");
  const filename = path.join(appRoot, manifestName);
  try {
    await fs.writeFile(filename, envelope, { flag: "wx" });
  } catch (error) {
    if (error.code !== "EEXIST" || !(await fs.lstat(filename)).isFile() ||
      !(await fs.readFile(filename)).equals(envelope)) throw error;
  }
  return filename;
}

export async function readSignedWindowsInstalledManifest(appRoot, configuration) {
  const identity = manifestIdentity(configuration);
  await checkAppRoot(appRoot, configuration);
  const filename = path.join(appRoot, manifestName);
  const info = await fs.lstat(filename);
  if (!info.isFile() || info.nlink !== 1 || info.size > maxEnvelope) throw new Error("Invalid installed manifest file");
  const envelope = await fs.readFile(filename);
  if (envelope.length !== info.size) throw new Error("The installed manifest changed while reading");
  const { keyId, payload, signature } = JSON.parse(envelope);
  const expected = configuration.publicKeys[keyId];
  if (!expected) throw new Error("The installed manifest uses an unknown key");
  const key = createPublicKey({ key: Buffer.concat([ed25519Prefix, Buffer.from(expected, "base64")]), type: "spki", format: "der" });
  const raw = Buffer.from(payload, "base64");
  if (!verify(null, raw, key, Buffer.from(signature, "base64"))) throw new Error("The installed manifest signature is invalid");
  const { files, ...actual } = JSON.parse(raw);
  if (!isDeepStrictEqual(actual, identity)) throw new Error("The installed manifest is for a different release");
  checkFileList(files);
  return { envelope, files };
}

export async function readVerifiedWindowsInstalledManifest(appRoot, configuration) {
  const { envelope, files } = await readSignedWindowsInstalledManifest(appRoot, configuration);
  const packaged = await collectFiles(appRoot);
  if (packaged.length !== files.length) throw new Error("The prepackaged tree differs from the signed file set");
  const actualFiles = new Map(packaged.map(entry => [entry.path, entry]));
  for (const file of files) {
    if (!isDeepStrictEqual(actualFiles.get(file.path), file)) throw new Error("An installed file does not match the signed manifest");
  }
  return envelope;
}

async function cli(args) {
  const verifying = args.length === 3 && args[2] === "--verify";
  const signing = args.length === 4 && args[2] === "--key-id" && args[3];
  if (process.platform !== "win32" || args[0] !== "--app-root" || !args[1] || (!verifying && !signing)) {
    throw new Error("Expected Windows manifest --app-root and either --key-id or --verify");
  }
  const helper = path.join(args[1], "resources", "runtime", "bin", "lazymind-update-helper.exe");
  const { WINDOWS_POLICY_UPDATE_PRIVATE_KEY: privateKey, ...env } = process.env;
  const { status, config } = JSON.parse(execFileSync(helper, ["config"], { encoding: "utf8", windowsHide: true, timeout: 35_000, maxBuffer: maxEnvelope, env }));
  if (status !== "enabled") throw new Error("The staged Windows update helper has no enabled configuration");
  if (verifying) await readVerifiedWindowsInstalledManifest(args[1], config);
  else await writeWindowsInstalledManifest({ appRoot: args[1], configuration: config, keyId: args[3], privateKey });
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try { await cli(process.argv.slice(2)); }
  catch (error) { console.error(error.message); process.exitCode = 1; }
}
