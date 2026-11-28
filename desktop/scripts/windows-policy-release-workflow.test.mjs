import assert from "node:assert/strict";
import fs from "node:fs";
import { createRequire } from "node:module";
import test from "node:test";
import vm from "node:vm";

const electron = createRequire(new URL("../electron/package.json", import.meta.url));
const yaml = createRequire(electron.resolve("electron-builder/package.json"))("js-yaml");
const workflow = name => yaml.load(fs.readFileSync(new URL(`../../.github/workflows/${name}.yml`, import.meta.url), "utf8"));
const releaseWorkflow = workflow("desktop-release");
const windowsWorkflow = workflow("windows-installer");
const publication = workflow("desktop-update-publish");
const privateKeyName = "WINDOWS_POLICY_UPDATE_PRIVATE_KEY";
const releaseKeyFixture = "official-windows-key-never-pass-outside-lane";
const releasePublisher = "./.github/workflows/desktop-update-publish.yml";
const policyConfiguration = JSON.stringify({ mode: "windows-policy" });
const privateKeySteps = workflow => Object.entries(workflow.jobs).flatMap(([job, definition]) =>
  (definition.steps ?? []).filter(step => JSON.stringify(step.env ?? {}).includes(privateKeyName))
    .map(step => ({ job, step })));

function evaluate(expression, context) {
  if (typeof expression !== "string") return expression;
  const source = expression.match(/^\s*\$\{\{\s*([\s\S]*?)\s*\}\}\s*$/)?.[1];
  assert.ok(source, `expected a workflow expression but got ${expression}`);
  return vm.runInNewContext(source, {
    fromJSON: JSON.parse, startsWith: (value, prefix) => value.startsWith(prefix),
    ...context,
  }, { timeout: 300 });
}

function envValue(value, context) {
  return typeof value === "string" && value.trim().startsWith("${{") ? evaluate(value, context) : value;
}

function enabled(step, context) {
  if (step.if === undefined) return true;
  const expression = step.if.trim().startsWith("${{") ? step.if : "${{ " + step.if + " }}";
  return Boolean(evaluate(expression, context));
}

function signerContext(runnerOS) {
  return {
    github: { event_name: "push", ref: "refs/tags/v1.2.3", ref_name: "v1.2.3", ref_type: "tag", ref_protected: true, repository: "LazyAGI/LazyMind" },
    inputs: { enable_updates: true, enable_test_signed_updates: false, test_signed_updates: false, unsigned_update_test: false, use_current_run: true, platform: "windows" },
    runner: { os: runnerOS, arch: "X64" },
    matrix: { platform: "windows" },
    vars: {
      LAZYMIND_DESKTOP_UPDATES_ENABLED: "true", LAZYMIND_DESKTOP_UPDATE_PUBLICATION_ENABLED: "true",
      LAZYMIND_WINDOWS_UPDATE_CONFIG_JSON: policyConfiguration,
    },
    env: { LAZYMIND_UPDATE_BUILD_CONFIG_JSON: policyConfiguration },
    secrets: {
      [privateKeyName]: releaseKeyFixture, LAZYMIND_UPDATE_POLICY_PRIVATE_KEY: "legacy-official-key",
      WINDOWS_TEST_UPDATE_POLICY_PRIVATE_KEY: "qa-key",
    },
    needs: { verify: { result: "success" } },
    cancelled: () => false, failure: () => false, success: () => true,
  };
}

function assertProtectedSigner(job, step, runnerOS) {
  const positive = signerContext(runnerOS);
  const receivesKey = context => {
    if (!enabled(job, context) || !enabled(step, context)) return false;
    const runner = job["runs-on"];
    if (typeof runner === "string" && runner.startsWith("windows-") && context.runner.os !== "Windows") return false;
    return Object.values(step.env ?? {}).some(value => envValue(value, context) === releaseKeyFixture);
  };
  assert.equal(receivesKey(positive), true, "protected Windows release tag must reach its dedicated signer");
  // Offer the sentinel key in all four contexts: the signer must reject the
  // workflow input itself, without relying on a missing Actions secret.
  for (const [label, context] of [
    ["unprotected tag", { ...positive, github: { ...positive.github, ref_protected: false } }],
    ["dispatch from the protected tag", { ...positive, github: { ...positive.github, event_name: "workflow_dispatch" } }],
    ["QA signing selection", { ...positive, inputs: { ...positive.inputs, enable_test_signed_updates: true, test_signed_updates: true } }],
    ["macOS", { ...positive, inputs: { ...positive.inputs, platform: "macos" }, runner: { ...positive.runner, os: "macOS" }, matrix: { platform: "macos" } }],
  ]) {
    assert.equal(receivesKey(context), false, label);
  }
}

