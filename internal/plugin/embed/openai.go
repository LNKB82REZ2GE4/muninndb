package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/scrypster/muninndb/internal/plugin"
)

// Cloud-tuned defaults, used whenever ProviderHTTPConfig leaves Timeout/
// MaxBatchSize at zero. A self-hosted OpenAI-compatible endpoint (e.g. a local
// GPU embedder) can override both via MUNINN_OPENAI_EMBED_TIMEOUT and
// MUNINN_OPENAI_EMBED_MAX_BATCH (see cmd/muninn/server.go buildEmbedder).
const (
	defaultOpenAITimeout      = 10 * time.Second
	defaultOpenAIMaxBatchSize = 2048
)

type OpenAIProvider struct {
	client       *http.Client
	baseURL      string
	model        string
	apiKey       string
	timeout      time.Duration
	maxBatchSize int
}

type openAIEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type openAIEmbedData struct {
	Embedding []float32 `json:"embedding"`
	Index     int       `json:"index"`
}

type openAIEmbedResponse struct {
	Data []openAIEmbedData `json:"data"`
}

func (p *OpenAIProvider) Name() string {
	return "openai"
}

func (p *OpenAIProvider) Init(ctx context.Context, cfg ProviderHTTPConfig) (int, error) {
	p.baseURL = cfg.BaseURL
	p.model = cfg.Model
	p.apiKey = cfg.APIKey

	if p.apiKey == "" {
		return 0, fmt.Errorf("API authentication failed — check MUNINNDB_EMBED_API_KEY")
	}

	p.timeout = cfg.HTTPTimeout
	if p.timeout <= 0 {
		p.timeout = defaultOpenAITimeout
	}
	p.maxBatchSize = cfg.MaxBatchSize
	if p.maxBatchSize <= 0 {
		p.maxBatchSize = defaultOpenAIMaxBatchSize
	}
	slog.Info("openai embed provider HTTP config resolved",
		"base_url", p.baseURL, "timeout", p.timeout, "max_batch_size", p.maxBatchSize)

	// Create HTTP client. Timeout defaults to the cloud-tuned 10s but is
	// overridable (MUNINN_OPENAI_EMBED_TIMEOUT) for self-hosted endpoints that
	// need longer, e.g. a GPU embedder processing a large batch.
	transport := &http.Transport{
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	p.client = &http.Client{
		Timeout:   p.timeout,
		Transport: plugin.WrapTransport(transport),
	}

	// Embed probe text to detect dimension
	probeCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	reqBody, _ := json.Marshal(openAIEmbedRequest{
		Model: p.model,
		Input: []string{"dimension detection probe"},
	})

	req, err := http.NewRequestWithContext(probeCtx, "POST",
		p.baseURL+"/v1/embeddings", bytes.NewReader(reqBody))
	if err != nil {
		return 0, fmt.Errorf("cannot create embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", p.apiKey))

	resp, err := p.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("API authentication failed — check MUNINNDB_EMBED_API_KEY (%w)", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("API authentication failed — check MUNINNDB_EMBED_API_KEY (status %d: %s)",
			resp.StatusCode, string(bodyBytes))
	}

	var openaiResp openAIEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&openaiResp); err != nil {
		return 0, fmt.Errorf("failed to decode OpenAI response: %w", err)
	}

	if len(openaiResp.Data) == 0 {
		return 0, fmt.Errorf("OpenAI returned no embeddings")
	}

	dim := len(openaiResp.Data[0].Embedding)
	if dim == 0 {
		return 0, fmt.Errorf("OpenAI returned empty embedding")
	}

	slog.Info("OpenAI dimension probe successful", "dimension", dim)

	return dim, nil
}

func (p *OpenAIProvider) EmbedBatch(ctx context.Context, texts []string) ([]float32, error) {
	reqBody, _ := json.Marshal(openAIEmbedRequest{
		Model: p.model,
		Input: texts,
	})

	req, err := http.NewRequestWithContext(ctx, "POST",
		p.baseURL+"/v1/embeddings", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("openai embed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", p.apiKey))

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, providerTransportError("openai", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, providerHTTPError("openai", resp)
	}

	var openaiResp openAIEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&openaiResp); err != nil {
		return nil, fmt.Errorf("openai decode: %w", err)
	}

	// Sort by Index field to handle out-of-order API responses (allowed by spec).
	sort.Slice(openaiResp.Data, func(i, j int) bool {
		return openaiResp.Data[i].Index < openaiResp.Data[j].Index
	})
	result := make([]float32, 0)
	for _, data := range openaiResp.Data {
		result = append(result, data.Embedding...)
	}

	return result, nil
}

func (p *OpenAIProvider) MaxBatchSize() int {
	if p.maxBatchSize > 0 {
		return p.maxBatchSize
	}
	return defaultOpenAIMaxBatchSize
}

func (p *OpenAIProvider) Close() error {
	if p.client != nil {
		p.client.CloseIdleConnections()
	}
	return nil
}
