#!/usr/bin/env python3
"""Build or publish prebuilt Markdown slices for a ModelScope knowledge market release."""

import argparse
import hashlib
import json
import os
import re
import subprocess
import sys
import zlib
from contextlib import contextmanager
from functools import partial
from pathlib import Path
from tempfile import TemporaryDirectory
from urllib.error import HTTPError
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, Request, build_opener

from lazymind.parsing.service.prebuilt_publish import (
    PublicationConflict,
    PublicationError,
    _clone_branch,
    _commit_file,
    _fetch_branch,
    _load_package,
    _push_branch,
    _read_git_file,
    _read_local,
    bind_upstream_manifest,
    publish_batch,
)
from lazymind.parsing.service.prebuilt_upstream import UpstreamSource, valid_dataset_repo


ORIGIN = 'https://modelscope.cn'
PILOT_REPO = 'sunjh2222/lazymind-prebuilt-chunks-demo'
PILOT_PREFIX = 'inline-v1/ci'
BRANCH = 'master'
ITEM_ID = 'local-prebuilt-demo'
PILOT_SOURCES = {
    'reading-room.md': '14449fa5e45edbf24bb14b3f0126277ff03d755cd88fb983309e0165902f8c77',
    'tool-shelf.md': 'c0cb363ee7ed5c91eef3bb01692d10bb4a8ecacdb45ba779a77f6ca3251274f8',
}
PILOT_V2_READING_ROOM = 'aa3bb57c450ed7fc7dfd68f6e7e8f0be10d9ab5c99fc77770dc33b312d7417f6'
# Pin the reviewed document metadata and decoded slices. Git revisions and
# gzip encodings can vary; the manifest still binds each compressed object.
PILOT_DOCUMENTS_SHA256 = {
    'v1': 'e842506260a8aaff355e1c46c59b87ed4c27dd2874056e01832a1209640dcc5b',
    'v2': '921c8463c68a5ea981fc66ec4638713824fdc37c61f1f1433310305db33733ac',
}


class _SameOrigin(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        target = urlsplit(newurl)
        if req.get_method() != 'GET' or (target.scheme, target.netloc) != ('https', 'modelscope.cn'):
            raise PublicationError('ModelScope redirected outside the permitted download origin')
        return super().redirect_request(req, fp, code, msg, headers, newurl)


def _read_modelscope(opener, base, path):
    if path.endswith('/latest.json'):
        limit = 64 << 10
    elif path.endswith('/manifest.json'):
        limit = 4 << 20
    else:
        limit = 16 << 20
    req = Request(base + path, headers={'Accept-Encoding': 'identity'})
    try:
        with opener.open(req, timeout=45) as response:
            data = response.read(limit + 1)
            if response.status != 200 or not data or len(data) > limit:
                raise PublicationError(f'ModelScope download has an invalid response for {path}')
            return data
    except HTTPError as exc:
        if exc.code == 404:
            return None
        raise PublicationError(f'ModelScope download failed for {path}: HTTP {exc.code}') from None
    except OSError:
        raise PublicationError(f'ModelScope download failed for {path}') from None


def _previous_manifest(latest, read_file, prefix, workdir, item_id=ITEM_ID):
    if latest is None:
        return None
    try:
        release = json.loads(latest)
        version, relative = release['version'], release['manifest_path']
        if (release['item_id'] != item_id or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._-]{0,31}', version)
                or relative != f'releases/{version}/manifest.json'):
            raise ValueError
        manifest = read_file(f'{prefix}/{relative}')
        if manifest is None or hashlib.sha256(manifest).hexdigest() != release['manifest_sha256']:
            raise ValueError
        data = json.loads(manifest)
        if data['item_id'] != item_id or data['version'] != version:
            raise ValueError
    except (KeyError, TypeError, ValueError):
        raise PublicationError('The previous ModelScope release pointer or manifest failed validation') from None
    previous_batch = workdir / 'previous'
    previous = previous_batch / relative
    previous.parent.mkdir(parents=True)
    (previous_batch / 'latest.json').write_bytes(latest)
    previous.write_bytes(manifest)
    return previous


