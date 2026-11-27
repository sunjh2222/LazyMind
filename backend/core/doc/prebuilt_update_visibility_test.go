package doc

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"lazymind/core/common/orm"
)

func TestVersionedPrebuiltVisibilityRequiresCurrentBindingAndIndex(t *testing.T) {
	db := newDocumentTestDB(t)
	installOriginalSourceTransport(t)
	const datasetID, kbID = "library", "worker-library"
	seedDocumentServiceDatasetWithKBAndAlgo(t, db, datasetID, kbID, "general_algo", "owner", time.Now())
	for _, row := range []orm.Document{
		{
			ID: "retired-binding", LazyllmDocID: "new-binding", DatasetID: datasetID,
			Ext: json.RawMessage(`{"prebuilt_versioned":true}`), BaseModel: orm.BaseModel{CreateUserID: "owner"},
		},
		{
			ID: "legacy-core-id", LazyllmDocID: "legacy-binding", DatasetID: datasetID,
			Ext: json.RawMessage(`{}`), BaseModel: orm.BaseModel{CreateUserID: "owner"},
		},
	} {
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	svc := NewDocumentVisibilityService(db.DB, db.DB)
	svc.ready.Store(true)
	ctx := context.Background()
	caller := DatasetCatalogCaller{UserID: "owner"}
	old := DocumentVisibilityRef{KnowledgeID: kbID, DocumentID: "retired-binding"}
	current := DocumentVisibilityRef{KnowledgeID: kbID, DocumentID: "new-binding"}
	legacy := DocumentVisibilityRef{KnowledgeID: kbID, DocumentID: "legacy-core-id"}
	input := []DocumentVisibilityRef{old, current, legacy}
	wantVisible := []DocumentVisibilityRef{current, legacy}

	assertVisible := func() {
		t.Helper()
		visible, err := svc.Filter(ctx, caller, input)
		if err != nil || !slices.Equal(visible, wantVisible) {
			t.Errorf("visibility returned %v, want %v: %v", visible, wantVisible, err)
		}
	}
	assertIndexed := func(want ...DocumentVisibilityRef) {
		t.Helper()
		indexed, err := svc.filterCurrentIndexes(ctx, wantVisible)
		if err != nil || !slices.Equal(indexed, want) {
			t.Errorf("index gate returned %v, want %v: %v", indexed, want, err)
		}
	}
	assertVisible()
	assertIndexed(legacy)
	if err := db.Create(&orm.DocumentProcessingState{
		DatasetID: datasetID, DocumentID: old.DocumentID, IndexStatus: "pending",
	}).Error; err != nil {
		t.Fatal(err)
	}
	assertIndexed(legacy)
	if err := db.Model(&orm.DocumentProcessingState{}).
		Where("dataset_id = ? AND document_id = ?", datasetID, old.DocumentID).Update("index_status", "succeeded").Error; err != nil {
		t.Fatal(err)
	}
	assertVisible()
	assertIndexed(current, legacy)
}
