package documentservice

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/txmdocuments"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
)

type syncRun struct {
	ctx     context.Context
	client  Reader
	h       datasource.StreamHandler
	cur     cursor
	sources []txmdocuments.SourceType
	limit   int64
}

func (c *Connector) runSync(
	ctx context.Context, ds *types.DataSourceConfig, old *types.SyncCursor, h datasource.StreamHandler, full bool,
) (*types.SyncCursor, error) {
	selected, sources, err := parseConfig(ds)
	if err != nil {
		return nil, err
	}
	if h == nil {
		return nil, fmt.Errorf("%w: missing Document Service stream handler", datasource.ErrInvalidConfig)
	}
	client, err := c.reader()
	if err != nil {
		return nil, err
	}
	cur, err := prepareCursor(old, client.Scope(), selected, full)
	if err != nil {
		return nil, err
	}
	r := &syncRun{ctx: ctx, client: client, h: h, cur: cur, sources: sources, limit: utils.GetMaxFileSize()}
	if r.cur.Phase == phaseScan {
		if err := r.scan(); err != nil {
			return nil, r.canceledOr(err)
		}
	}
	if err := r.reconcile(); err != nil {
		return nil, r.canceledOr(err)
	}
	return r.cur.toSyncCursor(), nil
}

func (r *syncRun) canceledOr(err error) error {
	if r.ctx.Err() != nil {
		return errors.Join(datasource.ErrSyncCanceled, r.ctx.Err())
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(datasource.ErrSyncCanceled, err)
	}
	return err
}

func (r *syncRun) checkpoint() error {
	if err := r.canceledOr(nil); err != nil {
		return err
	}
	return r.h.Checkpoint(r.ctx, r.cur.toSyncCursor())
}

// A source failure can preserve accepted items within the current page. The
// token still names that page so all other items are reconsidered on retry.
// Handler failures bypass this method: never checkpoint after a failed Emit.
func (r *syncRun) sourceFailure(err error) error {
	if canceled := r.canceledOr(err); errors.Is(canceled, datasource.ErrSyncCanceled) {
		return canceled
	}
	if checkpointErr := r.checkpoint(); checkpointErr != nil {
		return errors.Join(err, checkpointErr)
	}
	return err
}

func (r *syncRun) scan() error {
	visited := make(map[string]bool)
	for {
		if err := r.canceledOr(nil); err != nil {
			return err
		}
		token := r.cur.NextPage
		if visited[token] || len(visited) >= 100000 {
			return r.sourceFailure(invalidResponse("pagination did not converge"))
		}
		visited[token] = true
		page, err := r.client.ListDocuments(r.ctx, token, r.sources)
		if err != nil {
			return r.sourceFailure(err)
		}
		if err := validatePage(page, token); err != nil {
			return r.sourceFailure(err)
		}
		for _, entry := range page.Documents {
			mapped, err := mapEntry(entry)
			if err != nil {
				return r.sourceFailure(err)
			}
			if !slices.Contains(r.cur.SelectedSourceIDs, mapped.sourceID) {
				return r.sourceFailure(invalidResponse("document outside selected source filter"))
			}
			known, exists := r.cur.Documents[mapped.item.ExternalID]
			if exists && known.SourceID != mapped.sourceID {
				return r.sourceFailure(invalidResponse("canonical source owner changed"))
			}
			if exists {
				known.LastSeenGeneration = r.cur.Generation
				r.cur.Documents[mapped.item.ExternalID] = known
			}
			if known.AcceptedFingerprint == mapped.fingerprint &&
				(!r.cur.FullScan || known.AcceptedGeneration == r.cur.Generation) {
				continue
			}
			if rejection := mapped.rejection(r.limit); rejection != nil {
				mapped.item.PermanentRejection = rejection
				mapped.item.RetryOnFailure = false
				if err := r.h.Emit(r.ctx, mapped.item); err != nil {
					return err
				}
				// Preserve the accepted fingerprint so this version can be retried
				// if file support or the deployment limit changes in a later scan.
				continue
			}
			body, err := r.client.Download(r.ctx, entry, r.limit)
			if err != nil {
				return r.sourceFailure(err)
			}
			if int64(len(body)) != mapped.size || int64(len(body)) > r.limit ||
				fmt.Sprintf("sha256:%x", sha256.Sum256(body)) != entry.CurrentVersion.GetBlobDigest() {
				return r.sourceFailure(invalidResponse("download does not match immutable version digest/size"))
			}
			mapped.item.Content = body
			if err := r.h.Emit(r.ctx, mapped.item); err != nil {
				return err
			}
			r.cur.Documents[mapped.item.ExternalID] = inventoryEntry{
				AcceptedFingerprint: mapped.fingerprint, AcceptedGeneration: r.cur.Generation,
				LastSeenGeneration: r.cur.Generation, SourceID: mapped.sourceID, Title: mapped.item.Title,
			}
		}
		r.cur.NextPage = page.NextPageToken
		if page.NextPageToken == "" {
			r.cur.Phase = phaseDeletions
			r.cur.DeletionIDs = make([]string, 0)
			for id, known := range r.cur.Documents {
				if known.LastSeenGeneration != r.cur.Generation &&
					slices.Contains(r.cur.SelectedSourceIDs, known.SourceID) {
					r.cur.DeletionIDs = append(r.cur.DeletionIDs, id)
				}
			}
			sort.Strings(r.cur.DeletionIDs)
		}
		if err := r.checkpoint(); err != nil {
			return err
		}
		if r.cur.Phase == phaseDeletions {
			return nil
		}
	}
}