def _pilot_snapshot(source_dir, workdir, requested):
    source_dir = source_dir.resolve()
    sources = {path.relative_to(source_dir).as_posix(): path for path in source_dir.rglob('*.md')}
    if sources.keys() != PILOT_SOURCES.keys():
        raise PublicationError('Publishing is limited to the two approved Markdown fixtures')
    snapshot = workdir / 'source'
    snapshot.mkdir()
    stage = 'v1'
    for name, path in sources.items():
        if path.is_symlink() or not path.is_file():
            raise PublicationError('Publishing requires regular pilot fixture files')
        data = path.read_bytes()
        digest = hashlib.sha256(data).hexdigest()
        if digest != PILOT_SOURCES[name]:
            if name != 'reading-room.md' or digest != PILOT_V2_READING_ROOM:
                raise PublicationError(f'Publishing requires the approved bytes for {name}')
            stage = 'v2'
        (snapshot / name).write_bytes(data)
    if requested == 'v2' and stage == 'v1':
        reading_room = snapshot / 'reading-room.md'
        updated = reading_room.read_bytes().replace(b'14:30', b'15:00', 1)
        if hashlib.sha256(updated).hexdigest() != PILOT_V2_READING_ROOM:
            raise PublicationError('The selected v2 snapshot does not match its approved bytes')
        reading_room.write_bytes(updated)
        stage = 'v2'
    if requested and requested != stage:
        raise PublicationError('The source does not match the selected pilot snapshot')
    return snapshot, stage


def _verify_pilot_release(batch, read_object):
    _, _, raw_manifest, _ = _load_package(batch)
    manifest = json.loads(raw_manifest)
    documents = manifest['documents']
    transport = {'object_path', 'object_sha256', 'size_bytes'}
    identity = [{key: value for key, value in doc.items() if key not in transport} for doc in documents]
    digest = hashlib.sha256((json.dumps(identity, ensure_ascii=False, sort_keys=True) + '\n').encode()).hexdigest()
    stage = next((name for name, approved in PILOT_DOCUMENTS_SHA256.items() if digest == approved), None)
    if manifest['item_id'] != ITEM_ID or stage is None:
        raise PublicationError('The manifest does not match an approved pilot release')

    for doc in documents:
        path, size = doc['object_path'], doc['size_bytes']
        try:
            data = _read_local(batch / path, size)
        except FileNotFoundError:
            data = read_object(path)
        if data is None or len(data) != size or hashlib.sha256(data).hexdigest() != doc['object_sha256']:
            raise PublicationError(f'Pilot object bytes do not match the manifest: {path}')
        decoded_size = doc['decoded_size_bytes']
        decoder = zlib.decompressobj(16 + zlib.MAX_WBITS)
        try:
            raw = decoder.decompress(data, decoded_size + 1)
        except zlib.error:
            raise PublicationError(f'Pilot object cannot be decoded: {path}') from None
        # Core reads exactly one gzip member and rejects even zero padding.
        if not decoder.eof or decoder.unused_data:
            raise PublicationError(f'Pilot object must contain one complete gzip member: {path}')
        if len(raw) != decoded_size or hashlib.sha256(raw).hexdigest() != doc['chunk_fingerprint']:
            raise PublicationError(f'Pilot object does not contain the approved slices: {path}')
    return stage, {doc['id']: doc for doc in documents}


def _exchange_git_token(opener, api_token):
    if not re.fullmatch(r'[A-Za-z0-9._-]+', api_token):
        raise PublicationError('MODELSCOPE_API_TOKEN has an invalid format')
    # The ModelScope legacy login response carries a separate Git access token.
    req = Request(f'{ORIGIN}/api/v1/login', method='POST', data=json.dumps({'AccessToken': api_token}).encode(),
                  headers={'Content-Type': 'application/json', 'Cookie': f'm_session_id={api_token}'})
    try:
        with opener.open(req, timeout=30) as response:
            body = response.read((64 << 10) + 1)
        if len(body) > 64 << 10:
            raise ValueError
        result = json.loads(body)
        token = result['Data']['AccessToken']
        if result.get('Success') is False or not re.fullmatch(r'[!-~]+', token):
            raise ValueError
        return token
    except (OSError, ValueError, KeyError, TypeError):
        raise PublicationError('ModelScope did not issue a Git token for the API credential') from None


_GIT_REPOSITORY_ENV = (
    'GIT_DIR', 'GIT_COMMON_DIR', 'GIT_WORK_TREE', 'GIT_INDEX_FILE', 'GIT_OBJECT_DIRECTORY',
    'GIT_ALTERNATE_OBJECT_DIRECTORIES', 'GIT_NAMESPACE', 'GIT_CONFIG', 'GIT_TEMPLATE_DIR',
)


