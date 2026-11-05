package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/trae"
	"github.com/gin-gonic/gin"
)

const traeChatEndpoint = "/api/ide/v2/llm_raw_chat"

func traePAT(account *Account) string {
	if account == nil {
		return ""
	}
	for _, key := range []string{"api_key", "trae_pat", "refresh_token", "pat"} {
		if value := strings.TrimSpace(account.GetCredential(key)); value != "" {
			return value
		}
	}
	return ""
}

func traeBaseURL(account *Account) string {
	if account != nil {
		for _, key := range []string{"base_url", "trae_base_url"} {
			if value := strings.TrimSpace(account.GetCredential(key)); value != "" {
				return strings.TrimRight(value, "/")
			}
		}
	}
	return trae.DefaultBaseURL
}

func (s *OpenAIGatewayService) traeClient(account *Account) *trae.Client {
	proxyURL := ""
	if account != nil && account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	accountID := int64(0)
	concurrency := 1
	if account != nil {
		accountID = account.ID
		concurrency = account.Concurrency
	}
	return trae.NewClient(traeBaseURL(account), func(req *http.Request) (*http.Response, error) {
		if s == nil || s.httpUpstream == nil {
			return nil, fmt.Errorf("upstream HTTP client is not configured")
		}
		return s.httpUpstream.Do(req, proxyURL, accountID, concurrency)
	})
}

func (s *OpenAIGatewayService) isTraeAccount(account *Account) bool {
	return account != nil && account.Platform == PlatformTrae
}

func mapTraeErrorStatus(err error) (int, string, bool) {
	e, ok := err.(*trae.Error)
	if !ok {
		return http.StatusBadGateway, "upstream_error", false
	}
	switch e.Kind {
	case trae.ErrorKindBadRequest:
		return http.StatusBadRequest, "invalid_request_error", false
	case trae.ErrorKindModelNotFound:
		return http.StatusNotFound, "invalid_request_error", false
	case trae.ErrorKindAuth:
		return http.StatusBadGateway, "authentication_error", true
	case trae.ErrorKindTransient:
		status := e.Status
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		return status, "upstream_error", true
	case trae.ErrorKindAborted:
		return 499, "request_cancelled", false
	default:
		return http.StatusBadGateway, "upstream_error", false
	}
}

func traeClientErrorMessage(err error) string {
	if e, ok := err.(*trae.Error); ok {
		switch e.Kind {
		case trae.ErrorKindAuth:
			return "Trae upstream authentication failed; check the account PAT"
		case trae.ErrorKindBadRequest, trae.ErrorKindModelNotFound:
			return e.Message
		}
	}
	return "Trae upstream request failed"
}

func writeTraeProtocolError(c *gin.Context, protocol string, err error) {
	status, errType, _ := mapTraeErrorStatus(err)
	message := traeClientErrorMessage(err)
	switch protocol {
	case APIProtocolAnthropic:
		writeAnthropicError(c, status, errType, message)
	case APIProtocolResponses:
		writeOpenAIResponsesFallbackError(c, status, errType, message)
	default:
		writeChatCompletionsError(c, status, errType, message)
	}
}

func (s *OpenAIGatewayService) traeForwardError(c *gin.Context, protocol string, err error, written bool) error {
	if written {
		return err
	}
	status, _, failover := mapTraeErrorStatus(err)
	if failover {
		body, _ := json.Marshal(map[string]any{"error": map[string]any{"message": traeClientErrorMessage(err), "type": "upstream_error"}})
		stage := GatewayFailureStageInference
		scope := GatewayFailureScopeAccount
		if e, ok := err.(*trae.Error); ok && e.Kind == trae.ErrorKindAuth {
			stage = GatewayFailureStageAccountAuth
		}
		return &UpstreamFailoverError{
			StatusCode:   status,
			ResponseBody: body,
			Stage:        stage,
			Scope:        scope,
		}
	}
	writeTraeProtocolError(c, protocol, err)
	return err
}

func randomHexID(prefix string, size int) string {
	buf := make([]byte, size)
	_, _ = rand.Read(buf)
	return prefix + hex.EncodeToString(buf)
}

func traeStringPtr(value string) *string {
	return &value
}

