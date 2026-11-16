package documentservice

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types"
)

// ListResources exposes source filters, not the person-only directory API.
func (*Connector) ListResources(
	_ context.Context, ds *types.DataSourceConfig, parentID string,
) ([]types.Resource, error) {
	if _, _, err := parseConfig(ds); err != nil {
		return nil, err
	}
	if parentID != "" {
		return []types.Resource{}, nil
	}
	out := make([]types.Resource, 0, 3)
	for _, id := range []string{"manual", "github", "feishu"} {
		out = append(out, types.Resource{ExternalID: id, Name: sourceLabels[id], Type: "source"})
	}
	return out, nil
}

// ResolveResourceAncestors validates flat source IDs; they have no ancestors.
func (*Connector) ResolveResourceAncestors(
	_ context.Context, ds *types.DataSourceConfig, ids []string,
) ([]string, error) {
	if ds == nil {
		_, _, err := parseConfig(nil)
		return nil, err
	}
	configCopy := *ds
	configCopy.ResourceIDs = ids
	_, _, err := parseConfig(&configCopy)
	return []string{}, err
}
