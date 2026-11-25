"""Publication contract against a real Git ref with a local ModelScope stand-in."""

import gzip
import hashlib
import importlib
import json
import os
from pathlib import Path
import shutil
import subprocess

import pytest

from lazymind.parsing.service.prebuilt_package import build_package


PREFIX = 'inline-v1'
BRANCH = 'master'
REF = f'refs/heads/{BRANCH}'
LATEST = f'{PREFIX}/latest.json'
SOURCES = Path(__file__).resolve().parents[1] / 'fixtures' / 'market_prebuilt'


@pytest.fixture
def publisher():
    module = 'lazymind.parsing.service.prebuilt_publish'
    try:
        return importlib.import_module(module)
    except ModuleNotFoundError as exc:
        if exc.name != module:
            raise
        pytest.fail('Publication implementation pending: expected prebuilt_publish.publish_batch', pytrace=False)


@pytest.fixture
def git_target(tmp_path, monkeypatch):
    # The fake SDK writes immutable payloads via ordinary commits. Keep its
    # subprocess separate from the hook that coordinates publication pushes.
    run = subprocess.run
    monkeypatch.setenv('GIT_CONFIG_NOSYSTEM', '1')
    monkeypatch.setenv('GIT_CONFIG_GLOBAL', os.devnull)
    monkeypatch.setenv('GIT_TERMINAL_PROMPT', '0')
    monkeypatch.setenv('GIT_ALLOW_PROTOCOL', 'file')
    monkeypatch.setenv('GIT_AUTHOR_NAME', 'Prebuilt fixture')
    monkeypatch.setenv('GIT_AUTHOR_EMAIL', 'prebuilt-fixture@example.invalid')
    monkeypatch.setenv('GIT_COMMITTER_NAME', 'Prebuilt fixture')
    monkeypatch.setenv('GIT_COMMITTER_EMAIL', 'prebuilt-fixture@example.invalid')
    # Environment-scoped defaults also apply inside the publisher's clones.
    settings = (('core.autocrlf', 'false'), ('commit.gpgsign', 'false'), ('push.gpgsign', 'false'))
    for index, (key, setting) in enumerate(settings):
        monkeypatch.setenv(f'GIT_CONFIG_KEY_{index}', key)
        monkeypatch.setenv(f'GIT_CONFIG_VALUE_{index}', setting)
    monkeypatch.setenv('GIT_CONFIG_COUNT', str(len(settings)))

    bare, checkout = tmp_path / 'hub.git', tmp_path / 'sdk-checkout'
    options = {'check': True, 'capture_output': True, 'timeout': 30}
    run(['git', 'init', '--bare', f'--initial-branch={BRANCH}', str(bare)], **options)
    run(['git', 'clone', bare.as_uri(), str(checkout)], **options)

    def sdk_git(*args):
        return run(['git', '-C', str(checkout), *args], **options).stdout

    sdk_git('commit', '--allow-empty', '-m', 'Initialize empty dataset')
    sdk_git('push', 'origin', f'HEAD:{REF}')

    class Target:
        uri = bare.as_uri()

        def __init__(self):
            self.uploaded = []
            self.readbacks = []

        def git_read(self, path):
            lookup = ['git', '-C', str(bare)]
            present = run([*lookup, 'ls-tree', '--name-only', REF, '--', path], **options).stdout
            if not present:
                return None
            return run([*lookup, 'show', f'{REF}:{path}'], **options).stdout

        def read_file(self, path):
            data = self.git_read(path)
            self.readbacks.append((path, data))
            return data

        def upload_file(self, path, data):
            assert path.startswith((f'{PREFIX}/objects/', f'{PREFIX}/releases/')), path
            assert self.git_read(path) is None, f'Would overwrite an immutable dependency: {path}'
            sdk_git('fetch', 'origin', BRANCH)
            sdk_git('reset', '--hard', 'FETCH_HEAD')
            target = checkout / path
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)
            sdk_git('add', '--', path)
            sdk_git('commit', '-m', f'Upload immutable {path}')
            sdk_git('push', 'origin', f'HEAD:{REF}')
            self.uploaded.append(path)

        def tip(self):
            return run(['git', '-C', str(bare), 'rev-parse', REF], **options).stdout

    return Target()


@pytest.fixture
def build_release(tmp_path):
    def build(revision, previous=None, bookmark=None):
        source = shutil.copytree(SOURCES, tmp_path / f'source-{revision}')
        if bookmark is not None:
            document = source / 'reading-room.md'
            text = document.read_text(encoding='utf-8').replace('蓝羽书签', bookmark)
            document.write_text(text, encoding='utf-8')
        batch = tmp_path / f'batch-{revision}'
        assert build_package(source, batch, item_id='local-prebuilt-demo', source_revision=revision,
                             previous_manifest=previous)
        return batch
    return build


def publish(publisher, target, batch, expected_latest=None, read_file=None):
    publisher.publish_batch(
        batch, git_remote=target.uri, branch=BRANCH, prefix=PREFIX, expected_latest=expected_latest,
        read_file=target.read_file if read_file is None else read_file, upload_file=target.upload_file,
    )


