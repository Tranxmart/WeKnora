package documentservice

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/txmdocuments"
	"github.com/Tencent/WeKnora/internal/types"
)

var sourceTypes = map[string]txmdocuments.SourceType{
	"manual": txmdocuments.SourceManual,
	"github": txmdocuments.SourceGitHub,
	"feishu": txmdocuments.SourceFeishu,
}

var sourceLabels = map[string]string{"manual": "Manual", "github": "GitHub", "feishu": "Feishu"}

var allSourceIDs = []string{"feishu", "github", "manual"}

// Besides whole sources, a selection may name directories ("dir:<uuid>",
// including descendants) and single documents ("doc:<uuid>").
const (
	dirPrefix = "dir:"
	docPrefix = "doc:"
)

func validSelection(id string) bool {
	if _, ok := sourceTypes[id]; ok {
		return true
	}
	for _, prefix := range []string{dirPrefix, docPrefix} {
		if rest, ok := strings.CutPrefix(id, prefix); ok {
			return canonicalUUID(rest)
		}
	}
	return false
}

// listedSources names the sources to enumerate. Directory and document
// selections need every source: only the listed entry says where it lives.
func listedSources(selected []string) []string {
	out := make([]string, 0, len(selected))
	for _, id := range selected {
		if _, ok := sourceTypes[id]; !ok {
			return allSourceIDs
		}
		out = append(out, id)
	}
	return out
}

// inScope reports whether a listed document is selected by source, by itself,
// or by any directory among its ancestors.
func inScope(selected []string, m mappedEntry, hierarchy []*txmdocuments.HierarchySegment) bool {
	if slices.Contains(selected, m.sourceID) || slices.Contains(selected, docPrefix+m.item.ExternalID) {
		return true
	}
	for _, segment := range hierarchy {
		if slices.Contains(selected, dirPrefix+segment.GetKey()) {
			return true
		}
	}
	return false
}

func parseConfig(ds *types.DataSourceConfig) ([]string, []txmdocuments.SourceType, error) {
	if ds == nil {
		return nil, nil, datasource.ErrInvalidConfig
	}
	if len(ds.Credentials) != 0 {
		return nil, nil, fmt.Errorf("%w: Document Service credentials are deployment managed",
			datasource.ErrInvalidConfig)
	}
	// No endpoint, token, or other per-data-source overrides are supported.
	if len(ds.Settings) != 0 {
		return nil, nil, fmt.Errorf("%w: Document Service does not accept per-data-source settings",
			datasource.ErrInvalidConfig)
	}
	ids := ds.ResourceIDs
	if len(ids) == 0 {
		ids = allSourceIDs
	}
	selected := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !validSelection(id) {
			return nil, nil, fmt.Errorf("%w: invalid Document Service source %q", datasource.ErrInvalidConfig, id)
		}
		selected[id] = true
	}
	ids = make([]string, 0, len(selected))
	for id := range selected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	listed := listedSources(ids)
	enums := make([]txmdocuments.SourceType, len(listed))
	for i, id := range listed {
		enums[i] = sourceTypes[id]
	}
	return ids, enums, nil
}

func sourceID(source txmdocuments.SourceType) string {
	for id, value := range sourceTypes {
		if value == source {
			return id
		}
	}
	return ""
}
