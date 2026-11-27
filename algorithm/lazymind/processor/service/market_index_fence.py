from contextvars import ContextVar

from sqlalchemy import bindparam, text

from lazymind.config import config
from lazymind.parsing.service.embedding_reset import _engine, IndexBusy
from lazymind.processor.service.prebuilt_import import PREBUILT_GROUPS, read_prebuilt_nodes


MARKET_INDEX_CONFIRMATION = '_market_index_confirmation'
MARKET_INDEX_GROUPS = frozenset(('block', 'line'))
market_index_task = ContextVar('market_index_task', default=None)


def _valid_id(value):
    return isinstance(value, str) and bool(value.strip())


def _valid_version(value):
    return type(value) is int and value > 0


def verify_market_index_confirmation(task_type, payload):
    """Read the confirmed model, dataset and current document bindings in one Core DB snapshot."""
    llm_config = payload['llm_config']
    confirmation = llm_config[MARKET_INDEX_CONFIRMATION]
    groups = payload.get('ng_names')
    if (task_type != 'DOC_REPARSE' or payload.get('strategy') != 'reembed'
            or payload.get('processing_level') != 'indexed' or not isinstance(groups, list)
            or len(groups) != len(MARKET_INDEX_GROUPS)
            or not all(isinstance(group, str) and group in MARKET_INDEX_GROUPS for group in groups)
            or groups[0] == groups[1] or not isinstance(confirmation, dict)):
        raise IndexBusy('invalid market text indexing confirmation')
    if (confirmation.get('source') not in ('own', 'cloud')
            or not all(_valid_id(confirmation.get(key)) for key in ('model_id', 'signature', 'dataset_id', 'kb_id'))
            or not all(_valid_version(confirmation.get(key)) for key in ('generation', 'processing_revision'))
            or confirmation['kb_id'] != payload.get('kb_id')
            or confirmation['generation'] != llm_config.get('_embedding_generation')):
        raise IndexBusy('invalid market text indexing confirmation')

    documents, files = confirmation.get('documents'), payload.get('file_infos')
    if not isinstance(documents, list) or not documents or not isinstance(files, list) or not files:
        raise IndexBusy('invalid market text indexing document scope')
    bindings, core_ids = {}, set()
    for document in documents:
        if (not isinstance(document, dict)
                or not all(_valid_id(document.get(key)) for key in ('core_id', 'doc_id', 'core_task_id'))
                or not _valid_version(document.get('resource_version'))):
            raise IndexBusy('invalid market text indexing document binding')
        core_id, doc_id = document['core_id'], document['doc_id']
        if core_id in core_ids or doc_id in bindings:
            raise IndexBusy('duplicate market text indexing document binding')
        core_ids.add(core_id)
        bindings[doc_id] = (core_id, doc_id, document['resource_version'], 'core-task:' + document['core_task_id'])

    # DocServer creates a per-document Worker task while retaining the batch's confirmation.
    expected = set()
    for info in files:
        doc_id = info.get('doc_id') if isinstance(info, dict) else None
        if not _valid_id(doc_id) or doc_id not in bindings or bindings[doc_id] in expected:
            raise IndexBusy('market text indexing document is outside the confirmed scope')
        expected.add(bindings[doc_id])

    url = config['core_database_url']
    if not url:
        raise IndexBusy('Core database is required to verify market text indexing')
    query = text('''SELECT d.id, d.lazyllm_doc_id, d.resource_version, s.index_artifact_ref
        FROM documents d JOIN datasets ds ON ds.id = d.dataset_id
        JOIN document_processing_states s ON s.dataset_id = d.dataset_id AND s.document_id = d.id
        JOIN embedding_index_states e ON e.scope = 'global'
        WHERE ds.id = :dataset_id AND ds.kb_id = :kb_id AND ds.processing_revision = :processing_revision
          AND ds.processing_level = 'chunked' AND ds.deleted_at IS NULL AND d.deleted_at IS NULL
          AND s.parse_status = 'succeeded' AND s.chunk_status = 'succeeded'
          AND e.phase = 'ready' AND e.generation = :generation AND e.active_source = :source
          AND e.active_model_id = :model_id AND e.signature = :signature AND d.id IN :core_ids
        ''').bindparams(bindparam('core_ids', expanding=True))
    try:
        keys = ('dataset_id', 'kb_id', 'processing_revision', 'generation', 'source', 'model_id', 'signature')
        parameters = {key: confirmation[key] for key in keys}
        parameters['core_ids'] = [row[0] for row in expected]
        with _engine(url).connect() as conn:
            rows = set(map(tuple, conn.execute(query, parameters)))
    except Exception:
        # Driver errors can include connection information. Keep task diagnostics free of that data.
        raise IndexBusy('could not verify the confirmed market text indexing state') from None
    if rows != expected:
        raise IndexBusy('the confirmed market indexing model or document binding changed')


def require_market_text_nodes(store, node_groups, name_to_id, files, kb_id):
    """Match the retained segments to the exact document hierarchy from each installed sidecar."""
    expected = {group: {} for group in PREBUILT_GROUPS}
    for info in files:
        try:
            groups = read_prebuilt_nodes(info, kb_id, require_source=False)
        except (OSError, EOFError, KeyError, TypeError, ValueError, RuntimeError):
            raise RuntimeError('installed market text sidecar is unavailable or invalid') from None
        for group, nodes in groups.items():
            if {node.number for node in nodes} != set(range(1, len(nodes) + 1)):
                raise RuntimeError('installed market text sidecar has invalid node numbering')
            for node in nodes:
                expected[group][info['doc_id'], node.uid] = (node.number, node.parent, node.text)

    impl = store.seg_impl
    segments = getattr(impl, 'segment_store', impl)
    scope = {'docid': [info['doc_id'] for info in files], 'kb_id': kb_id}
    for group, parent_group in PREBUILT_GROUPS.items():
        if (group not in node_groups or node_groups[group].get('parent') != parent_group
                or not store.is_group_active(group) or (group in MARKET_INDEX_GROUPS and group not in name_to_id)):
            raise RuntimeError('required market text node group is unavailable')
        for row in segments.get(store._gen_collection_name(group), scope):
            metadata, number = row.get('meta'), row.get('number')
            identity = (row.get('doc_id'), row.get('uid'))
            if (row.get('kb_id') != kb_id or row.get('group') != group or not _valid_version(number)
                    or not isinstance(metadata, dict) or '_lazyllm_null_node' in metadata
                    or (number, row.get('parent') or None, row.get('content')) != expected[group].pop(identity, None)):
                raise RuntimeError('retained market text nodes are incomplete or invalid')
        if expected[group]:
            raise RuntimeError('retained market text nodes are incomplete or invalid')