func traeChatUsage(usage trae.Usage) *apicompat.ChatUsage {
	out := &apicompat.ChatUsage{
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
	}
	if usage.CacheReadInputTokens > 0 {
		out.PromptTokensDetails = &apicompat.ChatTokenDetails{CachedTokens: usage.CacheReadInputTokens}
	}
	if usage.ReasoningTokens > 0 {
		out.CompletionTokensDetails = &apicompat.ChatTokenDetails{ReasoningTokens: usage.ReasoningTokens}
	}
	return out
}

func (s *OpenAIGatewayService) traeForwardUsage(usage trae.Usage) OpenAIUsage {
	return OpenAIUsage{
		InputTokens:          usage.PromptTokens,
		OutputTokens:         usage.CompletionTokens,
		CacheReadInputTokens: usage.CacheReadInputTokens,
	}
}

func traeFinishReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "length", "max_tokens":
		return "length"
	case "tool_calls", "tool_use":
		return "tool_calls"
	case "content_filter":
		return "content_filter"
	default:
		return "stop"
	}
}

// ---------------------------------------------------------------------------
// Inbound conversion helpers
// ---------------------------------------------------------------------------

func (s *OpenAIGatewayService) traeMessagesFromChat(ctx context.Context, client *trae.Client, req *apicompat.ChatCompletionsRequest) ([]trae.Message, error) {
	items := make([]trae.Message, 0, len(req.Messages)+1)
	if strings.TrimSpace(req.Instructions) != "" {
		items = append(items, trae.Message{Role: "system", Content: []trae.ContentPart{{Type: "text", Text: req.Instructions}}})
	}
	for _, message := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role == "tool" || role == "function" {
			role = "user"
		}
		if role != "system" && role != "assistant" {
			role = "user"
		}
		parts := make([]trae.ContentPart, 0, 2)
		if len(message.Content) > 0 {
			var text string
			if err := json.Unmarshal(message.Content, &text); err == nil {
				if text != "" {
					parts = append(parts, trae.ContentPart{Type: "text", Text: text})
				}
			} else {
				var contentParts []apicompat.ChatContentPart
				if err := json.Unmarshal(message.Content, &contentParts); err == nil {
					for _, part := range contentParts {
						switch part.Type {
						case "text", "input_text":
							if part.Text != "" {
								parts = append(parts, trae.ContentPart{Type: "text", Text: part.Text})
							}
						case "image_url", "input_image":
							if part.ImageURL == nil || strings.TrimSpace(part.ImageURL.URL) == "" {
								continue
							}
							url, err := client.FetchImageAsDataURL(ctx, part.ImageURL.URL)
							if err != nil {
								return nil, err
							}
							parts = append(parts, trae.ContentPart{Type: "image_url", ImageURL: &trae.ImageURL{URL: url}})
						case "file":
							if part.File != nil && part.File.Filename != "" {
								parts = append(parts, trae.ContentPart{Type: "text", Text: fmt.Sprintf("[attached file: %s]", part.File.Filename)})
							}
						}
					}
				}
			}
		}
		if strings.TrimSpace(message.ReasoningContent) != "" && role == "assistant" {
			parts = append(parts, trae.ContentPart{Type: "text", Text: message.ReasoningContent})
		}
		if len(message.ToolCalls) > 0 {
			raw, _ := json.Marshal(message.ToolCalls)
			parts = append(parts, trae.ContentPart{Type: "text", Text: "[tool calls] " + string(raw)})
		}
		if message.FunctionCall != nil {
			raw, _ := json.Marshal(message.FunctionCall)
			parts = append(parts, trae.ContentPart{Type: "text", Text: "[function call] " + string(raw)})
		}
		if len(parts) == 0 {
			continue
		}
		items = append(items, trae.Message{Role: role, Content: parts})
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("request contains no messages")
	}
	return items, nil
}

// ---------------------------------------------------------------------------
// Chat Completions
// ---------------------------------------------------------------------------