def _read_git_credential(workdir, git_remote, env):
    reject_prompt = workdir / 'modelscope-reject-prompt.sh'
    reject_prompt.write_text('#!/bin/sh\nexit 1\n', encoding='utf-8')
    reject_prompt.chmod(0o700)
    lookup_env = {**os.environ, **env, 'GIT_ASKPASS': str(reject_prompt), 'GCM_INTERACTIVE': 'never',
                  'GIT_CONFIG_COUNT': '0'}
    try:
        result = subprocess.run(
            ['git', '-C', str(workdir), 'credential', 'fill'], input=f'url={git_remote}\n\n'.encode(),
            capture_output=True, timeout=30, env={key: val for key, val in lookup_env.items() if val is not None},
        )
        if not result.returncode:
            fields = dict(line.split('=', 1) for line in result.stdout.decode().splitlines() if '=' in line)
            username, password = fields.get('username', ''), fields.get('password', '')
            if re.fullmatch(r'[A-Za-z0-9._-]+', username) and re.fullmatch(r'[!-~]+', password):
                return username, password
    except (OSError, subprocess.TimeoutExpired, UnicodeError):
        pass
    raise PublicationError('Set MODELSCOPE_API_TOKEN or configure a write credential for the ModelScope Git repository')


@contextmanager
def _temporary_environment(env):
    previous = {key: os.environ.get(key) for key in env}
    for key, value in env.items():
        if value is None:
            os.environ.pop(key, None)
        else:
            os.environ[key] = value
    try:
        yield
    finally:
        for key, value in previous.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value


@contextmanager
def _git_environment(opener, workdir, git_remote):
    token = os.environ.get('MODELSCOPE_API_TOKEN', '').strip()
    env = {
        **dict.fromkeys(_GIT_REPOSITORY_ENV),
        'GIT_ALLOW_PROTOCOL': 'https', 'GIT_TERMINAL_PROMPT': '0', 'GIT_CONFIG_PARAMETERS': '',
        'GIT_TRACE': '0', 'GIT_TRACE_CURL': '0', 'GIT_CURL_VERBOSE': '0', 'GIT_TRACE_PACKET': '0',
        'GIT_TRACE2': '0', 'GIT_TRACE2_EVENT': '0', 'GIT_TRACE2_PERF': '0',
        'GIT_TRACE2_ENV_VARS': '', 'GIT_TRACE2_CONFIG_PARAMS': '',
    }
    if token:
        username, password = 'oauth2', _exchange_git_token(opener, token)
    else:
        username, password = _read_git_credential(workdir, git_remote, env)

    # Keep the user's Git rules during credential lookup only. Any rules
    # inherited by clone/push could redirect the approved publication target.
    askpass = workdir / 'modelscope-askpass.sh'
    user_remote = git_remote.replace('https://', f'https://{username}@', 1)
    askpass.write_text(
        '#!/bin/sh\ncase "$1" in\n'
        f'  "Username for \'{git_remote}\'"*) printf \'%s\\n\' "$LAZYMIND_MODELSCOPE_GIT_USER" ;;\n'
        f'  "Password for \'{user_remote}\'"*) printf \'%s\\n\' "$LAZYMIND_MODELSCOPE_GIT_TOKEN" ;;\n'
        '  *) exit 1 ;;\nesac\n',
        encoding='utf-8',
    )
    askpass.chmod(0o700)
    env.update(GIT_ASKPASS=str(askpass), GIT_CONFIG_NOSYSTEM='1', GIT_CONFIG_GLOBAL=os.devnull,
               LAZYMIND_MODELSCOPE_GIT_USER=username, LAZYMIND_MODELSCOPE_GIT_TOKEN=password,
               MODELSCOPE_API_TOKEN='', LC_ALL='C')
    settings = [('http.followRedirects', 'false'), ('credential.useHttpPath', 'true'), ('credential.helper', '')]
    env['GIT_CONFIG_COUNT'] = str(len(settings))
    for index, (key, value) in enumerate(settings):
        env[f'GIT_CONFIG_KEY_{index}'] = key
        env[f'GIT_CONFIG_VALUE_{index}'] = value
    with _temporary_environment(env):
        yield


def _check_git_pointer(checkout, expected_latest, prefix=PILOT_PREFIX):
    if _read_git_file(checkout, 'HEAD', f'{prefix}/latest.json') != expected_latest:
        raise PublicationConflict('The current Git release does not match the verified public pointer')


_UNCHECKED_POINTER = object()


