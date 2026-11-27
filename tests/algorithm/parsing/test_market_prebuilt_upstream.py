"""Registered upstream scans through real local Git and the publication CLI."""

import gzip
import hashlib
import importlib.util
import io
import json
from contextlib import nullcontext
from pathlib import Path
import socket
import subprocess
import sys
from urllib.error import HTTPError

import pytest

from lazyllm import FileSystemQueue
from lazymind.parsing.service import prebuilt_publish, prebuilt_upstream
from test_market_prebuilt_publish import SOURCES, git_target  # noqa: F401


REPO = 'example/published-chunks'
PREFIX = 'market/bookshop'
ITEM = 'market-bookshop'
LATEST = f'{PREFIX}/latest.json'
TARGET = f'https://modelscope.cn/datasets/{REPO}'
SOURCE = 'example/bookshop'
REF = 'refs/heads/main'


def _git_run(directory, *args):
    return subprocess.run(['git', '-C', str(directory), *args], capture_output=True,
                          check=True, timeout=30).stdout.decode().strip()


@pytest.fixture
def upstream_cli():
    script = Path(__file__).resolve().parents[3] / 'scripts' / 'publish_market_prebuilt.py'
    spec = importlib.util.spec_from_file_location('publish_market_registered_upstream_test', script)
    cli = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(cli)
    return cli


@pytest.fixture
def source_git(tmp_path, monkeypatch):
    repo = tmp_path / 'source-git'
    repo.mkdir()
    _git_run(repo, 'init', '-q', '-b', 'main')
    _git_run(repo, 'config', 'user.name', 'Source fixture')
    _git_run(repo, 'config', 'user.email', 'source@example.invalid')
    _git_run(repo, 'config', 'commit.gpgsign', 'false')
    _git_run(repo, 'config', 'core.autocrlf', 'false')

    def commit(message):
        _git_run(repo, 'add', '-A')
        _git_run(repo, 'commit', '-qm', message)
        return _git_run(repo, 'rev-parse', 'HEAD')

    fetches = []
    run = subprocess.run

    def redirect_source(cmd, **kwargs):
        if cmd[:2] == ['git', '-C'] and 'fetch' in cmd and '--no-tags' in cmd and '--' in cmd:
            official = {f'https://huggingface.co/datasets/{SOURCE}', f'https://modelscope.cn/datasets/{SOURCE}.git'}
            assert cmd[-2] in official
            assert cmd[-1] == REF
            assert kwargs['env']['GIT_ALLOW_PROTOCOL'] == 'https'
            assert 'MODELSCOPE_API_TOKEN' not in kwargs['env'] and 'HF_TOKEN' not in kwargs['env']
            assert 'http.followRedirects=false' in cmd
            fetches.append(cmd[-2])
            cmd = [*cmd[:-2], repo.as_uri(), REF]
            kwargs['env'] = {**kwargs['env'], 'GIT_ALLOW_PROTOCOL': 'file'}
        return run(cmd, **kwargs)

    monkeypatch.setattr(prebuilt_upstream.subprocess, 'run', redirect_source)
    return repo, commit, fetches


