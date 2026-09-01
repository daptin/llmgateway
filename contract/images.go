package contract

type ImageGenerationRequest struct {
	Prompt         string
	N              int
	Size           string
	Quality        string
	ResponseFormat string
}

type ImageEditRequest struct {
	Images            []MediaFile
	Mask              *MediaFile
	Prompt            string
	N                 int
	Size              string
	Quality           string
	ResponseFormat    string
	User              string
	Background        string
	InputFidelity     string
	OutputFormat      string
	OutputCompression *int
	Moderation        string
}

type ImageVariationRequest struct {
	Image          MediaFile
	N              int
	Size           string
	ResponseFormat string
	User           string
}

type ImageResponse struct {
	Created int64
	Data    []GeneratedImage
}

type GeneratedImage struct {
	URL           string
	Base64        string
	RevisedPrompt string
}