def _upload_immutable(git_remote, path, data, *, expected_latest=_UNCHECKED_POINTER, prefix=PILOT_PREFIX):
    pattern = r'(?:objects/[a-f0-9]{64}\.jsonl\.gz|releases/[A-Za-z0-9][A-Za-z0-9._-]{0,31}/manifest\.json)'
    if not re.fullmatch(f'{re.escape(prefix)}/{pattern}', path):
        raise PublicationError('Refusing an upload outside the configured immutable release paths')
    with TemporaryDirectory(prefix='market-prebuilt-object-') as checkout:
        _clone_branch(git_remote, BRANCH, checkout)
        if expected_latest is not _UNCHECKED_POINTER:
            _check_git_pointer(checkout, expected_latest, prefix)
        current = _read_git_file(checkout, 'HEAD', path)
        if current == data:
            return
        if current is not None:
            raise PublicationConflict(f'An immutable ModelScope file already differs at {path}')
        _commit_file(checkout, path, data, f'Publish immutable LazyMind knowledge market file {path}')
        if _push_branch(checkout, BRANCH):
            return
        _fetch_branch(checkout, BRANCH)
        current = _read_git_file(checkout, 'FETCH_HEAD', path)
        if current != data:
            if current is not None:
                raise PublicationConflict(f'Another publisher wrote different immutable bytes at {path}')
            raise PublicationError(f'The immutable Git push was not confirmed for {path}; retry the batch')