def assert_published(target, batch):
    latest = (batch / 'latest.json').read_bytes()
    assert target.git_read(LATEST) == latest
    pointer = json.loads(latest)
    manifest_path = pointer['manifest_path']
    raw_manifest = target.git_read(f'{PREFIX}/{manifest_path}')
    assert raw_manifest == (batch / manifest_path).read_bytes()
    assert hashlib.sha256(raw_manifest).hexdigest() == pointer['manifest_sha256']
    for entry in json.loads(raw_manifest)['documents']:
        data = target.git_read(f'{PREFIX}/{entry["object_path"]}')
        assert data is not None and len(data) == entry['size_bytes']
        assert hashlib.sha256(data).hexdigest() == entry['object_sha256']
    return batch / manifest_path


def is_git_push(args):
    return isinstance(args, (list, tuple)) and args and Path(args[0]).stem == 'git' and 'push' in args


def test_initial_publication_advances_pointer_after_verified_dependencies(
        publisher, git_target, build_release, monkeypatch):
    baseline = build_release('baseline')
    assert git_target.git_read(LATEST) is None
    expected = {
        f'{PREFIX}/{path.relative_to(baseline).as_posix()}': path.read_bytes()
        for path in baseline.rglob('*') if path.is_file() and path.name != 'latest.json'
    }
    assert len(expected) == 3
    real_run = subprocess.run
    first_push = False

    def verify_first_pointer_push(args, *positional, **kwargs):
        nonlocal first_push
        if is_git_push(args) and not first_push:
            assert git_target.git_read(LATEST) is None
            assert all((path, data) in git_target.readbacks for path, data in expected.items())
            first_push = True
        return real_run(args, *positional, **kwargs)

    # SDK dependency commits bypass this wrapper. Observe the publisher's
    # first pointer push while the remote still has no published release.
    monkeypatch.setattr(subprocess, 'run', verify_first_pointer_push)
    publish(publisher, git_target, baseline)
    assert first_push
    assert len(git_target.uploaded) == len(expected) and set(git_target.uploaded) == set(expected)
    assert_published(git_target, baseline)


@pytest.mark.parametrize('dependency', ['object', 'manifest'])
def test_existing_release_survives_an_invalid_dependency_readback(
        publisher, git_target, build_release, dependency):
    baseline = build_release('baseline')
    publish(publisher, git_target, baseline)
    previous = assert_published(git_target, baseline)
    before = git_target.git_read(LATEST)
    candidate = build_release('bad-readback', previous, '绿羽书签')
    new_file, = (candidate.glob('objects/*.jsonl.gz') if dependency == 'object'
                 else candidate.glob('releases/*/manifest.json'))
    dependency_path = f'{PREFIX}/{new_file.relative_to(candidate).as_posix()}'
    original = new_file.read_bytes()
    if dependency == 'object':
        changed = original[:4] + bytes((original[4] ^ 1,)) + original[5:]
        assert gzip.decompress(changed) == gzip.decompress(original)
    else:
        assert original.endswith(b'\n')
        changed = original[:-1] + b' '
        assert json.loads(changed) == json.loads(original)
    assert len(changed) == len(original) and hashlib.sha256(changed).digest() != hashlib.sha256(original).digest()
    corrupted = []

    def damaged_readback(path):
        data = git_target.read_file(path)
        if path == dependency_path and data is not None:
            assert data == original
            corrupted.append(path)
            assert git_target.git_read(LATEST) == before
            return changed
        return data

    with pytest.raises(publisher.PublicationError) as error:
        publish(publisher, git_target, candidate, before, read_file=damaged_readback)
    assert not isinstance(error.value, publisher.PublicationConflict)
    assert corrupted and dependency_path in git_target.uploaded
    assert git_target.git_read(dependency_path) == original
    assert_published(git_target, baseline)


def test_stale_publisher_cannot_overwrite_another_publishers_pointer(
        publisher, git_target, build_release, monkeypatch):
    baseline = build_release('baseline')
    publish(publisher, git_target, baseline)
    previous = assert_published(git_target, baseline)
    before = git_target.git_read(LATEST)
    contender = build_release('contender', previous, '绿羽书签')
    winner = build_release('winner', previous, '朱羽书签')
    contender_latest = (contender / 'latest.json').read_bytes()
    winner_latest = (winner / 'latest.json').read_bytes()
    assert len({before, contender_latest, winner_latest}) == 3
    dependencies = {
        f'{PREFIX}/{path.relative_to(contender).as_posix()}': path.read_bytes()
        for path in contender.rglob('*') if path.is_file() and path.name != 'latest.json'
    }

    real_run = subprocess.run
    winner_tip = None

    def race_pointer_push(args, *positional, **kwargs):
        nonlocal winner_tip
        if winner_tip is None and is_git_push(args):
            command = list(args[:args.index('push')])
            planned = real_run([*command, 'show', f'HEAD:{LATEST}'],
                               cwd=kwargs.get('cwd'), capture_output=True, check=True, timeout=30).stdout
            assert planned == contender_latest
            assert dependencies and all((path, data) in git_target.readbacks for path, data in dependencies.items())
            assert git_target.git_read(LATEST) == before
            # Set the guard before invoking the competitor to let its actual push run.
            winner_tip = b''
            publish(publisher, git_target, winner, before)
            assert_published(git_target, winner)
            winner_tip = git_target.tip()
        return real_run(args, *positional, **kwargs)

    # Run the competitor before A's push negotiates the remote ref. A normal
    # non-forced push can reject this stale commit, and a conditional lease
    # can too. An unconditional force push would replace the winner here.
    monkeypatch.setattr(subprocess, 'run', race_pointer_push)
    with pytest.raises(publisher.PublicationConflict):
        publish(publisher, git_target, contender, before)
    assert winner_tip and git_target.tip() == winner_tip
    assert_published(git_target, winner)
