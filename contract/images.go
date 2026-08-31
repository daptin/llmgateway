package contract

type ImageGenerationRequest struct {
	Prompt         string
	N              int
	Size           string
	Quality        string
	ResponseFormat string
}

type ImageGenerationResponse struct {
	Created int64
	Data    []GeneratedImage
}

type GeneratedImage struct {
	URL           string
	Base64        string
	RevisedPrompt string
}
