package contract

type SearchRequest struct {
	Queries          []string
	MaxResults       int
	DomainFilter     []string
	MaxTokensPerPage int
	Country          string
}

type SearchResponse struct {
	Results []SearchResult
}

type SearchResult struct {
	Title       string
	URL         string
	Snippet     string
	Date        string
	LastUpdated string
}
