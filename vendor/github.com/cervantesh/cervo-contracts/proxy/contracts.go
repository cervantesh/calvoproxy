package proxy

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Temperature float32       `json:"temperature,omitempty"`
}

type ChatChoice struct {
	Message ChatMessage `json:"message"`
}

type ChatResponse struct {
	Choices []ChatChoice `json:"choices"`
	Error   interface{}  `json:"error,omitempty"`
}

type EmbeddingRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type EmbeddingDatumFloat32 struct {
	Embedding []float32 `json:"embedding"`
}

type EmbeddingDatumFloat64 struct {
	Embedding []float64 `json:"embedding"`
}

type EmbeddingResponseFloat32 struct {
	Data []EmbeddingDatumFloat32 `json:"data"`
}

type EmbeddingResponseFloat64 struct {
	Data []EmbeddingDatumFloat64 `json:"data"`
}
