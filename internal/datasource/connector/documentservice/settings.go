package documentservice

import (
	"fmt"
	"sort"

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
		ids = []string{"manual", "github", "feishu"}
	}
	selected := make(map[string]bool, len(ids))
	for _, id := range ids {
		if _, ok := sourceTypes[id]; !ok {
			return nil, nil, fmt.Errorf("%w: invalid Document Service source %q", datasource.ErrInvalidConfig, id)
		}
		selected[id] = true
	}
	ids = make([]string, 0, len(selected))
	for id := range selected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	enums := make([]txmdocuments.SourceType, len(ids))
	for i, id := range ids {
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
