package knowledge_market

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"lazymind/core/common"
	"lazymind/core/common/orm"
	"lazymind/core/doc"
	"lazymind/core/log"
	"lazymind/core/modelprovider"
)

const (
	installPreviewTimeout    = 10 * time.Second
	installConfirmationTTL   = 10 * time.Minute
	installConfirmationBytes = 8 + sha256.Size
)

var (
	// A restart or another Core process must require a new preview.
	installConfirmationKey        = []byte(rand.Text())
	errInvalidInstallConfirmation = errors.New("invalid install confirmation")
	errStaleInstallConfirmation   = errors.New("install confirmation is no longer current; preview again")
)

type InstallRequest struct {
	ProcessingLevel   string `json:"processing_level,omitempty" enum:"chunked,indexed"`
	ConfirmationToken string `json:"confirmation_token,omitempty"`
}

type InstallPreview struct {
	ItemID         string                    `json:"item_id" required:"true"`
	Version        string                    `json:"version" required:"true"`
	ManifestSHA256 string                    `json:"manifest_sha256" required:"true"`
	DocumentCount  int                       `json:"document_count" required:"true"`
	ChunkCount     int                       `json:"chunk_count" required:"true"`
	SizeBytes      int64                     `json:"size_bytes" required:"true"`
	VectorIndex    InstallPreviewVectorIndex `json:"vector_index" required:"true"`
}

type InstallPreviewVectorIndex struct {
	Available         bool   `json:"available" required:"true"`
	ModelName         string `json:"model_name,omitempty"`
	ConfirmationToken string `json:"confirmation_token,omitempty"`
}

// installIndexConfirmation records the verified intent for the later indexing phase.
type installIndexConfirmation struct {
	Source     string `json:"source"`
	ModelID    string `json:"model_id"`
	Signature  string `json:"signature"`
	Generation int64  `json:"generation"`
}

func (confirmation installIndexConfirmation) valid() bool {
	return (confirmation.Source == "own" || confirmation.Source == "cloud") && confirmation.ModelID != "" &&
		confirmation.Signature != "" && confirmation.Generation > 0
}

func installIndexIdentity(state orm.EmbeddingIndexState) installIndexConfirmation {
	return installIndexConfirmation{Source: state.ActiveSource, ModelID: state.ActiveModelID,
		Signature: state.Signature, Generation: state.Generation}
}

func currentInstallIndexModel(ctx context.Context, db *gorm.DB, userID string) (*installIndexConfirmation, string, error) {
	if !db.WithContext(ctx).Migrator().HasTable(&orm.EmbeddingIndexState{}) {
		return nil, "", nil
	}
	// The legacy readiness resolver can lazily initialize private provider state.
	// Keep its reads on one snapshot and discard any such maintenance writes.
	tx := db.WithContext(ctx).Begin(&sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if tx.Error != nil {
		return nil, "", tx.Error
	}
	defer tx.Rollback()
	var state orm.EmbeddingIndexState
	if err := tx.Take(&state, "scope = ?", "global").Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, "", nil
		}
		return nil, "", err
	}
	confirmation := installIndexIdentity(state)
	if state.Phase != "ready" || !confirmation.valid() {
		return nil, "", nil
	}
	ready, err := modelprovider.IsModelReady(ctx, tx, userID, "embed_main")
	if err != nil || !ready {
		return nil, "", err
	}
	name := strings.TrimSpace(state.ActiveModelName)
	if name == "" {
		name = confirmation.ModelID
	}
	return &confirmation, name, nil
}

func lockInstallIndexConfirmation(tx *gorm.DB, expected *installIndexConfirmation) error {
	var state orm.EmbeddingIndexState
	err := tx.Clauses(clause.Locking{Strength: "SHARE"}).Take(&state, "scope = ?", "global").Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return errStaleInstallConfirmation
	}
	if err != nil {
		return err
	}
	if expected == nil || state.Phase != "ready" || installIndexIdentity(state) != *expected {
		return errStaleInstallConfirmation
	}
	return nil
}

func readVerifiedInstallRelease(ctx context.Context, item orm.KnowledgeMarketItem) (*prebuiltRelease, error) {
	release, err := readPrebuiltRelease(ctx, prebuiltReleaseURL(item), item.ID)
	if err != nil {
		return nil, err
	}
	base, err := prebuiltReleaseBase(release.URL)
	if err != nil {
		return nil, err
	}
	if _, err := readPrebuiltDocuments(ctx, base, release); err != nil {
		return nil, err
	}
	return release, nil
}

