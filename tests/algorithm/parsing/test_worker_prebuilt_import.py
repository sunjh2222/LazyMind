import gzip
import importlib
import json
from copy import deepcopy
from pathlib import Path
from threading import Event
from types import SimpleNamespace
from urllib.parse import urlsplit

from fastapi import FastAPI
from fastapi.testclient import TestClient
from lazyllm.tools.rag.doc_service.doc_server import DocServer
from lazyllm.tools.rag.parsing_service.impl import _Processor
from lazyllm.tools.rag.parsing_service.server import DocumentProcessor
from lazyllm.tools.rag.store import HybridStore, LAZY_ROOT_NAME, MapStore, SQLiteStore
from lazyllm.tools.rag.store.document_store import _DocumentStore

from lazymind.config import config


PREBUILT_DEMO = (Path(__file__).resolve().parents[3]
                 / 'backend/core/knowledge_market/testdata/local_prebuilt_demo')


def test_doc_server_preserves_prebuilt_reference_in_worker_queue(monkeypatch, tmp_path):
    documents = json.loads((PREBUILT_DEMO / 'releases/v1/manifest.json').read_text(encoding='utf-8'))['documents']
    db_config = {
        'db_type': 'sqlite', 'db_name': str(tmp_path / 'pipeline.db'),
        'user': None, 'password': None, 'host': None, 'port': None,
    }
    monkeypatch.setitem(config._impl, 'database_url', f"sqlite:///{db_config['db_name']}")
    processor = DocumentProcessor._Impl(db_config=db_config, num_workers=0)
    stop_callbacks = Event()
    monkeypatch.setattr(processor, 'process_finished_task', stop_callbacks.wait)
    parser_app = FastAPI()
    parser_app.add_api_route('/health', processor.get_health, methods=['GET'])
    parser_app.add_api_route('/doc/add', processor.add_doc, methods=['POST'])
    parser_app.add_api_route('/algo/{algo_id}/groups', processor.get_algo_groups, methods=['GET'])
    doc_server = DocServer._Impl(
        storage_dir=str(tmp_path), db_config=db_config, parser_url='http://parser.test',
    )
    try:
        processor.register_algorithm(
            'demo-text', store=None, reader=None, node_groups={'block': {'parent': None}},
            display_name='Demo text', description='Local metadata transport test',
        )
        with TestClient(parser_app) as parser_client:
            # Keep ParserClient serialization and both handlers real; only HTTP transport is local.
            def forward_request(method, url, *, timeout, **kwargs):
                target = urlsplit(url)
                assert (target.scheme, target.netloc) == ('http', 'parser.test')
                return parser_client.request(method, target.path, **kwargs)

            monkeypatch.setattr('lazyllm.tools.rag.doc_service.parser_client.requests.request', forward_request)
            doc_server.ensure_kb_registered('demo-library', 'demo-text')
            app = FastAPI()
            DocServer._register_openapi_routes(app, doc_server)
            items = []
            for document in documents:
                source_path = tmp_path / document['source_path']
                source_path.write_text(document['title'], encoding='utf-8')
                items.append({
                    'doc_id': document['id'], 'file_path': str(source_path),
                    'metadata': {'prebuilt_path': str(PREBUILT_DEMO / document['object_path'])},
                })

            with TestClient(app) as client:
                response = client.post('/v1/docs/add', json={
                    'kb_id': 'demo-library', 'items': items, 'processing_level': 'chunked',
                })
            assert response.status_code == 200, response.text
            body = response.json()
            assert body['code'] == 200, body
            module = importlib.import_module('lazymind.processor.service.worker')
            worker = object.__new__(module.DocumentProcessorWorker._Impl)
            for expected, accepted in zip(items, body['data']['items'], strict=True):
                assert accepted['accepted'], accepted
                queued = processor._waiting_task_queue.peek({'task_id': accepted['task_id']})
                assert queued and queued['task_type'] == 'DOC_ADD'
                payload = json.loads(queued['message'])
                worker._validate_task_payload(queued['task_type'], payload)
                assert payload['kb_id'] == 'demo-library' and payload['processing_level'] == 'chunked'
                assert payload['llm_config'] is None
                [file_info] = payload['file_infos']
                assert file_info['doc_id'] == expected['doc_id']
                assert file_info['file_path'] == expected['file_path']
                assert file_info['metadata']['prebuilt_path'] == expected['metadata']['prebuilt_path']
    finally:
        stop_callbacks.set()
        if processor._post_func_thread is not None:
            processor._post_func_thread.join()
        doc_server.stop()
        if processor._db_manager is not None:
            processor._db_manager.dispose()
        for queue in (processor._waiting_task_queue, processor._finished_task_queue):
            if queue is not None:
                queue._sql_manager.dispose()