function commands(script = "") {
  return script.replace(/`[ \t]*\r?\n[ \t]*/g, " ").split(/\r?\n/).map(line => line.trim());
}

function argumentVariable(command, option) {
  return command?.match(new RegExp(`${option}["']?(?:\\s+|,\\s*)"?(\\$(?:env:)?[A-Za-z][\\w]*)`, "i"))?.[1];
}

const isNodeManifestCommand = line => /^(?:&\s+)?node(?:\.exe)?\s/i.test(line) && /windows-policy-release\.mjs/.test(line);
const isNsisBuilderCommand = line =>
  /^(?:&\s+)?(?:pnpm(?:\.cmd)?|Invoke-Native\s+['"]pnpm(?:\.cmd)?['"])\s/i.test(line) &&
  /\belectron-builder\b/.test(line) && /(?:--win|-w)["']?(?:[\s,]+|=)['"]?nsis(?:[\s'",]|$)/i.test(line);

function packDirectoryArgument(step, packing) {
  const call = commands(step.run).find(line => isNsisBuilderCommand(line) && argumentVariable(line, "--prepackaged"));
  if (call) return argumentVariable(call, "--prepackaged");
  const delegate = commands(step.run).find(line => /^(?:&\s+|(?:pwsh|powershell)(?:\.exe)?\s)/i.test(line) &&
    /build-windows-x64\.ps1/.test(line) && argumentVariable(line, "-Prepackaged"));
  const actual = commands(packing).find(line => isNsisBuilderCommand(line) &&
    /^\$prepackaged$/i.test(argumentVariable(line, "--prepackaged") ?? ""));
  return actual ? argumentVariable(delegate, "-Prepackaged") : undefined;
}

test("the protected Windows tag still requests the configured Windows policy and only its own publication", () => {
  const vars = {
    LAZYMIND_DESKTOP_UPDATES_ENABLED: "true",
    LAZYMIND_DESKTOP_UPDATE_PUBLICATION_ENABLED: "true",
    LAZYMIND_WINDOWS_UPDATE_CONFIG_JSON: JSON.stringify({ mode: "windows-policy" }),
  };
  const github = { event_name: "push", ref: "refs/tags/v1.2.3", ref_name: "v1.2.3", ref_type: "tag", ref_protected: true };
  const build = releaseWorkflow.jobs.windows;
  assert.equal(build.uses, "./.github/workflows/windows-installer.yml");
  assert.equal(evaluate(build.with.git_ref, { vars, github }), github.ref);
  const updates = evaluate(build.with.enable_updates, { vars, github });
  assert.equal(updates, true);
  assert.equal(evaluate(windowsWorkflow.env.LAZYMIND_UPDATE_BUILD_CONFIG_JSON, {
    inputs: { enable_updates: updates, enable_test_signed_updates: false }, vars, github,
  }), vars.LAZYMIND_WINDOWS_UPDATE_CONFIG_JSON);

  const publish = releaseWorkflow.jobs.publish_windows_update;
  assert.equal(publish.uses, releasePublisher);
  assert.deepEqual([publish.needs].flat(), ["windows"]);
  assert.equal(publish.with.platform, "windows");
  assert.equal(releaseWorkflow.jobs.publish_macos_update.with.platform, "macos");
});

test("the final staged Windows tree is signed between app packaging and NSIS packaging", () => {
  const steps = windowsWorkflow.jobs["build-windows-installer"].steps;
  const signers = privateKeySteps(windowsWorkflow);
  assert.equal(signers.length, 1, "the Windows release Ed25519 key goes only to the manifest signer");
  assert.equal(signers[0].job, "build-windows-installer");
  const sign = signers[0].step;
  assertProtectedSigner(windowsWorkflow.jobs[signers[0].job], sign, "Windows");
  assert.match(sign.run, /windows-policy-release\.mjs/);
  assert.doesNotMatch(JSON.stringify(sign.env), /WINDOWS_TEST_CSC_LINK|\bWIN_CSC_LINK\b|MAC_CSC_LINK/);
  const signIndex = steps.indexOf(sign);
  const stageIndex = steps.findIndex((step, index) => index > signIndex && step.run?.includes("prepare-update-release.mjs"));
  assert.ok(signIndex > 0 && stageIndex > signIndex, "the signed tree precedes immutable release staging");
  const packing = fs.readFileSync(new URL("./build-windows-x64.ps1", import.meta.url), "utf8");
  const appRoot = argumentVariable(commands(sign.run).find(isNodeManifestCommand), "--app-root");
  assert.match(appRoot ?? "", /^\$env:\w+$/i, "the actual manifest command receives the staged application path");
  const packs = steps.slice(signIndex + 1, stageIndex)
    .flatMap(step => {
      const arg = packDirectoryArgument(step, packing);
      return arg ? [{ step, arg }] : [];
    });
  assert.equal(packs.length, 1, "exactly one NSIS command consumes a prepackaged directory after the manifest step");
  const [{ step: pack, arg }] = packs;
  assert.match(arg, /^\$env:\w+$/i);
  const sourceBinding = sign.env?.[appRoot.split(":")[1]];
  const source = sourceBinding?.match(/^\s*\$\{\{\s*steps\.(?<id>[\w-]+)\.outputs\.(?<name>[\w-]+)\s*\}\}\s*$/);
  assert.ok(source, "the signer receives the actual prepackaging step's output directory");
  const producer = steps.slice(0, signIndex).find(step => step.id === source.groups.id);
  assert.ok(producer && /build-windows-x64\.ps1/.test(producer.run ?? ""), "the directory output comes from the Windows app packager");
  const output = source.groups.name;
  const outputAssignment = new RegExp(`${output}=.*(?:\\$env:GITHUB_OUTPUT|\\$GITHUB_OUTPUT)`, "i");
  assert.ok(commands(producer.run).some(line => outputAssignment.test(line)) || commands(packing).some(line => outputAssignment.test(line)),
    "the app packager emits the directory as an Actions output");

  const sealed = String.raw`C:\release with spaces\win-unpacked`;
  const context = { ...signerContext("Windows"), steps: { [source.groups.id]: { outputs: { [output]: sealed } } } };
  assert.equal(envValue(sourceBinding, context), sealed);
  assert.equal(envValue(pack.env?.[arg.split(":")[1]], context), sealed,
    "the final NSIS --prepackaged operand resolves to the exact directory passed to --app-root");
  assert.equal(enabled(pack, context), true, "the protected Windows tag may reach final NSIS packaging");
});

test("formal Windows policy signing uses its own key and precedes policy publication", () => {
  const signers = privateKeySteps(publication);
  assert.equal(signers.length, 1, "the dedicated key may not reach native verification or artifact upload steps");
  assert.equal(signers[0].job, "publish");
  const signing = signers[0].step;
  assertProtectedSigner(publication.jobs[signers[0].job], signing, "Linux");
  const steps = publication.jobs.publish.steps;
  assert.match(signing.run, /update-publisher["']?\s+sign/);
  const upload = steps.find(step => step.run?.includes("publish-desktop-update.py"));
  assert.ok(upload && steps.indexOf(signing) < steps.indexOf(upload));
  assert.doesNotMatch(JSON.stringify(upload.env), new RegExp(privateKeyName));
  assert.ok(windowsWorkflow.jobs["publish-windows-rag"].steps.some(step => step.run?.includes("publish-python-components.py")),
    "the existing Windows RAG archive and catalog publication remains attached to the installer");
});

test("a Windows policy release still runs native format verification and the installed A-to-B smoke", () => {
  const configuration = JSON.stringify({ mode: "windows-policy" });
  const context = {
    inputs: { use_current_run: true, platform: "windows", unsigned_update_test: false, test_signed_updates: false },
    matrix: { platform: "windows" },
    vars: { LAZYMIND_WINDOWS_UPDATE_CONFIG_JSON: configuration },
  };
  const verify = publication.jobs.verify;
  assert.equal(enabled(verify, context), true);
  assert.deepEqual(evaluate(verify.strategy.matrix.platform, context), ["windows"]);
  const release = verify.steps.find(step => step.uses?.startsWith("actions/download-artifact") && step.with?.path === "publication/current");
  const format = verify.steps.find(step => step.run?.includes("verify-update-ci-pairs.py"));
  assert.ok(release && format && verify.steps.indexOf(release) < verify.steps.indexOf(format));
  assert.equal(enabled(release, context), true);
  assert.equal(enabled(format, context), true);
  assert.notEqual(format["continue-on-error"], true);
  assert.doesNotMatch(format.run, /verify-update-ci-pairs\.py[^\n]*\|\|\s*true/);
  const publish = publication.jobs.publish;
  assert.deepEqual([publish.needs].flat(), ["verify"]);
  for (const result of ["success", "failure", "skipped"]) {
    assert.equal(enabled(publish, { ...context, needs: { verify: { result } }, cancelled: () => false, failure: () => result === "failure" }),
      result === "success");
  }

  const plan = publication.jobs.plan_installed_upgrade;
  assert.deepEqual([plan.needs].flat(), ["publish"]);
  const upgrade = publication.jobs.test_installed_upgrade;
  assert.deepEqual([upgrade.needs].flat(), ["plan_installed_upgrade"]);
  const follow = { ...context, needs: { plan_installed_upgrade: { outputs: { lanes: '[{"platform":"windows"}]' } } } };
  assert.equal(enabled(upgrade, follow), true);
  const smoke = upgrade.steps.find(step => step.run?.includes("run-published-upgrade-smoke.py run"));
  assert.ok(smoke && enabled(smoke, follow));
  assert.notEqual(smoke["continue-on-error"], true);
  assert.equal(evaluate(smoke.env.SMOKE_PUBLIC_CONFIG, follow), configuration);
  for (const job of [verify, upgrade]) {
    assert.equal(job.steps.some(step => step.run?.includes("windows-test-certificate.ps1") && enabled(step, follow)), false);
  }
});
