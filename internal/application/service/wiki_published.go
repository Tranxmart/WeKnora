package service

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types"
)

func (s *wikiPageService) ListPublishedWikiEntries(ctx context.Context, kbID, folderID string, offset, limit int) (types.PublishedWikiEntries, error) {
	return s.repo.ListPublishedWikiEntries(ctx, kbID, folderID, offset, limit)
}

func (s *wikiPageService) SearchPublishedWiki(ctx context.Context, kbID, query, pageType string, offset, limit int) ([]types.PublishedWikiArticle, error) {
	return s.repo.SearchPublishedWiki(ctx, kbID, query, pageType, offset, limit)
}

func (s *wikiPageService) PublishedWikiRevision(ctx context.Context, kbID, pageID string, version int, withContent bool) (string, error) {
	return s.repo.PublishedWikiRevision(ctx, kbID, pageID, version, withContent)
}
