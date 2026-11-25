"""Build a local release batch from a complete Markdown source snapshot."""

import argparse
import gzip
import hashlib
import inspect
import io
import json
import tempfile
import unicodedata
from contextvars import Context
from pathlib import Path
from uuid import uuid4

from lazyllm.common import new_session
from lazyllm.tools.rag import AdaptiveTransform
from lazyllm.tools.rag.readers import MarkdownReader

from lazymind.parsing.engine.transform import GeneralParser, LineSplitter
from lazymind.parsing.service import build_document


_PROFILE = 'general_algo'
_ROOT = '__lazyllm_root__'
_MAX_RELEASE_BYTES = 512 << 20


def _sha256(data):
    return hashlib.sha256(data).hexdigest()


def _json_bytes(value):
    return (json.dumps(value, ensure_ascii=False, sort_keys=True) + '\n').encode('utf-8')


def _builder_fingerprint():
    components = (__file__, *(inspect.getfile(part) for part in (
        build_document, MarkdownReader, AdaptiveTransform, GeneralParser, LineSplitter,
    )))
    return _sha256(_json_bytes([_sha256(Path(path).read_bytes()) for path in components]))


def _build_document(path, source_path):
    # The parsing service normally reads embedding budgets from its session.
    # The publisher always cuts text with the default profile in a fresh one.
    with new_session(f'market-prebuilt-{uuid4().hex}'):
        reader = MarkdownReader()
        reader.use_cache(False)
        roots = reader(path)
        for node in roots:
            node.docpath = str(path)
        blocks = build_document._build_block_transform().batch_forward(roots, 'block')
        lines = build_document._build_line_transform().batch_forward(blocks, 'line')
    groups = ((_ROOT, roots), ('block', blocks), ('line', lines))
    if not all(nodes for _, nodes in groups):
        raise ValueError(f'Missing text or slices in {source_path}')

    ids = {node.uid: f'{group}-{number}' for group, nodes in groups for number, node in enumerate(nodes, 1)}
    rows = []
    for group, nodes in groups:
        for number, node in enumerate(nodes, 1):
            if node.embedding or not node.text.strip() or '\0' in node.text:
                raise ValueError(f'Invalid text-only node in {source_path}')
            row = _json_bytes({
                'id': ids[node.uid],
                'group': group,
                'parent_id': ids[node.parent.uid] if node.parent else '',
                'text': node.text,
                'number': number,
                'metadata': {'source_path': source_path},
            })
            if len(row) >= 1 << 20:
                raise ValueError(f'Text node exceeds the package record limit in {source_path}')
            rows.append(row)
    raw = b''.join(rows)
    # A zero timestamp and no filename make the gzip object reproducible.
    stream = io.BytesIO()
    with gzip.GzipFile(fileobj=stream, mode='wb', filename='', mtime=0) as archive:
        archive.write(raw)
    compressed = stream.getvalue()
    if len(raw) > 32 << 20 or len(compressed) > 16 << 20:
        raise ValueError(f'Object exceeds the package size limit in {source_path}')
    return raw, compressed, len(blocks) + len(lines)


def _load_previous(path, item_id, fingerprint):
    if path is None:
        return {}, False
    manifest = json.loads(path.read_bytes())
    if (manifest['schema_version'] != 1 or manifest['item_id'] != item_id
            or manifest['chunk_profile'] != _PROFILE):
        raise ValueError('Previous manifest belongs to a different release channel')
    documents = {document['id']: document for document in manifest['documents']}
    if not documents or len(documents) != len(manifest['documents']):
        raise ValueError('Previous manifest has no documents or duplicate identities')
    return documents, manifest.get('builder_fingerprint') == fingerprint


def _metadata(path, source_path):
    source = path.read_bytes()
    text = source.decode('utf-8')
    heading = next((line.strip()[2:].strip() for line in text.splitlines() if line.strip().startswith('# ')), '')
    title = heading or path.stem
    identity = Path(source_path).with_suffix('').as_posix()
    if (not title or len(title.encode('utf-8')) > 240 or len(identity.encode('utf-8')) > 255
            or any(char in title for char in '/\\') or '\\' in source_path
            or any(unicodedata.category(char) == 'Cc' for char in title + source_path)):
        raise ValueError(f'Invalid document name or title in {source_path}')
    return {'id': identity, 'title': title, 'source_path': source_path, 'source_sha256': _sha256(source)}