def _upstream_source(args):
    fields = (args.upstream_provider, args.upstream_repo, args.upstream_ref, args.upstream_dir, args.item_id)
    if not any(value is not None for value in fields):
        if args.source_dir is None or not args.source_revision or args.repo != PILOT_REPO:
            raise PublicationError('The local pilot requires its source directory, source revision and approved repository')
        if args.publish and args.prefix != PILOT_PREFIX:
            raise PublicationError(f'Pilot publishing is limited to the approved {PILOT_PREFIX} prefix')
        return None
    if not all(fields) or args.source_dir is not None or args.source_revision is not None or args.pilot_snapshot is not None:
        raise PublicationError('Register the upstream provider, repository, ref, directory and item ID together')
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._-]{0,63}', args.item_id) or args.item_id == ITEM_ID:
        raise PublicationError('The upstream release requires a valid non-pilot market item ID')
    if args.repo.casefold() == PILOT_REPO.casefold():
        raise PublicationError('An upstream release requires a separate ModelScope destination repository')
    upstream = UpstreamSource(*fields[:4])
    if upstream.provider == 'modelscope' and upstream.repo.casefold() == args.repo.casefold():
        raise PublicationError('The ModelScope upstream and destination must be different repositories')
    return upstream


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source-dir', type=Path, help='Local source directory for the two-document pilot')
    parser.add_argument('--source-revision', help='Local pilot revision')
    parser.add_argument('--pilot-snapshot', choices=PILOT_DOCUMENTS_SHA256, help='Select the approved v1 or v2 sample')
    parser.add_argument('--upstream-provider', choices=['huggingface', 'modelscope'])
    parser.add_argument('--upstream-repo', help='Public upstream dataset ID (owner/repository)')
    parser.add_argument('--upstream-ref', help='Qualified Git ref, for example refs/heads/main')
    parser.add_argument('--upstream-dir', help='Directory of tracked Markdown documents, or . for the repository root')
    parser.add_argument('--item-id', help='Knowledge market item ID for the registered upstream')
    parser.add_argument('--repo', required=True, help='Destination ModelScope dataset ID (owner/repository)')
    parser.add_argument('--prefix', required=True)
    parser.add_argument('--output-dir', type=Path, help='Retain the batch at a new local path')
    parser.add_argument('--publish', action='store_true', help='Publish to the configured ModelScope dataset; default is dry-run')
    args = parser.parse_args()
    try:
        if not valid_dataset_repo(args.repo):
            raise PublicationError('The ModelScope destination must be an owner/repository ID')
        if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._-]*(?:/[A-Za-z0-9][A-Za-z0-9._-]*)*', args.prefix):
            raise PublicationError('The ModelScope prefix must be a relative repository path')
        upstream = _upstream_source(args)
        item_id = args.item_id if upstream else ITEM_ID
        output_parent = None
        if upstream and args.output_dir:
            if args.output_dir.exists() or args.output_dir.is_symlink():
                raise PublicationError('The output directory must be a new local path')
            output_parent = args.output_dir.parent
        dataset = f'{ORIGIN}/datasets/{args.repo}'
        base = f'{dataset}/resolve/{BRANCH}/'
        pointer_path = f'{args.prefix}/latest.json'
        opener = build_opener(_SameOrigin())
        read_file = partial(_read_modelscope, opener, base)

        def read_object(path):
            return read_file(f'{args.prefix}/{path}')

        with TemporaryDirectory(prefix='market-prebuilt-', dir=output_parent) as temporary:
            workdir = Path(temporary)
            source, revision, stage = args.source_dir, args.source_revision, None
            if not upstream and (args.publish or args.pilot_snapshot):
                source, stage = _pilot_snapshot(source, workdir, args.pilot_snapshot)
            # Read the publication baseline before selecting the upstream commit,
            # so a stale publisher cannot derive its candidate from a newer pointer.
            latest = read_file(pointer_path)
            previous = _previous_manifest(latest, read_file, args.prefix, workdir, item_id)
            prior_manifest = None
            if upstream and previous:
                _, _, raw_manifest, _ = _load_package(workdir / 'previous')
                prior_manifest = json.loads(raw_manifest)
                if prior_manifest.get('upstream') != upstream.provenance:
                    raise PublicationConflict('The target prefix belongs to a different upstream source')
            prior_documents = None
            if stage:
                prior_stage = None
                if previous:
                    prior_stage, prior_documents = _verify_pilot_release(workdir / 'previous', read_object)
                if stage == prior_stage:
                    print('Source documents are unchanged')
                    return 0
                if (prior_stage, stage) not in ((None, 'v1'), ('v1', 'v2')):
                    raise PublicationError('The pilot must start at v1 and advance from a verified v1 to v2')
            batch = workdir / 'batch' if upstream else args.output_dir or workdir / 'batch'
            with _temporary_environment({
                'LAZYLLM_HOME': str(workdir / 'lazyllm'), 'LAZYMIND_HOME': str(workdir / 'lazymind'),
                'LAZYLLM_LOG_DIR': str(workdir / 'lazyllm' / 'logs'), 'LAZYLLM_TRACE_ENABLED': '0',
            }):
                from lazymind.parsing.service.prebuilt_package import _builder_fingerprint, build_package
                if upstream:
                    checkout = workdir / 'upstream'
                    revision = upstream.fetch(checkout)
                    if (prior_manifest and prior_manifest['source_revision'] == revision
                            and prior_manifest.get('builder_fingerprint') == _builder_fingerprint()):
                        print(f'Upstream commit {revision} and the slicing rules are unchanged')
                        return 0
                    source = workdir / 'source'
                    upstream.materialize(checkout, revision, source)
                changed = build_package(source, batch, item_id=item_id, source_revision=revision,
                                        previous_manifest=previous)
            if not changed:
                if stage:
                    raise PublicationError('The selected pilot snapshot did not produce a new release')
                print('Source documents are unchanged')
                return 0
            if upstream:
                bind_upstream_manifest(batch, upstream.provenance)
                if args.output_dir:
                    batch.rename(args.output_dir)
                    batch = args.output_dir
            if stage:
                built_stage, documents = _verify_pilot_release(batch, read_object)
                if built_stage != stage or (
                    prior_documents and documents['tool-shelf'] != prior_documents['tool-shelf']
                ):
                    raise PublicationError('The built package did not preserve the approved pilot change')
            candidate = (batch / 'latest.json').read_bytes()
            version = json.loads(candidate)['version']
            if not args.publish:
                object_count = sum(1 for _ in (batch / 'objects').glob('*.jsonl.gz'))
                print(f'Dry run: prepared {object_count} new object(s) for {version}')
                return 0
            git_remote = f'{dataset}.git'
            with _git_environment(opener, workdir, git_remote):
                checkout = workdir / 'git-baseline'
                checkout.mkdir()
                _clone_branch(git_remote, BRANCH, checkout)
                _check_git_pointer(checkout, latest, args.prefix)
                publish_batch(batch, git_remote=git_remote, branch=BRANCH, prefix=args.prefix,
                              expected_latest=latest, read_file=read_file,
                              upload_file=partial(_upload_immutable, git_remote, expected_latest=latest, prefix=args.prefix))
            if read_file(pointer_path) != candidate:
                raise PublicationError('Git accepted the pointer but anonymous ModelScope readback has not confirmed it')
            print(f'Published and verified {base}{pointer_path}')
    except (PublicationError, OSError, ValueError, KeyError, TypeError) as exc:
        print(f'Error: {exc}', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