func (s *OpenAIGatewayService) forwardTraeChatCompletions(ctx context.Context, c *gin.Context, account *Account, body []byte, defaultMappedModel string) (*OpenAIForwardResult, error) {
	startTime := time.Now()
	var req apicompat.ChatCompletionsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return nil, fmt.Errorf("parse chat completions request: %w", err)
	}
	originalModel := strings.TrimSpace(req.Model)
	if originalModel == "" {
		writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, fmt.Errorf("missing model in request")
	}
	billingModel := resolveOpenAIForwardModel(account, originalModel, defaultMappedModel)
	req.Model = billingModel
	SetOpsUpstreamModel(c, billingModel)
	SetActualOpenAIUpstreamEndpoint(c, traeChatEndpoint)

	client := s.traeClient(account)
	messages, err := s.traeMessagesFromChat(ctx, client, &req)
	if err != nil {
		writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, err
	}

	responseID := randomHexID("chatcmpl-", 12)
	created := time.Now().Unix()
	var firstTokenMs *int
	written := false
	writeHeaders := func() {
		if written {
			return
		}
		written = true
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Header().Set("X-Accel-Buffering", "no")
		c.Writer.WriteHeader(http.StatusOK)
	}
	writeChunk := func(chunk apicompat.ChatCompletionsChunk) {
		writeHeaders()
		raw, err := json.Marshal(chunk)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", raw)
		c.Writer.Flush()
	}

	if !req.Stream {
		outcome, err := client.Chat(ctx, traePAT(account), billingModel, messages, nil)
		if err != nil {
			return nil, s.traeForwardError(c, APIProtocolChatCompletions, err, written)
		}
		content, _ := json.Marshal(outcome.Text)
		response := apicompat.ChatCompletionsResponse{
			ID:      responseID,
			Object:  "chat.completion",
			Created: created,
			Model:   originalModel,
			Choices: []apicompat.ChatChoice{{
				Index: 0,
				Message: apicompat.ChatMessage{
					Role:             "assistant",
					Content:          content,
					ReasoningContent: outcome.Reasoning,
				},
				FinishReason: traeFinishReason(outcome.FinishReason),
			}},
			Usage: traeChatUsage(outcome.Usage),
		}
		c.JSON(http.StatusOK, response)
		return &OpenAIForwardResult{
			RequestID:                   responseID,
			Usage:                       s.traeForwardUsage(outcome.Usage),
			Model:                       originalModel,
			BillingModel:                billingModel,
			UpstreamModel:               outcome.ModelName,
			UpstreamResponseModel:       outcome.ModelName,
			UpstreamEndpoint:            traeChatEndpoint,
			Stream:                      false,
			Duration:                    time.Since(startTime),
			UpstreamResponseServiceTier: "",
		}, nil
	}

	emit := func(text, reasoning string) error {
		if firstTokenMs == nil {
			elapsed := int(time.Since(startTime).Milliseconds())
			firstTokenMs = &elapsed
		}
		if text != "" {
			value := text
			writeChunk(apicompat.ChatCompletionsChunk{
				ID: responseID, Object: "chat.completion.chunk", Created: created, Model: originalModel,
				Choices: []apicompat.ChatChunkChoice{{Index: 0, Delta: apicompat.ChatDelta{Content: &value}}},
			})
		}
		if reasoning != "" {
			value := reasoning
			writeChunk(apicompat.ChatCompletionsChunk{
				ID: responseID, Object: "chat.completion.chunk", Created: created, Model: originalModel,
				Choices: []apicompat.ChatChunkChoice{{Index: 0, Delta: apicompat.ChatDelta{ReasoningContent: &value}}},
			})
		}
		return nil
	}

	outcome, err := client.Chat(ctx, traePAT(account), billingModel, messages, emit)
	if err != nil {
		return nil, s.traeForwardError(c, APIProtocolChatCompletions, err, written)
	}
	writeChunk(apicompat.ChatCompletionsChunk{
		ID: responseID, Object: "chat.completion.chunk", Created: created, Model: originalModel,
		Choices: []apicompat.ChatChunkChoice{{Index: 0, Delta: apicompat.ChatDelta{Role: "assistant"}, FinishReason: traeStringPtr(traeFinishReason(outcome.FinishReason))}},
		Usage:   traeChatUsage(outcome.Usage),
	})
	writeHeaders()
	_, _ = fmt.Fprint(c.Writer, "data: [DONE]\n\n")
	c.Writer.Flush()
	return &OpenAIForwardResult{
		RequestID:                   responseID,
		Usage:                       s.traeForwardUsage(outcome.Usage),
		Model:                       originalModel,
		BillingModel:                billingModel,
		UpstreamModel:               outcome.ModelName,
		UpstreamResponseModel:       outcome.ModelName,
		UpstreamEndpoint:            traeChatEndpoint,
		Stream:                      true,
		Duration:                    time.Since(startTime),
		FirstTokenMs:                firstTokenMs,
		UpstreamResponseServiceTier: "",
	}, nil
}

