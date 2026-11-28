package assistant

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ToolSpec defines an OpenAI-compatible function tool schema.
type ToolSpec struct {
	Type     string           `json:"type"`
	Function ToolFunctionSpec `json:"function"`
}

// ToolFunctionSpec describes the function name, description, and JSON Schema parameters.
type ToolFunctionSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ToolCall represents one assistant tool invocation.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction holds tool name and JSON arguments string.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// PromptPart allows attaching Anthropic/OpenRouter cache_control breakpoints to prefix blocks.
type PromptPart struct {
	Type         string         `json:"type"`
	Text         string         `json:"text"`
	CacheControl map[string]any `json:"cache_control,omitempty"`
}

// ChatMessage represents a message sent to OpenRouter /chat/completions.
type ChatMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// StreamUsage captures token and cost metrics returned by OpenRouter.
type StreamUsage struct {
	PromptTokens     int     `json:"prompt_tokens"`
	CachedTokens     int     `json:"cached_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// ChatTurnResult is the completed result of one streaming chat completion call.
type ChatTurnResult struct {
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        StreamUsage
}

// DecisionQuestion specifies one typed question for OpenRouter's Decisions API.
type DecisionQuestion struct {
	ID           string   `json:"id"`
	Type         string   `json:"type"` // "noul", "choice", "score"
	Instructions string   `json:"instructions,omitempty"`
	Prompt       string   `json:"prompt,omitempty"`
	Criteria     any      `json:"criteria,omitempty"`
	Options      []string `json:"options,omitempty"`
}

// DecisionAnswer captures the returned top choice and probabilities.
type DecisionAnswer struct {
	ID            string             `json:"id"`
	Value         string             `json:"value"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

type openRouterClient struct {
	http *http.Client
}

func newOpenRouterClient() *openRouterClient {
	return &openRouterClient{
		http: &http.Client{Timeout: 120 * time.Second},
	}
}

func applyOpenRouterHeaders(req *http.Request, apiKey string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("HTTP-Referer", "https://github.com/bdatdo0601/fleet")
	req.Header.Set("X-Title", "fleet-assistant")
}

func buildChatRequestBody(cfg Config, model string, msgs []ChatMessage, tools []ToolSpec, stream bool, maxTokens int) map[string]any {
	body := map[string]any{
		"model":    model,
		"messages": msgs,
		"stream":   stream,
	}
	if maxTokens > 0 {
		body["max_tokens"] = maxTokens
	}
	if len(tools) > 0 {
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	if stream {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if strings.Contains(strings.ToLower(cfg.BaseURL), "openrouter.ai") {
		prov := map[string]any{}
		if cfg.ZDR {
			prov["zdr"] = true
		}
		if cfg.DataCollection != "" {
			prov["data_collection"] = cfg.DataCollection
		}
		if len(prov) > 0 {
			body["provider"] = prov
		}
	}
	return body
}

// StreamChat performs a streaming chat completion request, calling onDelta for each text chunk.
func (c *openRouterClient) StreamChat(
	ctx context.Context,
	cfg Config,
	model string,
	msgs []ChatMessage,
	tools []ToolSpec,
	onDelta func(string),
) (ChatTurnResult, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return ChatTurnResult{}, newErr("assistant API key is not configured", http.StatusBadRequest)
	}
	payload := buildChatRequestBody(cfg, model, msgs, tools, true, 4096)
	rawBody, err := json.Marshal(payload)
	if err != nil {
		return ChatTurnResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL+"/chat/completions", bytes.NewReader(rawBody))
	if err != nil {
		return ChatTurnResult{}, err
	}
	applyOpenRouterHeaders(req, cfg.APIKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return ChatTurnResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		errBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return ChatTurnResult{}, fmt.Errorf("LLM HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBytes)))
	}
	return readChatSSE(resp.Body, onDelta)
}

type streamChunk struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int     `json:"prompt_tokens"`
		CompletionTokens    int     `json:"completion_tokens"`
		Cost                float64 `json:"cost"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

func readChatSSE(r io.Reader, onDelta func(string)) (ChatTurnResult, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

	var content strings.Builder
	var finishReason string
	var usage StreamUsage
	tcMap := map[int]*ToolCall{}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil {
			usage.PromptTokens = chunk.Usage.PromptTokens
			usage.CompletionTokens = chunk.Usage.CompletionTokens
			usage.CostUSD = chunk.Usage.Cost
			if chunk.Usage.PromptTokensDetails != nil {
				usage.CachedTokens = chunk.Usage.PromptTokensDetails.CachedTokens
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		ch := chunk.Choices[0]
		if ch.FinishReason != "" {
			finishReason = ch.FinishReason
		}
		if ch.Delta.Content != "" {
			content.WriteString(ch.Delta.Content)
			if onDelta != nil {
				onDelta(ch.Delta.Content)
			}
		}
		mergeToolCallDeltas(tcMap, ch.Delta.ToolCalls)
	}
	if err := scanner.Err(); err != nil {
		return ChatTurnResult{}, err
	}
	return ChatTurnResult{
		Content:      content.String(),
		ToolCalls:    sortedToolCalls(tcMap),
		FinishReason: finishReason,
		Usage:        usage,
	}, nil
}

func mergeToolCallDeltas(tcMap map[int]*ToolCall, deltas []struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
},
) {
	for _, tc := range deltas {
		cur, exists := tcMap[tc.Index]
		if !exists {
			cur = &ToolCall{Type: cmp.Or(tc.Type, "function")}
			tcMap[tc.Index] = cur
		}
		if tc.ID != "" {
			cur.ID = tc.ID
		}
		if tc.Type != "" {
			cur.Type = tc.Type
		}
		if tc.Function.Name != "" {
			cur.Function.Name += tc.Function.Name
		}
		if tc.Function.Arguments != "" {
			cur.Function.Arguments += tc.Function.Arguments
		}
	}
}

func sortedToolCalls(tcMap map[int]*ToolCall) []ToolCall {
	if len(tcMap) == 0 {
		return nil
	}
	indices := make([]int, 0, len(tcMap))
	for idx := range tcMap {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	out := make([]ToolCall, 0, len(indices))
	for _, idx := range indices {
		tc := *tcMap[idx]
		if tc.ID == "" {
			tc.ID = fmt.Sprintf("call_%d", idx)
		}
		out = append(out, tc)
	}
	return out
}

// CompleteText performs a non-streaming completion for Observer, Reflector, or connectivity checks.
func (c *openRouterClient) CompleteText(ctx context.Context, cfg Config, model, sysPrompt, userPrompt string, maxTokens int) (string, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return "", newErr("assistant API key is not configured", http.StatusBadRequest)
	}
	msgs := []ChatMessage{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: userPrompt},
	}
	payload := buildChatRequestBody(cfg, model, msgs, nil, false, maxTokens)
	rawBody, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL+"/chat/completions", bytes.NewReader(rawBody))
	if err != nil {
		return "", err
	}
	applyOpenRouterHeaders(req, cfg.APIKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("LLM HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBytes)))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return "", err
	}
	if len(parsed.Choices) == 0 {
		return "", errors.New("LLM returned no choices")
	}
	return parsed.Choices[0].Message.Content, nil
}

func decisionsEndpoint(baseURL string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base, ok := strings.CutSuffix(trimmed, "/api/v1"); ok {
		return base + "/api/alpha/decisions"
	}
	return trimmed + "/api/alpha/decisions"
}

func buildDecisionQuestionsMap(questions []DecisionQuestion) map[string]any {
	qMap := make(map[string]any, len(questions))
	for _, q := range questions {
		if q.ID == "" {
			continue
		}
		entry := map[string]any{
			"type":         cmp.Or(q.Type, "choice"),
			"instructions": cmp.Or(q.Instructions, q.Prompt),
		}
		if q.Criteria != nil {
			entry["criteria"] = q.Criteria
		} else if len(q.Options) > 0 {
			crit := make(map[string]string, len(q.Options))
			for _, opt := range q.Options {
				crit[opt] = opt
			}
			entry["criteria"] = crit
		}
		qMap[q.ID] = entry
	}
	return qMap
}

// Decide evaluates typed System-1 decisions via OpenRouter's Decisions API with a short timeout.
func (c *openRouterClient) Decide(ctx context.Context, cfg Config, input string, questions []DecisionQuestion) (map[string]DecisionAnswer, error) {
	if strings.TrimSpace(cfg.APIKey) == "" || strings.TrimSpace(cfg.Models.Decision) == "" {
		return nil, errors.New("decision model not configured")
	}
	dCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	payload := map[string]any{
		"model":     cfg.Models.Decision,
		"state":     map[string]any{"input": input},
		"questions": buildDecisionQuestionsMap(questions),
	}
	rawBody, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(dCtx, http.MethodPost, decisionsEndpoint(cfg.BaseURL), bytes.NewReader(rawBody))
	if err != nil {
		return nil, err
	}
	applyOpenRouterHeaders(req, cfg.APIKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("decisions HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBytes)))
	}
	var parsed struct {
		Answers map[string]struct {
			Type          string   `json:"type"`
			Choice        string   `json:"choice"`
			Noul          *float64 `json:"noul"`
			Value         any      `json:"value"`
			Confidence    float64  `json:"confidence"`
			Probabilities []struct {
				Key         string  `json:"key"`
				Level       string  `json:"level"`
				Probability float64 `json:"probability"`
			} `json:"probabilities"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return nil, err
	}
	out := make(map[string]DecisionAnswer, len(parsed.Answers))
	for id, a := range parsed.Answers {
		val := a.Choice
		if val == "" && a.Noul != nil {
			if *a.Noul >= 0.5 {
				val = "true"
			} else {
				val = "false"
			}
		}
		if val == "" && a.Value != nil {
			val = fmt.Sprint(a.Value)
		}
		probs := make(map[string]float64, len(a.Probabilities))
		for _, p := range a.Probabilities {
			k := cmp.Or(p.Key, p.Level)
			if k != "" {
				probs[k] = p.Probability
			}
		}
		out[id] = DecisionAnswer{ID: id, Value: val, Confidence: a.Confidence, Probabilities: probs}
	}
	return out, nil
}
