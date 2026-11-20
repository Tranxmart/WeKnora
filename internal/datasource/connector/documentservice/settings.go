package documentservice

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

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

// Manual directories retain their UUID selection IDs. Provider paths include
// the source and ordered stable keys; display labels never identify a selection.
const (
	dirPrefix  = "dir:"
	docPrefix  = "doc:"
	pathPrefix = "path:"
	// Document Service bounds hierarchy depth and key size. Allow for JSON
	// escaping and base64 expansion before allocating a decoded selection.
	maxHierarchyDepth    = 64
	maxHierarchyKeyBytes = 512
	maxDirectoryIDBytes  = maxHierarchyDepth*(6*maxHierarchyKeyBytes+3)*4/3 + 32
)

func directoryID(source string, keys []string) string {
	if source == "manual" && canonicalUUID(keys[len(keys)-1]) {
		return dirPrefix + keys[len(keys)-1]
	}
	raw, _ := json.Marshal(keys)
	return pathPrefix + source + ":" + base64.RawURLEncoding.EncodeToString(raw)
}

func parseDirectoryID(id string) (string, []string, bool) {
	rest, ok := strings.CutPrefix(id, pathPrefix)
	if !ok || len(rest) > maxDirectoryIDBytes {
		return "", nil, false
	}
	source, encoded, ok := strings.Cut(rest, ":")
	if _, known := sourceTypes[source]; !ok || !known {
		return "", nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	var keys []string
	if err != nil || json.Unmarshal(raw, &keys) != nil || len(keys) == 0 || len(keys) > maxHierarchyDepth {
		return "", nil, false
	}
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if strings.TrimSpace(key) == "" || len(key) > maxHierarchyKeyBytes || !utf8.ValidString(key) ||
			strings.ContainsRune(key, 0) || seen[key] {
			return "", nil, false
		}
		seen[key] = true
	}
	// Require one canonical encoding, including the UUID form for manual paths.
	return source, keys, directoryID(source, keys) == id
}

func validSelection(id string) bool {
	if _, ok := sourceTypes[id]; ok {
		return true
	}
	for _, prefix := range []string{dirPrefix, docPrefix} {
		if rest, ok := strings.CutPrefix(id, prefix); ok {
			return canonicalUUID(rest)
		}
	}
	_, _, ok := parseDirectoryID(id)
	return ok
}

// UUID selections need every source; provider paths carry their source filter.
func listedSources(selected []string) []string {
	selectedSources := make(map[string]bool)
	for _, id := range selected {
		if _, ok := sourceTypes[id]; ok {
			selectedSources[id] = true
		} else if source, _, ok := parseDirectoryID(id); ok {
			selectedSources[source] = true
		} else {
			return allSourceIDs
		}
	}
	out := make([]string, 0, len(selectedSources))
	for _, source := range allSourceIDs {
		if selectedSources[source] {
			out = append(out, source)
		}
	}
	return out
}

// inScope reports whether a listed document is selected by source, by itself,
// or by any directory among its ancestors.
func inScope(selected []string, m mappedEntry, hierarchy []*txmdocuments.HierarchySegment) bool {
	if slices.Contains(selected, m.sourceID) || slices.Contains(selected, docPrefix+m.item.ExternalID) {
		return true
	}
	keys := make([]string, 0, len(hierarchy))
	for _, segment := range hierarchy {
		keys = append(keys, segment.GetKey())
		if slices.Contains(selected, dirPrefix+segment.GetKey()) {
			return true
		}
		if slices.Contains(selected, directoryID(m.sourceID, keys)) {
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
