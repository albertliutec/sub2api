package trae

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	chatConnectTimeout = 30 * time.Second
	chatIdleTimeout    = 120 * time.Second
	maxImageBytes      = 10 << 20
)

type chatRequest struct {
	ConfigName     string    `json:"config_name"`
	ConversationID string    `json:"conversation_id"`
	ModelName      string    `json:"model_name"`
	SessionID      string    `json:"session_id"`
	Messages       []Message `json:"messages"`
	Tools          []any     `json:"tools"`
	UserInput      string    `json:"user_input"`
}

type streamOutput struct {
	Response         any             `json:"response"`
	ReasoningContent any             `json:"reasoning_content"`
	ToolCalls        json.RawMessage `json:"tool_calls"`
}

type streamUsage struct {
	PromptTokens         int `json:"prompt_tokens"`
	CompletionTokens     int `json:"completion_tokens"`
	TotalTokens          int `json:"total_tokens"`
	ReasoningTokens      int `json:"reasoning_tokens"`
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
}

func jsonText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, item := range v {
			b.WriteString(jsonText(item))
		}
		return b.String()
	case map[string]any:
		if text, ok := v["text"].(string); ok {
			return text
		}
		raw, _ := json.Marshal(v)
		return string(raw)
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// FetchImageAsDataURL downloads a remote image and converts it to the data URL
// form Trae accepts. Data URLs pass through unchanged.
func (c *Client) FetchImageAsDataURL(ctx context.Context, rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", newError(ErrorKindBadRequest, http.StatusBadRequest, "image URL is empty")
	}
	if strings.HasPrefix(strings.ToLower(rawURL), "data:") {
		return rawURL, nil
	}
	if !strings.HasPrefix(strings.ToLower(rawURL), "http://") && !strings.HasPrefix(strings.ToLower(rawURL), "https://") {
		return "", newError(ErrorKindBadRequest, http.StatusBadRequest, "only http(s) or data URL images are supported")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", newError(ErrorKindBadRequest, http.StatusBadRequest, "invalid image URL")
	}
	req.Header.Set("User-Agent", upstreamUA)
	resp, err := c.doRequest(req)
	if err != nil {
		return "", wrapError(ErrorKindBadRequest, http.StatusBadRequest, "download image failed", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", newError(ErrorKindBadRequest, http.StatusBadRequest, fmt.Sprintf("download image failed (HTTP %d)", resp.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return "", wrapError(ErrorKindBadRequest, http.StatusBadRequest, "read image failed", err)
	}
	if len(data) > maxImageBytes {
		return "", newError(ErrorKindBadRequest, http.StatusBadRequest, "image exceeds the 10MB limit")
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	if !strings.HasPrefix(contentType, "image/") {
		return "", newError(ErrorKindBadRequest, http.StatusBadRequest, "downloaded file is not an image")
	}
	return "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// Chat executes one llm_raw_chat request. onDelta is called synchronously for
// each text/reasoning delta.
func (c *Client) Chat(ctx context.Context, pat, requestedModel string, messages []Message, onDelta DeltaFunc) (Outcome, error) {
	pat = strings.TrimSpace(pat)
	if pat == "" {
		return Outcome{}, newError(ErrorKindAuth, http.StatusUnauthorized, "Trae PAT is required")
	}
	info, err := c.ResolveModel(ctx, pat, requestedModel)
	if err != nil {
		return Outcome{}, err
	}
	if !info.Multimodal {
		for _, message := range messages {
			for _, part := range message.Content {
				if part.Type == "image_url" {
					return Outcome{}, newError(ErrorKindBadRequest, http.StatusBadRequest,
						fmt.Sprintf("model %s does not support image input; choose a multimodal Trae model", requestedModel))
				}
			}
		}
	}

	pushed := false
	safeDelta := onDelta
	if onDelta != nil {
		safeDelta = func(text, reasoning string) error {
			if text != "" || reasoning != "" {
				pushed = true
			}
			return onDelta(text, reasoning)
		}
	}

	outcome, err := c.chatOnce(ctx, pat, info, messages, safeDelta)
	if err != nil {
		if e, ok := err.(*Error); ok && e.Kind == ErrorKindAuth && !pushed {
			c.InvalidateJWT(pat)
			return c.chatOnce(ctx, pat, info, messages, safeDelta)
		}
		if e, ok := err.(*Error); ok && e.Kind == ErrorKindTransient && !pushed {
			select {
			case <-ctx.Done():
				return Outcome{}, newError(ErrorKindAborted, 499, "client canceled the request")
			case <-time.After(1200 * time.Millisecond):
			}
			return c.chatOnce(ctx, pat, info, messages, safeDelta)
		}
	}
	return outcome, err
}

func (c *Client) chatOnce(ctx context.Context, pat string, info ModelInfo, messages []Message, onDelta DeltaFunc) (Outcome, error) {
	token, err := c.JWT(ctx, pat)
	if err != nil {
		return Outcome{}, err
	}
	requestID := genRequestID()
	conversationID := genConversationID()
	sessionID := genSessionID()

	payload := chatRequest{
		ConfigName:     info.ConfigName,
		ConversationID: conversationID,
		ModelName:      info.ModelName,
		SessionID:      sessionID,
		Messages:       messages,
		Tools:          []any{},
		UserInput:      "",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Outcome{}, wrapError(ErrorKindFatal, 0, "marshal Trae chat request", err)
	}
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.baseURL+"/api/ide/v2/llm_raw_chat", bytes.NewReader(body))
	if err != nil {
		return Outcome{}, wrapError(ErrorKindFatal, 0, "build Trae chat request", err)
	}
	extra, _ := json.Marshal(map[string]any{
		"agent_loop_id":         sessionID,
		"api_host":              c.baseURL,
		"api_key":               token,
		"base_url":              c.baseURL + "/trae-cli/api/v1/llm/proxy",
		"config_name":           info.ConfigName,
		"config_source":         1,
		"display_name":          info.ConfigName,
		"model_name":            info.ModelName,
		"session_id":            conversationID,
		"user_prompt_submit_id": sessionID,
	})
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+token)
	req.Header.Set("X-Cloudide-Token", token)
	req.Header.Set("X-Ide-Function", "chat")
	req.Header.Set("X-Ide-Version", ideVersion)
	req.Header.Set("Extra", string(extra))
	baseHeaders(req, requestID)

	type responseResult struct {
		resp *http.Response
		err  error
	}
	connectTimer := time.NewTimer(chatConnectTimeout)
	defer connectTimer.Stop()
	responseCh := make(chan responseResult, 1)
	go func() {
		resp, err := c.doRequest(req)
		responseCh <- responseResult{resp: resp, err: err}
	}()

	var resp *http.Response
	select {
	case <-ctx.Done():
		cancel()
		return Outcome{}, newError(ErrorKindAborted, 499, "client canceled the request")
	case <-connectTimer.C:
		cancel()
		return Outcome{}, newError(ErrorKindTransient, http.StatusGatewayTimeout, "Trae chat connect timeout (30s)")
	case result := <-responseCh:
		if result.err != nil {
			if ctx.Err() != nil {
				return Outcome{}, newError(ErrorKindAborted, 499, "client canceled the request")
			}
			return Outcome{}, wrapError(ErrorKindTransient, 0, "Trae chat request failed: "+result.err.Error(), result.err)
		}
		resp = result.resp
	}
	if resp == nil {
		return Outcome{}, newError(ErrorKindProtocol, http.StatusBadGateway, "Trae chat returned no response")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		text, _ := readLimitedBody(resp, 1<<20)
		kind := ErrorKindFatal
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			kind = ErrorKindAuth
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			kind = ErrorKindTransient
		}
		return Outcome{}, newError(kind, resp.StatusCode, fmt.Sprintf("Trae chat failed (HTTP %d)", resp.StatusCode)).withDetail(sanitizeUpstreamDetail(text))
	}

	lines := make(chan string, 64)
	scanErr := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-reqCtx.Done():
				return
			}
		}
		scanErr <- scanner.Err()
		close(lines)
	}()

	idleTimer := time.NewTimer(chatIdleTimeout)
	defer idleTimer.Stop()
	resetIdle := func() {
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		idleTimer.Reset(chatIdleTimeout)
	}

	var (
		eventName     string
		dataLines     []string
		text          strings.Builder
		reasoning     strings.Builder
		finishReason  string
		usage         streamUsage
		streamErrText string
	)

	dispatch := func() error {
		if len(dataLines) == 0 {
			eventName = ""
			return nil
		}
		data := strings.Join(dataLines, "\n")
		event := eventName
		eventName = ""
		dataLines = dataLines[:0]

		switch event {
		case "output":
			var out streamOutput
			if err := json.Unmarshal([]byte(data), &out); err != nil {
				return nil
			}
			partText := jsonText(out.Response)
			partReasoning := jsonText(out.ReasoningContent)
			if partText != "" {
				text.WriteString(partText)
			}
			if partReasoning != "" {
				reasoning.WriteString(partReasoning)
			}
			if (partText != "" || partReasoning != "") && onDelta != nil {
				if err := onDelta(partText, partReasoning); err != nil {
					return err
				}
			}
		case "token_usage":
			_ = json.Unmarshal([]byte(data), &usage)
		case "done":
			var done struct {
				FinishReason string `json:"finish_reason"`
			}
			_ = json.Unmarshal([]byte(data), &done)
			finishReason = strings.TrimSpace(done.FinishReason)
		case "error":
			streamErrText = sanitizeUpstreamDetail(data)
		}
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			cancel()
			return Outcome{}, newError(ErrorKindAborted, 499, "client canceled the request")
		case <-idleTimer.C:
			cancel()
			return Outcome{}, newError(ErrorKindTransient, http.StatusGatewayTimeout, "Trae SSE stream idle timeout (120s)")
		case line, ok := <-lines:
			if !ok {
				if err := dispatch(); err != nil {
					return Outcome{}, err
				}
				select {
				case err := <-scanErr:
					if err != nil && ctx.Err() == nil {
						return Outcome{}, wrapError(ErrorKindTransient, http.StatusBadGateway, "Trae SSE stream interrupted: "+err.Error(), err)
					}
				default:
				}
				goto finished
			}
			resetIdle()
			line = strings.TrimSuffix(line, "\r")
			if line == "" {
				if err := dispatch(); err != nil {
					return Outcome{}, err
				}
				continue
			}
			if strings.HasPrefix(line, "event:") {
				eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
				continue
			}
			if strings.HasPrefix(line, "data:") {
				dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
	}

finished:
	if streamErrText != "" && text.Len() == 0 {
		return Outcome{}, newError(ErrorKindTransient, http.StatusBadGateway, "Trae stream error: "+streamErrText)
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	if finishReason == "" {
		finishReason = "stop"
	}
	if finishReason == "max_tokens" {
		finishReason = "length"
	}
	return Outcome{
		ConfigName:   info.ConfigName,
		ModelName:    info.ModelName,
		FinishReason: finishReason,
		Text:         text.String(),
		Reasoning:    reasoning.String(),
		Usage: Usage{
			PromptTokens:         usage.PromptTokens,
			CompletionTokens:     usage.CompletionTokens,
			TotalTokens:          usage.TotalTokens,
			ReasoningTokens:      usage.ReasoningTokens,
			CacheReadInputTokens: usage.CacheReadInputTokens,
		},
	}, nil
}
