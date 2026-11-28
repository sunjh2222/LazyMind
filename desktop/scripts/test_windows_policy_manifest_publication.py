"""Exercise the ModelScope publisher after signed-policy validation."""
import copy
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest
from urllib.error import HTTPError


spec = importlib.util.spec_from_file_location(
    'publish_desktop', Path(__file__).with_name('publish-desktop-update.py'))
publisher = importlib.util.module_from_spec(spec)
spec.loader.exec_module(publisher)


class WindowsPolicyManifestPublicationTests(unittest.TestCase):
    repo = 'fixture/updates'
    base = f'https://modelscope.cn/datasets/{repo}/resolve/master/'
    prefix = 'updates/windows/stable/'
    pointer = prefix + 'policy.json'
    envelope = b'fixture signed Windows policy'
    previous = b'fixture earlier signed policy'
    manifest = 'installed-manifest.json'

    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.config = {
            'mode': 'windows-policy', 'platform': 'windows', 'arch': 'amd64',
            'audience': 'official', 'channel': 'stable', 'policyURL': self.base + self.pointer,
            'allowedOrigins': ['https://modelscope.cn'],
        }
        self.config_path = self.root / 'config.json'
        self.policy_path = self.root / 'policy.json'
        self.policy_path.write_bytes(self.envelope)
        self.release = self.write_release('windows-amd64-1.2.3')
        self.policy = {'policySequence': 2, 'releases': [self.release]}
        self.prior_policy = {'policySequence': 1, 'releases': []}
        self.manifest_file = self.root / self.release['releaseId'] / self.manifest
        self.manifest_url = self.release['installedManifest']['url']
        self.manifest_destination = self.manifest_url.removeprefix(self.base)
        self.remote = {self.config['policyURL']: self.previous}
        self.events = []
        self.corrupt_manifest_readback = False

    def write_release(self, release_id, *, include_manifest=True):
        directory = self.root / release_id
        directory.mkdir()
        release = {'releaseId': release_id, 'platform': 'windows'}
        for kind, filename in [
            ('package', 'package.exe'), ('feed', 'feed.json'), ('components', 'components.json'),
            ('installedManifest', self.manifest),
        ]:
            if kind == 'installedManifest' and not include_manifest:
                continue
            raw = f'{release_id} {kind}'.encode()
            (directory / filename).write_bytes(raw)
            release[kind] = {
                'url': f'{self.base}{self.prefix}releases/{release_id}/{filename}',
                'size': len(raw), 'sha256': hashlib.sha256(raw).hexdigest(),
            }
        return release

    def validate(self, raw):
        if raw == self.envelope:
            return copy.deepcopy(self.policy)
        if raw == self.previous:
            return copy.deepcopy(self.prior_policy)
        raise ValueError('unrecognized policy fixture')

    def fetch(self, url):
        self.events.append(('fetch', url))
        if url not in self.remote:
            raise HTTPError(url, 404, 'fixture not found', {}, None)
        return io.BytesIO(self.remote[url])

    def upload_file(self, *, path_or_fileobj, path_in_repo, **_):
        self.events.append(('upload', path_in_repo))
        raw = Path(path_or_fileobj).read_bytes()
        if self.corrupt_manifest_readback and path_in_repo == self.manifest_destination:
            raw = b'!' + raw[1:]
        self.remote[self.base + path_in_repo] = raw

    def publish(self):
        self.config_path.write_text(json.dumps(self.config), encoding='utf-8')
        publisher.publish(self.root, self.policy_path, self.config_path, self.repo,
                          api=self, fetch=self.fetch, validate=self.validate)

    def test_installed_manifest_is_read_back_before_publishing_the_policy(self):
        self.publish()
        manifest_upload = ('upload', self.manifest_destination)
        policy_upload = ('upload', self.pointer)
        self.assertIn(manifest_upload, self.events)
        self.assertIn(policy_upload, self.events)
        uploaded = self.events.index(manifest_upload)
        pointer = self.events.index(policy_upload)
        self.assertIn(('fetch', self.manifest_url), self.events[uploaded + 1:pointer])
        self.assertEqual(self.remote[self.manifest_url], self.manifest_file.read_bytes())
        self.assertIn(('fetch', self.config['policyURL']), self.events[pointer + 1:])

    def test_each_release_requires_its_own_manifest_descriptor(self):
        self.policy['releases'].append(self.write_release('windows-amd64-1.2.2', include_manifest=False))
        with self.assertRaises(ValueError):
            self.publish()
        self.assertFalse(any(action == 'upload' for action, _ in self.events))

    def test_missing_or_invalid_local_manifest_prevents_publication(self):
        raw = self.manifest_file.read_bytes()
        for data in (None, raw[:-1], b'!' + raw[1:]):
            with self.subTest(data=data):
                self.events.clear()
                self.remote = {self.config['policyURL']: self.previous}
                self.manifest_file.unlink(missing_ok=True)
                if data is not None:
                    self.manifest_file.write_bytes(data)
                with self.assertRaises(ValueError):
                    self.publish()
                self.assertFalse(any(action == 'upload' for action, _ in self.events))

    def test_manifest_destination_must_name_the_release_and_use_the_canonical_filename(self):
        for target in ('other-release/installed-manifest.json', 'windows-amd64-1.2.3/manifest.json'):
            with self.subTest(target=target):
                self.events.clear()
                self.remote = {self.config['policyURL']: self.previous}
                self.release['installedManifest']['url'] = f'{self.base}{self.prefix}releases/{target}'
                with self.assertRaises(ValueError):
                    self.publish()
                self.assertFalse(any(action == 'upload' for action, _ in self.events))

    def test_corrupted_manifest_readback_leaves_the_previous_policy_in_place(self):
        self.corrupt_manifest_readback = True
        with self.assertRaises(ValueError):
            self.publish()
        manifest_upload = ('upload', self.manifest_destination)
        self.assertIn(manifest_upload, self.events)
        self.assertIn(('fetch', self.manifest_url), self.events[self.events.index(manifest_upload) + 1:])
        self.assertNotIn(('upload', self.pointer), self.events)
        self.assertEqual(self.remote[self.config['policyURL']], self.previous)

    def test_existing_remote_manifest_can_be_reused_after_verification(self):
        self.prior_policy['releases'] = [copy.deepcopy(self.release)]
        self.policy['releases'].append(self.write_release('windows-amd64-1.2.4'))
        raw = self.manifest_file.read_bytes()
        self.manifest_file.unlink()
        self.remote[self.manifest_url] = b'!' + raw[1:]
        with self.assertRaises(ValueError):
            self.publish()
        self.assertFalse(any(action == 'upload' for action, _ in self.events))

        self.remote[self.manifest_url] = raw
        self.events.clear()
        self.publish()
        pointer = self.events.index(('upload', self.pointer))
        self.assertIn(('fetch', self.manifest_url), self.events[:pointer])
        self.assertNotIn(('upload', self.manifest_destination), self.events)

    def test_existing_official_windows_policy_can_publish_without_a_manifest(self):
        self.config['mode'] = 'official'
        del self.release['installedManifest']
        self.manifest_file.unlink()
        self.publish()
        self.assertEqual(self.remote[self.config['policyURL']], self.envelope)


if __name__ == '__main__':
    unittest.main()
