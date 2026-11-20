package documentservice

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/txmdocuments"
	api "github.com/Tencent/WeKnora/internal/txmdocuments/gen/document_service_api"
	models "github.com/Tencent/WeKnora/internal/txmdocuments/gen/document_service_models"
	"github.com/Tencent/WeKnora/internal/types"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeReader struct {
	scope       string
	pages       map[string]txmdocuments.Page
	listErr     map[string]error
	listTokens  []string
	listSources [][]txmdocuments.SourceType
	downloads   []*txmdocuments.Entry
	downloadErr map[string]error
	content     map[string][]byte
	summaries   map[string]*txmdocuments.Summary
	batchCalls  [][]string
	batchErr    error
	batchResult []*txmdocuments.Summary
}

func newFake(entries ...*txmdocuments.Entry) *fakeReader {
	return &fakeReader{
		scope: "https://documents.example|https://document-service.txm.internal",
		pages: map[string]txmdocuments.Page{"": {Documents: entries}}, listErr: map[string]error{},
		downloadErr: map[string]error{}, content: map[string][]byte{}, summaries: map[string]*txmdocuments.Summary{},
	}
}

func (f *fakeReader) Scope() string { return f.scope }
func (f *fakeReader) ListDocuments(
	_ context.Context, token string, sources []txmdocuments.SourceType,
) (txmdocuments.Page, error) {
	f.listTokens = append(f.listTokens, token)
	f.listSources = append(f.listSources, slices.Clone(sources))
	return f.pages[token], f.listErr[token]
}

func (f *fakeReader) Download(_ context.Context, entry *txmdocuments.Entry, _ int64) ([]byte, error) {
	f.downloads = append(f.downloads, entry)
	if err := f.downloadErr[entry.Document.GetDocumentId()]; err != nil {
		return nil, err
	}
	if content, ok := f.content[entry.Document.GetDocumentId()]; ok {
		return content, nil
	}
	return []byte("hello"), nil
}

func (f *fakeReader) BatchGetDocuments(_ context.Context, ids []string) ([]*txmdocuments.Summary, error) {
	f.batchCalls = append(f.batchCalls, slices.Clone(ids))
	if f.batchErr != nil || f.batchResult != nil {
		return f.batchResult, f.batchErr
	}
	out := make([]*txmdocuments.Summary, 0)
	for _, id := range ids {
		if summary := f.summaries[id]; summary != nil {
			out = append(out, summary)
		}
	}
	return out, nil
}

type fakeHandler struct {
	items           []types.FetchedItem
	checkpoints     []*types.SyncCursor
	emitErrAt       int
	checkpointErrAt int
}

var errTest = errors.New("injected failure")

func (h *fakeHandler) Emit(_ context.Context, item types.FetchedItem) error {
	h.items = append(h.items, item)
	if h.emitErrAt > 0 && len(h.items) == h.emitErrAt {
		return errTest
	}
	return nil
}

func (h *fakeHandler) Checkpoint(_ context.Context, c *types.SyncCursor) error {
	h.checkpoints = append(h.checkpoints, c)
	if h.checkpointErrAt > 0 && len(h.checkpoints) == h.checkpointErrAt {
		return errTest
	}
	return nil
}

func (h *fakeHandler) latest() *types.SyncCursor {
	if len(h.checkpoints) == 0 {
		return nil
	}
	return h.checkpoints[len(h.checkpoints)-1]
}

func entryFor(number int, source txmdocuments.SourceType) *txmdocuments.Entry {
	id := fmt.Sprintf("00000000-0000-4000-8000-%012d", number)
	versionID := fmt.Sprintf("10000000-0000-4000-8000-%012d", number)
	return &txmdocuments.Entry{
		Document: &txmdocuments.Document{
			DocumentId: proto.String(id), CurrentDocumentVersionId: proto.String(versionID),
			CurrentFilename: proto.String("hello.md"), Status: txmdocuments.StatusActive.Enum(),
			CreatedAt: timestamppb.New(time.Unix(10, 0)), UpdatedAt: timestamppb.New(time.Unix(20, 0)),
		},
		CurrentVersion: &txmdocuments.Version{
			DocumentVersionId: proto.String(versionID), DocumentId: proto.String(id),
			VersionNumber: proto.Int32(1), FilenameAtUpload: proto.String("original.md"),
			BlobDigest: proto.String(fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("hello")))),
			SizeBytes:  proto.Int64(5), DetectedMediaType: proto.String("text/markdown"),
			SourceType: source.Enum(), SourceKey: proto.String("source-" + id),
			SourceRevision: proto.String("rev1"),
		},
		Sources: []*txmdocuments.SourceRef{{
			DocumentSourceRefId: proto.String(fmt.Sprintf("20000000-0000-4000-8000-%012d", number)),
			DocumentId:          proto.String(id), SourceType: source.Enum(), SourceKey: proto.String("source-" + id),
			SourceState: models.DocumentSourceRef_ACTIVE.Enum(),
		}},
	}
}

