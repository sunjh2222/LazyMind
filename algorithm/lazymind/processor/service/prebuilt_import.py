import gzip
import json
from hashlib import sha256
from pathlib import Path

from lazyllm.tools.rag.doc_node import DocNode, NULL_NODE_META_KEY
from lazyllm.tools.rag.global_metadata import RAG_DOC_FILE_NAME, RAG_DOC_ID, RAG_DOC_PATH, RAG_KB_ID
from lazyllm.tools.rag.store import LAZY_ROOT_NAME
from lazyllm.tools.rag.store.store_base import INSERT_BATCH_SIZE


PREBUILT_GROUPS = {LAZY_ROOT_NAME: None, 'block': LAZY_ROOT_NAME, 'line': 'block'}


def read_prebuilt_nodes(info, kb_id, *, require_source=True):
    metadata = dict(info['metadata'])
    artifact = Path(metadata.pop('prebuilt_path'))
    source = Path(info['file_path'])
    if not artifact.is_absolute() or not source.is_absolute():
        raise ValueError('Prebuilt text import requires absolute file paths')
    artifact, source = artifact.resolve(strict=True), source.resolve(strict=require_source)
    if (artifact.parent != source.parent or not artifact.name.endswith('.jsonl.gz')
            or not artifact.is_file() or (require_source and not source.is_file())):
        raise ValueError('Prebuilt text object must be a .jsonl.gz file next to the registered document')

    doc_id = info['doc_id']
    metadata.update({
        RAG_DOC_ID: doc_id, RAG_KB_ID: kb_id, RAG_DOC_PATH: str(source),
        RAG_DOC_FILE_NAME: metadata.get(RAG_DOC_FILE_NAME) or metadata.get('display_name') or source.name,
    })
    rows, groups, uids = {}, {}, {}
    numbers = {group: set() for group in PREBUILT_GROUPS}
    with gzip.open(artifact, 'rt', encoding='utf-8') as stream:
        for line_number, line in enumerate(stream, start=1):
            row = json.loads(line)
            if not isinstance(row, dict):
                raise ValueError(f'Invalid prebuilt text row at line {line_number}')
            row_id, group, number = row.get('id'), row.get('group'), row.get('number')
            node_meta = row.get('metadata')
            if group == '__lazyllm_root__':
                group = LAZY_ROOT_NAME
            if (not isinstance(row_id, str) or not row_id or row_id in rows
                    or not isinstance(group, str) or group not in PREBUILT_GROUPS
                    or type(number) is not int or number < 1 or number in numbers[group]
                    or not isinstance(row.get('text'), str) or not row['text'].strip()
                    or not isinstance(node_meta, dict) or NULL_NODE_META_KEY in node_meta):
                raise ValueError(f'Invalid prebuilt text node at line {line_number}')
            rows[row_id], groups[row_id] = row, group
            numbers[group].add(number)
            uid_key = json.dumps([kb_id, doc_id, row_id], ensure_ascii=False).encode('utf-8')
            uids[row_id] = 'prebuilt-' + sha256(uid_key).hexdigest()
    if not all(numbers.values()):
        raise ValueError('Prebuilt text object must contain source, block and line nodes')

    nodes = {group: [] for group in PREBUILT_GROUPS}
    for row_id, row in rows.items():
        group = groups[row_id]
        parent = row.get('parent_id')
        if group == LAZY_ROOT_NAME:
            if parent not in (None, ''):
                raise ValueError('Prebuilt source node cannot have a parent')
            parent_uid = None
        else:
            if not isinstance(parent, str) or groups.get(parent) != PREBUILT_GROUPS[group]:
                raise ValueError('Prebuilt text node has a missing or invalid parent')
            parent_uid = uids[parent]
        node = DocNode(uid=uids[row_id], text=row['text'], group=group, parent=parent_uid,
                       metadata=dict(row['metadata']), global_metadata=metadata)
        node.number = row['number']
        nodes[group].append(node)
    return nodes


def import_prebuilt_documents(store, file_infos, kb_id):
    if not all(store.is_group_active(group) for group in PREBUILT_GROUPS):
        raise ValueError('Prebuilt text import requires active source, block and line node groups')
    # Validate the entire batch before touching the shared segment store.
    documents = [read_prebuilt_nodes(info, kb_id) for info in file_infos]
    impl = store.seg_impl
    segments = getattr(impl, 'segment_store', impl)
    for groups in documents:
        for group, nodes in groups.items():
            collection = store._gen_collection_name(group)
            for offset in range(0, len(nodes), INSERT_BATCH_SIZE):
                batch = [store._serialize_node(node) for node in nodes[offset:offset + INSERT_BATCH_SIZE]]
                for segment in batch:
                    segment.pop('embedding', None)
                if not segments.upsert(collection, batch):
                    raise RuntimeError(f'Failed to write prebuilt text group {group!r}')
