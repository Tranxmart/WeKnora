package repository

import (
	"fmt"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestPublishedWikiRevision(t *testing.T) {
	db := setupWikiPagesTestDB(t)
	ctx := t.Context()
	repo := NewWikiPageRepository(db)
	page := makeWikiPage("base", "concept/camera", types.WikiPageTypeConcept, types.WikiPageStatusPublished)
	page.Version = 3
	page.Content = "# Current\n"
	require.NoError(t, repo.Create(ctx, page))
	for _, rev := range []types.WikiPageRevision{
		{ID: uuid.NewString(), PageID: page.ID, KnowledgeBaseID: "base", Version: 1, Status: "draft", Content: "Draft"},
		{ID: uuid.NewString(), PageID: page.ID, KnowledgeBaseID: "base", Version: 2, Status: "published", Content: "# Prior\n"},
	} {
		require.NoError(t, db.Create(&rev).Error)
	}
	for version, want := range map[int]string{2: "# Prior\n", 3: "# Current\n"} {
		got, err := repo.PublishedWikiRevision(ctx, "base", page.ID, version, true)
		require.NoError(t, err)
		require.Equal(t, want, got)
		got, err = repo.PublishedWikiRevision(ctx, "base", page.ID, version, false)
		require.NoError(t, err)
		require.Empty(t, got)
	}
	for _, version := range []int{1, 4} {
		_, err := repo.PublishedWikiRevision(ctx, "base", page.ID, version, true)
		require.ErrorIs(t, err, ErrWikiPageNotFound)
	}
	_, err := repo.PublishedWikiRevision(ctx, "other", page.ID, 2, true)
	require.ErrorIs(t, err, ErrWikiPageNotFound)
	require.NoError(t, db.Model(page).Update("status", types.WikiPageStatusArchived).Error)
	_, err = repo.PublishedWikiRevision(ctx, "base", page.ID, 2, true)
	require.ErrorIs(t, err, ErrWikiPageNotFound)
	require.NoError(t, db.Model(page).Update("status", types.WikiPageStatusPublished).Error)
	require.NoError(t, db.Delete(page).Error)
	_, err = repo.PublishedWikiRevision(ctx, "base", page.ID, 2, true)
	require.ErrorIs(t, err, ErrWikiPageNotFound)
}

func TestPublishedWikiEntriesAndSearch(t *testing.T) {
	db := setupWikiPagesTestDB(t)
	ctx := t.Context()
	repo := NewWikiPageRepository(db)
	folder := &types.WikiFolder{ID: uuid.NewString(), KnowledgeBaseID: "base", Name: "Lenses", Path: "Lenses"}
	child := &types.WikiFolder{ID: uuid.NewString(), KnowledgeBaseID: "base", Name: "Prime", ParentID: folder.ID, Path: "Lenses/Prime"}
	require.NoError(t, db.Create(folder).Error)
	require.NoError(t, db.Create(child).Error)
	for i := range 24 {
		page := makeWikiPage("base", fmt.Sprintf("concept/camera-%02d", i), types.WikiPageTypeConcept, types.WikiPageStatusPublished)
		page.Content = "body-only punct.il"
		require.NoError(t, repo.Create(ctx, page))
	}
	draft := makeWikiPage("base", "concept/punct.il-draft", types.WikiPageTypeConcept, types.WikiPageStatusDraft)
	require.NoError(t, repo.Create(ctx, draft))
	entity := makeWikiPage("base", "entity/punct.il", types.WikiPageTypeEntity, types.WikiPageStatusPublished)
	require.NoError(t, repo.Create(ctx, entity))

	entries, err := repo.ListPublishedWikiEntries(ctx, "base", "", 0, 20)
	require.NoError(t, err)
	require.Equal(t, []types.PublishedWikiFolder{{ID: folder.ID, Name: folder.Name, HasChildren: true}}, entries.Folders)
	require.Len(t, entries.Articles, 19)
	second, err := repo.ListPublishedWikiEntries(ctx, "base", "", 20, 20)
	require.NoError(t, err)
	require.Empty(t, second.Folders)
	require.Len(t, second.Articles, 6)
	_, err = repo.ListPublishedWikiEntries(ctx, "other", folder.ID, 0, 20)
	require.ErrorIs(t, err, ErrWikiFolderNotFound)

	hits, err := repo.SearchPublishedWiki(ctx, "base", "punct.il", "", 0, 2)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	require.Equal(t, entity.ID, hits[0].ID)
	first, err := repo.SearchPublishedWiki(ctx, "base", "punct.il", "concept", 0, 20)
	require.NoError(t, err)
	secondHits, err := repo.SearchPublishedWiki(ctx, "base", "punct.il", "concept", 20, 20)
	require.NoError(t, err)
	require.Len(t, first, 20)
	require.Len(t, secondHits, 4)
	seen := make(map[string]bool, 24)
	for _, hit := range append(first, secondHits...) {
		require.False(t, seen[hit.ID])
		seen[hit.ID] = true
		require.Equal(t, "concept", hit.PageType)
	}
	require.False(t, seen[draft.ID])
}
