package documentservice

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/txmdocuments"
	"github.com/Tencent/WeKnora/internal/types"
	"google.golang.org/protobuf/proto"
)

func resourceIndex(resources []types.Resource) map[string]types.Resource {
	out := make(map[string]types.Resource, len(resources))
	for _, resource := range resources {
		out[resource.ExternalID] = resource
	}
	return out
}

func TestResourceTreeUsesAllPagesAndStableSourcePaths(t *testing.T) {
	manual := inDirs(entryFor(1, txmdocuments.SourceManual), testDir)
	github := inDirs(entryFor(2, txmdocuments.SourceGitHub), "repo:Tranxmart/tranxmart", "docs/team")
	feishu := inDirs(entryFor(3, txmdocuments.SourceFeishu), "space:engineering", "node:team")
	otherRepo := inDirs(entryFor(4, txmdocuments.SourceGitHub), "repo:other", "docs/team")
	// Labels are mutable and need not be unique, even in the same source.
	for _, entry := range []*txmdocuments.Entry{github, feishu, otherRepo} {
		entry.Sources[0].HierarchyPath[1].DisplayName = proto.String("Team docs")
	}
	f := newFake(manual, github)
	first := f.pages[""]
	first.NextPageToken = "second"
	f.pages[""], f.pages["second"] = first, txmdocuments.Page{Documents: []*txmdocuments.Entry{feishu, otherRepo}}
	c := NewConnectorWithClient(f)
	resources, err := c.ListResources(context.Background(), config("manual"), "")
	if err != nil {
		t.Fatal(err)
	}
	nodes := resourceIndex(resources)
	if len(nodes) != 14 || len(resources) != len(nodes) || !reflect.DeepEqual(f.listTokens, []string{"", "second"}) {
		t.Fatalf("nodes=%+v calls=%v", nodes, f.listTokens)
	}
	for _, sources := range f.listSources {
		if len(sources) != 3 {
			t.Fatal("picker was restricted to the saved selection")
		}
	}
	for _, entry := range []*txmdocuments.Entry{manual, github, feishu, otherRepo} {
		source := sourceID(entry.Sources[0].GetSourceType())
		parent := source
		var keys []string
		for _, segment := range entry.Sources[0].GetHierarchyPath() {
			keys = append(keys, segment.GetKey())
			id := directoryID(source, keys)
			node := nodes[id]
			if node.ParentID != parent || node.Name != segment.GetDisplayName() || !node.HasChildren ||
				node.Type != "folder" || !validSelection(id) {
				t.Fatalf("invalid folder: %+v", node)
			}
			parent = id
		}
		leaf := nodes[docPrefix+entry.Document.GetDocumentId()]
		if leaf.ParentID != parent || leaf.Name != "hello.md" || leaf.HasChildren || leaf.Type != "document" {
			t.Fatalf("invalid leaf: %+v", leaf)
		}
	}
	if len(f.downloads) != 0 || len(f.batchCalls) != 0 {
		t.Fatal("picker downloaded documents or queried lifecycle summaries")
	}
	before := directoryID("github", []string{"repo:Tranxmart/tranxmart", "docs/team"})
	github.Sources[0].HierarchyPath[1].DisplayName = proto.String("Renamed team")
	resources, err = c.ListResources(context.Background(), config(), "")
	if err != nil || resourceIndex(resources)[before].Name != "Renamed team" {
		t.Fatalf("rename changed directory identity: %v", err)
	}
	children, err := c.ListResources(context.Background(), config(), before)
	if err != nil || len(children) != 0 {
		t.Fatalf("full tree unexpectedly needs lazy loading: %v %v", children, err)
	}
	ancestors, err := c.ResolveResourceAncestors(context.Background(), config(), []string{before})
	if err != nil || len(ancestors) != 0 {
		t.Fatalf("full tree needs ancestor requests: %v %v", ancestors, err)
	}
}

func TestResourceTreeRejectsIncompleteOrInvalidListing(t *testing.T) {
	for _, scenario := range []string{
		"failed page", "repeated token", "invalid hierarchy", "duplicate document", "canceled",
	} {
		t.Run(scenario, func(t *testing.T) {
			entry := inDirs(entryFor(1, txmdocuments.SourceGitHub), "repo:team")
			f := newFake(entry)
			f.pages[""] = txmdocuments.Page{Documents: []*txmdocuments.Entry{entry}, NextPageToken: "second"}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "failed page":
				f.listErr["second"] = errTest
			case "repeated token":
				f.pages["second"] = txmdocuments.Page{NextPageToken: "second"}
			case "invalid hierarchy":
				entry.Sources[0].HierarchyPath[0].Key = proto.String(strings.Repeat("x", 513))
			case "duplicate document":
				f.pages["second"] = txmdocuments.Page{Documents: []*txmdocuments.Entry{entry}}
			case "canceled":
				cancel()
			}
			resources, err := NewConnectorWithClient(f).ListResources(ctx, config(), "")
			if err == nil || resources != nil {
				t.Fatalf("exposed incomplete picker: %v %v", resources, err)
			}
		})
	}
}