func installConfirmationMAC(expires []byte, userID string, release *prebuiltRelease, model *installIndexConfirmation) []byte {
	claims, _ := json.Marshal(struct {
		UserID  string
		Release *prebuiltRelease
		Model   *installIndexConfirmation
	}{userID, release, model})
	mac := hmac.New(sha256.New, installConfirmationKey)
	mac.Write([]byte("lazymind.market.install.indexed.v1"))
	mac.Write(expires)
	mac.Write(claims)
	return mac.Sum(nil)
}

func issueInstallConfirmation(userID string, release *prebuiltRelease, model *installIndexConfirmation) string {
	var expires [8]byte
	binary.BigEndian.PutUint64(expires[:], uint64(time.Now().Add(installConfirmationTTL).Unix()))
	return base64.RawURLEncoding.EncodeToString(append(expires[:], installConfirmationMAC(expires[:], userID, release, model)...))
}

// MarketInstallPreview reads the release metadata and exposes only model availability and a scoped confirmation.
func MarketInstallPreview(w http.ResponseWriter, r *http.Request) {
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
	ctx, cancel := context.WithTimeout(r.Context(), installPreviewTimeout)
	defer cancel()
	var item orm.KnowledgeMarketItem
	if err := db.WithContext(ctx).Take(&item, "id = ? AND status = ?", itemID, "published").Error; err != nil {
		replyServiceError(w, err)
		return
	}
	if prebuiltReleaseURL(item) == "" {
		common.ReplyErr(w, "prebuilt install preview is not available for this item", http.StatusNotFound)
		return
	}
	release, err := readVerifiedInstallRelease(ctx, item)
	if err != nil {
		common.ReplyErr(w, "could not verify prebuilt knowledge release", http.StatusBadGateway)
		return
	}
	preview := InstallPreview{ItemID: itemID, Version: release.Version, ManifestSHA256: release.ManifestSHA256,
		DocumentCount: release.DocumentCount, ChunkCount: release.ChunkCount, SizeBytes: release.Size}
	model, name, err := currentInstallIndexModel(ctx, db, userID)
	if err != nil {
		log.Logger.Warn().Str("market_item_id", itemID).Msg("could not check model availability for market install preview")
	}
	if model != nil {
		preview.VectorIndex = InstallPreviewVectorIndex{Available: true, ModelName: name,
			ConfirmationToken: issueInstallConfirmation(userID, release, model)}
	}
	common.ReplyOK(w, preview)
}

func decodeInstallRequest(w http.ResponseWriter, r *http.Request) (InstallRequest, error) {
	var request InstallRequest
	if r.Body == nil {
		return request, nil
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	if err := decoder.Decode(&request); err != nil {
		if errors.Is(err, io.EOF) {
			return request, nil
		}
		return InstallRequest{}, errors.New("invalid install request")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return InstallRequest{}, errors.New("invalid install request")
	}
	switch request.ProcessingLevel {
	case "", doc.ProcessingLevelChunked:
	case doc.ProcessingLevelIndexed:
		if request.ConfirmationToken == "" {
			return InstallRequest{}, errors.New("confirmation_token is required for indexed installation")
		}
	default:
		return InstallRequest{}, errors.New("processing_level must be chunked or indexed")
	}
	return request, nil
}

func confirmIndexedInstall(ctx context.Context, db *gorm.DB, item orm.KnowledgeMarketItem, userID, token string) (
	*prebuiltRelease, *installIndexConfirmation, time.Time, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(raw) != installConfirmationBytes {
		return nil, nil, time.Time{}, errInvalidInstallConfirmation
	}
	expiresAt := time.Unix(int64(binary.BigEndian.Uint64(raw[:8])), 0)
	if !time.Now().Before(expiresAt) {
		return nil, nil, time.Time{}, errStaleInstallConfirmation
	}
	ctx, cancel := context.WithTimeout(ctx, installPreviewTimeout)
	defer cancel()
	release, err := readVerifiedInstallRelease(ctx, item)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	model, _, err := currentInstallIndexModel(ctx, db, userID)
	if err != nil || model == nil || !hmac.Equal(raw[8:], installConfirmationMAC(raw[:8], userID, release, model)) {
		return nil, nil, time.Time{}, errStaleInstallConfirmation
	}
	return release, model, expiresAt, nil
}
