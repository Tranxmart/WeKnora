package documentservice

import (
	"context"
	"fmt"
	"sort"

	"github.com/Tencent/WeKnora/internal/txmdocuments"
	"github.com/Tencent/WeKnora/internal/types"
)

// ListResources returns the complete tree from retained document hierarchies.
// Empty directories cannot be discovered through the service-reader API.
func (c *Connector) ListResources(
	ctx context.Context, ds *types.DataSourceConfig, parentID string,
) ([]types.Resource, error) {
	if _, _, err := parseConfig(ds); err != nil {
		return nil, err
	}
	if parentID != "" {
		return []types.Resource{}, nil
	}
	client, err := c.reader()
	if err != nil {
		return nil, err
	}
	nodes := make(map[string]types.Resource)
	for _, id := range []string{"manual", "github", "feishu"} {
		nodes[id] = types.Resource{ExternalID: id, Name: sourceLabels[id], Type: "source"}
	}
	_, sources, _ := parseConfig(&types.DataSourceConfig{})
	visited := make(map[string]bool)
	for token := ""; ; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if visited[token] || len(visited) >= 100000 {
			return nil, invalidResponse("pagination did not converge")
		}
		visited[token] = true
		page, err := client.ListDocuments(ctx, token, sources)
		if err != nil {
			return nil, err
		}
		if err := validatePage(page, token); err != nil {
			return nil, err
		}
		for _, entry := range page.Documents {
			if err := addResources(nodes, entry); err != nil {
				return nil, err
			}
		}
		token = page.NextPageToken
		if token == "" {
			break
		}
	}
	out := make([]types.Resource, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, node)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ParentID != out[j].ParentID {
			return out[i].ParentID < out[j].ParentID
		}
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ExternalID < out[j].ExternalID
	})
	return out, nil
}

func addResources(nodes map[string]types.Resource, entry *txmdocuments.Entry) error {
	mapped, err := mapEntry(entry)
	if err != nil {
		return err
	}
	parent := mapped.sourceID
	keys := make([]string, 0, len(entry.Sources[0].GetHierarchyPath()))
	for _, segment := range entry.Sources[0].GetHierarchyPath() {
		keys = append(keys, segment.GetKey())
		id := directoryID(mapped.sourceID, keys)
		node := nodes[id]
		if node.ExternalID != "" && node.ParentID != parent {
			return invalidResponse("directory has conflicting parents")
		}
		node.ExternalID, node.Name, node.Type, node.ParentID = id, segment.GetDisplayName(), "folder", parent
		nodes[id] = node
		markHasChildren(nodes, parent)
		parent = id
	}
	id := docPrefix + mapped.item.ExternalID
	if _, exists := nodes[id]; exists {
		return invalidResponse(fmt.Sprintf("duplicate document %s", mapped.item.ExternalID))
	}
	nodes[id] = types.Resource{
		ExternalID: id, Name: entry.Document.GetCurrentFilename(), Type: "document", ParentID: parent,
	}
	markHasChildren(nodes, parent)
	return nil
}

func markHasChildren(nodes map[string]types.Resource, id string) {
	node := nodes[id]
	node.HasChildren = true
	nodes[id] = node
}

// ResolveResourceAncestors validates selections; ListResources supplies the full tree.
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
