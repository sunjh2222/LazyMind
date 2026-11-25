"""Read Markdown from one immutable commit of a public dataset."""

import hashlib
import os
import re
import subprocess
from dataclasses import dataclass
from pathlib import PurePosixPath
from urllib.parse import quote, urlsplit
from urllib.request import HTTPRedirectHandler, Request, build_opener

from .prebuilt_publish import PublicationError


_DATASET_REPO = re.compile(r'[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*')
_COMMIT = re.compile(r'[a-f0-9]{40}')
_ORIGINS = {'huggingface': 'huggingface.co', 'modelscope': 'modelscope.cn'}
_OBJECT_HOSTS = {
    'huggingface': frozenset({
        'cas-bridge.xethub.hf.co', 'cas-bridge.xethub-eu.hf.co', 'cdn-lfs.huggingface.co', 'cdn-lfs.hf.co',
        'cdn-lfs-us-1.hf.co', 'cdn-lfs-eu-1.hf.co', 'us.aws.cdn.hf.co', 'us.gcp.cdn.hf.co',
    }),
    'modelscope': frozenset({'cdn-lfs-cn-1.modelscope.cn'}),
}
_LFS_HEADER = b'version https://git-lfs.github.com/spec/'
_LFS_POINTER = re.compile(re.escape(_LFS_HEADER) + rb'v1\noid sha256:([a-f0-9]{64})\nsize ([1-9][0-9]{0,8})\n?')
_DOCUMENT_LIMIT = 32 << 20
_SNAPSHOT_LIMIT = 512 << 20


def _safe_parts(path):
    parts = path.split('/')
    if (not all(part and part not in {'.', '..'} and part.casefold() != '.git' for part in parts)
            or any(ord(char) < 32 or ord(char) == 127 or char in '\\:' for char in path)):
        raise PublicationError('The upstream directory or document path is invalid')
    return parts


def valid_dataset_repo(value):
    return _DATASET_REPO.fullmatch(value) is not None and not value.lower().endswith('.git')


@dataclass(frozen=True)
class UpstreamSource:
    provider: str
    repo: str
    ref: str
    directory: str

    def __post_init__(self):
        if self.provider not in _ORIGINS or not valid_dataset_repo(self.repo):
            raise PublicationError('An upstream official dataset provider and owner/repo ID are required')
        if not self.ref.startswith(('refs/heads/', 'refs/tags/')):
            raise PublicationError('The upstream ref must be a qualified branch or tag')
        if self.directory != '.':
            _safe_parts(self.directory)

    @property
    def dataset_url(self):
        return f'https://{_ORIGINS[self.provider]}/datasets/{self.repo}'

    @property
    def git_url(self):
        return self.dataset_url + ('.git' if self.provider == 'modelscope' else '')

    @property
    def provenance(self):
        return {'provider': self.provider, 'repo': self.repo, 'ref': self.ref, 'directory': self.directory}

    def fetch(self, checkout):
        """Fetch only the registered ref and pin the actual commit returned by Git."""
        _source_git(checkout.parent, 'check-ref-format', self.ref)
        checkout.mkdir()
        _source_git(checkout, 'init', '--bare', '--quiet')
        _source_git(checkout, 'fetch', '--quiet', '--depth=1', '--no-tags', '--', self.git_url, self.ref)
        commit = _source_git(checkout, 'rev-parse', '--verify', 'FETCH_HEAD^{commit}').decode('ascii').strip()
        if not _COMMIT.fullmatch(commit):
            raise PublicationError('The registered upstream ref did not resolve to a Git commit')
        return commit

    def materialize(self, checkout, commit, destination):
        """Extract literal tracked Markdown blobs without executing a checkout filter."""
        if not _COMMIT.fullmatch(commit):
            raise PublicationError('Invalid upstream commit')
        tree = f'{commit}^{{tree}}' if self.directory == '.' else f'{commit}:{self.directory}'
        if _source_git(checkout, 'cat-file', '-t', tree).strip() != b'tree':
            raise PublicationError('The registered upstream directory is not a Git tree')
        entries = _source_git(checkout, 'ls-tree', '-r', '-l', '-z', tree).split(b'\0')
        documents = []
        for entry in filter(None, entries):
            info, name = entry.split(b'\t', 1)
            mode, kind, blob, size = info.split()
            if mode in (b'120000', b'160000'):
                raise PublicationError('The upstream directory contains a symbolic link or submodule')
            if not name.endswith(b'.md'):
                continue
            relative = PurePosixPath(*_safe_parts(name.decode('utf-8')))
            if kind != b'blob' or mode not in (b'100644', b'100755') or int(size) > _DOCUMENT_LIMIT:
                raise PublicationError(f'Unsupported or oversized upstream Markdown file: {str(relative)!r}')
            documents.append((relative, blob.decode('ascii')))
        if not 1 <= len(documents) <= 1000:
            raise PublicationError('Expected a complete upstream snapshot containing 1 to 1000 Markdown documents')

        destination.mkdir()
        remaining = _SNAPSHOT_LIMIT
        for relative, blob in documents:
            data = _source_git(checkout, 'cat-file', 'blob', blob)
            if data.startswith(_LFS_HEADER):
                pointer = _LFS_POINTER.fullmatch(data.replace(b'\r\n', b'\n'))
                if pointer is None:
                    raise PublicationError(f'Unsupported upstream LFS pointer: {str(relative)!r}')
                digest, size = pointer.groups()
                if int(size) > min(_DOCUMENT_LIMIT, remaining):
                    raise PublicationError('Upstream Markdown data exceeds the package size limit')
                path = PurePosixPath(self.directory) / relative
                data = _read_source_object(self, commit, path.as_posix(), digest.decode('ascii'), int(size))
            remaining -= len(data)
            if remaining < 0:
                raise PublicationError('Upstream Markdown data exceeds the package size limit')
            target = destination.joinpath(*relative.parts)
            target.parent.mkdir(parents=True, exist_ok=True)
            with target.open('xb') as stream:
                stream.write(data)