// ---------------------------------------------------------------------------
// Anthropic Messages
// ---------------------------------------------------------------------------

func (s *OpenAIGatewayService) forwardTraeAnthropic(ctx context.Context, c *gin.Context, account *Account, body []byte, defaultMappedModel string) (*OpenAIForwardResult, error) {
	startTime := time.Now()
	var anthropicReq apicompat.AnthropicRequest
	if err := json.Unmarshal(body, &anthropicReq); err != nil {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return nil, fmt.Errorf("parse anthropic request: %w", err)
	}
	originalModel := strings.TrimSpace(anthropicReq.Model)
	if originalModel == "" {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, fmt.Errorf("missing model in request")
	}
	clientStream := anthropicReq.Stream
	chatReq, err := apicompat.AnthropicToChatCompletionsRequestWithMinOutputTokens(&anthropicReq, configuredMinOutputTokens(s.cfg))
	if err != nil {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, err
	}
	billingModel := resolveOpenAIForwardModel(account, originalModel, defaultMappedModel)
	chatReq.Model = billingModel
	chatReq.Stream = clientStream
	SetOpsUpstreamModel(c, billingModel)
	SetActualOpenAIUpstreamEndpoint(c, traeChatEndpoint)

	client := s.traeClient(account)
	messages, err := s.traeMessagesFromChat(ctx, client, chatReq)
	if err != nil {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, err
	}
	responseID := randomHexID("msg_", 12)
	created := time.Now().Unix()
	var firstTokenMs *int
	wrote := false
	writeHeaders := func() {
		if wrote {
			return
		}
		wrote = true
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Header().Set("X-Accel-Buffering", "no")
		c.Writer.WriteHeader(http.StatusOK)
	}
	state := apicompat.NewChatCompletionsToAnthropicStreamState(originalModel)
	emitChunk := func(chunk *apicompat.ChatCompletionsChunk) {
		for _, event := range apicompat.ChatCompletionsChunkToAnthropicEvents(chunk, state) {
			sse, err := apicompat.ResponsesAnthropicEventToSSE(event)
			if err != nil {
				continue
			}
			writeHeaders()
			_, _ = fmt.Fprint(c.Writer, sse)
		}
		if wrote {
			c.Writer.Flush()
		}
	}
	emit := func(text, reasoning string) error {
		if firstTokenMs == nil {
			elapsed := int(time.Since(startTime).Milliseconds())
			firstTokenMs = &elapsed
		}
		choice := apicompat.ChatChunkChoice{Index: 0}
		if text != "" {
			value := text
			choice.Delta.Content = &value
		}
		if reasoning != "" {
			value := reasoning
			choice.Delta.ReasoningContent = &value
		}
		if choice.Delta.Content != nil || choice.Delta.ReasoningContent != nil {
			emitChunk(&apicompat.ChatCompletionsChunk{ID: responseID, Object: "chat.completion.chunk", Created: created, Model: originalModel, Choices: []apicompat.ChatChunkChoice{choice}})
		}
		return nil
	}

	if !clientStream {
		outcome, err := client.Chat(ctx, traePAT(account), billingModel, messages, nil)
		if err != nil {
			return nil, s.traeForwardError(c, APIProtocolAnthropic, err, false)
		}
		content, _ := json.Marshal(outcome.Text)
		cc := &apicompat.ChatCompletionsResponse{
			ID: responseID, Object: "chat.completion", Created: created, Model: originalModel,
			Choices: []apicompat.ChatChoice{{Index: 0, Message: apicompat.ChatMessage{Role: "assistant", Content: content, ReasoningContent: outcome.Reasoning}, FinishReason: traeFinishReason(outcome.FinishReason)}},
			Usage:   traeChatUsage(outcome.Usage),
		}
		c.JSON(http.StatusOK, apicompat.ChatCompletionsResponseToAnthropic(cc, originalModel))
		return &OpenAIForwardResult{
			RequestID:                   responseID,
			Usage:                       s.traeForwardUsage(outcome.Usage),
			Model:                       originalModel,
			BillingModel:                billingModel,
			UpstreamModel:               outcome.ModelName,
			UpstreamResponseModel:       outcome.ModelName,
			UpstreamEndpoint:            traeChatEndpoint,
			Stream:                      false,
			Duration:                    time.Since(startTime),
			UpstreamResponseServiceTier: "",
		}, nil
	}

	outcome, err := client.Chat(ctx, traePAT(account), billingModel, messages, emit)
	if err != nil {
		return nil, s.traeForwardError(c, APIProtocolAnthropic, err, wrote)
	}
	emitChunk(&apicompat.ChatCompletionsChunk{
		ID: responseID, Object: "chat.completion.chunk", Created: created, Model: originalModel,
		Choices: []apicompat.ChatChunkChoice{{Index: 0, FinishReason: traeStringPtr(traeFinishReason(outcome.FinishReason))}},
		Usage:   traeChatUsage(outcome.Usage),
	})
	for _, event := range apicompat.FinalizeChatCompletionsAnthropicStream(state) {
		sse, err := apicompat.ResponsesAnthropicEventToSSE(event)
		if err != nil {
			continue
		}
		writeHeaders()
		_, _ = fmt.Fprint(c.Writer, sse)
	}
	c.Writer.Flush()
	return &OpenAIForwardResult{
		RequestID:                   responseID,
		Usage:                       s.traeForwardUsage(outcome.Usage),
		Model:                       originalModel,
		BillingModel:                billingModel,
		UpstreamModel:               outcome.ModelName,
		UpstreamResponseModel:       outcome.ModelName,
		UpstreamEndpoint:            traeChatEndpoint,
		Stream:                      true,
		Duration:                    time.Since(startTime),
		FirstTokenMs:                firstTokenMs,
		UpstreamResponseServiceTier: "",
	}, nil
}

