// Package txmdocuments reads retained documents using identity-issued service
// credentials. Canonical protobufs and Connect bindings are vendored in gen.
package txmdocuments

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	api "github.com/Tencent/WeKnora/internal/txmdocuments/gen/document_service_api"
	"github.com/Tencent/WeKnora/internal/txmdocuments/gen/document_service_api/document_service_apiconnect"
	models "github.com/Tencent/WeKnora/internal/txmdocuments/gen/document_service_models"
	"github.com/Tencent/WeKnora/internal/utils"
	"google.golang.org/protobuf/proto"
)

// Entry is one retained document with its current immutable version and owning source.
type (
	Entry = api.DocumentListEntry
	// Version is the canonical immutable document version.
	Version = models.DocumentVersion
	// SourceRef identifies the canonical provider owner and current hierarchy.
	SourceRef = models.DocumentSourceRef
	// SourceType identifies a supported provider or manual upload.
	SourceType = models.SourceType_Enum
	// Summary carries canonical access and lifecycle status for a document.
	Summary = api.DocumentSummary
	// Document is the canonical logical document identity.
	Document = models.Document
	// HierarchySegment is one stable ancestor key and its current display name.
	HierarchySegment = models.HierarchySegment
)

// Canonical source types, logical document states, and readable access.
const (
	SourceManual      = models.SourceType_MANUAL_UPLOAD
	SourceGitHub      = models.SourceType_GITHUB
	SourceFeishu      = models.SourceType_FEISHU_DOCUMENT
	StatusDeleted     = models.Document_DELETED
	StatusActive      = models.Document_ACTIVE
	AccessReadable    = api.Access_READABLE
	defaultServiceURL = "http://document-service.service.svc.cluster.local:8080"
	defaultResource   = "https://document-service.txm.internal"
)

// Config names trusted service endpoints and the projected service-account JWT.
// IdentityAudience must match the audience of the deployed projected volume.
// It is not an OAuth audience parameter; identity selects the target by Resource.
type Config struct {
	IdentityServiceURL string
	IdentityTokenFile  string
	IdentityAudience   string
	ServiceURL         string
	Resource           string
}

// ConfigFromEnv reads deployment identity configuration and service endpoint defaults.
func ConfigFromEnv() Config {
	return Config{
		IdentityServiceURL: os.Getenv("IDENTITY_SERVICE_URL"),
		IdentityTokenFile:  os.Getenv("IDENTITY_TOKEN_FILE"),
		IdentityAudience:   os.Getenv("IDENTITY_AUDIENCE"),
		ServiceURL:         envDefault("DOCUMENT_SERVICE_URL", defaultServiceURL),
		Resource:           envDefault("DOCUMENT_SERVICE_RESOURCE", defaultResource),
	}
}

func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// Page contains at most 100 retained documents and an opaque continuation token.
type Page struct {
	Documents     []*Entry
	NextPageToken string
}

// Client deliberately exposes only the three RPCs authorized for this consumer.
// Identity, RPC and uncredentialed object downloads have separate HTTP clients.
type Client struct {
	rpc      document_service_apiconnect.DocumentServiceClient
	tokens   *tokenCache
	download *http.Client
	scope    string
	now      func() time.Time
}

// New validates configuration without reading credentials or doing network I/O.
func New(cfg Config) (*Client, error) { return newClient(cfg, dependencies{}) }

type dependencies struct {
	identityTransport http.RoundTripper
	rpcTransport      http.RoundTripper
	downloadClient    *http.Client
	readFile          func(string) ([]byte, error)
	now               func() time.Time
}

