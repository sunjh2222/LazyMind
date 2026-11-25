import gzip
import hashlib
import json
from pathlib import Path
import shutil

import pytest

from lazyllm import globals as lazyllm_globals
from lazymind.parsing.service import prebuilt_package


FIXTURES = Path(__file__).resolve().parents[1] / 'fixtures' / 'market_prebuilt'
FROZEN = (Path(__file__).resolve().parents[3]
          / 'backend/core/knowledge_market/testdata/local_prebuilt_demo')


@pytest.fixture
def source(tmp_path):
    return shutil.copytree(FIXTURES, tmp_path / 'source')


def build(source, output, revision='synthetic-v1', previous=None):
    return prebuilt_package.build_package(
        source, output, item_id='local-prebuilt-demo', source_revision=revision,
        previous_manifest=previous,
    )


def read_release(root):
    pointer = json.loads((root / 'latest.json').read_bytes())
    assert pointer['manifest_path'] == f'releases/{pointer["version"]}/manifest.json'
    path = root / pointer['manifest_path']
    data = path.read_bytes()
    manifest = json.loads(data)
    documents = manifest['documents']
    assert pointer['manifest_sha256'] == hashlib.sha256(data).hexdigest()
    assert pointer['item_id'] == manifest['item_id'] == 'local-prebuilt-demo'
    assert pointer['version'] == manifest['version']
    assert pointer['document_count'] == len(documents)
    assert pointer['size_bytes'] == sum(doc['size_bytes'] for doc in documents)
    assert pointer['chunk_count'] == sum(doc['chunk_count'] for doc in documents)
    return path, manifest


def test_first_release_matches_frozen_chunks_and_is_reproducible(source, tmp_path):
    first, second = tmp_path / 'first', tmp_path / 'second'
    relocated = tmp_path / 'relocated'
    relocated.mkdir()
    for path in reversed(sorted(source.glob('*.md'))):
        shutil.copyfile(path, relocated / path.name)
    assert build(source, first)
    assert build(relocated, second)

    reference = json.loads((FROZEN / 'releases/v1/manifest.json').read_bytes())
    _, manifest = read_release(first)
    assert manifest['schema_version'] == 1
    assert manifest['chunk_profile'] == 'general_algo'
    assert isinstance(manifest['builder_fingerprint'], str) and manifest['builder_fingerprint']
    assert manifest['source_revision'] == 'synthetic-v1'
    assert [doc['id'] for doc in manifest['documents']] == ['reading-room', 'tool-shelf']
    assert sum(doc['chunk_count'] for doc in manifest['documents']) == 21
    for built, frozen in zip(manifest['documents'], reference['documents']):
        assert built['source_sha256'] == frozen['source_sha256']
        assert built['chunk_count'] == frozen['chunk_count']
        compressed = (first / built['object_path']).read_bytes()
        assert len(compressed) == built['size_bytes']
        assert hashlib.sha256(compressed).hexdigest() == built['object_sha256']
        assert built['object_path'] == f'objects/{built["object_sha256"]}.jsonl.gz'
        raw = gzip.decompress(compressed)
        assert len(raw) == built['decoded_size_bytes']
        # Compare decoded bytes across zlib versions. Both builds below must
        # also reproduce the compressed bytes on the same runner.
        assert raw == gzip.decompress((FROZEN / frozen['object_path']).read_bytes())
        assert hashlib.sha256(raw).hexdigest() == built['chunk_fingerprint'] == frozen['chunk_fingerprint']
    first_files = {path.relative_to(first): path.read_bytes() for path in first.rglob('*') if path.is_file()}
    second_files = {path.relative_to(second): path.read_bytes() for path in second.rglob('*') if path.is_file()}
    assert first_files == second_files
    assert len(first_files) == 4