def test_upstream_scan_publishes_changes_and_deletions(tmp_path, monkeypatch, source_git, upstream_cli,
                                                       git_target):  # noqa: F811
    source, commit, fetches = source_git
    docs = source / 'docs'
    docs.mkdir()
    (source / 'outside.md').write_bytes(b'Not part of the registered source directory.\n')
    for name in ('reading-room.md', 'tool-shelf.md'):
        (docs / name).write_bytes((SOURCES / name).read_bytes().replace(b'\r\n', b'\n'))
    first_revision = commit('initial markdown snapshot')

    cli = upstream_cli
    real_clone, real_push = prebuilt_publish._clone_branch, prebuilt_publish._push_branch
    pushes, rejected_objects = [], set()

    def clone(remote, branch, checkout):
        assert remote == f'{TARGET}.git' and branch == 'master'
        return real_clone(git_target.uri, branch, checkout)

    def push(checkout, branch):
        pushes.extend(_git_run(checkout, 'diff-tree', '--no-commit-id', '--name-only', '-r', 'HEAD').splitlines())
        return real_push(checkout, branch)

    class AnonymousHTTP:
        def open(self, request, timeout):
            base = f'{TARGET}/resolve/master/'
            assert request.full_url.startswith(base) and not request.has_header('Authorization')
            path = request.full_url.removeprefix(base)
            data = git_target.git_read(path)
            if data is None:
                raise HTTPError(request.full_url, 404, 'Missing fixture file', {}, None)
            if rejected_objects and path.startswith(f'{PREFIX}/objects/') and path not in rejected_objects:
                data = b'simulated corrupt public readback'
            response = io.BytesIO(data)
            response.status = 200
            return response

    def no_network(*args, **kwargs):
        pytest.fail('Unexpected connection outside the test Git and HTTP endpoints')

    for module in (cli, prebuilt_publish):
        monkeypatch.setattr(module, '_clone_branch', clone)
        monkeypatch.setattr(module, '_push_branch', push)
    monkeypatch.setattr(cli, 'build_opener', lambda *args: AnonymousHTTP())
    monkeypatch.setattr(cli, '_git_environment', lambda *args: nullcontext())
    monkeypatch.setattr(socket, 'create_connection', no_network)
    monkeypatch.setenv('LAZYLLM_HOME', str(tmp_path / 'runtime'))

    def run(directory='docs'):
        pushes.clear()
        with monkeypatch.context() as invocation:
            invocation.setattr(FileSystemQueue, '__queue_pool__', {})
            invocation.setattr(sys, 'argv', [
                cli.__file__, '--upstream-provider', 'huggingface', '--upstream-repo', SOURCE, '--upstream-ref', REF,
                '--upstream-dir', directory, '--item-id', ITEM, '--repo', REPO, '--prefix', PREFIX, '--publish',
            ])
            return cli.main()

    def manifest():
        pointer = json.loads(git_target.git_read(LATEST))
        path = f'{PREFIX}/{pointer["manifest_path"]}'
        raw = git_target.git_read(path)
        assert hashlib.sha256(raw).hexdigest() == pointer['manifest_sha256']
        release = json.loads(raw)
        return path, release, {doc['id']: doc for doc in release['documents']}

    def object_path(doc):
        return f'{PREFIX}/{doc["object_path"]}'

    assert run() == 0
    first_path, first, original = manifest()
    assert first['source_revision'] == first_revision
    assert first['upstream'] == {'provider': 'huggingface', 'repo': SOURCE, 'ref': REF, 'directory': 'docs'}
    assert set(original) == {'reading-room', 'tool-shelf'}
    assert set(pushes[:-2]) == set(map(object_path, original.values()))
    assert pushes[-2:] == [first_path, LATEST]

    first_tip = git_target.tip()
    assert run() == 0 and not pushes and git_target.tip() == first_tip
    (source / 'outside.md').write_bytes(b'An irrelevant change changes the commit ID.\n')
    assert commit('change outside the source directory') != first_revision
    assert run() == 0 and not pushes and git_target.tip() == first_tip

    notice = docs / 'reading-room.md'
    notice.write_bytes(notice.read_bytes().replace(b'14:30', b'15:00'))
    changed_revision = commit('change one published document')
    assert run() == 0
    changed_path, changed, updated = manifest()
    assert changed['source_revision'] == changed_revision and updated['tool-shelf'] == original['tool-shelf']
    changed_object = object_path(updated['reading-room'])
    assert b'15:00' in gzip.decompress(git_target.git_read(changed_object))
    assert pushes == [changed_object, changed_path, LATEST]

    (docs / 'tool-shelf.md').unlink()
    deleted_revision = commit('withdraw one source document')
    assert run() == 0
    deleted_path, deleted, remaining = manifest()
    assert deleted['source_revision'] == deleted_revision and remaining == {'reading-room': updated['reading-room']}
    assert pushes == [deleted_path, LATEST]
    assert git_target.git_read(first_path) is not None and git_target.git_read(object_path(original['tool-shelf']))

    pointer_before = git_target.git_read(LATEST)
    fetch_count = len(fetches)
    assert run(directory='.') == 1 and not pushes and len(fetches) == fetch_count
    assert git_target.git_read(LATEST) == pointer_before

    rejected_objects.add(changed_object)
    notice.write_bytes(notice.read_bytes().replace(b'15:00', b'16:00'))
    commit('change a retained document')
    assert run() == 1 and LATEST not in pushes
    assert git_target.git_read(LATEST) == pointer_before


def test_same_modelscope_repository_is_rejected_before_remote_operations(monkeypatch, capsys, upstream_cli):
    def forbidden(*args, **kwargs):
        pytest.fail('A conflicting ModelScope registration reached a remote operation')

    for name in ('_read_modelscope', '_git_environment', 'publish_batch'):
        monkeypatch.setattr(upstream_cli, name, forbidden)
    monkeypatch.setattr(prebuilt_upstream.UpstreamSource, 'fetch', forbidden)
    monkeypatch.setattr(sys, 'argv', [
        upstream_cli.__file__, '--upstream-provider', 'modelscope', '--upstream-repo', SOURCE.upper(),
        '--upstream-ref', REF, '--upstream-dir', 'docs', '--item-id', ITEM,
        '--repo', SOURCE, '--prefix', PREFIX, '--publish',
    ])
    assert upstream_cli.main() == 1
    assert 'ModelScope upstream and destination must be different repositories' in capsys.readouterr().err


def test_modelscope_source_materializes_only_verified_commit_pinned_lfs(tmp_path, monkeypatch, source_git):
    repo, commit, _ = source_git
    data = b'# Reading room\n\nOpens at 14:30.\n'
    (repo / 'guide.md').write_bytes(
        b'version https://git-lfs.github.com/spec/v1\n'
        + f'oid sha256:{hashlib.sha256(data).hexdigest()}\nsize {len(data)}\n'.encode(),
    )
    revision = commit('Git LFS pointer')
    source = prebuilt_upstream.UpstreamSource('modelscope', SOURCE, REF, '.')
    checkout = tmp_path / 'checkout'
    assert source.fetch(checkout) == revision

    class ObjectHTTP:
        def open(self, request, timeout):
            assert request.full_url == f'https://modelscope.cn/datasets/{SOURCE}/resolve/{revision}/guide.md'
            assert not request.has_header('Authorization')
            response = io.BytesIO(payload)
            response.status = 200
            return response

    payload = data
    monkeypatch.setattr(prebuilt_upstream, 'build_opener', lambda *args: ObjectHTTP())
    source.materialize(checkout, revision, tmp_path / 'snapshot')
    assert (tmp_path / 'snapshot' / 'guide.md').read_bytes() == data

    payload = data.replace(b'14:30', b'15:00')
    with pytest.raises(prebuilt_publish.PublicationError, match='committed size and SHA-256'):
        source.materialize(checkout, revision, tmp_path / 'corrupt-snapshot')