func config(ids ...string) *types.DataSourceConfig {
	return &types.DataSourceConfig{ResourceIDs: ids}
}

func mustCursor(t *testing.T, raw *types.SyncCursor) cursor {
	t.Helper()
	c, err := decodeCursor(raw)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func syncOnce(t *testing.T, f *fakeReader, old *types.SyncCursor, ids ...string) (*types.SyncCursor, *fakeHandler) {
	t.Helper()
	h := &fakeHandler{}
	next, err := NewConnectorWithClient(f).FetchStream(context.Background(), config(ids...), old, h)
	if err != nil {
		t.Fatal(err)
	}
	return next, h
}

func TestDeploymentClientIsLazyAndCached(t *testing.T) {
	f := newFake()
	calls := 0
	c := &Connector{newClient: func() (Reader, error) { calls++; return f, nil }}
	if c.Type() != types.ConnectorTypeDocumentService || calls != 0 {
		t.Fatal("construction initialized the deployment client")
	}
	for i := 0; i < 2; i++ {
		if err := c.Validate(context.Background(), config()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 || !reflect.DeepEqual(f.listTokens, []string{"", ""}) {
		t.Fatalf("factory/list calls = %d/%v", calls, f.listTokens)
	}
}

func TestResourcesAndConfiguration(t *testing.T) {
	c := NewConnector()
	resources, err := c.ListResources(context.Background(), config(), "")
	if err != nil || len(resources) != 3 {
		t.Fatalf("resources=%v err=%v", resources, err)
	}
	for _, r := range resources {
		if r.HasChildren || r.Type != "source" {
			t.Fatalf("unexpected resource: %+v", r)
		}
	}
	children, err := c.ListResources(context.Background(), config(), "manual")
	if err != nil || len(children) != 0 {
		t.Fatalf("children=%v err=%v", children, err)
	}
	for _, ds := range []*types.DataSourceConfig{
		nil, config("unknown"), config("Manual"), config("dir:"), config("dir:not-a-uuid"),
		config("doc:00000000-0000-4000-8000-00000000000A"), config("folder:00000000-0000-4000-8000-000000000001"),
		{Credentials: map[string]interface{}{"token": ""}},
		{Settings: map[string]interface{}{"url": "https://elsewhere"}},
	} {
		if _, _, err := parseConfig(ds); !errors.Is(err, datasource.ErrInvalidConfig) {
			t.Fatalf("accepted config %+v: %v", ds, err)
		}
	}
	ids, _, err := parseConfig(config("github", "manual", "github"))
	if err != nil || !reflect.DeepEqual(ids, []string{"github", "manual"}) {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
	ids, sources, err := parseConfig(config("manual", "doc:"+testDir, "dir:"+testDir))
	if err != nil || !reflect.DeepEqual(ids, []string{"dir:" + testDir, "doc:" + testDir, "manual"}) ||
		len(sources) != 3 {
		t.Fatalf("ids=%v sources=%v err=%v", ids, sources, err)
	}
}

func TestValidateOnlyOneBoundedPage(t *testing.T) {
	f := newFake(entryFor(1, txmdocuments.SourceGitHub))
	page := f.pages[""]
	page.NextPageToken = "next"
	f.pages[""] = page
	if err := NewConnectorWithClient(f).Validate(context.Background(), config()); err != nil {
		t.Fatal(err)
	}
	if len(f.listTokens) != 1 || len(f.downloads) != 0 {
		t.Fatal("validation enumerated or downloaded content")
	}
}

func TestCompletedScanStartsAtHeadAndSkipsUnchanged(t *testing.T) {
	e := entryFor(1, txmdocuments.SourceManual)
	f := newFake(e)
	first, h := syncOnce(t, f, nil)
	if len(h.items) != 1 || h.items[0].ExternalID != e.Document.GetDocumentId() || !h.items[0].RetryOnFailure {
		t.Fatalf("items=%+v", h.items)
	}
	if f.downloads[0] != e ||
		f.downloads[0].CurrentVersion.GetDocumentVersionId() != e.Document.GetCurrentDocumentVersionId() {
		t.Fatal("download did not pin the listed version")
	}
	_, h = syncOnce(t, f, first)
	if len(h.items) != 0 || len(f.downloads) != 1 || !reflect.DeepEqual(f.listTokens, []string{"", ""}) {
		t.Fatalf("unchanged scan emitted=%d downloads=%d tokens=%v", len(h.items), len(f.downloads), f.listTokens)
	}
	if mustCursor(t, first).Phase != phaseCompleted {
		t.Fatal("scan did not complete")
	}
}

func TestPageResumeAndIndependentCheckpoints(t *testing.T) {
	a, b := entryFor(1, txmdocuments.SourceManual), entryFor(2, txmdocuments.SourceManual)
	f := newFake(a)
	f.pages[""] = txmdocuments.Page{Documents: []*txmdocuments.Entry{a}, NextPageToken: "p2"}
	f.pages["p2"] = txmdocuments.Page{Documents: []*txmdocuments.Entry{b}}
	f.listErr["p2"] = errTest
	h := &fakeHandler{}
	if _, err := NewConnectorWithClient(f).FetchStream(
		context.Background(), config(), nil, h,
	); !errors.Is(err, errTest) {
		t.Fatal(err)
	}
	checkpoint := h.checkpoints[0]
	if c := mustCursor(t, checkpoint); c.NextPage != "p2" || len(c.Documents) != 1 || c.Phase != phaseScan {
		t.Fatalf("checkpoint=%+v", c)
	}
	delete(f.listErr, "p2")
	f.listTokens = nil
	final, resumed := syncOnce(t, f, h.latest())
	if !reflect.DeepEqual(f.listTokens, []string{"p2"}) || len(resumed.items) != 1 ||
		resumed.items[0].ExternalID != b.Document.GetDocumentId() {
		t.Fatalf("tokens=%v items=%v", f.listTokens, resumed.items)
	}
	if len(mustCursor(t, checkpoint).Documents) != 1 || len(mustCursor(t, final).Documents) != 2 {
		t.Fatal("checkpoint snapshot mutated")
	}
}

func TestDownloadFailurePreservesSuccessfulCurrentPageItems(t *testing.T) {
	a, b := entryFor(1, txmdocuments.SourceManual), entryFor(2, txmdocuments.SourceManual)
	f := newFake(a, b)
	f.downloadErr[b.Document.GetDocumentId()] = errTest
	h := &fakeHandler{}
	if _, err := NewConnectorWithClient(f).FetchStream(
		context.Background(), config(), nil, h,
	); !errors.Is(err, errTest) {
		t.Fatal(err)
	}
	c := mustCursor(t, h.latest())
	if c.NextPage != "" || len(c.Documents) != 1 || c.Phase != phaseScan {
		t.Fatalf("cursor=%+v", c)
	}
	delete(f.downloadErr, b.Document.GetDocumentId())
	_, resumed := syncOnce(t, f, h.latest())
	if len(resumed.items) != 1 || resumed.items[0].ExternalID != b.Document.GetDocumentId() {
		t.Fatalf("replayed accepted item: %+v", resumed.items)
	}
}

func TestEmitFailureNeverCheckpointsCurrentPage(t *testing.T) {
	f := newFake(entryFor(1, txmdocuments.SourceManual), entryFor(2, txmdocuments.SourceManual))
	h := &fakeHandler{emitErrAt: 2}
	if next, err := NewConnectorWithClient(f).FetchStream(
		context.Background(), config(), nil, h,
	); !errors.Is(err, errTest) || next != nil {
		t.Fatalf("next=%v err=%v", next, err)
	}
	if len(h.checkpoints) != 0 {
		t.Fatal("failed Emit was followed by checkpoint")
	}
	_, replay := syncOnce(t, f, nil)
	if len(replay.items) != 2 {
		t.Fatal("uncheckpointed page was not replayed")
	}
}

func TestCheckpointFailureAbortsBeforeNextPage(t *testing.T) {
	f := newFake(entryFor(1, txmdocuments.SourceManual))
	page := f.pages[""]
	page.NextPageToken = "next"
	f.pages[""] = page
	h := &fakeHandler{checkpointErrAt: 1}
	if _, err := NewConnectorWithClient(f).FetchStream(
		context.Background(), config(), nil, h,
	); !errors.Is(err, errTest) {
		t.Fatal(err)
	}
	if len(f.listTokens) != 1 {
		t.Fatal("continued fetching after checkpoint failure")
	}
}

func TestFullSyncKeepsInventoryAndResumesSuccessfulItems(t *testing.T) {
	a, b := entryFor(1, txmdocuments.SourceManual), entryFor(2, txmdocuments.SourceManual)
	f := newFake(a, b)
	baseline, _ := syncOnce(t, f, nil)
	f.downloads = nil
	f.downloadErr[b.Document.GetDocumentId()] = errTest
	h := &fakeHandler{}
	if _, err := NewConnectorWithClient(f).FetchFullStream(
		context.Background(), config(), baseline, h,
	); !errors.Is(err, errTest) {
		t.Fatal(err)
	}
	if len(h.items) != 1 || len(mustCursor(t, h.latest()).Documents) != 2 {
		t.Fatal("full sync discarded baseline")
	}
	delete(f.downloadErr, b.Document.GetDocumentId())
	resumed := &fakeHandler{}
	if _, err := NewConnectorWithClient(f).FetchFullStream(
		context.Background(), config(), h.latest(), resumed,
	); err != nil {
		t.Fatal(err)
	}
	if len(resumed.items) != 1 || resumed.items[0].ExternalID != b.Document.GetDocumentId() {
		t.Fatalf("full replay=%v", resumed.items)
	}
}

func TestCategoryChangesRestartAndRetainExcludedInventory(t *testing.T) {
	a, b := entryFor(1, txmdocuments.SourceManual), entryFor(2, txmdocuments.SourceGitHub)
	f := newFake(a, b)
	baseline, _ := syncOnce(t, f, nil)
	f.pages[""] = txmdocuments.Page{Documents: []*txmdocuments.Entry{b}}
	f.summaries[a.Document.GetDocumentId()] = tombstone(a)
	changed, h := syncOnce(t, f, baseline, "github")
	if len(h.items) != 0 || len(f.batchCalls) != 0 || len(mustCursor(t, changed).Documents) != 2 {
		t.Fatal("selection change deleted excluded inventory")
	}
	f.pages[""] = txmdocuments.Page{Documents: []*txmdocuments.Entry{a, b}}
	_, h = syncOnce(t, f, changed)
	if len(h.items) != 0 {
		t.Fatal("reselecting source redownloaded unchanged inventory")
	}
}

func TestScopeChangeRequiresNewDataSource(t *testing.T) {
	f := newFake(entryFor(1, txmdocuments.SourceManual))
	baseline, _ := syncOnce(t, f, nil)
	f.scope += "/changed"
	if _, err := NewConnectorWithClient(f).FetchStream(
		context.Background(), config(), baseline, &fakeHandler{},
	); !errors.Is(err, datasource.ErrInvalidConfig) {
		t.Fatal(err)
	}
}

func TestPermanentRejectionsDoNotDownloadOrAdvanceAcceptedVersion(t *testing.T) {
	for _, category := range []string{"unsupported", "oversized", "empty"} {
		t.Run(category, func(t *testing.T) {
			t.Setenv("MAX_FILE_SIZE_MB", "1")
			e := entryFor(1, txmdocuments.SourceManual)
			f := newFake(e)
			baseline, _ := syncOnce(t, f, nil)
			accepted := mustCursor(t, baseline).Documents[e.Document.GetDocumentId()].AcceptedFingerprint
			e = proto.Clone(e).(*txmdocuments.Entry)
			e.CurrentVersion.SourceRevision = proto.String("changed")
			switch category {
			case "unsupported":
				e.Document.CurrentFilename = proto.String("unsafe.exe")
			case "oversized":
				e.CurrentVersion.SizeBytes = proto.Int64(1024*1024 + 1)
			case "empty":
				e.CurrentVersion.SizeBytes = proto.Int64(0)
			}
			f.pages[""] = txmdocuments.Page{Documents: []*txmdocuments.Entry{e}}
			next, h := syncOnce(t, f, baseline)
			if len(h.items) != 1 || h.items[0].PermanentRejection == nil ||
				h.items[0].RetryOnFailure || len(f.downloads) != 1 {
				t.Fatalf("rejection=%+v downloads=%d", h.items, len(f.downloads))
			}
			if mustCursor(t, next).Documents[e.Document.GetDocumentId()].AcceptedFingerprint != accepted {
				t.Fatal("rejection replaced accepted fingerprint")
			}
			_, retry := syncOnce(t, f, next)
			if len(retry.items) != 1 || retry.items[0].PermanentRejection == nil {
				t.Fatal("rejection not reconsidered")
			}
		})
	}
}

func TestNewVersionAndHierarchyChangesUpdateTheSameDocument(t *testing.T) {
	e := entryFor(1, txmdocuments.SourceGitHub)
	f := newFake(e)
	baseline, first := syncOnce(t, f, nil)
	previousFingerprint := first.items[0].Metadata[FingerprintMetadataKey]
	updated := proto.Clone(e).(*txmdocuments.Entry)
	updated.Document.CurrentFilename = proto.String("renamed.md")
	updated.Document.CurrentDocumentVersionId = proto.String("10000000-0000-4000-8000-000000000999")
	updated.CurrentVersion.DocumentVersionId = updated.Document.CurrentDocumentVersionId
	updated.CurrentVersion.VersionNumber = proto.Int32(2)
	updated.CurrentVersion.SourceRevision = proto.String("rev2")
	updated.Sources[0].HierarchyPath = []*txmdocuments.HierarchySegment{{
		Key: proto.String("repository-key"), DisplayName: proto.String("Repository"),
		Kind: models.HierarchySegment_CONTAINER.Enum(),
	}}
	f.pages[""] = txmdocuments.Page{Documents: []*txmdocuments.Entry{updated}}
	next, h := syncOnce(t, f, baseline)
	if len(h.items) != 1 || h.items[0].ExternalID != e.Document.GetDocumentId() ||
		h.items[0].FileName != "GitHub/Repository/renamed.md" ||
		h.items[0].Metadata[FingerprintMetadataKey] == previousFingerprint {
		t.Fatalf("updated item=%+v", h.items)
	}
	if f.downloads[len(f.downloads)-1].CurrentVersion.GetDocumentVersionId() !=
		updated.CurrentVersion.GetDocumentVersionId() || len(mustCursor(t, next).Documents) != 1 {
		t.Fatal("version update changed identity or downloaded another version")
	}
	updated.Sources[0].HierarchyPath[0].DisplayName = proto.String("Moved")
	_, h = syncOnce(t, f, next)
	if len(h.items) != 1 || h.items[0].FileName != "GitHub/Moved/renamed.md" {
		t.Fatalf("hierarchy update=%+v", h.items)
	}
}

func TestNewRejectedDocumentIsReconsideredAfterLimitChanges(t *testing.T) {
	t.Setenv("MAX_FILE_SIZE_MB", "1")
	e := entryFor(1, txmdocuments.SourceManual)
	body := []byte(strings.Repeat("x", 1024*1024+1))
	e.CurrentVersion.SizeBytes = proto.Int64(int64(len(body)))
	e.CurrentVersion.BlobDigest = proto.String(fmt.Sprintf("sha256:%x", sha256.Sum256(body)))
	f := newFake(e)
	f.content[e.Document.GetDocumentId()] = body
	baseline, h := syncOnce(t, f, nil)
	if len(h.items) != 1 || h.items[0].PermanentRejection == nil ||
		len(f.downloads) != 0 || len(mustCursor(t, baseline).Documents) != 0 {
		t.Fatal("new oversized document was downloaded or accepted")
	}
	t.Setenv("MAX_FILE_SIZE_MB", "2")
	next, h := syncOnce(t, f, baseline)
	if len(h.items) != 1 || h.items[0].PermanentRejection != nil ||
		len(f.downloads) != 1 || len(mustCursor(t, next).Documents) != 1 {
		t.Fatal("raised limit did not reconsider rejected document")
	}
}

func tombstone(e *txmdocuments.Entry) *txmdocuments.Summary {
	return &txmdocuments.Summary{
		DocumentId: proto.String(e.Document.GetDocumentId()), Access: api.Access_READABLE.Enum(),
		SourceType: e.Sources[0].GetSourceType().Enum(), Status: txmdocuments.StatusDeleted.Enum(),
	}
}

func TestOnlyExplicitReadableTombstonesDeleteAndRemainRetryable(t *testing.T) {
	entries := []*txmdocuments.Entry{
		entryFor(1, txmdocuments.SourceManual), entryFor(2, txmdocuments.SourceManual),
		entryFor(3, txmdocuments.SourceManual), entryFor(4, txmdocuments.SourceManual),
	}
	f := newFake(entries...)
	baseline, _ := syncOnce(t, f, nil)
	f.pages[""] = txmdocuments.Page{}
	f.summaries[entries[0].Document.GetDocumentId()] = tombstone(entries[0])
	f.summaries[entries[1].Document.GetDocumentId()] = &txmdocuments.Summary{
		DocumentId: entries[1].Document.DocumentId, Access: api.Access_RESTRICTED.Enum(),
	}
	f.summaries[entries[2].Document.GetDocumentId()] = tombstone(entries[2])
	f.summaries[entries[2].Document.GetDocumentId()].Status = txmdocuments.StatusActive.Enum()
	next, h := syncOnce(t, f, baseline)
	if len(h.items) != 1 || !h.items[0].IsDeleted || !h.items[0].RetryOnFailure ||
		h.items[0].ExternalID != entries[0].Document.GetDocumentId() {
		t.Fatalf("deletions=%+v", h.items)
	}
	if len(mustCursor(t, next).Documents) != 4 {
		t.Fatal("tombstone consumed inventory despite disabled deletions possibility")
	}
	_, h = syncOnce(t, f, next)
	if len(h.items) != 1 || !h.items[0].IsDeleted {
		t.Fatal("explicit tombstone was not retried")
	}
}

func TestDeletionBatchesResumeWithoutRepeatingCompletedBatch(t *testing.T) {
	f := newFake()
	c := cursor{
		SchemaVersion: 1, ServiceScope: f.scope, SelectedSourceIDs: []string{"manual"},
		Phase: phaseCompleted, Generation: 1, Documents: map[string]inventoryEntry{},
	}
	for n := 1; n <= 205; n++ {
		e := entryFor(n, txmdocuments.SourceManual)
		mapped, err := mapEntry(e)
		if err != nil {
			t.Fatal(err)
		}
		c.Documents[e.Document.GetDocumentId()] = inventoryEntry{
			AcceptedFingerprint: mapped.fingerprint, SourceID: "manual", LastSeenGeneration: 1,
		}
		f.summaries[e.Document.GetDocumentId()] = tombstone(e)
	}
	h := &fakeHandler{emitErrAt: 101}
	if _, err := NewConnectorWithClient(f).FetchStream(
		context.Background(), config("manual"), c.toSyncCursor(), h,
	); !errors.Is(err, errTest) {
		t.Fatal(err)
	}
	if cur := mustCursor(t, h.latest()); cur.Phase != phaseDeletions || cur.DeletionIndex != 100 {
		t.Fatalf("cursor=%+v", cur)
	}
	listCalls := len(f.listTokens)
	f.batchCalls = nil
	_, resumed := syncOnce(t, f, h.latest(), "manual")
	if len(resumed.items) != 105 || len(f.listTokens) != listCalls || len(f.batchCalls) != 2 ||
		len(f.batchCalls[0]) != 100 || len(f.batchCalls[1]) != 5 {
		t.Fatalf("resumed=%d batches=%v", len(resumed.items), f.batchCalls)
	}
}

func TestInvalidBatchResponseNeverDeletes(t *testing.T) {
	e := entryFor(1, txmdocuments.SourceManual)
	f := newFake(e)
	baseline, _ := syncOnce(t, f, nil)
	f.pages[""] = txmdocuments.Page{}
	f.batchResult = []*txmdocuments.Summary{tombstone(entryFor(2, txmdocuments.SourceManual))}
	h := &fakeHandler{}
	if _, err := NewConnectorWithClient(f).FetchStream(
		context.Background(), config(), baseline, h,
	); !errors.Is(err, datasource.ErrFetchFailed) {
		t.Fatal(err)
	}
	if len(h.items) != 0 {
		t.Fatal("foreign batch identity emitted deletion")
	}
}

func TestCanceledRunDoesNotCheckpoint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := &fakeHandler{}
	if _, err := NewConnectorWithClient(newFake()).FetchStream(
		ctx, config(), nil, h,
	); !errors.Is(err, datasource.ErrSyncCanceled) {
		t.Fatal(err)
	}
	if len(h.checkpoints) != 0 {
		t.Fatal("canceled run checkpointed")
	}
}

func TestMappingHierarchyAndSemanticFingerprint(t *testing.T) {
	e := entryFor(1, txmdocuments.SourceFeishu)
	e.Sources[0].HierarchyPath = []*txmdocuments.HierarchySegment{
		{
			Key: proto.String("stable/a"), DisplayName: proto.String("../team\\docs"),
			Kind: models.HierarchySegment_CONTAINER.Enum(),
		},
		{Key: proto.String("stable/b"), DisplayName: proto.String(".."), Kind: models.HierarchySegment_DOCUMENT.Enum()},
	}
	e.Sources[0].SourceMetadata, _ = structpb.NewStruct(map[string]interface{}{
		"original_url": "https://example.com/doc", "last_seen_at": "yesterday",
		"download_url": "https://secret.example/token",
	})
	mapped, err := mapEntry(e)
	if err != nil {
		t.Fatal(err)
	}
	if mapped.item.Title != "Feishu/.._team_docs/_/hello.md" || mapped.item.FileName != mapped.item.Title ||
		!strings.Contains(mapped.item.Metadata["hierarchy_path"], "stable/a") {
		t.Fatalf("hierarchy=%+v", mapped.item)
	}
	if !mapped.item.UpdatedAt.IsZero() || mapped.item.Metadata["document_service_updated_at"] == "" ||
		mapped.item.URL != "https://example.com/doc" {
		t.Fatal("timestamps or original URL mapped incorrectly")
	}
	if strings.Contains(mapped.item.Metadata["source_metadata"], "secret.example") {
		t.Fatal("capability URL persisted")
	}
	e.Sources[0].LastSeenAt = timestamppb.Now()
	e.Sources[0].SourceMetadata.Fields["last_seen_at"] = structpb.NewStringValue("today")
	e.Sources[0].SourceMetadata.Fields["download_url"] = structpb.NewStringValue("https://another.secret")
	unchanged, err := mapEntry(e)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.fingerprint != mapped.fingerprint {
		t.Fatal("volatile metadata changed fingerprint")
	}
	e.Sources[0].HierarchyPath[0].DisplayName = proto.String("Renamed")
	renamed, err := mapEntry(e)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.fingerprint == mapped.fingerprint || !strings.Contains(renamed.item.Title, "Renamed") {
		t.Fatal("hierarchy rename did not change fingerprint/title")
	}
}

func TestOriginalURLFallsBackToVersionMetadata(t *testing.T) {
	e := entryFor(1, txmdocuments.SourceGitHub)
	e.CurrentVersion.SourceMetadata, _ = structpb.NewStruct(map[string]interface{}{
		"original_url": "https://github.com/owner/repository/blob/revision/file.md",
	})
	mapped, err := mapEntry(e)
	if err != nil {
		t.Fatal(err)
	}
	if mapped.item.URL != "https://github.com/owner/repository/blob/revision/file.md" ||
		mapped.item.Metadata["original_url"] != mapped.item.URL {
		t.Fatalf("URL=%q metadata=%v", mapped.item.URL, mapped.item.Metadata)
	}
}

func TestMalformedMetadataRejectedBeforeDownload(t *testing.T) {
	for name, mutate := range map[string]func(*txmdocuments.Entry){
		"missing_document":  func(e *txmdocuments.Entry) { e.Document = nil },
		"missing_version":   func(e *txmdocuments.Entry) { e.CurrentVersion = nil },
		"noncanonical_uuid": func(e *txmdocuments.Entry) { e.Document.DocumentId = proto.String("not-a-uuid") },
		"version_link": func(e *txmdocuments.Entry) {
			e.Document.CurrentDocumentVersionId = proto.String("10000000-0000-4000-8000-999999999999")
		},
		"foreign_version": func(e *txmdocuments.Entry) {
			e.CurrentVersion.DocumentId = proto.String("00000000-0000-4000-8000-999999999999")
		},
		"missing_size":    func(e *txmdocuments.Entry) { e.CurrentVersion.SizeBytes = nil },
		"negative_size":   func(e *txmdocuments.Entry) { e.CurrentVersion.SizeBytes = proto.Int64(-1) },
		"digest":          func(e *txmdocuments.Entry) { e.CurrentVersion.BlobDigest = proto.String("sha256:bad") },
		"multiple_owners": func(e *txmdocuments.Entry) { e.Sources = append(e.Sources, e.Sources[0]) },
		"source_pair":     func(e *txmdocuments.Entry) { e.CurrentVersion.SourceKey = proto.String("other") },
		"unknown_source": func(e *txmdocuments.Entry) {
			e.Sources[0].SourceType = models.SourceType_SOURCE_TYPE_UNKNOWN.Enum()
		},
		"invalid_hierarchy": func(e *txmdocuments.Entry) {
			e.Sources[0].HierarchyPath = []*txmdocuments.HierarchySegment{{Key: proto.String("")}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := entryFor(1, txmdocuments.SourceManual)
			mutate(e)
			f, h := newFake(e), &fakeHandler{}
			if _, err := NewConnectorWithClient(f).FetchStream(
				context.Background(), config(), nil, h,
			); !errors.Is(err, datasource.ErrFetchFailed) {
				t.Fatal(err)
			}
			if len(f.downloads) != 0 || len(h.items) != 0 {
				t.Fatal("invalid metadata reached download/ingestion")
			}
		})
	}
}

func TestDownloadDigestAndSizeMustMatchListedVersion(t *testing.T) {
	for _, content := range [][]byte{[]byte("wrong"), []byte("hello extra"), {}} {
		e := entryFor(1, txmdocuments.SourceManual)
		f, h := newFake(e), &fakeHandler{}
		f.content[e.Document.GetDocumentId()] = content
		if _, err := NewConnectorWithClient(f).FetchStream(
			context.Background(), config(), nil, h,
		); !errors.Is(err, datasource.ErrFetchFailed) {
			t.Fatal(err)
		}
		if len(h.items) != 0 || len(mustCursor(t, h.latest()).Documents) != 0 {
			t.Fatal("unverified bytes were accepted")
		}
	}
}

func TestCursorRejectsMalformedAndUnknownSchema(t *testing.T) {
	f := newFake(entryFor(1, txmdocuments.SourceManual))
	baseline, _ := syncOnce(t, f, nil)
	for _, mutation := range []func(map[string]interface{}){
		func(m map[string]interface{}) { m["schemaVersion"] = 2 },
		func(m map[string]interface{}) { m["phase"] = "other" },
		func(m map[string]interface{}) { m["selectedSourceIDs"] = []string{"unknown"} },
		func(m map[string]interface{}) { m["selectedSourceIDs"] = []string{"dir:unknown"} },
		func(m map[string]interface{}) { m["deletionIndex"] = 2 },
		func(m map[string]interface{}) {
			m["documents"] = map[string]interface{}{"invalid": map[string]interface{}{}}
		},
	} {
		raw, _ := json.Marshal(baseline)
		var cursorCopy types.SyncCursor
		_ = json.Unmarshal(raw, &cursorCopy)
		mutation(cursorCopy.ConnectorCursor)
		if _, err := decodeCursor(&cursorCopy); !errors.Is(err, datasource.ErrInvalidConfig) {
			t.Fatal(err)
		}
	}
}

const testDir = "30000000-0000-4000-8000-000000000001"

func inDirs(e *txmdocuments.Entry, keys ...string) *txmdocuments.Entry {
	e.Sources[0].HierarchyPath = nil
	for _, key := range keys {
		e.Sources[0].HierarchyPath = append(e.Sources[0].HierarchyPath, &txmdocuments.HierarchySegment{
			Key: proto.String(key), DisplayName: proto.String("d-" + key[len(key)-1:]),
			Kind: models.HierarchySegment_CONTAINER.Enum(),
		})
	}
	return e
}

func ids(items []types.FetchedItem) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		id := item.ExternalID[len(item.ExternalID)-1:]
		if item.IsDeleted {
			id = "-" + id
		}
		out = append(out, id)
	}
	return out
}

func TestDirectorySelectionFollowsAddsUpdatesMovesAndDeletes(t *testing.T) {
	sub := "30000000-0000-4000-8000-000000000002"
	inDir := inDirs(entryFor(1, txmdocuments.SourceManual), testDir)
	inSub := inDirs(entryFor(2, txmdocuments.SourceManual), testDir, sub)
	root := entryFor(3, txmdocuments.SourceManual)
	other := entryFor(4, txmdocuments.SourceGitHub)
	f := newFake(inDir, inSub, root, other)
	sel := "dir:" + testDir
	baseline, h := syncOnce(t, f, nil, sel)
	if !reflect.DeepEqual(ids(h.items), []string{"1", "2"}) || len(f.listSources[0]) != 3 {
		t.Fatalf("items=%v sources=%v", ids(h.items), f.listSources)
	}

	added := inDirs(entryFor(5, txmdocuments.SourceManual), testDir, sub)
	updated := proto.Clone(inSub).(*txmdocuments.Entry)
	updated.CurrentVersion.SourceRevision = proto.String("rev2")
	moved := inDirs(proto.Clone(inDir).(*txmdocuments.Entry))
	f.pages[""] = txmdocuments.Page{Documents: []*txmdocuments.Entry{moved, updated, root, other, added}}
	next, h := syncOnce(t, f, baseline, sel)
	if !reflect.DeepEqual(ids(h.items), []string{"-1", "2", "5"}) || !mustCursor(t, next).Documents[moved.Document.GetDocumentId()].Excluded {
		t.Fatalf("items=%v", ids(h.items))
	}

	f.pages[""] = txmdocuments.Page{Documents: []*txmdocuments.Entry{inDir, root, other, added}}
	f.summaries[updated.Document.GetDocumentId()] = tombstone(updated)
	_, h = syncOnce(t, f, next, sel)
	if !reflect.DeepEqual(ids(h.items), []string{"1", "-2"}) {
		t.Fatalf("moved back and deleted=%v", ids(h.items))
	}
}

func TestDocumentSelectionAndNarrowingFromSource(t *testing.T) {
	a, b := entryFor(1, txmdocuments.SourceManual), entryFor(2, txmdocuments.SourceFeishu)
	f := newFake(a)
	baseline, h := syncOnce(t, f, nil, "manual")
	if !reflect.DeepEqual(ids(h.items), []string{"1"}) {
		t.Fatalf("items=%v", ids(h.items))
	}
	sel := "doc:" + b.Document.GetDocumentId()
	f.pages[""] = txmdocuments.Page{Documents: []*txmdocuments.Entry{a, b}}
	next, h := syncOnce(t, f, baseline, sel)
	if !reflect.DeepEqual(ids(h.items), []string{"-1", "2"}) {
		t.Fatalf("narrowed items=%v", ids(h.items))
	}
	f.pages[""] = txmdocuments.Page{Documents: []*txmdocuments.Entry{a}}
	f.summaries[b.Document.GetDocumentId()] = tombstone(b)
	_, h = syncOnce(t, f, next, sel)
	if !reflect.DeepEqual(ids(h.items), []string{"-1", "-2"}) {
		t.Fatalf("deleted items=%v", ids(h.items))
	}
}
