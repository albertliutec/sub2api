package trae

// ContentPart is the Anthropic-style content block accepted by Trae's
// llm_raw_chat endpoint. Trae currently understands text and data-URL images.
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
	VideoURL *VideoURL `json:"video_url,omitempty"`
}

type ImageURL struct {
	URL string `json:"url"`
}

type VideoURL struct {
	URL string `json:"url"`
}

// Message is the Trae-native message shape.
type Message struct {
	Role    string        `json:"role"`
	Content []ContentPart `json:"content"`
}

// ModelInfo is one entry of Trae's dynamic model registry.
type ModelInfo struct {
	ConfigName      string
	ModelName       string
	PromptMaxTokens int
	MaxTokens       int
	Multimodal      bool
	Invisible       bool
}

// Usage follows Trae's token_usage event. CompletionTokens already includes
// reasoning tokens, so callers must not add ReasoningTokens again.
type Usage struct {
	PromptTokens         int
	CompletionTokens     int
	TotalTokens          int
	ReasoningTokens      int
	CacheReadInputTokens int
}

// Outcome is the aggregate of one llm_raw_chat stream.
type Outcome struct {
	ConfigName   string
	ModelName    string
	FinishReason string
	Text         string
	Reasoning    string
	Usage        Usage
}

// DeltaFunc receives incremental text and reasoning output. Returning an error
// stops consumption (used when the downstream client disconnects).
type DeltaFunc func(text, reasoning string) error