func newClient(cfg Config, deps dependencies) (*Client, error) {
	if cfg.ServiceURL == "" {
		cfg.ServiceURL = defaultServiceURL
	}
	if cfg.Resource == "" {
		cfg.Resource = defaultResource
	}
	for _, field := range []struct{ name, value string }{
		{"IDENTITY_SERVICE_URL", cfg.IdentityServiceURL},
		{"IDENTITY_TOKEN_FILE", cfg.IdentityTokenFile},
		{"IDENTITY_AUDIENCE", cfg.IdentityAudience},
	} {
		if strings.TrimSpace(field.value) == "" {
			return nil, fmt.Errorf("document service requires %s", field.name)
		}
	}
	identityURL, err := trustedURL(cfg.IdentityServiceURL)
	if err != nil {
		return nil, errors.New(
			"IDENTITY_SERVICE_URL must be an absolute http(s) URL without credentials, query or fragment",
		)
	}
	serviceURL, err := trustedURL(cfg.ServiceURL)
	if err != nil {
		return nil, errors.New(
			"DOCUMENT_SERVICE_URL must be an absolute http(s) URL without credentials, query or fragment",
		)
	}
	resource, err := trustedURL(cfg.Resource)
	if err != nil || resource.Scheme != "https" {
		return nil, errors.New(
			"DOCUMENT_SERVICE_RESOURCE must be an absolute https URL without credentials, query or fragment",
		)
	}
	identityURL.Path = strings.TrimRight(identityURL.Path, "/") + "/token"
	identityURL.RawPath = ""
	if deps.now == nil {
		deps.now = time.Now
	}
	if deps.readFile == nil {
		deps.readFile = os.ReadFile
	}
	tokens := &tokenCache{
		client:    originClient(identityURL, deps.identityTransport, nil),
		endpoint:  identityURL.String(),
		resource:  cfg.Resource,
		tokenFile: cfg.IdentityTokenFile,
		readFile:  deps.readFile,
		now:       deps.now,
	}
	baseURL := strings.TrimRight(serviceURL.String(), "/")
	allowed := map[string]bool{}
	for _, procedure := range []string{
		document_service_apiconnect.DocumentServiceListDocumentsProcedure,
		document_service_apiconnect.DocumentServiceCreateDocumentDownloadProcedure,
		document_service_apiconnect.DocumentServiceBatchGetDocumentsProcedure,
	} {
		allowed[strings.TrimRight(serviceURL.Path, "/")+procedure] = true
	}
	rpcHTTP := originClient(serviceURL, deps.rpcTransport, allowed)
	if deps.downloadClient == nil {
		cfg := utils.DefaultSSRFSafeHTTPClientConfig()
		cfg.DisableCompression = true
		deps.downloadClient = utils.NewSSRFSafeHTTPClient(cfg)
	}
	// Copy caller-provided clients as well, so no cookie jar or redirect policy
	// can leak credentials or transform the exact signed capability URL.
	download := *deps.downloadClient
	download.Jar = nil
	download.CheckRedirect = noRedirect
	return &Client{
		rpc: document_service_apiconnect.NewDocumentServiceClient(
			rpcHTTP,
			baseURL,
			connect.WithInterceptors(authInterceptor{tokens}),
		),
		tokens: tokens, download: &download, now: deps.now,
		scope: baseURL + "|" + cfg.Resource,
	}, nil
}

func trustedURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" ||
		u.ForceQuery ||
		u.Fragment != "" ||
		u.Opaque != "" {
		return nil, errors.New("invalid trusted endpoint")
	}
	return u, nil
}

// Scope identifies the service endpoint and resource used to bind synchronization cursors.
func (c *Client) Scope() string { return c.scope }

// ListDocuments lists one bounded page of active retained documents.
func (c *Client) ListDocuments(ctx context.Context, pageToken string, sources []SourceType) (Page, error) {
	response, err := c.rpc.ListDocuments(
		ctx,
		connect.NewRequest(
			&api.ListDocumentsRequest{
				PageSize:    proto.Int32(100),
				PageToken:   proto.String(pageToken),
				SourceTypes: sources,
			},
		),
	)
	if err != nil {
		return Page{}, rpcError("ListDocuments", err)
	}
	return Page{Documents: response.Msg.GetDocuments(), NextPageToken: response.Msg.GetNextPageToken()}, nil
}

// BatchGetDocuments reads access and lifecycle summaries for at most 100 distinct IDs.
func (c *Client) BatchGetDocuments(ctx context.Context, ids []string) ([]*Summary, error) {
	if len(ids) > 100 {
		return nil, errors.New("document service BatchGetDocuments accepts at most 100 IDs")
	}
	response, err := c.rpc.BatchGetDocuments(ctx, connect.NewRequest(&api.BatchGetDocumentsRequest{DocumentIds: ids}))
	if err != nil {
		return nil, rpcError("BatchGetDocuments", err)
	}
	return response.Msg.GetDocuments(), nil
}

func rpcError(method string, err error) error {
	// Connect errors may contain server response details or transport URLs.
	return connect.NewError(connect.CodeOf(err), fmt.Errorf("document service %s failed", method))
}
