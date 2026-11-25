package types

// PublishedWikiArticle is the wiki metadata available to an external reader.
type PublishedWikiArticle struct {
	ID       string `json:"article_id"`
	Title    string `json:"title"`
	Slug     string `json:"slug"`
	PageType string `json:"page_type"`
	Version  int    `json:"revision"`
}

type PublishedWikiFolder struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	HasChildren bool   `json:"has_children"`
}

type PublishedWikiEntries struct {
	Folders  []PublishedWikiFolder  `json:"folders"`
	Articles []PublishedWikiArticle `json:"articles"`
}