// Absence, permission loss, and source disablement never imply deletion. Only
// an explicit readable logical tombstone may remove previously indexed data.
func (r *syncRun) reconcile() error {
	for r.cur.DeletionIndex < len(r.cur.DeletionIDs) {
		if err := r.canceledOr(nil); err != nil {
			return err
		}
		end := min(r.cur.DeletionIndex+100, len(r.cur.DeletionIDs))
		ids := slices.Clone(r.cur.DeletionIDs[r.cur.DeletionIndex:end])
		summaries, err := r.client.BatchGetDocuments(r.ctx, ids)
		if err != nil {
			return r.sourceFailure(err)
		}
		seen := make(map[string]bool)
		for _, summary := range summaries {
			if summary == nil || !slices.Contains(ids, summary.GetDocumentId()) || seen[summary.GetDocumentId()] {
				return r.sourceFailure(invalidResponse("invalid batch document identity"))
			}
			seen[summary.GetDocumentId()] = true
			if summary.GetAccess() != txmdocuments.AccessReadable {
				continue
			}
			known := r.cur.Documents[summary.GetDocumentId()]
			if sourceID(summary.GetSourceType()) != known.SourceID ||
				(summary.GetStatus() != txmdocuments.StatusActive &&
					summary.GetStatus() != txmdocuments.StatusDeleted) {
				return r.sourceFailure(invalidResponse("invalid readable document summary"))
			}
		}
		for _, summary := range summaries {
			if summary.GetAccess() != txmdocuments.AccessReadable || summary.GetStatus() != txmdocuments.StatusDeleted {
				continue
			}
			known := r.cur.Documents[summary.GetDocumentId()]
			item := types.FetchedItem{
				ExternalID: summary.GetDocumentId(), Title: known.Title, SourceResourceID: known.SourceID,
				IsDeleted: true, RetryOnFailure: true,
				Metadata: map[string]string{
					"channel": types.ChannelDocumentService, "document_id": summary.GetDocumentId(),
					"source_type": known.SourceID,
				},
			}
			if err := r.h.Emit(r.ctx, item); err != nil {
				return err
			}
			// Keep tombstones in inventory. A disabled-deletions handler cannot
			// acknowledge actual removal; later enabled runs must emit it again.
		}
		r.cur.DeletionIndex = end
		if err := r.checkpoint(); err != nil {
			return err
		}
	}
	r.cur.Phase, r.cur.FullScan = phaseCompleted, false
	r.cur.DeletionIDs, r.cur.DeletionIndex = nil, 0
	return r.checkpoint()
}
