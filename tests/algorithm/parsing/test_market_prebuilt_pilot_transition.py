"""Pilot CLI transitions with real Git writes and isolated ModelScope calls."""

from contextlib import contextmanager
import gzip
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import socket
import subprocess
import sys
from types import SimpleNamespace
from urllib.error import HTTPError

import pytest

from lazyllm import FileSystemQueue
from lazymind.parsing.service import prebuilt_publish
from lazymind.parsing.service.prebuilt_package import build_package
from test_market_prebuilt_publish import SOURCES, git_target  # noqa: F401


REPO = 'sunjh2222/lazymind-prebuilt-chunks-demo'
PREFIX = 'inline-v1/ci'
BRANCH = 'master'
LATEST = f'{PREFIX}/latest.json'
DATASET = f'https://modelscope.cn/datasets/{REPO}'
GIT_REMOTE = f'{DATASET}.git'
HTTP_BASE = f'{DATASET}/resolve/{BRANCH}/'
READING_ROOM_V1 = '14449fa5e45edbf24bb14b3f0126277ff03d755cd88fb983309e0165902f8c77'
READING_ROOM_V2 = 'aa3bb57c450ed7fc7dfd68f6e7e8f0be10d9ab5c99fc77770dc33b312d7417f6'
TOOL_SHELF = 'c0cb363ee7ed5c91eef3bb01692d10bb4a8ecacdb45ba779a77f6ca3251274f8'


def sha256(data):
    return hashlib.sha256(data).hexdigest()


@pytest.fixture
def source(tmp_path):
    def snapshot(name, opening=b'14:30'):
        directory = tmp_path / name
        directory.mkdir()
        for basename, digest in {'reading-room.md': READING_ROOM_V1, 'tool-shelf.md': TOOL_SHELF}.items():
            data = (SOURCES / basename).read_bytes().replace(b'\r\n', b'\n')
            assert sha256(data) == digest
            if basename == 'reading-room.md':
                assert data.count(b'14:30') == 1
                data = data.replace(b'14:30', opening)
                if opening == b'15:00':
                    assert sha256(data) == READING_ROOM_V2
            (directory / basename).write_bytes(data)
        return directory
    return snapshot


@pytest.fixture
def hub(monkeypatch, git_target, tmp_path):  # noqa: F811
    path = Path(__file__).resolve().parents[3] / 'scripts' / 'publish_market_prebuilt.py'
    spec = importlib.util.spec_from_file_location('publish_market_prebuilt_pilot_test', path)
    cli = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(cli)
    reads, pushes, auth, overrides = [], [], [], {}
    real_clone, real_push = prebuilt_publish._clone_branch, prebuilt_publish._push_branch

    def clone(remote, branch, checkout):
        assert (remote, branch) == (GIT_REMOTE, BRANCH)
        return real_clone(git_target.uri, branch, checkout)

    def push(checkout, branch):
        paths = subprocess.run(
            ['git', '-C', str(checkout), 'diff-tree', '--no-commit-id', '--name-only', '-r', 'HEAD'],
            check=True, capture_output=True, timeout=30,
        ).stdout.decode().splitlines()
        assert len(paths) == 1
        pushes.append(paths[0])
        return real_push(checkout, branch)

    for module in (cli, prebuilt_publish):
        monkeypatch.setattr(module, '_clone_branch', clone)
        monkeypatch.setattr(module, '_push_branch', push)

    class AnonymousHTTP:
        def open(self, request, timeout):
            assert request.get_method() == 'GET' and not request.has_header('Authorization')
            assert request.full_url.startswith(HTTP_BASE + PREFIX + '/')
            relative = request.full_url.removeprefix(HTTP_BASE)
            data = overrides[relative] if relative in overrides else git_target.git_read(relative)
            reads.append((relative, data))
            if data is None:
                raise HTTPError(request.full_url, 404, 'Missing fixture file', {}, None)
            response = io.BytesIO(data)
            response.status = 200
            return response

    def no_network(*args, **kwargs):
        pytest.fail('Unexpected network connection outside the ModelScope stand-in')

    @contextmanager
    def authenticate(opener, workdir, remote):
        assert (type(opener), remote) == (AnonymousHTTP, GIT_REMOTE)
        auth.append(remote)
        yield

    monkeypatch.setattr(cli, 'build_opener', lambda *args: AnonymousHTTP())
    monkeypatch.setattr(cli, '_git_environment', authenticate)
    monkeypatch.setattr(socket, 'create_connection', no_network)
    monkeypatch.setenv('LAZYLLM_HOME', str(tmp_path / 'seed-runtime'))
    monkeypatch.setattr(FileSystemQueue, '__queue_pool__', {})

    def run(directory, revision):
        reads.clear()
        pushes.clear()
        auth.clear()
        with monkeypatch.context() as invocation:
            # Each real CLI process owns its own queues under its temporary home.
            invocation.setattr(FileSystemQueue, '__queue_pool__', {})
            invocation.setattr(sys, 'argv', [
                str(path), '--source-dir', str(directory), '--source-revision', revision,
                '--repo', REPO, '--prefix', PREFIX, '--publish',
            ])
            return cli.main()

    def seed(batch):
        prebuilt_publish.publish_batch(
            batch, git_remote=GIT_REMOTE, branch=BRANCH, prefix=PREFIX, expected_latest=git_target.git_read(LATEST),
            read_file=git_target.git_read, upload_file=lambda path, data: cli._upload_immutable(GIT_REMOTE, path, data),
        )

    def state():
        return git_target.tip(), git_target.git_read(LATEST)

    return SimpleNamespace(run=run, seed=seed, state=state, read=git_target.git_read,
                           reads=reads, pushes=pushes, auth=auth, overrides=overrides)


