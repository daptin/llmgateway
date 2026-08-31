package contract

type EmbeddingInput struct {
	Texts  []string
	Tokens [][]int64
}

type EmbeddingsRequest struct {
	Input          EmbeddingInput
	Dimensions     int
	EncodingFormat string
	User           string
}

type EmbeddingsResponse struct {
	Data []Embedding
}

type Embedding struct {
	Index  int
	Vector []float64
	Base64 string
}
