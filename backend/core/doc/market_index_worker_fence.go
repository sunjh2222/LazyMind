package doc

import "lazymind/core/common/orm"

// marketIndexConfirmationConfig supplies the Worker with the exact confirmed scope of this reembed batch.
func marketIndexConfirmationConfig(dataset orm.Dataset, expected MarketEmbeddingIdentity, documents []orm.Document, tasks []orm.Task) map[string]any {
	if len(documents) == 0 || len(documents) != len(tasks) {
		return nil
	}
	bindings := make([]map[string]any, 0, len(documents))
	for i, document := range documents {
		if tasks[i].DocID != document.ID {
			return nil
		}
		bindings = append(bindings, map[string]any{
			"core_id": document.ID, "doc_id": document.LazyllmDocID, "resource_version": document.ResourceVersion,
			"core_task_id": tasks[i].ID,
		})
	}
	return map[string]any{
		"source": expected.Source, "model_id": expected.ModelID, "signature": expected.Signature, "generation": expected.Generation,
		"dataset_id": dataset.ID, "kb_id": dataset.KbID, "processing_revision": dataset.ProcessingRevision, "documents": bindings,
	}
}
