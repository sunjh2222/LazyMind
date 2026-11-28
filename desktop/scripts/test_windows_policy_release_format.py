"""Exercise the packaged-runtime evidence boundary without running an installer."""

import hashlib
import importlib.util
import io
import json
from pathlib import Path
from contextlib import contextmanager
from types import SimpleNamespace
import tempfile
import unittest
from unittest import mock


def source_script(filename):
    here = Path(__file__).resolve()
    repo_script = here.with_name(filename)
    if not repo_script.is_file():
        repo_script = here.parents[5] / 'LazyMind/desktop/scripts' / filename
    spec = importlib.util.spec_from_file_location(filename[:-3], repo_script)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


recipe = source_script('verify-update-format.py')
planner = source_script('prepare-update-ci-plan.py')
smoke = source_script('smoke-published-upgrade.py')
BASE = 'https://modelscope.cn/datasets/fixture/updates/resolve/master/windows/stable/'


class WindowsPolicyFormatEvidenceTest(unittest.TestCase):
    def setUp(self):
        root = tempfile.TemporaryDirectory(prefix='lm-format-test-')
        self.addCleanup(root.cleanup)
        self.root = Path(root.name)
        self.resources = self.root / 'app/resources'
        self.runtime = self.resources / 'runtime'
        self.packaged = self.root / 'release'
        self.config = {'releaseId': 'fixture-target', 'version': '1.1.0', 'platform': 'windows', 'arch': 'amd64',
                       'signingIdentity': 'fixture-publisher', 'updaterProtocol': 2, 'mode': 'windows-policy',
                       'audience': 'official', 'channel': 'stable', 'trustDomain': 'fixture-windows',
                       'policyURL': BASE + 'policy.json', 'allowedOrigins': ['https://modelscope.cn']}
        self.release = {key: self.config[key] for key in ('releaseId', 'version', 'platform', 'arch', 'signingIdentity', 'updaterProtocol')}
        rag = {'id': 'rag', 'revision': 'a' * 64, 'tag': 'b' * 64, 'pythonAbi': 'cp311',
               'baseFingerprint': 'c' * 64, 'packages': {'fixture': '1'}}
        catalog = {'schemaVersion': 1, 'platform': 'windows', 'arch': 'amd64', 'components': {'rag': rag}}
        self.component = self.write(self.packaged / 'components.json', json.dumps(catalog).encode())
        self.release['components'] = self.component
        self.write(self.runtime / 'config/python-components.json', (self.packaged / 'components.json').read_bytes())
        payload = b'MZ fixture installer bytes'
        self.release['package'] = {**self.write(self.packaged / 'package.exe', payload), 'unpackedBytes': 4096}
        # Signature correctness is tested by the signing and Go identity
        # suites. Here the release record must bind the exact installed bytes
        # before invoking the identity verifier or the bundled interpreter.
        self.manifest = b'fixture signed manifest envelope'
        self.release['installedManifest'] = self.write(self.packaged / 'installed-manifest.json', self.manifest)
        self.release['feed'] = self.write(self.packaged / 'feed.json', b'{}')
        for kind, name in [('package', 'package.exe'), ('components', 'components.json'), ('feed', 'feed.json'),
                           ('installedManifest', 'installed-manifest.json')]:
            self.release[kind]['url'] = f'{BASE}releases/{self.config["releaseId"]}/{name}'
        self.write(self.resources / 'installed-manifest.json', self.manifest)
        runtime_manifest = self.write(self.runtime / 'manifest.json', b'{}')
        self.write(self.runtime / 'config/update-format-v2.json', b'{"version":2,"families":{},"segments":[]}')
        self.write(self.runtime / 'bin/lazymind-update-helper.exe', b'MZ identity fixture')
        self.write(self.runtime / 'deps/python/algorithm/Scripts/python.exe', b'MZ Python fixture')
        native = {key: self.release[key] for key in ('releaseId', 'version', 'platform', 'arch', 'signingIdentity')}
        native.update(protocol=2, mode=self.config['mode'])
        self.write(self.resources / 'app-update.yml', json.dumps({'lazymind': native}).encode())
        self.record = {'config': self.config, 'release': self.release, 'manifestSHA256': runtime_manifest['sha256'],
                       'formatProfile': recipe.format_profile(self.runtime), 'componentDescriptor': catalog['components']['rag']}
        self.write(self.packaged / 'release.json', json.dumps(self.record).encode())

    @staticmethod
    def write(path, raw):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(raw)
        return {'size': len(raw), 'sha256': hashlib.sha256(raw).hexdigest()}

    def release_inputs(self):
        with mock.patch.object(recipe.sys, 'platform', 'win32'), mock.patch.object(recipe.platform, 'machine', return_value='AMD64'):
            return recipe.release_inputs(self.packaged, False)

    def test_windows_policy_requires_the_exact_staged_manifest(self):
        record, package, _, _ = self.release_inputs()
        self.assertEqual(record['config'], self.config)
        self.assertEqual(package, self.packaged / 'package.exe')
        (self.packaged / 'installed-manifest.json').write_bytes(b'0' * len(self.manifest))
        with self.assertRaisesRegex(ValueError, 'installed-manifest.json.*SHA-256'):
            self.release_inputs()

    def test_installed_evidence_requires_identity_verification(self):
        calls = []

        def helper(args, **kwargs):
            self.assertEqual(args, [str(self.runtime / 'bin/lazymind-update-helper.exe'), 'verify-identity'])
            calls.append(args)
            return json.dumps({'status': 'enabled', 'config': self.config})

        with mock.patch.object(recipe, 'verify_windows_signature', side_effect=AssertionError('wrong trust route')), \
             mock.patch.object(recipe, 'output', side_effect=helper):
            runtime, python = recipe.verify_runtime(self.resources, self.record, self.packaged / 'components.json')
            self.assertEqual(runtime, self.runtime)
            self.assertEqual(python, self.runtime / 'deps/python/algorithm/Scripts/python.exe')
            self.assertEqual(len(calls), 1)
            (self.resources / 'installed-manifest.json').write_bytes(b'0' * len(self.manifest))
            with self.assertRaisesRegex(ValueError, 'installed-manifest.json.*SHA-256'):
                recipe.verify_runtime(self.resources, self.record, self.packaged / 'components.json')
            self.assertEqual(len(calls), 1, 'helper ran before rejecting the unbound installed manifest')

    def test_windows_policy_uses_native_format_evidence_without_authenticode(self):
        package = self.packaged / 'package.exe'
        app = self.resources.parent
        self.write(app / 'LazyMind.exe', b'MZ launcher fixture')
        self.write(app / 'Uninstall LazyMind.exe', b'MZ uninstaller fixture')
        # The temporary Windows-runner branch is exercised with process and
        # uninstall operations mocked. It never uses the host's installation.
        with mock.patch.dict(recipe.os.environ, {'RUNNER_ENVIRONMENT': 'github-hosted', 'GITHUB_ACTIONS': 'true'}), \
             mock.patch.object(recipe, 'verify_windows_signature', side_effect=AssertionError('wrong trust route')), \
             mock.patch.object(recipe, 'install_windows_default', return_value=app) as install, \
             mock.patch.object(recipe, 'run_windows_installer') as uninstall, \
             mock.patch.object(recipe, 'remove_empty_install_tree'):
            with recipe.materialized_app(package, self.release, self.root / 'target-app', False, mode=self.config['mode']) as resources:
                self.assertEqual(resources, self.resources)
            self.assertEqual(install.call_args.args[0], package)
            self.assertEqual(uninstall.call_count, 1)

    def test_legacy_official_evidence_still_uses_the_legacy_identity_provider(self):
        self.config['mode'] = 'official'
        self.release.pop('installedManifest')
        (self.packaged / 'installed-manifest.json').unlink()
        native_path = self.resources / 'app-update.yml'
        metadata = json.loads(native_path.read_bytes())
        metadata['lazymind']['mode'] = 'official'
        native_path.write_text(json.dumps(metadata), encoding='utf-8')
        (self.packaged / 'release.json').write_text(json.dumps(self.record), encoding='utf-8')
        self.assertEqual(self.release_inputs()[0]['config']['mode'], 'official')
        with mock.patch.object(recipe, 'verify_windows_signature') as verify, \
             mock.patch.object(recipe, 'output', return_value=json.dumps({'status': 'enabled', 'config': self.config})) as helper:
            recipe.verify_runtime(self.resources, self.record, self.packaged / 'components.json')
            verify.assert_called_once_with(self.runtime / 'bin/lazymind-update-helper.exe', self.config['signingIdentity'])
            self.assertEqual(helper.call_args.args[0][-1], 'config')

    def make_source(self):
        source = self.root / 'source-release'
        record = json.loads(json.dumps(self.record))
        for item in ('config', 'release'):
            record[item].update(releaseId='fixture-source', version='1.0.0')
        for name in ('package.exe', 'components.json', 'feed.json', 'installed-manifest.json'):
            self.write(source / name, (self.packaged / name).read_bytes())
        for kind in ('package', 'components', 'feed', 'installedManifest'):
            record['release'][kind]['url'] = record['release'][kind]['url'].replace('fixture-target', 'fixture-source')
        self.write(source / 'release.json', json.dumps(record).encode())
        return source, record

    def test_windows_policy_signed_plan_reaches_native_format_worker(self):
        source, old = self.make_source()
        selected = self.root / 'plan'
        policy = {'policySequence': 41, 'releases': [old['release']], 'revoked': []}

        # The external Go verifier owns signature validation. Use its signed
        # history result as input to the real publication planner here.
        with mock.patch.object(planner, 'verified_policy', side_effect=lambda *_, source_only=False:
                               old['release'] if source_only else policy):
            planner.prepare(self.packaged, None, None, Path('fixture-verified-policy'), selected,
                            verify_source_only=True, fetch=lambda _: io.BytesIO(b'accepted signed history'))
        pairs = json.loads((selected / 'pairs.json').read_bytes())
        self.assertEqual(len(pairs), 1)
        self.assertEqual(pairs[0]['sourceRelease'], old['release'])
        self.assertTrue(pairs[0]['baseline'])
        self.assertEqual(pairs[0]['targetReleaseId'], self.config['releaseId'])
        self.assertEqual(json.loads((selected / 'records/fixture-target/release.json').read_bytes()), self.record)

        events = []

        @contextmanager
        def materialized(package, release, directory, unsigned_test, *, mode=None):
            self.assertEqual(mode, 'windows-policy', 'format evidence lost the release mode')
            self.assertFalse(unsigned_test)
            events.append(release['releaseId'])
            yield self.resources

        def index(_python, action, fixture, *_):
            events.append(action)
            index_file = fixture / 'indexed-rows'
            if action == 'create-index':
                self.write(index_file, b'durable lookup marker from source release')
            else:
                self.assertEqual(index_file.read_bytes(), b'durable lookup marker from source release')
            return True

        output = self.root / 'format-evidence.json'
        args = SimpleNamespace(source=source, target=self.packaged, output=output,
                               source_record_output=None, published_source=None, unsigned_test=False,
                               source_components_dir=None, target_components_dir=None)
        with mock.patch.object(recipe.sys, 'platform', 'win32'), mock.patch.object(recipe.platform, 'machine', return_value='AMD64'), \
             mock.patch.object(recipe, 'materialized_app', side_effect=materialized), \
             mock.patch.object(recipe, 'verify_runtime', return_value=(self.runtime, self.runtime / 'python.exe')), \
             mock.patch.object(recipe, 'extract_overlay', return_value=self.runtime), \
             mock.patch.object(recipe, 'index_worker', side_effect=index):
            recipe.generate_evidence(args)
        self.assertEqual(events, ['fixture-source', 'create-index', 'fixture-target', 'read-index'])
        self.assertTrue(json.loads(output.read_bytes())['passed'])

    def test_installed_smoke_provisions_windows_policy_without_qa_certificate(self):
        source, _ = self.make_source()
        local = self.root / 'isolated-runner'
        app = local / 'Programs/LazyMind/LazyMind.exe'
        args = SimpleNamespace(source=source, target=self.packaged, app=app, report=self.root / 'unused.json',
                               runtime_root=None, provision_only=True, policy_url=self.config['policyURL'],
                               signer=self.config['signingIdentity'], selected='none')
        real_install = mock.Mock(return_value=app.parent)
        probe = {'release_inputs': recipe.release_inputs, 'install_windows_default': real_install,
                 'verify_windows_signature': mock.Mock(side_effect=AssertionError('official policy is not Authenticode'))}
        with mock.patch.object(recipe.sys, 'platform', 'win32'), mock.patch.object(recipe.platform, 'machine', return_value='AMD64'), \
             mock.patch.dict(recipe.os.environ, {'RUNNER_ENVIRONMENT': 'github-hosted', 'GITHUB_ACTIONS': 'true', 'LOCALAPPDATA': str(local)}), \
             mock.patch.object(smoke.runpy, 'run_path', return_value=probe):
            smoke.run(args)
        self.assertEqual(real_install.call_args.args[0], source / 'package.exe')


if __name__ == '__main__':
    unittest.main()