func TestProviderPathSelectionIsScopedAndSurvivesCursorRestarts(t *testing.T) {
	for _, source := range []txmdocuments.SourceType{txmdocuments.SourceGitHub, txmdocuments.SourceFeishu} {
		t.Run(sourceID(source), func(t *testing.T) {
			selected := inDirs(entryFor(1, source), "tree:team", "folder:design")
			nested := inDirs(entryFor(2, source), "tree:team", "folder:design", "folder:child")
			otherTree := inDirs(entryFor(3, source), "tree:other", "folder:design")
			otherSource := inDirs(entryFor(4, txmdocuments.SourceManual), "tree:team", "folder:design")
			f := newFake(selected, nested, otherTree, otherSource)
			// Fake source filtering mirrors ListDocuments' contract.
			f.pages[""] = txmdocuments.Page{Documents: []*txmdocuments.Entry{selected, nested, otherTree}}
			selection := directoryID(sourceID(source), []string{"tree:team", "folder:design"})
			baseline, h := syncOnce(t, f, nil, selection)
			if !reflect.DeepEqual(ids(h.items), []string{"1", "2"}) ||
				!reflect.DeepEqual(f.listSources[0], []txmdocuments.SourceType{source}) || len(f.downloads) != 2 {
				t.Fatalf(
					"unexpected scope: items=%v sources=%v downloads=%d",
					ids(h.items),
					f.listSources,
					len(f.downloads),
				)
			}
			mapped, err := mapEntry(otherSource)
			if err != nil || inScope([]string{selection}, mapped, otherSource.Sources[0].GetHierarchyPath()) {
				t.Fatal("provider selection leaked into another source")
			}
			added := inDirs(entryFor(5, source), "tree:team", "folder:design", "folder:new")
			selected.Sources[0].HierarchyPath[1].DisplayName = proto.String("Renamed")
			moved := inDirs(proto.Clone(nested).(*txmdocuments.Entry), "tree:other", "folder:design")
			f.pages[""] = txmdocuments.Page{Documents: []*txmdocuments.Entry{selected, moved, otherTree, added}}
			next, h := syncOnce(t, f, baseline, selection)
			if !reflect.DeepEqual(ids(h.items), []string{"1", "-2", "5"}) {
				t.Fatalf("rename, move and future child: %v", ids(h.items))
			}
			f.pages[""] = txmdocuments.Page{Documents: []*txmdocuments.Entry{selected, nested, otherTree, added}}
			_, h = syncOnce(t, f, next, selection)
			if !reflect.DeepEqual(ids(h.items), []string{"2"}) {
				t.Fatalf("move back: %v", ids(h.items))
			}
		})
	}
}

func TestProviderPathValidationAndSourceUnion(t *testing.T) {
	github := directoryID("github", []string{"repo:Tranxmart/tranxmart", "docs/design"})
	feishu := directoryID("feishu", []string{"space:team", "node:design"})
	ids, sources, err := parseConfig(config(github, feishu, github))
	if err != nil || len(ids) != 2 || !slices.Contains(sources, txmdocuments.SourceGitHub) ||
		!slices.Contains(sources, txmdocuments.SourceFeishu) || len(sources) != 2 {
		t.Fatalf("selection union: %v %v %v", ids, sources, err)
	}
	for _, id := range []string{
		"path:github:not-base64!", "path:unknown:W10", "path:github:W10", "path:github:bnVsbA",
		directoryID("github", []string{""}), directoryID("github", []string{"x", "x"}),
		directoryID("github", []string{strings.Repeat("x", 513)}),
		directoryID("github", []string{"bad\x00key"}),
		directoryID("github", strings.Split(strings.Repeat("x,", 64)+"y", ",")),
	} {
		if _, _, err := parseConfig(config(id)); !errors.Is(err, datasource.ErrInvalidConfig) {
			t.Fatalf("accepted invalid directory %q: %v", id, err)
		}
	}
}

func TestTeamSelectionsKeepIndependentInventories(t *testing.T) {
	otherDir := "30000000-0000-4000-8000-000000000002"
	f := newFake(
		inDirs(entryFor(1, txmdocuments.SourceManual), testDir),
		inDirs(entryFor(2, txmdocuments.SourceGitHub), "repo:docs", "team:a"),
		inDirs(entryFor(3, txmdocuments.SourceManual), otherDir),
		inDirs(entryFor(4, txmdocuments.SourceGitHub), "repo:docs", "team:b"),
	)
	c := NewConnectorWithClient(f)
	var baselineA *types.SyncCursor
	for _, team := range []struct {
		manual string
		path   string
		want   []string
	}{
		{testDir, "team:a", []string{"1", "2"}},
		{otherDir, "team:b", []string{"3", "4"}},
	} {
		h := &fakeHandler{}
		next, err := c.FetchStream(context.Background(), config(
			dirPrefix+team.manual, directoryID("github", []string{"repo:docs", team.path}),
		), nil, h)
		if err != nil || !reflect.DeepEqual(ids(h.items), team.want) || len(mustCursor(t, next).Documents) != 2 {
			t.Fatalf("team %s: items=%v err=%v", team.path, ids(h.items), err)
		}
		if baselineA == nil {
			baselineA = next
		}
	}
	// A run for another data source on the shared connector cannot change A's cursor.
	_, h := syncOnce(t, f, baselineA, dirPrefix+testDir, directoryID("github", []string{"repo:docs", "team:a"}))
	if len(h.items) != 0 {
		t.Fatalf("another team's sync changed A's inventory: %v", ids(h.items))
	}
}
