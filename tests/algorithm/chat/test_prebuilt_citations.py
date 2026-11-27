import pytest

from lazymind.chat.service.utils.citations import (
    build_source_node_from_item,
    citation_source,
    register_citation_item,
    reset_citation_state,
)


VERSIONED_DOC = 'prebuiltv_' + '1' * 32
CORE_DOC = 'doc_' + '2' * 32
ALGORITHM_KB = 'algorithm-library'
CORE_DATASET = 'personal-library'


def prebuilt_result(docid=VERSIONED_DOC):
    return {
        'docid': docid,
        'kb_id': ALGORITHM_KB,
        'group': 'block',
        'number': 1,
        'text': 'The reading room opens at 15:00.',
        'global_metadata': {
            'docid': docid,
            'kb_id': ALGORITHM_KB,
            'core_document_id': CORE_DOC,
            'dataset_id': CORE_DATASET,
        },
    }


@pytest.mark.parametrize('raw_docid', [True, False])
def test_versioned_prebuilt_source_opens_the_stable_core_document(raw_docid):
    item = prebuilt_result()
    if not raw_docid:
        item.pop('docid')
    item['uid'] = 'imported-block'

    source = build_source_node_from_item('1.1', item)

    assert source['document_id'] == CORE_DOC
    assert source['dataset_id'] == CORE_DATASET
    assert source['segement_id'] == 'imported-block'
    assert source['content'] == item['text']
    assert item['global_metadata']['docid'] == VERSIONED_DOC
    assert 'url' not in source


@pytest.mark.parametrize(('raw_id', 'core_metadata'), [
    ('ordinary-document', CORE_DOC),
    (VERSIONED_DOC, ''),
    (VERSIONED_DOC, 123),
])
def test_citation_keeps_existing_route_without_versioned_core_metadata(raw_id, core_metadata):
    item = prebuilt_result(raw_id)
    item['global_metadata']['core_document_id'] = core_metadata
    # Per-segment metadata is sourced from the content, unlike the global
    # metadata stamped by Core when it submits the document.
    item['metadata'] = {'core_document_id': 'untrusted-document', 'dataset_id': 'untrusted-library'}

    source = build_source_node_from_item('1.1', item)

    assert source['document_id'] == raw_id
    assert source['dataset_id'] == ALGORITHM_KB


def test_prebuilt_citation_groups_keep_distinct_raw_document_versions():
    config = {}
    reset_citation_state(config)
    v1 = prebuilt_result()
    v2 = prebuilt_result('prebuiltv_' + '3' * 32)
    v2['text'] = 'The reading room now opens at 16:00.'

    index_v1 = register_citation_item(v1, config)['citation_index']
    index_v2 = register_citation_item(v2, config)['citation_index']

    assert (index_v1, index_v2) == ('1.1', '2.1')
    assert register_citation_item(v1, config)['citation_index'] == index_v1
    assert citation_source(config, index_v1)['document_id'] == CORE_DOC
    assert citation_source(config, index_v2)['document_id'] == CORE_DOC
