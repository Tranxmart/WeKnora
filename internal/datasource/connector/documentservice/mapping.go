package documentservice

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/txmdocuments"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// FingerprintMetadataKey is shared with ingestion's replay protection.
const FingerprintMetadataKey = "document_service_fingerprint"

func canonicalUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

func hexDigest(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func invalidResponse(detail string) error {
	return fmt.Errorf("%w: invalid Document Service response: %s", datasource.ErrFetchFailed, detail)
}

// sanitizeLabel keeps display names as one path component. Stable hierarchy
// keys are never sanitized and are preserved separately in metadata.
func sanitizeLabel(value string) string {
	value = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || unicode.IsControl(r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(value))
	if value == "" || value == "." || value == ".." {
		return "_"
	}
	return value
}

func volatileMetadataKey(key string) bool {
	key = strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
	return key == "lastseen" || key == "lastseenat" || key == "downloadurl" || key == "downloadurlexpiresat"
}

// Capability URLs and observation timestamps have no semantic significance.
func semanticMetadata(value interface{}) interface{} {
	switch value := value.(type) {
	case map[string]interface{}:
		clean := make(map[string]interface{}, len(value))
		for key, field := range value {
			if !volatileMetadataKey(key) {
				clean[key] = semanticMetadata(field)
			}
		}
		return clean
	case []interface{}:
		clean := make([]interface{}, len(value))
		for i, field := range value {
			clean[i] = semanticMetadata(field)
		}
		return clean
	default:
		return value
	}
}

func metadataMap(value *structpb.Struct) map[string]interface{} {
	if value == nil {
		return map[string]interface{}{}
	}
	return semanticMetadata(value.AsMap()).(map[string]interface{})
}

func timestampMetadata(metadata map[string]string, key string, value *timestamppb.Timestamp) error {
	if value == nil {
		return nil
	}
	if err := value.CheckValid(); err != nil {
		return invalidResponse("invalid timestamp")
	}
	metadata[key] = value.AsTime().UTC().Format(time.RFC3339Nano)
	return nil
}

type mappedEntry struct {
	item        types.FetchedItem
	fingerprint string
	sourceID    string
	size        int64
}

func mapEntry(entry *txmdocuments.Entry) (mappedEntry, error) {
	if entry == nil || entry.Document == nil || entry.CurrentVersion == nil ||
		len(entry.Sources) != 1 || entry.Sources[0] == nil {
		return mappedEntry{}, invalidResponse("document, current version, and one canonical source are required")
	}
	doc, version, source := entry.Document, entry.CurrentVersion, entry.Sources[0]
	id, versionID := doc.GetDocumentId(), version.GetDocumentVersionId()
	if !canonicalUUID(id) || !canonicalUUID(versionID) || !canonicalUUID(source.GetDocumentSourceRefId()) ||
		version.GetDocumentId() != id || source.GetDocumentId() != id ||
		doc.GetCurrentDocumentVersionId() != versionID {
		return mappedEntry{}, invalidResponse("document/version/source identity mismatch")
	}
	if doc.GetStatus() != txmdocuments.StatusActive || strings.TrimSpace(doc.GetCurrentFilename()) == "" ||
		strings.TrimSpace(version.GetFilenameAtUpload()) == "" || version.GetVersionNumber() < 1 {
		return mappedEntry{}, invalidResponse("invalid active document metadata")
	}
	digest := version.GetBlobDigest()
	if !strings.HasPrefix(digest, "sha256:") || !hexDigest(strings.TrimPrefix(digest, "sha256:")) ||
		version.SizeBytes == nil || version.GetSizeBytes() < 0 {
		return mappedEntry{}, invalidResponse("missing or invalid immutable digest/size")
	}
	idSource := sourceID(source.GetSourceType())
	if idSource == "" || strings.TrimSpace(source.GetSourceKey()) == "" ||
		strings.ContainsRune(source.GetSourceKey(), 0) ||
		version.GetSourceType() != source.GetSourceType() || version.GetSourceKey() != source.GetSourceKey() ||
		(int32(source.GetSourceState()) != 1 && int32(source.GetSourceState()) != 2) {
		return mappedEntry{}, invalidResponse("invalid canonical source owner")
	}
	hierarchy := make([]map[string]interface{}, 0, len(source.GetHierarchyPath()))
	labels := []string{sourceLabels[idSource]}
	keys := make(map[string]bool)
	for _, segment := range source.GetHierarchyPath() {
		if segment == nil || strings.TrimSpace(segment.GetKey()) == "" ||
			strings.ContainsRune(segment.GetKey(), 0) || keys[segment.GetKey()] ||
			strings.TrimSpace(segment.GetDisplayName()) == "" ||
			(int32(segment.GetKind()) != 1 && int32(segment.GetKind()) != 2) {
			return mappedEntry{}, invalidResponse("invalid source hierarchy")
		}
		keys[segment.GetKey()] = true
		hierarchy = append(hierarchy, map[string]interface{}{
			"key": segment.GetKey(), "display_name": segment.GetDisplayName(), "kind": int32(segment.GetKind()),
		})
		labels = append(labels, sanitizeLabel(segment.GetDisplayName()))
	}
	filename := sanitizeLabel(doc.GetCurrentFilename())
	labels = append(labels, filename)
	currentMetadata := metadataMap(source.GetSourceMetadata())
	versionMetadata := metadataMap(version.GetSourceMetadata())
	semantic := map[string]interface{}{
		"document_version_id": versionID, "blob_digest": digest, "size_bytes": version.GetSizeBytes(),
		"current_filename": doc.GetCurrentFilename(), "filename_at_upload": version.GetFilenameAtUpload(),
		"source_type": idSource, "source_key": source.GetSourceKey(), "source_revision": version.GetSourceRevision(),
		"source_last_revision": source.GetLastRevision(),
		"source_event_key":     version.GetSourceEventKey(), "source_metadata": currentMetadata,
		"version_source_metadata": versionMetadata, "hierarchy_path": hierarchy,
	}
	raw, err := json.Marshal(semantic)
	if err != nil {
		return mappedEntry{}, invalidResponse("invalid source metadata")
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(raw))
	hierarchyJSON, _ := json.Marshal(hierarchy)
	currentJSON, _ := json.Marshal(currentMetadata)
	versionJSON, _ := json.Marshal(versionMetadata)
	metadata := map[string]string{
		"channel": types.ChannelDocumentService, FingerprintMetadataKey: fingerprint,
		"document_id": id, "document_version_id": versionID, "blob_digest": digest,
		"filename_at_upload": version.GetFilenameAtUpload(), "source_type": idSource,
		"source_key": source.GetSourceKey(), "source_revision": version.GetSourceRevision(),
		"source_last_revision": source.GetLastRevision(),
		"source_event_key":     version.GetSourceEventKey(), "hierarchy_path": string(hierarchyJSON),
		"source_metadata": string(currentJSON), "version_source_metadata": string(versionJSON),
	}
	if err := timestampMetadata(metadata, "document_service_updated_at", doc.GetUpdatedAt()); err != nil {
		return mappedEntry{}, err
	}
	if err := timestampMetadata(metadata, "document_service_created_at", doc.GetCreatedAt()); err != nil {
		return mappedEntry{}, err
	}
	originalURL := ""
	for _, sourceMetadata := range []map[string]interface{}{currentMetadata, versionMetadata} {
		for _, key := range []string{"original_url", "originalUrl", "url"} {
			if value, ok := sourceMetadata[key].(string); ok {
				parsed, err := url.Parse(value)
				if err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") &&
					parsed.Host != "" && parsed.User == nil {
					originalURL = value
					metadata["original_url"] = value
					break
				}
			}
		}
		if originalURL != "" {
			break
		}
	}
	return mappedEntry{item: types.FetchedItem{
		ExternalID: id, Title: strings.Join(labels, "/"), FileName: strings.Join(labels, "/"),
		URL: originalURL, ContentType: version.GetDetectedMediaType(), SourceResourceID: idSource,
		Metadata: metadata, RetryOnFailure: true,
	}, fingerprint: fingerprint, sourceID: idSource, size: version.GetSizeBytes()}, nil
}

func (m mappedEntry) rejection(limit int64) *types.SyncItemError {
	var code, message string
	switch {
	case !utils.IsSupportedImportExtension(path.Ext(m.item.FileName)):
		code, message = "unsupported_file_type", "Unsupported file type"
	case m.size == 0:
		code, message = "document_service_empty_file", "Document Service file is empty"
	case m.size > limit:
		code, message = "file_too_large", "Document Service file exceeds the maximum import size"
	default:
		return nil
	}
	return &types.SyncItemError{Title: m.item.Title, Code: code, Message: message}
}

func validatePage(page txmdocuments.Page, currentToken string) error {
	if len(page.Documents) > 100 || len(page.NextPageToken) > 2048 ||
		page.NextPageToken != "" && page.NextPageToken == currentToken {
		return invalidResponse("invalid list pagination")
	}
	seen := make(map[string]bool, len(page.Documents))
	for _, entry := range page.Documents {
		mapped, err := mapEntry(entry)
		if err != nil {
			return err
		}
		if seen[mapped.item.ExternalID] {
			return invalidResponse("duplicate document identity")
		}
		seen[mapped.item.ExternalID] = true
	}
	return nil
}
