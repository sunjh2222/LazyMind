package knowledge_market

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"lazymind/core/common"
	"lazymind/core/common/orm"
	"lazymind/core/doc"
)

// PrebuiltUpdatePreview describes slice files needed to update a personal
// knowledge base. Document identities come from the published manifests.
type PrebuiltUpdatePreview struct {
	InstalledVersion string   `json:"installed_version"`
	TargetVersion    string   `json:"target_version"`
	HasUpdate        bool     `json:"has_update"`
	Added            []string `json:"added" required:"true"`
	Changed          []string `json:"changed" required:"true"`
	Removed          []string `json:"removed" required:"true"`
	UnchangedCount   int      `json:"unchanged_count"`
	DownloadBytes    int64    `json:"download_bytes"`
}

// MarketPrebuiltUpdatePreview reads the version pointer and, if necessary,
// both pinned manifests. It does not fetch slice objects or enqueue work.
func MarketPrebuiltUpdatePreview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	db, ok := requireDB(w)
	if !ok {
		return
	}
	itemID := strings.TrimSpace(common.PathVar(r, "market_item_id"))
	userID := strings.TrimSpace(common.UserID(r))
	if itemID == "" || userID == "" {
		common.ReplyErr(w, "market_item_id and X-User-Id are required", http.StatusBadRequest)
		return
	}
	install, err := loadInstall(r, db, userID, itemID)
	if err != nil {
		replyServiceError(w, err)
		return
	}
	if install == nil {
		replyServiceError(w, errMarketNotInstalled)
		return
	}
	installed := decodeInstallConfig(install).Prebuilt
	if installed == nil || install.DatasetID == "" ||
		install.InstallState != string(orm.InstallStateDone) ||
		install.InstalledVersion != installed.Version || !validPrebuiltRelease(installed, itemID) {
		common.ReplyErr(w, "prebuilt update preview is unavailable for this installation", http.StatusConflict)
		return
	}
	if !doc.RequireMarketDatasetPermission(w, r, install.DatasetID) {
		return
	}
	var item orm.KnowledgeMarketItem
	if err := db.WithContext(r.Context()).Where("id = ? AND status = ?", itemID, "published").Take(&item).Error; err != nil {
		replyServiceError(w, err)
		return
	}
	releaseURL := prebuiltReleaseURL(item)
	if releaseURL == "" || releaseURL != installed.URL {
		replyServiceError(w, errMarketSourceChanged)
		return
	}
	if progress := parseProgress(r, db, install); progress.State != "done" || progress.Done != installed.DocumentCount {
		common.ReplyErr(w, "prebuilt documents are not ready for update preview", http.StatusConflict)
		return
	}
	preview, err := previewPrebuiltUpdate(r.Context(), installed)
	if err != nil {
		// Transport errors may contain signed URLs. Keep them out of this response.
		common.ReplyErr(w, "could not verify the prebuilt release for update", http.StatusBadGateway)
		return
	}
	common.ReplyOK(w, preview)
}

func previewPrebuiltUpdate(ctx context.Context, installed *prebuiltRelease) (*PrebuiltUpdatePreview, error) {
	latest, err := readPrebuiltUpdateTarget(ctx, installed)
	if err != nil {
		return nil, err
	}
	preview := &PrebuiltUpdatePreview{
		InstalledVersion: installed.Version,
		TargetVersion:    latest.Version,
		Added:            []string{},
		Changed:          []string{},
		Removed:          []string{},
	}
	if *installed == *latest {
		preview.UnchangedCount = installed.DocumentCount
		return preview, nil
	}
	base, err := prebuiltReleaseBase(installed.URL)
	if err != nil {
		return nil, err
	}
	currentDocuments, err := readPrebuiltDocuments(ctx, base, installed)
	if err != nil {
		return nil, err
	}
	targetDocuments, err := readPrebuiltDocuments(ctx, base, latest)
	if err != nil {
		return nil, err
	}
	preview.HasUpdate = true
	current := make(map[string]string, len(currentDocuments))
	for _, document := range currentDocuments {
		current[document.ID] = document.ObjectSHA256
	}
	for _, document := range targetDocuments {
		digest, exists := current[document.ID]
		delete(current, document.ID)
		switch {
		case !exists:
			preview.Added = append(preview.Added, document.ID)
		case digest == document.ObjectSHA256:
			preview.UnchangedCount++
			continue
		default:
			preview.Changed = append(preview.Changed, document.ID)
		}
		preview.DownloadBytes += document.Size
	}
	for id := range current {
		preview.Removed = append(preview.Removed, id)
	}
	sort.Strings(preview.Removed)
	return preview, nil
}

func readPrebuiltUpdateTarget(ctx context.Context, installed *prebuiltRelease) (*prebuiltRelease, error) {
	latest, err := readPrebuiltRelease(ctx, installed.URL, installed.ItemID)
	if err != nil {
		return nil, err
	}
	if *installed != *latest && (installed.Version == latest.Version || installed.ManifestSHA256 == latest.ManifestSHA256) {
		return nil, fmt.Errorf("prebuilt release identity was reused")
	}
	return latest, nil
}
