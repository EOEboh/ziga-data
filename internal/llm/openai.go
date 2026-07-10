package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
)

// OpenAIExtractor implements Extractor using the OpenAI Chat Completions API
// with strict structured outputs, so the response is guaranteed to be valid
// JSON matching the configured schema.
type OpenAIExtractor struct {
	client       openai.Client
	model        shared.ChatModel
	systemPrompt string
	jsonSchema   map[string]any
	// userText renders the user turn for a submission (text + date framing).
	userText func(text string, in Input) string
}

// NewOpenAIExtractor builds the extractor. The system prompt, JSON schema,
// and user-turn renderer come from the extract package so this file stays
// free of schema-specific knowledge.
func NewOpenAIExtractor(model, systemPrompt string, jsonSchema map[string]any, userText func(text string, in Input) string) *OpenAIExtractor {
	return &OpenAIExtractor{
		client:       openai.NewClient(), // reads OPENAI_API_KEY
		model:        shared.ChatModel(model),
		systemPrompt: systemPrompt,
		jsonSchema:   jsonSchema,
		userText:     userText,
	}
}

func (o *OpenAIExtractor) Extract(ctx context.Context, in Input) (*Result, error) {
	var parts []openai.ChatCompletionContentPartUnionParam
	if len(in.Image) > 0 {
		dataURL := fmt.Sprintf("data:%s;base64,%s",
			in.ImageMediaType, base64.StdEncoding.EncodeToString(in.Image))
		parts = append(parts, openai.ImageContentPart(
			openai.ChatCompletionContentPartImageImageURLParam{URL: dataURL},
		))
	}
	parts = append(parts, openai.TextContentPart(o.userText(in.Text, in)))

	resp, err := o.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: o.model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(o.systemPrompt),
			openai.UserMessage(parts),
		},
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   "lead_extraction",
					Strict: openai.Bool(true),
					Schema: o.jsonSchema,
				},
			},
		},
		// Extraction doesn't need deep reasoning; reasoning tokens bill as
		// output, so keep them at the floor. gpt-5.4 models accept
		// none/low/medium/high/xhigh — "minimal" is rejected.
		ReasoningEffort:     shared.ReasoningEffortNone,
		MaxCompletionTokens: openai.Int(1024),
	})
	if err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("openai: empty response")
	}
	msg := resp.Choices[0].Message
	if msg.Refusal != "" {
		return nil, fmt.Errorf("openai: model refused the request: %s", msg.Refusal)
	}
	if msg.Content == "" {
		return nil, fmt.Errorf("openai: response contained no content (finish_reason=%s)", resp.Choices[0].FinishReason)
	}

	var res Result
	if err := json.Unmarshal([]byte(msg.Content), &res); err != nil {
		return nil, fmt.Errorf("openai: parse structured output: %w", err)
	}
	return &res, nil
}
