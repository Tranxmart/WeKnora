package documentservice

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
)

const cursorSchemaVersion = 1

const (
	phaseScan      = "scan"
	phaseDeletions = "deletions"
	phaseCompleted = "completed"
)

type inventoryEntry struct {
	AcceptedFingerprint string `json:"acceptedFingerprint"`
	AcceptedGeneration  uint64 `json:"acceptedGeneration,omitempty"`
	LastSeenGeneration  uint64 `json:"lastSeenGeneration"`
	SourceID            string `json:"sourceID"`
	Title               string `json:"title,omitempty"`
}

// Inventory is only advanced after ingestion accepts an immutable version.
// Deletion candidates require a complete scan and explicit readable tombstones.
type cursor struct {
	SchemaVersion     int                       `json:"schemaVersion"`
	ServiceScope      string                    `json:"serviceScope"`
	SelectedSourceIDs []string                  `json:"selectedSourceIDs"`
	Phase             string                    `json:"phase"`
	Generation        uint64                    `json:"generation"`
	NextPage          string                    `json:"nextPage,omitempty"`
	Documents         map[string]inventoryEntry `json:"documents"`
	DeletionIDs       []string                  `json:"deletionIDs,omitempty"`
	DeletionIndex     int                       `json:"deletionIndex,omitempty"`
	FullScan          bool                      `json:"fullScan,omitempty"`
}

func invalidCursor() error {
	return fmt.Errorf("%w: invalid or unsupported Document Service cursor", datasource.ErrInvalidConfig)
}

func decodeCursor(old *types.SyncCursor) (cursor, error) {
	c := cursor{SchemaVersion: cursorSchemaVersion, Documents: make(map[string]inventoryEntry)}
	if old == nil || len(old.ConnectorCursor) == 0 {
		return c, nil
	}
	raw, err := json.Marshal(old.ConnectorCursor)
	if err != nil || json.Unmarshal(raw, &c) != nil || c.SchemaVersion != cursorSchemaVersion ||
		c.ServiceScope == "" || c.Generation == 0 {
		return cursor{}, invalidCursor()
	}
	if c.Phase != phaseScan && c.Phase != phaseDeletions && c.Phase != phaseCompleted ||
		len(c.NextPage) > 2048 || c.DeletionIndex < 0 || c.DeletionIndex > len(c.DeletionIDs) {
		return cursor{}, invalidCursor()
	}
	if c.Phase != phaseScan && c.NextPage != "" ||
		c.Phase != phaseDeletions && (len(c.DeletionIDs) != 0 || c.DeletionIndex != 0) {
		return cursor{}, invalidCursor()
	}
	if len(c.SelectedSourceIDs) == 0 || !slices.IsSorted(c.SelectedSourceIDs) {
		return cursor{}, invalidCursor()
	}
	for i, id := range c.SelectedSourceIDs {
		if _, ok := sourceTypes[id]; !ok || i > 0 && c.SelectedSourceIDs[i-1] == id {
			return cursor{}, invalidCursor()
		}
	}
	for id, entry := range c.Documents {
		if !canonicalUUID(id) || !hexDigest(entry.AcceptedFingerprint) ||
			entry.LastSeenGeneration > c.Generation || entry.AcceptedGeneration > c.Generation {
			return cursor{}, invalidCursor()
		}
		if _, ok := sourceTypes[entry.SourceID]; !ok {
			return cursor{}, invalidCursor()
		}
	}
	for i, id := range c.DeletionIDs {
		known, ok := c.Documents[id]
		if !ok || i > 0 && c.DeletionIDs[i-1] >= id || known.LastSeenGeneration == c.Generation ||
			!slices.Contains(c.SelectedSourceIDs, known.SourceID) {
			return cursor{}, invalidCursor()
		}
	}
	if c.Documents == nil {
		c.Documents = make(map[string]inventoryEntry)
	}
	return c, nil
}

func prepareCursor(old *types.SyncCursor, scope string, selected []string, full bool) (cursor, error) {
	c, err := decodeCursor(old)
	if err != nil {
		return cursor{}, err
	}
	if c.ServiceScope != "" && c.ServiceScope != scope {
		return cursor{}, fmt.Errorf(
			"%w: Document Service deployment or resource scope changed; create a new data source",
			datasource.ErrInvalidConfig)
	}
	c.ServiceScope = scope
	restart := c.Phase == "" || c.Phase == phaseCompleted ||
		!slices.Equal(c.SelectedSourceIDs, selected) || full && !c.FullScan
	if restart {
		if c.Generation == math.MaxUint64 {
			return cursor{}, invalidCursor()
		}
		c.Generation++
		c.SelectedSourceIDs = slices.Clone(selected)
		c.Phase, c.NextPage = phaseScan, ""
		c.DeletionIDs, c.DeletionIndex = nil, 0
		c.FullScan = full
	}
	return c, nil
}

// Round-tripping gives handlers a complete independent checkpoint snapshot.
func (c cursor) toSyncCursor() *types.SyncCursor {
	raw, _ := json.Marshal(c)
	fields := make(map[string]interface{})
	_ = json.Unmarshal(raw, &fields)
	return &types.SyncCursor{LastSyncTime: time.Now().UTC(), ConnectorCursor: fields}
}