def test_incremental_release_only_builds_changed_objects(source, tmp_path, monkeypatch):
    first = tmp_path / 'first'
    assert build(source, first)
    previous, original = read_release(first)
    called = []
    real_build_document = prebuilt_package._build_document

    def track(path, source_path):
        called.append(path.name)
        return real_build_document(path, source_path)

    monkeypatch.setattr(prebuilt_package, '_build_document', track)
    unchanged = tmp_path / 'unchanged'
    assert not build(source, unchanged, 'new-revision-same-documents', previous)
    assert not unchanged.exists() and not called

    path = source / 'reading-room.md'
    path.write_text(path.read_text(encoding='utf-8').replace('蓝羽书签', '绿羽书签'), encoding='utf-8')
    changed = tmp_path / 'changed'
    assert build(source, changed, 'synthetic-v2', previous)
    manifest_path, manifest = read_release(changed)
    assert called == ['reading-room.md']
    assert manifest['version'] != original['version']
    edited, reused = manifest['documents']
    assert edited['id'] == 'reading-room'
    assert edited['object_sha256'] != original['documents'][0]['object_sha256']
    assert '绿羽书签' in gzip.decompress((changed / edited['object_path']).read_bytes()).decode()
    assert reused == original['documents'][1]
    assert sorted(p.relative_to(changed).as_posix() for p in changed.glob('objects/*')) == [edited['object_path']]

    path.unlink()
    called.clear()
    removed = tmp_path / 'removed'
    # The previous incremental batch omitted the unchanged object's bytes.
    assert build(source, removed, 'synthetic-v3', manifest_path)
    _, after_removal = read_release(removed)
    assert after_removal['version'] != manifest['version']
    assert after_removal['documents'] == [reused]
    assert not called and not list(removed.glob('objects/*'))


def test_documents_with_same_basename_keep_relative_identity(tmp_path):
    source = tmp_path / 'nested'
    for folder in ('east', 'west'):
        destination = source / folder / 'reading-room.md'
        destination.parent.mkdir(parents=True)
        shutil.copyfile(FIXTURES / 'reading-room.md', destination)
    output = tmp_path / 'published'
    assert build(source, output)
    _, manifest = read_release(output)
    assert [doc['id'] for doc in manifest['documents']] == ['east/reading-room', 'west/reading-room']
    assert [doc['source_path'] for doc in manifest['documents']] == ['east/reading-room.md', 'west/reading-room.md']


def test_empty_source_cannot_replace_the_previous_release(source, tmp_path):
    first = tmp_path / 'first'
    assert build(source, first)
    previous, _ = read_release(first)
    before = (first / 'latest.json').read_bytes()
    for path in source.glob('*.md'):
        path.unlink()
    rejected = tmp_path / 'rejected'
    with pytest.raises(ValueError):
        build(source, rejected, 'empty-snapshot', previous)
    assert not (rejected / 'latest.json').exists()
    assert not list(rejected.glob('releases/*/manifest.json'))
    assert (first / 'latest.json').read_bytes() == before


def test_reader_failure_cannot_leave_a_publishable_release(source, tmp_path, monkeypatch):
    real_build_document = prebuilt_package._build_document
    built = []

    def fail_second(path, source_path):
        if path.name == 'tool-shelf.md':
            raise ValueError('unreadable fixture')
        result = real_build_document(path, source_path)
        built.append(path.name)
        return result

    monkeypatch.setattr(prebuilt_package, '_build_document', fail_second)
    rejected = tmp_path / 'rejected'
    with pytest.raises(ValueError, match='unreadable fixture'):
        build(source, rejected)
    assert built == ['reading-room.md']
    assert not (rejected / 'latest.json').exists()
    assert not list(rejected.glob('releases/*/manifest.json'))


def test_total_decoded_limit_counts_reused_objects(source, tmp_path, monkeypatch):
    first = tmp_path / 'first'
    assert build(source, first)
    previous, original = read_release(first)
    original_decoded = sum(doc['decoded_size_bytes'] for doc in original['documents'])
    edited = source / 'reading-room.md'
    edited.write_text(edited.read_text(encoding='utf-8') + '\n图书登记使用黄色书签。\n', encoding='utf-8')

    monkeypatch.setattr(prebuilt_package, '_MAX_RELEASE_BYTES', original_decoded)
    output = tmp_path / 'over-limit'
    with pytest.raises(ValueError, match='total decoded'):
        build(source, output, 'synthetic-v2', previous)
    assert not output.exists()


def test_ambient_embedding_profile_cannot_change_chunks(tmp_path, monkeypatch):
    source = tmp_path / 'source'
    source.mkdir()
    (source / 'long.md').write_text('# Study Guide\n\n' + '样本文字' * 250 + '。\n', encoding='utf-8')
    first, second = tmp_path / 'first', tmp_path / 'second'
    assert build(source, first)
    previous, original = read_release(first)

    models = {'embed_main': {'embed': {'max_input_tokens': 512}}}
    monkeypatch.setitem(lazyllm_globals['config'], 'dynamic_model_configs', models)
    assert build(source, second)
    assert read_release(second)[1] == original
    assert (second / original['documents'][0]['object_path']).read_bytes() == (
        first / original['documents'][0]['object_path']).read_bytes()
    assert not build(source, tmp_path / 'unchanged', 'new-source-revision', previous)
    assert lazyllm_globals['config']['dynamic_model_configs'] == models