// ---------------------------------------------------------------------------
// Responses API
// ---------------------------------------------------------------------------

func (s *OpenAIGatewayService) forwardTraeResponses(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
	startTime := time.Now()
	var responsesReq apicompat.ResponsesRequest
	if err := json.Unmarshal(body, &responsesReq); err != nil {
		writeOpenAIResponsesFallbackError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return nil, fmt.Errorf("parse responses request: %w", err)
	}
	originalModel := strings.TrimSpace(responsesReq.Model)
	if originalModel == "" {
		writeOpenAIResponsesFallbackError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, fmt.Errorf("missing model in request")
	}
	clientStream := responsesReq.Stream
	effectiveTools, err := apicompat.EffectiveResponsesTools(&responsesReq)
	if err != nil {
		writeOpenAIResponsesFallbackError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, err
	}
	customTools := apicompat.CustomToolNames(effectiveTools)
	functionTools := apicompat.FunctionToolNames(effectiveTools)
	toolSearch := apicompat.HasToolSearchTool(effectiveTools)
	namespaceTools := apicompat.NamespaceToolNames(effectiveTools)
	chatReq, err := apicompat.ResponsesToChatCompletionsRequestWithOptions(&responsesReq, &apicompat.ResponsesToChatOptions{ReasoningContentByID: s.reasoningContentByID})
	if err != nil {
		writeOpenAIResponsesFallbackError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, err
	}
	billingModel := resolveOpenAIForwardModel(account, originalModel, "")
	chatReq.Model = billingModel
	chatReq.Stream = clientStream
	SetOpsUpstreamModel(c, billingModel)
	SetActualOpenAIUpstreamEndpoint(c, traeChatEndpoint)

	client := s.traeClient(account)
	messages, err := s.traeMessagesFromChat(ctx, client, chatReq)
	if err != nil {
		writeOpenAIResponsesFallbackError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, err
	}
	responseID := randomHexID("resp_", 12)
	created := time.Now().Unix()
	var firstTokenMs *int
	wrote := false
	writeHeaders := func() {
		if wrote {
			return
		}
		wrote = true
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Header().Set("X-Accel-Buffering", "no")
		c.Writer.WriteHeader(http.StatusOK)
	}
	state := apicompat.NewChatCompletionsToResponsesStreamState(originalModel)
	state.CustomTools = customTools
	state.FunctionTools = functionTools
	state.ToolSearchDeclared = toolSearch
	state.NamespaceTools = namespaceTools
	writeEvents := func(events []apicompat.ResponsesStreamEvent) {
		if len(events) == 0 {
			return
		}
		writeHeaders()
		for _, event := range events {
			sse, err := apicompat.ResponsesEventToSSE(event)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprint(c.Writer, sse)
		}
		c.Writer.Flush()
	}
	emitChunk := func(chunk *apicompat.ChatCompletionsChunk) {
		events := apicompat.ChatCompletionsChunkToResponsesEvents(chunk, state)
		s.cacheReasoningItemsFromEvents(events)
		writeEvents(events)
	}
	emit := func(text, reasoning string) error {
		if firstTokenMs == nil {
			elapsed := int(time.Since(startTime).Milliseconds())
			firstTokenMs = &elapsed
		}
		choice := apicompat.ChatChunkChoice{Index: 0}
		if text != "" {
			value := text
			choice.Delta.Content = &value
		}
		if reasoning != "" {
			value := reasoning
			choice.Delta.ReasoningContent = &value
		}
		if choice.Delta.Content != nil || choice.Delta.ReasoningContent != nil {
			emitChunk(&apicompat.ChatCompletionsChunk{ID: responseID, Object: "chat.completion.chunk", Created: created, Model: originalModel, Choices: []apicompat.ChatChunkChoice{choice}})
		}
		return nil
	}

	if !clientStream {
		outcome, err := client.Chat(ctx, traePAT(account), billingModel, messages, nil)
		if err != nil {
			return nil, s.traeForwardError(c, APIProtocolResponses, err, false)
		}
		content, _ := json.Marshal(outcome.Text)
		cc := &apicompat.ChatCompletionsResponse{
			ID: responseID, Object: "chat.completion", Created: created, Model: originalModel,
			Choices: []apicompat.ChatChoice{{Index: 0, Message: apicompat.ChatMessage{Role: "assistant", Content: content, ReasoningContent: outcome.Reasoning}, FinishReason: traeFinishReason(outcome.FinishReason)}},
			Usage:   traeChatUsage(outcome.Usage),
		}
		responsesResp := apicompat.ChatCompletionsResponseToResponses(cc, originalModel, customTools, functionTools, toolSearch, namespaceTools)
		s.cacheReasoningItemsFromOutput(responsesResp.Output)
		c.JSON(http.StatusOK, responsesResp)
		return &OpenAIForwardResult{
			RequestID:                   responseID,
			ResponseID:                  responsesResp.ID,
			Usage:                       s.traeForwardUsage(outcome.Usage),
			Model:                       originalModel,
			BillingModel:                billingModel,
			UpstreamModel:               outcome.ModelName,
			UpstreamResponseModel:       outcome.ModelName,
			UpstreamEndpoint:            traeChatEndpoint,
			Stream:                      false,
			Duration:                    time.Since(startTime),
			UpstreamResponseServiceTier: "",
		}, nil
	}

	outcome, err := client.Chat(ctx, traePAT(account), billingModel, messages, emit)
	if err != nil {
		return nil, s.traeForwardError(c, APIProtocolResponses, err, wrote)
	}
	emitChunk(&apicompat.ChatCompletionsChunk{
		ID: responseID, Object: "chat.completion.chunk", Created: created, Model: originalModel,
		Choices: []apicompat.ChatChunkChoice{{Index: 0, FinishReason: traeStringPtr(traeFinishReason(outcome.FinishReason))}},
		Usage:   traeChatUsage(outcome.Usage),
	})
	writeEvents(apicompat.FinalizeChatCompletionsResponsesStream(state))
	return &OpenAIForwardResult{
		RequestID:                   responseID,
		Usage:                       s.traeForwardUsage(outcome.Usage),
		Model:                       originalModel,
		BillingModel:                billingModel,
		UpstreamModel:               outcome.ModelName,
		UpstreamResponseModel:       outcome.ModelName,
		UpstreamEndpoint:            traeChatEndpoint,
		Stream:                      true,
		Duration:                    time.Since(startTime),
		FirstTokenMs:                firstTokenMs,
		UpstreamResponseServiceTier: "",
	}, nil
}
