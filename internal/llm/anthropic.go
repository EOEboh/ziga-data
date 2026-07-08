package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
)

// AnthropicExtractor implements Extractor using the Claude API with
// structured outputs, so the response is guaranteed to be valid JSON
// matching the configured schema.
type AnthropicExtractor struct {
	client       anthropic.Client
	model        anthropic.Model
	systemPrompt string
	jsonSchema   map[string]any
	// userText renders the user turn for a submission (text + date framing).
	userText func(text string, in Input) string
}

// NewAnthropicExtractor builds the extractor. The system prompt, JSON schema,
// and user-turn renderer come from the extract package so this file stays
// free of schema-specific knowledge.
func NewAnthropicExtractor(model, systemPrompt string, jsonSchema map[string]any, userText func(text string, in Input) string) *AnthropicExtractor {
	return &AnthropicExtractor{
		client:       anthropic.NewClient(), // reads ANTHROPIC_API_KEY
		model:        anthropic.Model(model),
		systemPrompt: systemPrompt,
		jsonSchema:   jsonSchema,
		userText:     userText,
	}
}

func (a *AnthropicExtractor) Extract(ctx context.Context, in Input) (*Result, error) {
	var blocks []anthropic.ContentBlockParamUnion
	if len(in.Image) > 0 {
		blocks = append(blocks, anthropic.NewImageBlockBase64(
			in.ImageMediaType,
			base64.StdEncoding.EncodeToString(in.Image),
		))
	}
	blocks = append(blocks, anthropic.NewTextBlock(a.userText(in.Text, in)))

	resp, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     a.model,
		MaxTokens: 1024,
		System: []anthropic.TextBlockParam{
			{Text: a.systemPrompt, CacheControl: anthropic.NewCacheControlEphemeralParam()},
		},
		OutputConfig: anthropic.OutputConfigParam{
			Format: anthropic.JSONOutputFormatParam{Schema: a.jsonSchema},
		},
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(blocks...)},
	})
	if err != nil {
		return nil, fmt.Errorf("anthropic: %w", err)
	}
	if resp.StopReason == anthropic.StopReasonRefusal {
		return nil, fmt.Errorf("anthropic: model refused the request")
	}

	var text string
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			text = t.Text
			break
		}
	}
	if text == "" {
		return nil, fmt.Errorf("anthropic: response contained no text block (stop_reason=%s)", resp.StopReason)
	}

	var res Result
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		return nil, fmt.Errorf("anthropic: parse structured output: %w", err)
	}
	return &res, nil
}
