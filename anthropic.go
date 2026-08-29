package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const anthropicVersion = "2023-06-01"

// AnthropicMessage is one turn in the conversation sent to the model. Content is
// a heterogeneous list of blocks (text, tool_use, tool_result).
type AnthropicMessage struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// ContentBlock covers every block type used here. Empty fields are omitted so
// the same struct serialises correctly for text, tool_use and tool_result.
type ContentBlock struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"`

	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`

	// raw keeps the exact JSON of a block received from the API. Blocks such as
	// thinking must be echoed back byte-for-byte in the next request, so decoded
	// blocks re-marshal to their original bytes.
	raw json.RawMessage
}

func (c *ContentBlock) UnmarshalJSON(b []byte) error {
	type alias ContentBlock
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*c = ContentBlock(a)
	c.raw = append(json.RawMessage(nil), b...)
	return nil
}

func (c ContentBlock) MarshalJSON() ([]byte, error) {
	if len(c.raw) > 0 {
		return c.raw, nil
	}
	type alias ContentBlock
	return json.Marshal(alias(c))
}

type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type AnthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []AnthropicMessage `json:"messages"`
	Tools     []ToolDef          `json:"tools,omitempty"`
}

type AnthropicResponse struct {
	ID         string         `json:"id"`
	Role       string         `json:"role"`
	Content    []ContentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
}

// AnthropicClient is the seam that lets tests swap in a fake brain.
type AnthropicClient interface {
	CreateMessage(ctx context.Context, req AnthropicRequest) (*AnthropicResponse, error)
}

type httpAnthropicClient struct {
	apiKey string
	client *http.Client
	base   string
}

func NewAnthropicClient(apiKey string) AnthropicClient {
	return &httpAnthropicClient{
		apiKey: apiKey,
		client: &http.Client{
			Timeout: 45 * time.Second,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     5 * time.Minute,
				ForceAttemptHTTP2:   true,
			},
		},
		base: "https://api.anthropic.com",
	}
}

func (c *httpAnthropicClient) CreateMessage(ctx context.Context, req AnthropicRequest) (*AnthropicResponse, error) {
	if c.apiKey == "" {
		return nil, fmt.Errorf("anthropic: no API key configured")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	start := time.Now()
	resp, err := c.client.Do(httpReq)
	metrics.Record("anthropic.messages", time.Since(start))
	if err != nil {
		metrics.RecordErr("anthropic.messages", err.Error())
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("anthropic: status %d: %s", resp.StatusCode, string(raw))
	}
	var out AnthropicResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func textBlock(s string) ContentBlock { return ContentBlock{Type: "text", Text: s} }

func userText(s string) AnthropicMessage {
	return AnthropicMessage{Role: "user", Content: []ContentBlock{textBlock(s)}}
}

func assistantText(s string) AnthropicMessage {
	return AnthropicMessage{Role: "assistant", Content: []ContentBlock{textBlock(s)}}
}