def _source_git(directory, *args):
    # Upstream Git is anonymous. In particular, do not inherit the publication
    # token, the checkout credential helper, URL rewrites, or Git hooks.
    env = {
        'PATH': os.environ.get('PATH', os.defpath), 'GIT_CONFIG_GLOBAL': os.devnull, 'GIT_CONFIG_NOSYSTEM': '1',
        'GIT_ALLOW_PROTOCOL': 'https', 'GIT_TERMINAL_PROMPT': '0', 'GIT_LFS_SKIP_SMUDGE': '1', 'LC_ALL': 'C',
    }
    if os.name == 'nt' and 'SystemRoot' in os.environ:
        env['SystemRoot'] = os.environ['SystemRoot']
    try:
        result = subprocess.run(
            ['git', '-C', str(directory), '-c', 'http.followRedirects=false', *args],
            capture_output=True, env=env, timeout=120,
        )
    except (OSError, subprocess.TimeoutExpired):
        raise PublicationError(f'Could not run upstream Git {args[0]}') from None
    if result.returncode:
        raise PublicationError(f'Upstream Git {args[0]} failed for the registered public dataset')
    return result.stdout


class _SourceObjectRedirect(HTTPRedirectHandler):
    def __init__(self, origin, object_hosts):
        self.origin, self.hosts = origin, object_hosts | {origin}

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        target = urlsplit(newurl)
        if (urlsplit(req.full_url).netloc != self.origin or target.scheme != 'https'
                or target.netloc not in self.hosts or target.fragment):
            raise PublicationError('Upstream object redirected outside its official HTTPS origins')
        return super().redirect_request(req, fp, code, msg, headers, newurl)


def _read_source_object(source, commit, path, digest, size):
    request = Request(f'{source.dataset_url}/resolve/{commit}/{quote(path, safe="/")}',
                      headers={'Accept-Encoding': 'identity'})
    opener = build_opener(_SourceObjectRedirect(_ORIGINS[source.provider], _OBJECT_HOSTS[source.provider]))
    try:
        with opener.open(request, timeout=45) as response:
            data = response.read(size + 1)
            if response.status != 200 or len(data) != size or hashlib.sha256(data).hexdigest() != digest:
                raise PublicationError(f'Upstream object did not match its committed size and SHA-256: {path!r}')
            return data
    except OSError:
        raise PublicationError(f'Could not download the commit-pinned upstream object: {path!r}') from None
