package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/types/interfaces"

	"github.com/Tencent/WeKnora/internal/datasource"
	werrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestDocumentServiceFingerprintReplayDoesNotReplace(t *testing.T) {
	for _, channel := range []string{types.ChannelDocumentService, types.ChannelNextcloud} {
		t.Run(channel, func(t *testing.T) {
			svc, ds, repo, ks, _ := newConflictSkipHarness(t, types.ConflictStrategyOverwrite)
			ds.Type = channel
			repo.knowledge.Metadata = types.JSON(`{"document_service_fingerprint":"accepted"}`)
			item := &types.FetchedItem{
				ExternalID: "document", Content: []byte("content"), FileName: "manual/doc.md",
				Metadata: map[string]string{"document_service_fingerprint": "accepted"},
			}
			_, err := svc.ingestItem(context.Background(), ds, item, nil)
			if channel == types.ChannelDocumentService {
				var duplicate *types.DuplicateKnowledgeError
				require.ErrorAs(t, err, &duplicate)
				require.Zero(t, ks.replacements)
			} else {
				require.NoError(t, err)
				require.Equal(t, 1, ks.replacements)
			}
			item.Metadata["document_service_fingerprint"] = "changed"
			_, err = svc.ingestItem(context.Background(), ds, item, nil)
			require.NoError(t, err)
			require.Positive(t, ks.replacements)
		})
	}
}

func TestDocumentServiceExplicitRejectionsArePartialAndPreserveImports(t *testing.T) {
	h := newIngestAckHarness(t, true)
	h.connector.items = []types.FetchedItem{{
		ExternalID: "blocked",
		PermanentRejection: &types.SyncItemError{
			Title: "blocked.bin", Code: "unsupported_file_type", Message: "Unsupported file type",
		},
	}}
	require.NoError(t, h.svc.ProcessSync(context.Background(), h.task))
	log := h.syncLogRepo.logs[h.syncLogID]
	require.Equal(t, types.SyncLogStatusPartial, log.Status)
	require.Equal(t, 1, log.ItemsFailed)
	require.Contains(t, string(log.Result), "unsupported_file_type")
	require.Empty(t, h.sink.attempted)
	cursor, err := h.ds.ParseSyncCursor()
	require.NoError(t, err)
	require.Equal(t, 1, ingestAckOffset(cursor))
}

func TestDocumentServiceDoesNotAcceptStoredCredentialMutation(t *testing.T) {
	svc, ds, _, _, _ := newConflictSkipHarness(t, types.ConflictStrategyOverwrite)
	ds.Type = types.ConnectorTypeDocumentService
	_, err := svc.UpdateDataSourceCredentials(
		context.Background(), ds.ID, map[string]interface{}{"token": "secret"},
	)
	require.ErrorIs(t, err, datasource.ErrDeploymentManagedCredentials)
	require.ErrorIs(t, svc.ClearDataSourceCredentials(context.Background(), ds.ID),
		datasource.ErrDeploymentManagedCredentials)
}

func TestDocumentServiceUsesSourceIdentityDuplicateCheck(t *testing.T) {
	require.True(t, usesSourceIdentityDuplicateCheck(types.ChannelDocumentService))
}

type rejectingManagedConnector struct{ conflictSkipConnector }

func (rejectingManagedConnector) Type() string { return types.ConnectorTypeDocumentService }
func (rejectingManagedConnector) Validate(context.Context, *types.DataSourceConfig) error {
	return datasource.ErrInvalidConfig
}

func TestDocumentServiceValidatesSettingsWithoutStoredCredentials(t *testing.T) {
	svc, ds, _, _, _ := newConflictSkipHarness(t, types.ConflictStrategyOverwrite)
	ds.Type = types.ConnectorTypeDocumentService
	require.NoError(t, svc.connectorRegistry.Register(rejectingManagedConnector{}))
	incoming := *ds
	incoming.Config = types.JSON(`{"type":"document_service","resource_ids":["invalid"]}`)
	_, err := svc.UpdateDataSource(context.Background(), &incoming)
	require.ErrorIs(t, err, datasource.ErrInvalidConfig)

	incoming.Config = types.JSON(`{"type":"document_service","credentials":{"token":"secret"}}`)
	_, err = svc.UpdateDataSource(context.Background(), &incoming)
	require.ErrorIs(t, err, datasource.ErrDeploymentManagedCredentials)
}