def test_worker_imports_prebuilt_text_without_processing_and_isolates_nodes_on_retry(monkeypatch, tmp_path):
    fixture = PREBUILT_DEMO
    docs = json.loads((fixture / 'releases/v1/manifest.json').read_text(encoding='utf-8'))['documents']
    sources = {doc['id']: doc for doc in docs}
    assert sources.keys() == {'reading-room', 'tool-shelf'}
    rows_by_source = {}
    artifacts = {}
    for source_id, doc in sources.items():
        artifact = fixture / doc['object_path']
        with gzip.open(artifact, 'rt', encoding='utf-8') as stream:
            rows = [json.loads(line) for line in stream]
        if source_id == 'reading-room':
            # Give the worker a source with a locator without changing the published fixture.
            next(row for row in rows if row['id'] == 'line-1')['metadata']['page'] = 3
            artifact = tmp_path / 'reading-room-with-page.jsonl.gz'
            with gzip.open(artifact, 'wt', encoding='utf-8') as stream:
                for row in rows:
                    stream.write(json.dumps(row, ensure_ascii=False) + '\n')
        rows_by_source[source_id] = rows
        artifacts[source_id] = str(artifact)
    for rows in rows_by_source.values():
        assert {'block-1', 'line-1'} <= {row['id'] for row in rows}

    def unexpected_call(operation):
        def fail(*_args, **_kwargs):
            raise AssertionError(f'prebuilt text import unexpectedly called {operation}')

        return fail

    monkeypatch.setitem(config._impl, 'database_url', 'sqlite:///:memory:')
    module = importlib.import_module('lazymind.processor.service.worker')
    worker = object.__new__(module.DocumentProcessorWorker._Impl)
    worker._worker_id = 'prebuilt-import-test'

    # Re-execute both attempts to exercise idempotent writes. The queue lease
    # and node-group status transitions belong to separate worker tests.
    monkeypatch.setattr(module, '_recover_owned_node_groups', lambda *_args: None)
    monkeypatch.setattr(worker, '_wait_and_decide_ng', lambda _docs, groups, _kb: (set(), set(groups)))
    monkeypatch.setattr(worker, '_write_ng_status_batch', lambda *_args: None)

    node_groups = {
        LAZY_ROOT_NAME: {'parent': None},
        'block': {'parent': LAZY_ROOT_NAME},
        'line': {'parent': 'block'},
    }
    group_ids = {group: f'test-{group}' for group in node_groups}
    vectors = MapStore()
    vectors.connect()
    monkeypatch.setattr(vectors, 'upsert', unexpected_call('vector writes'))
    store = _DocumentStore(
        HybridStore(segment_store=SQLiteStore(str(tmp_path / 'segments.db')), vector_store=vectors),
        group_embed_keys={'block': {'embed_main'}, 'line': {'embed_main'}},
        embed={'embed_main': unexpected_call('embedding')},
    )
    store.activate_group(list(node_groups))
    reader = SimpleNamespace(load_data=unexpected_call('document Reader'))

    def payload_for(library, source_ids):
        files = []
        for source_id in source_ids:
            doc = sources[source_id]
            artifact = artifacts[source_id]
            # Keep the input document identity identical for two imports of the
            # same source to verify KB scoping at the worker boundary.
            files.append({'file_path': artifact, 'doc_id': f'local-{source_id}-v1',
                          'metadata': {'file_name': doc['source_path'], 'prebuilt_path': artifact}})
        return {'kb_id': library, 'processing_level': 'chunked', 'file_infos': files}

    jobs = {
        'import-job-one': payload_for('installed-library-one', ['reading-room', 'tool-shelf']),
        'import-job-two': payload_for('installed-library-two', ['reading-room']),
    }
    expected_documents = [
        ('installed-library-one', 'reading-room'),
        ('installed-library-one', 'tool-shelf'),
        ('installed-library-two', 'reading-room'),
    ]

    def verify_stored_nodes():
        seen_uids = set()
        snapshot = {}
        for library, source_id in expected_documents:
            doc_id = f'local-{source_id}-v1'
            uids_by_package_id = {'': None}
            rows = rows_by_source[source_id]
            for group in node_groups:
                package_group = '__lazyllm_root__' if group == LAZY_ROOT_NAME else group
                expected = sorted((row for row in rows if row['group'] == package_group),
                                  key=lambda row: row['number'])
                actual = store.get_nodes(group=group, kb_id=library, doc_ids={doc_id}, sort_by_number=True)
                for row, node in zip(expected, actual, strict=True):
                    assert node.uid and node.uid not in seen_uids
                    seen_uids.add(node.uid)
                    uids_by_package_id[row['id']] = node.uid
                    assert (node.text, node.number) == (row['text'], row['number'])
                    assert node.metadata['source_path'] == sources[source_id]['source_path']
                    if 'page' in row['metadata']:
                        assert node.metadata['page'] == row['metadata']['page']
                    assert not node.embedding
                    assert (node.parent or None) == uids_by_package_id[row['parent_id']]
                snapshot[library, doc_id, group] = [node.uid for node in actual]
        return snapshot

    processor = _Processor(store)
    monkeypatch.setattr(processor, '_create_nodes_recursive', unexpected_call('text splitting'))
    try:
        for task_id, payload in jobs.items():
            worker._exec_add_task(processor, task_id, deepcopy(payload), node_groups, group_ids, reader)
        before_retry = verify_stored_nodes()
        for task_id, payload in jobs.items():
            worker._exec_add_task(processor, task_id, deepcopy(payload), node_groups, group_ids, reader)
        assert verify_stored_nodes() == before_retry
    finally:
        processor.close()
