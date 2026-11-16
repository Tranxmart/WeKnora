package txmdocuments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"

	"connectrpc.com/connect"
	api "github.com/Tencent/WeKnora/internal/txmdocuments/gen/document_service_api"
	"google.golang.org/protobuf/proto"
)

// Download pins the listed immutable version and verifies the complete bytes.
// Signed URLs and identity credentials are never included in returned errors.
func (c *Client) Download(ctx context.Context, entry *Entry, maxBytes int64) ([]byte, error) {
	if err := validateEntry(entry, maxBytes); err != nil {
		return nil, err
	}
	version := entry.GetCurrentVersion()
	for attempt := 0; attempt < 2; attempt++ {
		response, err := c.rpc.CreateDocumentDownload(ctx, connect.NewRequest(&api.CreateDocumentDownloadRequest{
			DocumentId: proto.String(
				version.GetDocumentId(),
			), DocumentVersionId: proto.String(version.GetDocumentVersionId()),
		}))
		if err != nil {
			return nil, rpcError("CreateDocumentDownload", err)
		}
		capability := response.Msg
		if !sameVersion(version, capability.GetDocumentVersion()) {
			return nil, errors.New("document service download version metadata does not match listed version")
		}
		expires := capability.GetExpiresAt()
		if expires == nil || expires.CheckValid() != nil {
			return nil, errors.New("document service download expiration is missing or invalid")
		}
		if !c.now().Before(expires.AsTime()) {
			if attempt == 0 {
				continue
			}
			return nil, errors.New("document service download capability expired")
		}
		body, expired, err := c.downloadCapability(ctx, capability.GetDownloadUrl(), version, maxBytes)
		if expired && attempt == 0 {
			continue
		}
		return body, err
	}
	return nil, errors.New("document service download capability expired")
}

func validateEntry(entry *Entry, maxBytes int64) error {
	if entry == nil || entry.GetDocument() == nil || entry.GetCurrentVersion() == nil {
		return errors.New("document service listed document or version is missing")
	}
	doc, version := entry.GetDocument(), entry.GetCurrentVersion()
	if doc.GetDocumentId() == "" || version.GetDocumentVersionId() == "" ||
		version.GetDocumentId() != doc.GetDocumentId() ||
		doc.GetCurrentDocumentVersionId() != version.GetDocumentVersionId() {
		return errors.New("document service listed document and version identities do not match")
	}
	if version.SizeBytes == nil || version.GetSizeBytes() < 0 || maxBytes < 0 || version.GetSizeBytes() > maxBytes ||
		version.GetSizeBytes() == math.MaxInt64 {
		return errors.New("document service document size is missing, invalid or exceeds the download limit")
	}
	if !validDigest(version.GetBlobDigest()) {
		return errors.New("document service document digest is missing or invalid")
	}
	return nil
}

func validDigest(value string) bool {
	if len(value) != 71 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, c := range value[7:] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func sameVersion(a, b *Version) bool {
	return b != nil && b.SizeBytes != nil && a.GetDocumentId() == b.GetDocumentId() &&
		a.GetDocumentVersionId() == b.GetDocumentVersionId() &&
		a.GetBlobDigest() == b.GetBlobDigest() &&
		a.GetSizeBytes() == b.GetSizeBytes()
}

func (c *Client) downloadCapability(
	ctx context.Context,
	rawURL string,
	version *Version,
	maxBytes int64,
) ([]byte, bool, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u == nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil ||
		u.Fragment != "" ||
		u.Opaque != "" {
		return nil, false, errors.New("document service download capability URL is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, false, errors.New("document service download request is invalid")
	}
	request.Header.Set("Accept-Encoding", "identity")
	response, err := c.download.Do(request)
	if err != nil {
		return nil, false, errors.New("document service download request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, true, errors.New("document service download capability was rejected")
	}
	if response.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("document service download returned HTTP %d", response.StatusCode)
	}
	encoding := response.Header.Get("Content-Encoding")
	if response.Uncompressed || (encoding != "" && !strings.EqualFold(encoding, "identity")) ||
		response.Header.Get("Content-Range") != "" {
		return nil, false, errors.New("document service download must return complete identity-encoded content")
	}
	if response.ContentLength >= 0 && response.ContentLength != version.GetSizeBytes() {
		return nil, false, errors.New("document service download content length does not match listed version")
	}
	// The expected length is already bounded by maxBytes. Reading one extra byte
	// detects chunked oversized responses without allocating unbounded content.
	body, err := io.ReadAll(io.LimitReader(response.Body, version.GetSizeBytes()+1))
	if err != nil {
		return nil, false, errors.New("document service download content could not be read completely")
	}
	if int64(len(body)) > maxBytes || int64(len(body)) != version.GetSizeBytes() {
		return nil, false, errors.New("document service download length does not match listed version")
	}
	digest := sha256.Sum256(body)
	if "sha256:"+hex.EncodeToString(digest[:]) != version.GetBlobDigest() {
		return nil, false, errors.New("document service download digest does not match listed version")
	}
	return body, false, nil
}