type documentReplayKnowledge struct {
	*conflictSkipKnowledgeService
	repo *deletionLookupKnowledgeRepo
}

func (s *documentReplayKnowledge) ReplaceKnowledgeFromDataSource(
	ctx context.Context, id string, item *types.FetchedItem, metadata map[string]string, tags []string, channel string,
) (*types.Knowledge, error) {
	_, err := s.conflictSkipKnowledgeService.ReplaceKnowledgeFromDataSource(ctx, id, item, metadata, tags, channel)
	if err != nil {
		return nil, err
	}
	blob, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	s.repo.knowledge.Metadata = blob
	return s.repo.knowledge, nil
}

type documentCheckpointFault struct {
	interfaces.DataSourceRepository
	failure error
}

func (r documentCheckpointFault) UpdateSyncState(ctx context.Context, ds *types.DataSource) error {
	if r.failure != nil {
		return r.failure
	}
	return r.DataSourceRepository.UpdateSyncState(ctx, ds)
}

func TestDocumentServiceReplayAfterIngestionBeforeCheckpointFailure(t *testing.T) {
	svc, ds, repo, ks, log := newConflictSkipHarness(t, types.ConflictStrategyOverwrite)
	ds.Type = types.ConnectorTypeDocumentService
	svc.knowledgeService = &documentReplayKnowledge{conflictSkipKnowledgeService: ks, repo: repo}
	item := types.FetchedItem{
		ExternalID: "document", Content: []byte("content"), FileName: "Manual/doc.md", RetryOnFailure: true,
		Metadata: map[string]string{"document_service_fingerprint": "accepted"},
	}
	fault := errors.New("checkpoint storage unavailable")
	svc.dsRepo = documentCheckpointFault{DataSourceRepository: svc.dsRepo, failure: fault}
	previous := ds.LastSyncCursor
	first := newStreamHandler(svc, ds, &types.SyncResult{}, log)
	require.NoError(t, first.Emit(context.Background(), item))
	require.Equal(t, 1, ks.replacements)
	require.ErrorIs(t, first.Checkpoint(context.Background(), ingestAckCursor(1)), fault)
	// A new worker reads the prior persisted cursor after a failed write.
	ds.LastSyncCursor = previous
	replayed := &types.SyncResult{}
	second := newStreamHandler(svc, ds, replayed, log)
	require.NoError(t, second.Emit(context.Background(), item))
	require.Equal(t, 1, ks.replacements, "replay must acknowledge the stored fingerprint without replacement")
	require.Equal(t, 1, replayed.Skipped)
}

func TestDocumentServiceMissingModelsRequireConfigurationAndRetry(t *testing.T) {
	h := newIngestAckHarness(t, true)
	h.connector.items = h.connector.items[1:2]
	h.connector.items[0].RetryOnFailure = true
	h.sink.failure = werrors.NewImportPrerequisiteError("Configure a VLM model")
	require.Error(t, h.svc.ProcessSync(context.Background(), h.task))
	log := h.syncLogRepo.logs[h.syncLogID]
	require.Equal(t, types.SyncLogStatusFailed, log.Status)
	require.Contains(t, string(log.Result), "prerequisite_not_configured")
	cursor, err := h.ds.ParseSyncCursor()
	require.NoError(t, err)
	require.Zero(t, ingestAckOffset(cursor))
	h.sink.failure = nil
	require.NoError(t, h.svc.ProcessSync(context.Background(), h.task))
	require.True(t, h.sink.stored["blocked"])
}
