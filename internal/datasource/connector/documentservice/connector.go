// Package documentservice imports retained immutable Document Service versions.
package documentservice

import (
	"context"
	"fmt"
	"sync"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/txmdocuments"
	"github.com/Tencent/WeKnora/internal/types"
)

var (
	_ datasource.Connector              = (*Connector)(nil)
	_ datasource.FullStreamingConnector = (*Connector)(nil)
	_ datasource.FullSyncWithCursor     = (*Connector)(nil)
)

// Reader allows deterministic tests without opening sockets.
type Reader interface {
	ListDocuments(context.Context, string, []txmdocuments.SourceType) (txmdocuments.Page, error)
	Download(context.Context, *txmdocuments.Entry, int64) ([]byte, error)
	BatchGetDocuments(context.Context, []string) ([]*txmdocuments.Summary, error)
	Scope() string
}

// Connector shares one lazily configured deployment client across data sources.
// Construction and registration never access the network.
type Connector struct {
	once      sync.Once
	client    Reader
	clientErr error
	newClient func() (Reader, error)
}

// NewConnector creates a connector without initializing deployment identity.
func NewConnector() *Connector {
	return &Connector{newClient: newDeploymentClient}
}

func newDeploymentClient() (Reader, error) {
	return txmdocuments.New(txmdocuments.ConfigFromEnv())
}

// NewConnectorWithClient uses an existing reader, principally for tests.
func NewConnectorWithClient(reader Reader) *Connector {
	return &Connector{newClient: func() (Reader, error) { return reader, nil }}
}

func (c *Connector) reader() (Reader, error) {
	c.once.Do(func() {
		factory := c.newClient
		if factory == nil {
			factory = newDeploymentClient
		}
		c.client, c.clientErr = factory()
		if c.clientErr == nil && (c.client == nil || c.client.Scope() == "") {
			c.clientErr = fmt.Errorf("%w: Document Service deployment is not configured", datasource.ErrInvalidConfig)
		}
	})
	return c.client, c.clientErr
}

// Type returns the registered connector type.
func (*Connector) Type() string { return types.ConnectorTypeDocumentService }

// Validate makes one bounded list request; an unsaved draft needs no credentials.
func (c *Connector) Validate(ctx context.Context, ds *types.DataSourceConfig) error {
	_, sources, err := parseConfig(ds)
	if err != nil {
		return err
	}
	client, err := c.reader()
	if err != nil {
		return err
	}
	page, err := client.ListDocuments(ctx, "", sources)
	if err != nil {
		return err
	}
	return validatePage(page, "")
}

type collector struct{ items []types.FetchedItem }

func (h *collector) Emit(_ context.Context, item types.FetchedItem) error {
	h.items = append(h.items, item)
	return nil
}
func (*collector) Checkpoint(context.Context, *types.SyncCursor) error { return nil }

// FetchAll collects every selected document without a previous deletion baseline.
func (c *Connector) FetchAll(
	ctx context.Context, ds *types.DataSourceConfig, ids []string,
) ([]types.FetchedItem, error) {
	items, _, err := c.FetchAllFromCursor(ctx, ds, ids, nil)
	return items, err
}

// FetchAllFromCursor collects a full scan while retaining known deletion candidates.
func (c *Connector) FetchAllFromCursor(
	ctx context.Context, ds *types.DataSourceConfig, ids []string, old *types.SyncCursor,
) ([]types.FetchedItem, *types.SyncCursor, error) {
	if ds == nil {
		return nil, nil, datasource.ErrInvalidConfig
	}
	configCopy := *ds
	configCopy.ResourceIDs = ids
	h := &collector{}
	next, err := c.FetchFullStream(ctx, &configCopy, old, h)
	return h.items, next, err
}

// FetchIncremental collects changed documents and explicit deletions.
func (c *Connector) FetchIncremental(
	ctx context.Context, ds *types.DataSourceConfig, old *types.SyncCursor,
) ([]types.FetchedItem, *types.SyncCursor, error) {
	h := &collector{}
	next, err := c.FetchStream(ctx, ds, old, h)
	return h.items, next, err
}

// FetchStream downloads and acknowledges one changed document at a time.
func (c *Connector) FetchStream(
	ctx context.Context, ds *types.DataSourceConfig, old *types.SyncCursor, h datasource.StreamHandler,
) (*types.SyncCursor, error) {
	return c.runSync(ctx, ds, old, h, false)
}

// FetchFullStream re-fetches every document while preserving the deletion baseline.
func (c *Connector) FetchFullStream(
	ctx context.Context, ds *types.DataSourceConfig, old *types.SyncCursor, h datasource.StreamHandler,
) (*types.SyncCursor, error) {
	return c.runSync(ctx, ds, old, h, true)
}
