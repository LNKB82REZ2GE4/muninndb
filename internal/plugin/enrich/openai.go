package enrich

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/scrypster/muninndb/internal/plugin"
)

// OpenAILLMProvider is an HTTP client for OpenAI's /v1/chat/completions endpoint.
type OpenAILLMProvider struct {
	client    *http.Client
	baseURL   string
	model     string
	apiKey    string
	maxTokens int
}

// openaiChatRequest is the request structure for OpenAI chat API.
type openaiChatRequest struct {
	Model          string                `json:"model"`
	Messages       []openaiMessage       `json:"messages"`
	Temperature    float32               `json:"temperature"`
	MaxTokens      int                   `json:"max_tokens,omitempty"`
	ResponseFormat *openaiResponseFormat `json:"response_format,omitempty"`
}

// openaiMessage is a message in the OpenAI chat API.
type openaiMessage struct {
	Role             string          `json:"role"`
	Content          string          `json:"content"`
	Reasoning        json.RawMessage `json:"reasoning,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"` // LM Studio/OpenAI-compatible field
}

// openaiResponseFormat specifies JSON response format for OpenAI.
type openaiResponseFormat struct {
	Type string `json:"type"`
}

// openaiChatResponse is the response structure from OpenAI chat API.
type openaiChatResponse struct {
	Choices []struct {
		Message openaiMessage `json:"message"`
	} `json:"choices"`
}

// NewOpenAILLMProvider creates a new OpenAI provider.
func NewOpenAILLMProvider() *OpenAILLMProvider {
	return &OpenAILLMProvider{
		client: &http.Client{
			Timeout:   300 * time.Second,
			Transport: plugin.WrapTransport(nil),
		},
	}
}

// Name returns the provider name.
func (p *OpenAILLMProvider) Name() string {
	return "openai"
}

// Init initializes the provider and validates connectivity.
func (p *OpenAILLMProvider) Init(ctx context.Context, cfg LLMProviderConfig) error {
	p.baseURL = cfg.BaseURL
	p.model = cfg.Model
	p.apiKey = cfg.APIKey
	p.maxTokens = cfg.MaxTokens
	if p.maxTokens <= 0 {
		p.maxTokens = 16384
	}

	if p.apiKey == "" {
		return fmt.Errorf("openai provider requires API key")
	}

	// Send a probe completion request to validate connectivity.
	// The user message must contain the word "json" because Complete always
	// sets response_format:json_object — OpenAI rejects requests where none
	// of the messages mention json when that format is requested.
	_, err := p.Complete(ctx, "You are a connectivity probe. Respond with valid JSON only.", `{"ok":true}`)
	if err != nil {
		return fmt.Errorf("openai connectivity check failed: %w", err)
	}

	return nil
}

// Complete sends a chat completion request to OpenAI.
func (p *OpenAILLMProvider) Complete(ctx context.Context, system, user string) (string, error) {
	formats := []*openaiResponseFormat{{Type: "json_object"}, nil}
	var lastErr error
	maxTokens := p.maxTokens
	if maxTokens <= 0 {
		maxTokens = 16384
	}

	for i, format := range formats {
		req := openaiChatRequest{
			Model:       p.model,
			Temperature: 0.0,
			MaxTokens:   maxTokens,
			Messages: []openaiMessage{
				{Role: "system", Content: system},
				{Role: "user", Content: user},
			},
			ResponseFormat: format,
		}

		body, err := json.Marshal(req)
		if err != nil {
			return "", fmt.Errorf("failed to marshal request: %w", err)
		}

		httpReq, err := http.NewRequestWithContext(
			ctx,
			http.MethodPost,
			p.baseURL+"/v1/chat/completions",
			bytes.NewReader(body),
		)
		if err != nil {
			return "", fmt.Errorf("failed to create request: %w", err)
		}

		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

		resp, err := p.client.Do(httpReq)
		if err != nil {
			return "", providerTransportError(p.Name(), err)
		}

		if resp.StatusCode != http.StatusOK {
			// Some OpenAI-compatible backends (e.g. LM Studio) reject
			// json_object. Peek a bounded snippet to detect that case and
			// retry once without response_format; providerHTTPError drains
			// and discards the remainder of the body.
			peek := make([]byte, 4<<10)
			n, _ := io.ReadFull(resp.Body, peek)
			retryable := i == 0 && resp.StatusCode == http.StatusBadRequest &&
				strings.Contains(strings.ToLower(string(peek[:n])), "response_format")
			httpErr := providerHTTPError(p.Name(), resp)
			resp.Body.Close()
			if retryable {
				lastErr = httpErr
				continue
			}
			return "", httpErr
		}

		var chatResp openaiChatResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&chatResp)
		resp.Body.Close()
		if decodeErr != nil {
			return "", fmt.Errorf("failed to parse response: %w", decodeErr)
		}

		if len(chatResp.Choices) == 0 {
			return "", fmt.Errorf("openai response has no choices")
		}

		msg := chatResp.Choices[0].Message
		if content := strings.TrimSpace(msg.Content); content != "" {
			return content, nil
		}
		reasoning, err := reasoningPayload(msg.Reasoning)
		if err != nil {
			return "", fmt.Errorf("failed to parse openai reasoning payload: %w", err)
		}
		if reasoning != "" {
			return reasoning, nil
		}
		if rc := strings.TrimSpace(msg.ReasoningContent); rc != "" {
			return rc, nil
		}

		return "", fmt.Errorf("openai response has no content or reasoning")
	}

	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("openai completion failed")
}

func reasoningPayload(raw json.RawMessage) (string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil
	}
	if strings.HasPrefix(trimmed, `"`) {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", err
		}
		return strings.TrimSpace(value), nil
	}
	return trimmed, nil
}

// Close releases HTTP connections.
func (p *OpenAILLMProvider) Close() error {
	p.client.CloseIdleConnections()
	return nil
}
