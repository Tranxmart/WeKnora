package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

const publishedWikiMetadata = "id, title, slug, page_type, version"

func (r *wikiPageRepository) ListPublishedWikiEntries(
	ctx context.Context, kbID, folderID string, offset, limit int,
) (types.PublishedWikiEntries, error) {
	var entries types.PublishedWikiEntries
	folders := r.db.WithContext(ctx).Model(&types.WikiFolder{}).
		Where("knowledge_base_id = ? AND parent_id = ?", kbID, folderID)
	if folderID != types.WikiFolderRootID {
		if _, err := r.GetFolderByID(ctx, kbID, folderID); err != nil {
			return entries, err
		}
	}
	var folderCount int64
	if err := folders.Count(&folderCount).Error; err != nil {
		return entries, err
	}
	if int64(offset) < folderCount {
		const fields = `id, name, EXISTS (
			SELECT 1 FROM wiki_folders child
			WHERE child.knowledge_base_id = wiki_folders.knowledge_base_id
			  AND child.parent_id = wiki_folders.id AND child.deleted_at IS NULL
		) AS has_children`
		err := folders.Select(fields).Order("name ASC, id ASC").
			Offset(offset).Limit(limit).Find(&entries.Folders).Error
		if err != nil {
			return entries, err
		}
	}
	remaining := limit - len(entries.Folders)
	if remaining == 0 {
		return entries, nil
	}
	err := r.db.WithContext(ctx).Model(&types.WikiPage{}).
		Select(publishedWikiMetadata).
		Where("knowledge_base_id = ? AND folder_id = ? AND status = ?", kbID, folderID, types.WikiPageStatusPublished).
		Order("title ASC, id ASC").Offset(max(0, offset-int(folderCount))).Limit(remaining).
		Find(&entries.Articles).Error
	return entries, err
}

func (r *wikiPageRepository) SearchPublishedWiki(
	ctx context.Context, kbID, query, pageType string, offset, limit int,
) ([]types.PublishedWikiArticle, error) {
	query = r.literalWikiSearchQuery(query)
	db := r.rankedWikiSearch(ctx, []string{kbID}, query, publishedWikiMetadata).
		Where("status = ?", types.WikiPageStatusPublished)
	if pageType != "" {
		db = db.Where("page_type = ?", pageType)
	}
	var hits []types.PublishedWikiArticle
	err := db.Offset(offset).Limit(limit).Find(&hits).Error
	return hits, err
}

// PublishedWikiRevision resolves current and retained revisions from one SQL
// snapshot. The page must also be published now, even for a historical read.
func (r *wikiPageRepository) PublishedWikiRevision(
	ctx context.Context, kbID, pageID string, version int, withContent bool,
) (string, error) {
	projection := "''"
	if withContent {
		projection = "CASE WHEN page.version = ? THEN page.content ELSE rev.content END"
	}
	args := make([]any, 0, 8)
	if withContent {
		args = append(args, version)
	}
	args = append(args, version, kbID, pageID, types.WikiPageStatusPublished, version, types.WikiPageStatusPublished)
	row := r.db.WithContext(ctx).Raw(`
		SELECT `+projection+`
		FROM wiki_pages page
		LEFT JOIN wiki_page_revisions rev
		  ON rev.knowledge_base_id = page.knowledge_base_id
		  AND rev.page_id = page.id AND rev.version = ?
		WHERE page.knowledge_base_id = ? AND page.id = ? AND page.status = ?
		  AND page.deleted_at IS NULL
		  AND (page.version = ? OR rev.status = ?)
	`, args...).Row()
	var content string
	if err := row.Scan(&content); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrWikiPageNotFound
		}
		return "", err
	}
	return content, nil
}

func (r *wikiPageRepository) rankedWikiSearch(ctx context.Context, kbIDs []string, query, columns string) *gorm.DB {
	op, withEscape := r.wikiSearchMatchOp()
	pattern := query
	if withEscape {
		pattern = "%" + escapeLikePattern(query) + "%"
	}
	pred := func(column string) string {
		return wikiSearchPredicate(column, op, withEscape)
	}
	rankExpr := "CASE " +
		"WHEN " + pred("title") + " THEN 4 " +
		"WHEN " + pred("slug") + " THEN 3 " +
		"WHEN " + pred("summary") + " THEN 2 " +
		"WHEN " + pred("content") + " THEN 1 " +
		"ELSE 0 END AS match_rank"
	matchClause := "(" + pred("title") + " OR " + pred("content") +
		" OR " + pred("summary") + " OR " + pred("slug") + ")"
	matchArgs := wikiSearchMatchArgs(pattern, likeEscapeChar, withEscape, 4)
	whereArgs := append([]any{kbIDs}, matchArgs...)
	return r.db.WithContext(ctx).Model(&types.WikiPage{}).
		Select(columns+", "+rankExpr, matchArgs...).
		Where("knowledge_base_id IN ? AND "+matchClause, whereArgs...).
		Order("match_rank DESC, updated_at DESC, id ASC")
}