def release(hub, pointer):
    index = json.loads(pointer)
    manifest_path = f'{PREFIX}/{index["manifest_path"]}'
    manifest = hub.read(manifest_path)
    assert sha256(manifest) == index['manifest_sha256']
    documents = {doc['id']: doc for doc in json.loads(manifest)['documents']}
    assert set(documents) == {'reading-room', 'tool-shelf'}
    for doc in documents.values():
        data = hub.read(object_path(doc))
        assert len(data) == doc['size_bytes']
        assert sha256(data) == doc['object_sha256']
    return manifest_path, documents


def object_path(document):
    return f'{PREFIX}/{document["object_path"]}'


def assert_no_write(hub, before):
    assert hub.state() == before
    assert hub.pushes == []


@pytest.mark.parametrize('v1_revision', ['approved-v1', 'ed259a5a89d24085aba4d04aba844bfe39af6d04'])
def test_cli_publishes_approved_v1_then_only_changed_v2_object_and_manifest(hub, source, v1_revision):
    assert hub.run(source('v1'), v1_revision) == 0
    _, old_pointer = hub.state()
    old_path, old_documents = release(hub, old_pointer)
    assert json.loads(hub.read(old_path))['source_revision'] == v1_revision
    old_objects = [object_path(doc) for doc in old_documents.values()]
    assert sorted(hub.pushes[:-2]) == sorted(old_objects)
    assert hub.pushes[-2:] == [old_path, LATEST]
    assert all((path, hub.read(path)) in hub.reads for path in [*old_objects, old_path, LATEST])

    v2 = source('v2', b'15:00')
    assert hub.run(v2, 'approved-v2') == 0
    after = hub.state()
    assert after[1] != old_pointer
    new_path, new_documents = release(hub, after[1])
    changed = new_documents['reading-room']
    changed_object = object_path(changed)
    assert changed['source_sha256'] == READING_ROOM_V2
    assert b'15:00' in gzip.decompress(hub.read(changed_object))
    assert new_documents['tool-shelf'] == old_documents['tool-shelf']
    assert hub.pushes == [changed_object, new_path, LATEST]
    assert all((path, hub.read(path)) in hub.reads
               for path in [changed_object, object_path(new_documents['tool-shelf']), new_path, LATEST])
    assert release(hub, old_pointer) == (old_path, old_documents)

    assert hub.run(v2, 'another-v2-revision') == 0
    assert_no_write(hub, after)


def test_cli_approved_v2_requires_a_published_v1(hub, source):
    before = hub.state()
    assert before[1] is None
    assert hub.run(source('v2', b'15:00'), 'approved-v2') != 0
    assert (LATEST, None) in hub.reads
    assert_no_write(hub, before)
    assert hub.auth == []


