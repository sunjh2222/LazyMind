package knowledge_market

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"lazymind/core/common/orm"
	"lazymind/core/doc"
)

const (
	prebuiltReleaseOption = "_market_prebuilt_release_url"
	prebuiltPointerLimit  = 64 << 10
	prebuiltManifestLimit = 4 << 20
	prebuiltObjectLimit   = 16 << 20
	prebuiltDecodedLimit  = 32 << 20
	prebuiltTotalLimit    = 512 << 20
	prebuiltRecordLimit   = 1 << 20
	modelscopeLFSHost     = "cdn-lfs-cn-1.modelscope.cn"
)

var prebuiltVersionPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,31}$`)
var errPrebuiltRedirect = errors.New("prebuilt download redirected outside its configured origin")

type prebuiltRelease struct {
	URL            string `json:"url,omitempty"`
	ItemID         string `json:"item_id"`
	Version        string `json:"version"`
	ManifestPath   string `json:"manifest_path"`
	ManifestSHA256 string `json:"manifest_sha256"`
	DocumentCount  int    `json:"document_count"`
	ChunkCount     int    `json:"chunk_count"`
	Size           int64  `json:"size_bytes"`
}

type prebuiltManifest struct {
	Schema         int                `json:"schema_version"`
	ItemID         string             `json:"item_id"`
	Version        string             `json:"version"`
	SourceRevision string             `json:"source_revision"`
	ChunkProfile   string             `json:"chunk_profile"`
	Documents      []prebuiltDocument `json:"documents"`
}

type prebuiltDocument struct {
	ID               string `json:"id"`
	Title            string `json:"title"`
	SourcePath       string `json:"source_path"`
	SourceURL        string `json:"source_url,omitempty"`
	SourceSHA256     string `json:"source_sha256"`
	ChunkFingerprint string `json:"chunk_fingerprint"`
	ObjectPath       string `json:"object_path"`
	ObjectSHA256     string `json:"object_sha256"`
	Size             int64  `json:"size_bytes"`
	ChunkCount       int    `json:"chunk_count"`
}

type prebuiltNode struct {
	ID       string          `json:"id"`
	ParentID string          `json:"parent_id"`
	Group    string          `json:"group"`
	Number   int             `json:"number"`
	Text     string          `json:"text"`
	Metadata json.RawMessage `json:"metadata"`
}

func prebuiltReleaseURL(item orm.KnowledgeMarketItem) string {
	var options map[string]json.RawMessage
	var release string
	if json.Unmarshal(item.SourceOptions, &options) == nil {
		_ = json.Unmarshal(options[prebuiltReleaseOption], &release)
	}
	return release
}

func prebuiltReleaseBase(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "https" && !(u.Scheme == "http" && net.ParseIP(u.Hostname()).IsLoopback())) {
		return nil, fmt.Errorf("prebuilt release URL must be HTTPS or loopback HTTP")
	}
	return u, nil
}

func isModelScopeDatasetDownload(u *url.URL) bool {
	if u.Scheme != "https" || u.Host != "modelscope.cn" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || path.Clean(u.Path) != u.Path {
		return false
	}
	parts := strings.SplitN(u.EscapedPath(), "/", 7)
	return len(parts) == 7 && parts[0] == "" && parts[1] == "datasets" && parts[2] != "" &&
		parts[3] != "" && parts[4] == "resolve" && parts[5] != "" && parts[6] != ""
}

func safePrebuiltDownloadCause(err error) error {
	// Both url.Error and the error for a malformed Location can include a
	// signed URL. Retain only causes that cannot contain response data.
	switch {
	case errors.Is(err, errPrebuiltRedirect):
		return errPrebuiltRedirect
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, io.ErrUnexpectedEOF):
		return io.ErrUnexpectedEOF
	default:
		return errors.New("HTTP request failed")
	}
}

func getPrebuilt(ctx context.Context, u *url.URL, limit int64, expectedSHA256 string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", u.EscapedPath(), safePrebuiltDownloadCause(err))
	}
	req.Header.Set("Accept-Encoding", "identity")
	allowLFS := validPrebuiltHash(expectedSHA256) && isModelScopeDatasetDownload(u)
	client := http.Client{
		Timeout: 60 * time.Second,
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			// A request that has left the configured origin cannot redirect
			// again, even back to that origin.
			if len(via) >= 10 || next.URL.User != nil || via[len(via)-1].URL.Host != u.Host {
				return errPrebuiltRedirect
			}
			if next.URL.Host == u.Host && next.URL.Scheme == u.Scheme {
				return nil
			}
			if allowLFS && len(via) == 1 && next.URL.Scheme == "https" &&
				next.URL.Host == modelscopeLFSHost && next.URL.Fragment == "" {
				return nil
			}
			return errPrebuiltRedirect
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &url.Error{Op: "download", URL: u.EscapedPath(), Err: safePrebuiltDownloadCause(err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", u.Path, resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("download %s exceeds the size limit", u.Path)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", u.EscapedPath(), safePrebuiltDownloadCause(err))
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("download %s exceeds the size limit", u.Path)
	}
	return body, nil
}

func readPrebuiltRelease(ctx context.Context, rawURL, itemID string) (*prebuiltRelease, error) {
	u, err := prebuiltReleaseBase(rawURL)
	if err != nil {
		return nil, err
	}
	body, err := getPrebuilt(ctx, u, prebuiltPointerLimit, "")
	if err != nil {
		return nil, err
	}
	var release prebuiltRelease
	if err := json.Unmarshal(body, &release); err != nil {
		return nil, fmt.Errorf("invalid prebuilt release: %w", err)
	}
	if !validPrebuiltRelease(&release, itemID) {
		return nil, fmt.Errorf("invalid prebuilt release identity, size or counts")
	}
	release.URL = rawURL
	return &release, nil
}

func validPrebuiltRelease(release *prebuiltRelease, itemID string) bool {
	return release.ItemID == itemID && prebuiltVersionPattern.MatchString(release.Version) &&
		release.ManifestPath == "releases/"+release.Version+"/manifest.json" &&
		validPrebuiltHash(release.ManifestSHA256) &&
		release.DocumentCount >= 1 && release.DocumentCount <= 1000 &&
		release.ChunkCount >= release.DocumentCount && release.ChunkCount <= 1000000 &&
		release.Size >= 1 && release.Size <= prebuiltTotalLimit
}

func validPrebuiltHash(value string) bool {
	_, err := hex.DecodeString(value)
	return len(value) == sha256.Size*2 && value == strings.ToLower(value) && err == nil
}

func matchesPrebuiltHash(data []byte, want string) bool {
	return fmt.Sprintf("%x", sha256.Sum256(data)) == want
}

func readPrebuiltDocuments(ctx context.Context, base *url.URL, release *prebuiltRelease) ([]prebuiltDocument, error) {
	body, err := getPrebuilt(ctx, base.ResolveReference(&url.URL{Path: release.ManifestPath}),
		prebuiltManifestLimit, release.ManifestSHA256)
	if err != nil {
		return nil, err
	}
	if !matchesPrebuiltHash(body, release.ManifestSHA256) {
		return nil, fmt.Errorf("prebuilt manifest checksum mismatch")
	}
	var manifest prebuiltManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil, fmt.Errorf("invalid prebuilt manifest: %w", err)
	}
	if manifest.Schema != 1 || manifest.ItemID != release.ItemID || manifest.Version != release.Version ||
		manifest.ChunkProfile != "general_algo" || strings.TrimSpace(manifest.SourceRevision) == "" ||
		len(manifest.Documents) != release.DocumentCount {
		return nil, fmt.Errorf("invalid prebuilt manifest identity, profile or document count")
	}
	var size int64
	var chunks int
	seen := make(map[string]bool, len(manifest.Documents))
	for _, document := range manifest.Documents {
		if !validPrebuiltDocument(document) || seen[document.ID] {
			return nil, fmt.Errorf("invalid or duplicate prebuilt document %q", document.ID)
		}
		seen[document.ID] = true
		size += document.Size
		chunks += document.ChunkCount
	}
	if size != release.Size || chunks != release.ChunkCount {
		return nil, fmt.Errorf("prebuilt manifest size or chunk count mismatch")
	}
	return manifest.Documents, nil
}

func fetchPrebuiltFiles(ctx context.Context, release *prebuiltRelease, root string) ([]doc.MarketImportFile, error) {
	u, err := prebuiltReleaseBase(release.URL)
	if err != nil {
		return nil, err
	}
	documents, err := readPrebuiltDocuments(ctx, u, release)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	files := make([]doc.MarketImportFile, 0, len(documents))
	var decodedSize int64
	for idx, document := range documents {
		file, decoded, err := fetchPrebuiltFile(ctx, u, release, document, root, fmt.Sprint(idx), prebuiltTotalLimit-decodedSize)
		if err != nil {
			return nil, err
		}
		decodedSize += decoded
		files = append(files, file)
	}
	return files, nil
}

func fetchPrebuiltFile(ctx context.Context, base *url.URL, release *prebuiltRelease, document prebuiltDocument,
	root, name string, decodedLimit int64) (doc.MarketImportFile, int64, error) {
	compressed, err := getPrebuilt(ctx, base.ResolveReference(&url.URL{Path: document.ObjectPath}),
		document.Size, document.ObjectSHA256)
	if err != nil {
		return doc.MarketImportFile{}, 0, err
	}
	if int64(len(compressed)) != document.Size || !matchesPrebuiltHash(compressed, document.ObjectSHA256) {
		return doc.MarketImportFile{}, 0, fmt.Errorf("prebuilt object %q size or checksum mismatch", document.ID)
	}
	text, decoded, err := decodePrebuiltObject(compressed, document)
	if err != nil {
		return doc.MarketImportFile{}, 0, fmt.Errorf("invalid prebuilt object %q: %w", document.ID, err)
	}
	if decoded > decodedLimit {
		return doc.MarketImportFile{}, 0, fmt.Errorf("prebuilt release exceeds the decoded size limit")
	}
	stem := filepath.Join(root, name)
	original, artifact := stem+".md", stem+".jsonl.gz"
	if err := os.WriteFile(original, []byte(text), 0o600); err != nil {
		return doc.MarketImportFile{}, 0, err
	}
	if err := os.WriteFile(artifact, compressed, 0o600); err != nil {
		return doc.MarketImportFile{}, 0, err
	}
	folder := path.Dir(document.SourcePath)
	if folder == "." {
		folder = ""
	}
	return doc.MarketImportFile{
		LocalPath: original, DisplayName: strings.TrimSuffix(strings.TrimSpace(document.Title), ".md") + ".md",
		RelativePath: folder,
		PrebuiltPath: artifact, PrebuiltIdentity: release.ManifestSHA256 + ":" + document.ID,
		SourcePath: document.SourcePath, SourceURL: document.SourceURL,
	}, decoded, nil
}

func validPrebuiltDocument(document prebuiltDocument) bool {
	if document.ID == "" || len(document.ID) > 255 || strings.TrimSpace(document.Title) == "" ||
		len(document.Title) > 240 || strings.ContainsAny(document.Title, "/\\") ||
		strings.IndexFunc(document.Title+document.ID+document.SourcePath, unicode.IsControl) != -1 ||
		!fs.ValidPath(document.SourcePath) || strings.Contains(document.SourcePath, "\\") ||
		document.SourcePath == "." || !validPrebuiltHash(document.SourceSHA256) ||
		!validPrebuiltHash(document.ChunkFingerprint) || !validPrebuiltHash(document.ObjectSHA256) ||
		document.ObjectPath != "objects/"+document.ObjectSHA256+".jsonl.gz" ||
		document.Size < 1 || document.Size > prebuiltObjectLimit || document.ChunkCount < 1 ||
		document.ChunkCount > 1000000 {
		return false
	}
	if document.SourceURL == "" {
		return true
	}
	u, err := url.Parse(document.SourceURL)
	return err == nil && u.Hostname() != "" && u.User == nil && (u.Scheme == "https" || u.Scheme == "http")
}

func decodePrebuiltObject(compressed []byte, document prebuiltDocument) (string, int64, error) {
	zipped := bytes.NewReader(compressed)
	reader, err := gzip.NewReader(zipped)
	if err != nil {
		return "", 0, err
	}
	defer reader.Close()
	reader.Multistream(false)
	raw, err := io.ReadAll(io.LimitReader(reader, prebuiltDecodedLimit+1))
	if err != nil {
		return "", 0, err
	}
	if len(raw) > prebuiltDecodedLimit || zipped.Len() != 0 || !utf8.Valid(raw) ||
		!matchesPrebuiltHash(raw, document.ChunkFingerprint) {
		return "", 0, fmt.Errorf("decoded size, format or chunk checksum mismatch")
	}
	const rootGroup = "__lazyllm_root__"
	parentGroup := map[string]string{rootGroup: "", "block": rootGroup, "line": "block"}
	groups := make(map[string]string)
	counts := make(map[string]int)
	var roots strings.Builder
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64<<10), prebuiltRecordLimit)
	for scanner.Scan() {
		var node prebuiltNode
		if err := json.Unmarshal(scanner.Bytes(), &node); err != nil {
			return "", 0, err
		}
		parent, knownGroup := parentGroup[node.Group]
		if node.ID == "" || len(node.ID) > 255 || groups[node.ID] != "" || !knownGroup ||
			groups[node.ParentID] != parent || (parent == "" && node.ParentID != "") ||
			node.Number != counts[node.Group]+1 || strings.TrimSpace(node.Text) == "" || strings.ContainsRune(node.Text, 0) {
			return "", 0, fmt.Errorf("invalid node identity, order, hierarchy or text")
		}
		var metadata map[string]json.RawMessage
		if json.Unmarshal(node.Metadata, &metadata) != nil || metadata == nil {
			return "", 0, fmt.Errorf("invalid node metadata")
		}
		if _, hidden := metadata["_lazyllm_null_node"]; hidden {
			return "", 0, fmt.Errorf("prebuilt node metadata cannot hide a text node")
		}
		var source string
		if json.Unmarshal(metadata["source_path"], &source) != nil || source != document.SourcePath {
			return "", 0, fmt.Errorf("node source does not match its manifest")
		}
		groups[node.ID] = node.Group
		counts[node.Group]++
		if node.Group == rootGroup {
			if roots.Len() > 0 {
				roots.WriteString("\n\n")
			}
			roots.WriteString(node.Text)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", 0, err
	}
	if strings.TrimSpace(roots.String()) == "" || counts["block"] == 0 || counts["line"] == 0 ||
		counts["block"]+counts["line"] != document.ChunkCount {
		return "", 0, fmt.Errorf("missing text or unexpected chunk count")
	}
	return roots.String(), int64(len(raw)), nil
}
