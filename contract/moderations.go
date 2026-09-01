package contract

type ModerationRequest struct {
	Input []ModerationInput
}

type ModerationInput struct {
	Type     string
	Text     string
	ImageURL *ImageURL
}

type ModerationResponse struct {
	ID      string
	Results []ModerationResult
}

type ModerationResult struct {
	Flagged        bool
	Categories     map[string]bool
	CategoryScores map[string]float64
}