def test_cli_rejects_v1_that_claims_the_approved_source_hash(hub, source, tmp_path):
    def encode(value):
        return (json.dumps(value, ensure_ascii=False, sort_keys=True) + '\n').encode()

    batch = tmp_path / 'forged-batch'
    assert build_package(source('forged-v1', b'13:30'), batch, item_id='local-prebuilt-demo',
                         source_revision='approved-v1')
    latest = batch / 'latest.json'
    pointer = json.loads(latest.read_bytes())
    original = batch / pointer['manifest_path']
    manifest = json.loads(original.read_bytes())
    forged = next(doc for doc in manifest['documents'] if doc['id'] == 'reading-room')
    forged['source_sha256'] = READING_ROOM_V1
    manifest.pop('version')
    version = 'v-' + sha256(encode(manifest))[:24]
    raw = encode({**manifest, 'version': version})
    pointer.update(version=version, manifest_path=f'releases/{version}/manifest.json', manifest_sha256=sha256(raw))
    original.unlink()
    counterfeit = batch / pointer['manifest_path']
    counterfeit.parent.mkdir()
    counterfeit.write_bytes(raw)
    latest.write_bytes(encode(pointer))
    hub.seed(batch)
    before = hub.state()
    manifest_path, documents = release(hub, before[1])
    assert documents['reading-room']['source_sha256'] == READING_ROOM_V1
    assert b'13:30' in gzip.decompress(hub.read(object_path(documents['reading-room'])))

    assert hub.run(source('v2', b'15:00'), 'approved-v2') != 0
    assert (LATEST, before[1]) in hub.reads
    assert (manifest_path, raw) in hub.reads
    assert_no_write(hub, before)
    assert hub.auth == []


def test_cli_checks_old_changed_object_readback_before_auth_or_write(hub, source):
    assert hub.run(source('v1'), 'approved-v1') == 0
    before = hub.state()
    _, documents = release(hub, before[1])
    path = object_path(documents['reading-room'])
    data = hub.read(path)
    corrupt = data[:4] + bytes([data[4] ^ 1]) + data[5:]
    assert len(corrupt) == len(data) and sha256(corrupt) != sha256(data)
    assert gzip.decompress(corrupt) == gzip.decompress(data)
    hub.overrides[path] = corrupt

    assert hub.run(source('v2', b'15:00'), 'approved-v2') != 0
    assert (path, corrupt) in hub.reads
    assert_no_write(hub, before)
    assert hub.auth == []


def test_cli_refuses_v2_before_push_when_verified_public_v1_is_stale(hub, source, tmp_path):
    assert hub.run(source('v1'), 'approved-v1') == 0
    _, approved_pointer = hub.state()
    approved_manifest, _ = release(hub, approved_pointer)
    competing = tmp_path / 'competing-batch'
    assert build_package(source('competing', b'13:30'), competing, item_id='local-prebuilt-demo',
                         source_revision='competing-release')
    hub.seed(competing)
    before = hub.state()
    _, competing_documents = release(hub, before[1])
    assert before[1] != approved_pointer
    assert b'13:30' in gzip.decompress(hub.read(object_path(competing_documents['reading-room'])))
    hub.overrides[LATEST] = approved_pointer

    assert hub.run(source('v2', b'15:00'), 'approved-v2') != 0
    assert (LATEST, approved_pointer) in hub.reads
    assert hub.read(approved_manifest) is not None
    assert_no_write(hub, before)


@pytest.mark.parametrize('public_latest_missing', [False, True], ids=['current', 'stale-404'])
def test_cli_does_not_roll_back_v2_to_v1(hub, source, tmp_path, public_latest_missing):
    v1 = source('v1')
    assert hub.run(v1, 'approved-v1') == 0
    previous_path, _ = release(hub, hub.state()[1])
    previous = tmp_path / 'previous.json'
    previous.write_bytes(hub.read(previous_path))
    v2_batch = tmp_path / 'v2-batch'
    assert build_package(source('v2', b'15:00'), v2_batch, item_id='local-prebuilt-demo', source_revision='approved-v2',
                         previous_manifest=previous)
    hub.seed(v2_batch)
    before = hub.state()
    _, documents = release(hub, before[1])
    assert documents['reading-room']['source_sha256'] == READING_ROOM_V2
    if public_latest_missing:
        hub.overrides[LATEST] = None

    hub.run(v1, 'different-v1-revision')
    assert (LATEST, None if public_latest_missing else before[1]) in hub.reads
    assert_no_write(hub, before)
    if not public_latest_missing:
        assert hub.auth == []


def test_cli_still_rejects_two_documents_with_unapproved_bytes(hub, source):
    assert hub.run(source('v1'), 'approved-v1') == 0
    before = hub.state()
    assert hub.run(source('unapproved', b'13:30'), 'unapproved') != 0
    assert_no_write(hub, before)
    assert hub.auth == []
