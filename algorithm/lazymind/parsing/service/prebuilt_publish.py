"""Publish immutable prebuilt files and advance their Git-backed release pointer."""

import hashlib
import json
import os
import re
import subprocess
from functools import partial
from pathlib import Path
from tempfile import TemporaryDirectory


_VERSION = re.compile(r'[A-Za-z0-9][A-Za-z0-9._-]{0,31}')
_COMPONENT = re.compile(r'[A-Za-z0-9][A-Za-z0-9._-]*')
_SHA256 = re.compile(r'[a-f0-9]{64}')
_POINTER_LIMIT = 64 << 10
_MANIFEST_LIMIT = 4 << 20
_OBJECT_LIMIT = 16 << 20
_TOTAL_LIMIT = 512 << 20


class PublicationError(RuntimeError):
    """The package cannot be published safely."""


class PublicationConflict(PublicationError):
    """A different release displaced the expected publication baseline."""


def _sha256(data):
    return hashlib.sha256(data).hexdigest()


def _matches_object(data, *, digest, size):
    return len(data) == size and _sha256(data) == digest


def _read_local(path, limit):
    if path.is_symlink():
        raise PublicationError(f'Package file must not be a symbolic link: {path.name}')
    with path.open('rb') as stream:
        data = stream.read(limit + 1)
    if not data or len(data) > limit:
        raise PublicationError(f'Package file is empty or exceeds its size limit: {path.name}')
    return data


def _load_package(batch_dir):
    try:
        pointer_bytes = _read_local(batch_dir / 'latest.json', _POINTER_LIMIT)
        pointer = json.loads(pointer_bytes)
        version = pointer['version']
        manifest_path = f'releases/{version}/manifest.json'
        if not _VERSION.fullmatch(version) or pointer['manifest_path'] != manifest_path:
            raise ValueError('Invalid manifest path for release version')
        manifest_bytes = _read_local(batch_dir / manifest_path, _MANIFEST_LIMIT)
        if _sha256(manifest_bytes) != pointer['manifest_sha256']:
            raise ValueError('Manifest SHA-256 does not match the pointer')
        manifest = json.loads(manifest_bytes)
        item = pointer['item_id']
        documents = manifest['documents']
        if (not isinstance(item, str) or not item.strip()
                or manifest['schema_version'] != 1 or manifest['version'] != version
                or manifest['item_id'] != item or manifest['chunk_profile'] != 'general_algo'
                or not isinstance(manifest['source_revision'], str) or not manifest['source_revision'].strip()
                or not isinstance(documents, list) or not 1 <= len(documents) <= 1000
                or len(documents) != pointer['document_count']):
            raise ValueError('Manifest identity, profile, or document count is invalid')
        objects, identities = {}, set()
        size_bytes = chunk_count = 0
        for document in documents:
            digest, size = document['object_sha256'], document['size_bytes']
            path, chunks = document['object_path'], document['chunk_count']
            identity = document['id']
            if (not _SHA256.fullmatch(digest) or path != f'objects/{digest}.jsonl.gz'
                    or type(size) is not int or not 1 <= size <= _OBJECT_LIMIT
                    or type(chunks) is not int or chunks < 1
                    or not isinstance(identity, str) or not identity or identity in identities
                    or (path in objects and objects[path] != (digest, size))):
                raise ValueError('Manifest contains an invalid object reference')
            identities.add(identity)
            objects[path] = (digest, size)
            size_bytes += size
            chunk_count += chunks
        if (size_bytes != pointer['size_bytes'] or size_bytes > _TOTAL_LIMIT
                or chunk_count != pointer['chunk_count'] or chunk_count > 1_000_000):
            raise ValueError('Manifest totals do not match the pointer')
        return pointer_bytes, manifest_path, manifest_bytes, objects
    except (OSError, ValueError, KeyError, TypeError) as exc:
        raise PublicationError(f'Invalid local publication batch: {exc}') from exc


def bind_upstream_manifest(batch_dir, upstream):
    """Bind source provenance to the release version before exposing the staged batch."""
    raw_pointer, old_path, raw_manifest, _ = _load_package(batch_dir)
    manifest = json.loads(raw_manifest)
    del manifest['version']
    manifest['upstream'] = upstream

    def encode(value):
        return (json.dumps(value, ensure_ascii=False, sort_keys=True) + '\n').encode('utf-8')

    version = 'v-' + _sha256(encode(manifest))[:24]
    manifest_path = f'releases/{version}/manifest.json'
    manifest_bytes = encode({**manifest, 'version': version})
    if len(manifest_bytes) > _MANIFEST_LIMIT:
        raise PublicationError('Manifest with upstream provenance exceeds the package size limit')
    target = batch_dir / manifest_path
    target.parent.mkdir()
    target.write_bytes(manifest_bytes)
    pointer = json.loads(raw_pointer)
    pointer.update(version=version, manifest_path=manifest_path, manifest_sha256=_sha256(manifest_bytes))
    (batch_dir / 'latest.json').write_bytes(encode(pointer))
    previous = batch_dir / old_path
    previous.unlink()
    previous.parent.rmdir()


