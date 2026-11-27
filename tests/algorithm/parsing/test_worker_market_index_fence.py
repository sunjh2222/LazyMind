import importlib
import json
from pathlib import Path
import shutil
import sqlite3

import pytest

from lazyllm.tools.rag.parsing_service.impl import _Processor
from lazyllm.tools.rag.store import HybridStore, LAZY_ROOT_NAME, MapStore, SQLiteStore
from lazyllm.tools.rag.store.document_store import _DocumentStore

from lazymind.config import config
from lazymind.processor.service.prebuilt_import import import_prebuilt_documents


@pytest.mark.parametrize('mutation', [
    'none', 'model', 'signature', 'binding', 'version', 'task', 'missing_line', 'missing_tail', 'missing_sidecar',
])
def test_market_reembed_fences_the_real_worker_before_vector_io(monkeypatch, tmp_path, mutation):
    core = tmp_path / 'core.db'
    signature = 'v1:2:' + 'a' * 64 + ':' + 'b' * 64
    doc_ids = ('room-text', 'tool-text')
    with sqlite3.connect(core) as db:
        db.executescript('''
            CREATE TABLE datasets (id TEXT, kb_id TEXT, processing_revision INTEGER,
                                   processing_level TEXT, deleted_at TEXT);
            CREATE TABLE documents (id TEXT, dataset_id TEXT, lazyllm_doc_id TEXT,
                                    resource_version INTEGER, deleted_at TEXT);
            CREATE TABLE document_processing_states (dataset_id TEXT, document_id TEXT,
                parse_status TEXT, chunk_status TEXT, index_artifact_ref TEXT);
            CREATE TABLE embedding_index_states (scope TEXT, phase TEXT, generation INTEGER,
                active_source TEXT, active_model_id TEXT, signature TEXT);
            INSERT INTO datasets VALUES ('dataset', 'kb', 3, 'chunked', NULL);
            INSERT INTO documents VALUES ('room', 'dataset', 'room-text', 2, NULL);
            INSERT INTO documents VALUES ('tools', 'dataset', 'tool-text', 2, NULL);
            INSERT INTO document_processing_states
                VALUES ('dataset', 'room', 'succeeded', 'succeeded', 'core-task:room-index');
            INSERT INTO document_processing_states
                VALUES ('dataset', 'tools', 'succeeded', 'succeeded', 'core-task:tool-index');
        ''')
        db.execute("INSERT INTO embedding_index_states VALUES ('global', 'ready', 7, 'own', 'model-a', ?)",
                   (signature,))
        changes = {
            'model': "UPDATE embedding_index_states SET active_model_id = 'model-b'",
            'signature': "UPDATE embedding_index_states SET signature = 'other'",
            'binding': "UPDATE documents SET lazyllm_doc_id = 'another-doc' WHERE id = 'room'",
            'version': "UPDATE documents SET resource_version = 3 WHERE id = 'room'",
            'task': "UPDATE document_processing_states SET index_artifact_ref = 'core-task:new' "
            "WHERE document_id = 'room'",
        }
        if mutation in changes:
            db.execute(changes[mutation])

    monkeypatch.setitem(config._impl, 'database_url', 'sqlite:///:memory:')
    monkeypatch.setitem(config._impl, 'core_database_url', f'sqlite:///{core}')
    monkeypatch.setitem(config._impl, 'shared_upload_dir', str(tmp_path))
    worker_module = importlib.import_module('lazymind.processor.service.worker')
    worker = object.__new__(worker_module.DocumentProcessorWorker._Impl)
    worker._reader = None
    worker._worker_id = 'test-market-index-worker'
    base = type(worker).__mro__[1]
    base_module = importlib.import_module(base.__module__)
    monkeypatch.setattr(base_module, 'WORKER_ERROR_RETRY_INTERVAL', 0)

    vector_connections, embeddings, receipts = [], [], []
    vectors = MapStore()
    connect = vectors.connect

    def connect_vectors(*args, **kwargs):
        vector_connections.append(True)
        return connect(*args, **kwargs)

    def embed(value):
        embeddings.append(value)
        return [1.0, 2.0]

    monkeypatch.setattr(vectors, 'connect', connect_vectors)
    segments = SQLiteStore(str(tmp_path / 'segments.db'))
    store = _DocumentStore(HybridStore(segment_store=segments, vector_store=vectors),
                           group_embed_keys={'block': {'embed_main'}, 'line': {'embed_main'}},
                           embed={'embed_main': embed})
    groups = {LAZY_ROOT_NAME: {'parent': None}, 'block': {'parent': LAZY_ROOT_NAME}, 'line': {'parent': 'block'}}
    group_ids = {group: group for group in groups}
    store.activate_group(list(groups))
    processor = _Processor(store)
    processor._lazymind_generation_initialized = True

    def unexpected_reader(_worker):
        pytest.fail('market text indexing must not load a Reader')

    monkeypatch.setattr(base, '_get_processor', lambda _self: processor)
    monkeypatch.setattr(base, '_load_reader_from_db', unexpected_reader)
    monkeypatch.setattr(worker, '_resolve_callback_url', lambda _payload: None)
    monkeypatch.setattr(worker, '_load_all_ng_configs', lambda _groups: (groups, group_ids))
    monkeypatch.setattr(worker, '_write_ng_status_batch', lambda *_args: None)
    monkeypatch.setattr(worker, '_enqueue_finished_task',
                        lambda **receipt: receipts.append(receipt['task_status'].value))

    fixture = Path(__file__).resolve().parents[3] / 'backend/core/knowledge_market/testdata/local_prebuilt_demo'
    manifest = json.loads((fixture / 'releases/v1/manifest.json').read_text(encoding='utf-8'))
    files = []
    for document, doc_id in zip(manifest['documents'], doc_ids, strict=True):
        original = tmp_path / document['source_path']
        original.write_text('This original is never used for embedding.', encoding='utf-8')
        artifact = tmp_path / f'{doc_id}.jsonl.gz'
        shutil.copyfile(fixture / document['object_path'], artifact)
        files.append({'doc_id': doc_id, 'file_path': str(original), 'metadata': {'prebuilt_path': str(artifact)}})
    import_prebuilt_documents(store, files, 'kb')
    for info in files:
        Path(info['file_path']).unlink()
    line_collection = store._gen_collection_name('line')
    line_scope = {'docid': [doc_ids[0]], 'kb_id': 'kb'}
    if mutation == 'missing_sidecar':
        Path(files[0]['metadata']['prebuilt_path']).unlink()
    elif mutation == 'missing_line':
        assert segments.delete(line_collection, line_scope)
    elif mutation == 'missing_tail':
        lines = segments.get(line_collection, line_scope)
        assert len(lines) > 1
        tail = max(lines, key=lambda row: row['number'])
        assert tail['number'] == len(lines)
        assert segments.delete(line_collection, {**line_scope, 'uid': [tail['uid']]})
        remaining = segments.get(line_collection, line_scope)
        assert sorted(row['number'] for row in remaining) == list(range(1, len(lines)))

    confirmation = {'source': 'own', 'model_id': 'model-a', 'signature': signature, 'generation': 7,
                    'dataset_id': 'dataset', 'kb_id': 'kb', 'processing_revision': 3,
                    'documents': [{'core_id': 'room', 'doc_id': doc_ids[0], 'resource_version': 2,
                                   'core_task_id': 'room-index'},
                                  {'core_id': 'tools', 'doc_id': doc_ids[1], 'resource_version': 2,
                                   'core_task_id': 'tool-index'}]}
    payload = {'kb_id': 'kb', 'processing_level': 'indexed', 'strategy': 'reembed', 'ng_names': ['block', 'line'],
               'llm_config': {'_embedding_generation': 7, '_market_index_confirmation': confirmation,
                              'embed_main': {'source': 'openai', 'model': 'fixture-embedding'}},
               # The DocServer submits each document separately with the shared batch confirmation.
               'file_infos': files[:1]}
    try:
        worker._run_task('fixture-market-reembed', 'DOC_REPARSE', payload, False)
        if mutation == 'none':
            assert receipts == ['WORKING', 'SUCCESS']
            assert vector_connections and embeddings
            assert all('This original is never used' not in value for value in embeddings)
            assert any('蓝羽书签' in value for value in embeddings)
            assert not any('橙格工具盒' in value for value in embeddings)
        else:
            assert receipts == ['WORKING', 'FAILED']
            assert not vector_connections and not embeddings
    finally:
        processor._thread_pool.shutdown(wait=False)