def build_package(source_dir: Path, output_dir: Path, *, item_id: str, source_revision: str,
                  previous_manifest: Path | None = None) -> bool:
    """Write a new batch with a complete manifest and only newly needed objects."""
    if not item_id.strip() or not source_revision.strip() or not source_dir.is_dir() or output_dir.exists():
        raise ValueError('Require a source directory, item ID, revision and new output directory')
    source_dir = source_dir.resolve()
    paths = sorted(source_dir.rglob('*.md'))
    if not 1 <= len(paths) <= 1000:
        raise ValueError('Expected a complete snapshot containing 1 to 1000 Markdown documents')

    fingerprint = _builder_fingerprint()
    previous, same_builder = _load_previous(previous_manifest, item_id, fingerprint)
    old_objects = {entry['object_sha256'] for entry in previous.values()}
    documents = []
    total_decoded = 0
    with tempfile.TemporaryDirectory(prefix='.prebuilt-', dir=output_dir.parent) as temp:
        staged = Path(temp) / 'release'
        staged.mkdir()
        for path in paths:
            source_path = path.relative_to(source_dir).as_posix()
            if path.is_symlink() or not path.is_file():
                raise ValueError(f'Source document must be a regular file: {source_path}')
            entry = _metadata(path, source_path)
            prior = previous.get(entry['id'])
            if same_builder and prior and all(prior.get(key) == value for key, value in entry.items()):
                entry = prior
            else:
                # Keep the caller's session intact when the temporary session closes.
                raw, compressed, count = Context().run(_build_document, path, source_path)
                digest = _sha256(compressed)
                object_path = f'objects/{digest}.jsonl.gz'
                if digest not in old_objects:
                    target = staged / object_path
                    target.parent.mkdir(exist_ok=True)
                    target.write_bytes(compressed)
                entry.update({
                    'chunk_fingerprint': _sha256(raw),
                    'object_path': object_path,
                    'object_sha256': digest,
                    'size_bytes': len(compressed),
                    'decoded_size_bytes': len(raw),
                    'chunk_count': count,
                })
            total_decoded += entry['decoded_size_bytes']
            if total_decoded > _MAX_RELEASE_BYTES:
                raise ValueError('Snapshot exceeds the total decoded package size limit')
            documents.append(entry)

        if same_builder and {entry['id']: entry for entry in documents} == previous:
            return False

        total_bytes = sum(entry['size_bytes'] for entry in documents)
        total_chunks = sum(entry['chunk_count'] for entry in documents)
        if total_bytes > _MAX_RELEASE_BYTES or total_chunks > 1_000_000:
            raise ValueError('Snapshot exceeds the package size or chunk limit')
        manifest = {
            'schema_version': 1,
            'item_id': item_id,
            'source_revision': source_revision,
            'chunk_profile': _PROFILE,
            'builder_fingerprint': fingerprint,
            'documents': documents,
        }
        version = 'v-' + _sha256(_json_bytes(manifest))[:24]
        manifest_path = f'releases/{version}/manifest.json'
        manifest_bytes = _json_bytes({**manifest, 'version': version})
        if len(manifest_bytes) > 4 << 20:
            raise ValueError('Manifest exceeds the package size limit')
        manifest_target = staged / manifest_path
        manifest_target.parent.mkdir(parents=True)
        manifest_target.write_bytes(manifest_bytes)
        (staged / 'latest.json').write_bytes(_json_bytes({
            'item_id': item_id,
            'version': version,
            'manifest_path': manifest_path,
            'manifest_sha256': _sha256(manifest_bytes),
            'document_count': len(documents),
            'chunk_count': total_chunks,
            'size_bytes': total_bytes,
        }))
        staged.rename(output_dir)
    return True


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source-dir', type=Path, required=True)
    parser.add_argument('--output-dir', type=Path, required=True)
    parser.add_argument('--item-id', required=True)
    parser.add_argument('--source-revision', required=True)
    parser.add_argument('--previous-manifest', type=Path)
    args = parser.parse_args()
    if build_package(args.source_dir, args.output_dir, item_id=args.item_id,
                     source_revision=args.source_revision, previous_manifest=args.previous_manifest):
        print(f'Package batch ready at {args.output_dir}')
    else:
        print('Source documents are unchanged')


if __name__ == '__main__':
    main()