def _run_git(checkout, *args, data=None, allow_failure=False):
    env = {**os.environ, 'GIT_LFS_SKIP_SMUDGE': '1', 'GIT_TERMINAL_PROMPT': '0'}
    try:
        result = subprocess.run(
            ['git', '-C', str(checkout), *args], input=data, capture_output=True, timeout=120, env=env,
        )
    except subprocess.TimeoutExpired as exc:
        if allow_failure:
            return None
        raise PublicationError(f'Git {args[0]} timed out') from exc
    except OSError as exc:
        raise PublicationError(f'Could not run Git {args[0]}') from exc
    if result.returncode and not allow_failure:
        raise PublicationError(f'Git {args[0]} failed')
    return result


def _clone_branch(git_remote, branch, checkout):
    _run_git(checkout, 'clone', '--quiet', '--no-checkout', '--no-tags', '--depth', '1',
             '--branch', branch, '--', str(git_remote), '.')


def _read_git_file(checkout, revision, path):
    entry = _run_git(checkout, 'ls-tree', '-z', revision, '--', path).stdout
    if not entry:
        return None
    # Only ordinary Git blobs are supported for the pilot.
    if not entry.startswith(b'100644 blob '):
        raise PublicationError(f'Expected a regular Git file at {path}')
    return _run_git(checkout, 'show', f'{revision}:{path}').stdout


def _commit_file(checkout, path, data, message):
    _run_git(checkout, 'read-tree', 'HEAD')
    blob = _run_git(checkout, 'hash-object', '-w', '--stdin', data=data).stdout.decode('ascii').strip()
    _run_git(checkout, 'update-index', '--add', '--cacheinfo', f'100644,{blob},{path}')
    _run_git(checkout, '-c', 'user.name=LazyMind Knowledge Market',
             '-c', 'user.email=knowledge-market@users.noreply.github.com',
             '-c', 'commit.gpgsign=false', 'commit', '--quiet', '-m', message)


def _push_branch(checkout, branch):
    result = _run_git(checkout, 'push', '--no-signed', 'origin', f'HEAD:refs/heads/{branch}', allow_failure=True)
    return result is not None and result.returncode == 0


def _fetch_branch(checkout, branch):
    _run_git(checkout, 'fetch', '--quiet', '--depth', '1', 'origin', f'refs/heads/{branch}')


def _promote_pointer(git_remote, branch, path, expected_latest, pointer):
    with TemporaryDirectory(prefix='market-prebuilt-pointer-') as checkout:
        _clone_branch(git_remote, branch, checkout)
        current = _read_git_file(checkout, 'HEAD', path)
        if current == pointer:
            return
        if current != expected_latest:
            raise PublicationConflict('The published version changed since the build started')
        _commit_file(checkout, path, pointer, 'Publish verified LazyMind knowledge market release')
        if _push_branch(checkout, branch):
            return

        # A timeout or broken connection can occur after Git accepted the push.
        # Observe the authoritative ref before deciding what the failure meant.
        try:
            _fetch_branch(checkout, branch)
            current = _read_git_file(checkout, 'FETCH_HEAD', path)
        except PublicationError as exc:
            raise PublicationError('Git push outcome is unknown and must be checked before retrying') from exc
        if current == pointer:
            return
        if current != expected_latest:
            raise PublicationConflict('Another publisher advanced the release pointer')
        raise PublicationError('The release pointer was not advanced; retry from the current branch')


def _ensure_dependency(path, local, matches, read_file, upload_file):
    current = read_file(path)
    if current is None:
        if local is None:
            raise PublicationError(f'Reused dependency is missing from the remote release: {path}')
        try:
            upload_file(path, local)
        except PublicationError:
            # A concurrent upload or a lost write acknowledgement can still
            # leave the exact immutable bytes available on the target.
            current = read_file(path)
            if current is None:
                raise
        else:
            current = read_file(path)
    if current is None or not matches(current):
        raise PublicationError(f'Anonymous readback does not match the release dependency: {path}')


def publish_batch(batch_dir, *, git_remote, branch, prefix, expected_latest, read_file, upload_file):
    """Upload and verify dependencies, then conditionally publish latest.json."""
    if prefix and not all(_COMPONENT.fullmatch(part) for part in prefix.split('/')):
        raise PublicationError('Invalid release prefix')
    if not _COMPONENT.fullmatch(branch):
        raise PublicationError('Invalid publication branch')
    repository_path = f'{prefix}/' if prefix else ''
    batch_dir = Path(batch_dir)
    pointer, manifest_path, manifest, objects = _load_package(batch_dir)
    latest_path = repository_path + 'latest.json'
    if read_file(latest_path) not in (expected_latest, pointer):
        raise PublicationConflict('The published version changed since the build started')

    for path, (digest, size) in objects.items():
        matches = partial(_matches_object, digest=digest, size=size)
        candidate = batch_dir / path
        try:
            local = _read_local(candidate, size)
        except FileNotFoundError:
            local = None
        except OSError as exc:
            raise PublicationError(f'Could not read local dependency: {path}') from exc
        if local is not None and not matches(local):
            raise PublicationError(f'Local object SHA-256 does not match the manifest: {path}')
        _ensure_dependency(repository_path + path, local, matches, read_file, upload_file)
    _ensure_dependency(repository_path + manifest_path, manifest, lambda data: data == manifest, read_file, upload_file)
    _promote_pointer(git_remote, branch, latest_path, expected_latest, pointer)
