package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const publishedWikiPageSize = 20

func (h *WikiPageHandler) ListPublishedEntries(c *gin.Context) {
	kbID, offset, ok := h.publishedWikiPage(c)
	if !ok {
		return
	}
	folderID := c.Query("folder_id")
	if folderID != "" && !validPublishedWikiID(folderID) {
		c.Status(http.StatusBadRequest)
		return
	}
	entries, err := h.wikiService.ListPublishedWikiEntries(c.Request.Context(), kbID, folderID, offset, publishedWikiPageSize+1)
	if err != nil {
		writePublishedWikiError(c, err)
		return
	}
	folders := min(len(entries.Folders), publishedWikiPageSize)
	articles := min(len(entries.Articles), publishedWikiPageSize-folders)
	c.JSON(http.StatusOK, gin.H{
		"folders": entries.Folders[:folders], "articles": entries.Articles[:articles],
		"has_more": len(entries.Folders)+len(entries.Articles) > publishedWikiPageSize,
	})
}

func (h *WikiPageHandler) SearchPublished(c *gin.Context) {
	kbID, offset, ok := h.publishedWikiPage(c)
	if !ok {
		return
	}
	query := strings.TrimSpace(c.Query("q"))
	pageType := c.Query("page_type")
	if query == "" || utf8.RuneCountInString(query) > 2000 ||
		(pageType != "" && !types.IsValidWikiPageType(pageType)) {
		c.Status(http.StatusBadRequest)
		return
	}
	hits, err := h.wikiService.SearchPublishedWiki(c.Request.Context(), kbID, query, pageType, offset, publishedWikiPageSize+1)
	if err != nil {
		writePublishedWikiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"hits": hits[:min(len(hits), publishedWikiPageSize)], "has_more": len(hits) > publishedWikiPageSize,
	})
}

func (h *WikiPageHandler) GetPublishedRevision(c *gin.Context) {
	kbID, _, err := h.validateWikiKB(c)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	articleID := c.Param("article_id")
	revision, err := strconv.ParseInt(c.Param("revision"), 10, 32)
	if !validPublishedWikiID(articleID) || err != nil || revision < 1 {
		c.Status(http.StatusBadRequest)
		return
	}
	withContent := c.Request.Method == http.MethodGet
	markdown, err := h.wikiService.PublishedWikiRevision(c.Request.Context(), kbID, articleID, int(revision), withContent)
	if err != nil {
		writePublishedWikiError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	if !withContent {
		c.Status(http.StatusOK)
		return
	}
	c.Data(http.StatusOK, "text/markdown; charset=utf-8", []byte(markdown))
}

func (h *WikiPageHandler) publishedWikiPage(c *gin.Context) (string, int, bool) {
	kbID, _, err := h.validateWikiKB(c)
	if err != nil {
		c.Status(http.StatusNotFound)
		return "", 0, false
	}
	offset, err := strconv.ParseInt(c.DefaultQuery("offset", "0"), 10, 32)
	if err != nil || offset < 0 {
		c.Status(http.StatusBadRequest)
		return "", 0, false
	}
	return kbID, int(offset), true
}

func validPublishedWikiID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.String() == id
}

func writePublishedWikiError(c *gin.Context, err error) {
	if errors.Is(err, repository.ErrWikiPageNotFound) || errors.Is(err, repository.ErrWikiFolderNotFound) {
		c.Status(http.StatusNotFound)
		return
	}
	c.Status(http.StatusInternalServerError)
}
