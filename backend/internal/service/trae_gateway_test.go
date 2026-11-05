//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type traeGatewayTestUpstream struct {
	transport http.RoundTripper
}

func (u *traeGatewayTestUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.transport.RoundTrip(req)
}

func (u *traeGatewayTestUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.transport.RoundTrip(req)
}

type traeGatewayTestServer struct {
	server *httptest.Server

	mu      sync.Mutex
	chatReq map[string]any
	chatRaw []byte
}

const traeGatewayTestSSE = `event: output
data: {"response":"Hello"}

event: output
data: {"response":" world","reasoning_content":"think"}

event: token_usage
data: {"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"reasoning_tokens":2,"cache_read_input_tokens":3}

event: done
data: {"finish_reason":"stop"}

`

func newTraeGatewayTestServer(t *testing.T) *traeGatewayTestServer {
	t.Helper()
	ts := &traeGatewayTestServer{}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cloudide/api/v3/trae/oauth/ExchangeToken":
			assert.Equal(t, http.MethodPost, r.Method)
			payload := map[string]any{"code": 0, "Data": map[string]any{
				"Token":         "jwt-test",
				"TokenExpireAt": time.Now().Add(24 * time.Hour).UnixMilli(),
			}}
			_ = json.NewEncoder(w).Encode(payload)
		case "/api/ide/v1/cli/get_config_list":
			assert.Equal(t, "Cloud-IDE-JWT jwt-test", r.Header.Get("Authorization"))
			_, _ = io.WriteString(w, `{"code":0,"config_info_list":[{"config_name":"test-model","usage":"chat_completion","display_config":{"multimodal":true},"model_detail_list":[{"model_name":"test-model-real","max_tokens":4096,"prompt_max_tokens":128000}]},{"config_name":"text-only-model","usage":"chat_completion","display_config":{"multimodal":false},"model_detail_list":[{"model_name":"text-only-real","max_tokens":4096,"prompt_max_tokens":128000}]}]}`)
		case "/api/ide/v2/llm_raw_chat":
			assert.Equal(t, "Cloud-IDE-JWT jwt-test", r.Header.Get("Authorization"))
			raw, err := io.ReadAll(r.Body)
			assert.NoError(t, err)
			var payload map[string]any
			assert.NoError(t, json.Unmarshal(raw, &payload))
			ts.mu.Lock()
			ts.chatReq = payload
			ts.chatRaw = raw
			ts.mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, traeGatewayTestSSE)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.server.Close)
	return ts
}

func (ts *traeGatewayTestServer) lastChatRequest() (map[string]any, []byte) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.chatReq, append([]byte(nil), ts.chatRaw...)
}

func traeGatewayTestAccount(baseURL string) *Account {
	return &Account{
		ID:          901,
		Name:        "trae-test",
		Platform:    PlatformTrae,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":      "trae-pat",
			"base_url":     baseURL,
			"api_protocol": APIProtocolAdaptive,
			"account_mode": AccountModePayG,
		},
	}
}

func newTraeGatewayTestService(ts *traeGatewayTestServer) *OpenAIGatewayService {
	return &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: &traeGatewayTestUpstream{transport: http.DefaultTransport},
	}
}

func newTraeGatewayTestContext(path string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, recorder
}

func assertTraeUpstreamRequest(t *testing.T, ts *traeGatewayTestServer) {
	t.Helper()
	payload, raw := ts.lastChatRequest()
	require.NotEmpty(t, raw)
	require.Equal(t, "test-model", payload["config_name"])
	require.Equal(t, "test-model-real", payload["model_name"])
	require.Equal(t, "hello", firstTraeUserText(payload))
}

func firstTraeUserText(payload map[string]any) string {
	messages, _ := payload["messages"].([]any)
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if message["role"] != "user" {
			continue
		}
		parts, _ := message["content"].([]any)
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			if part["type"] == "text" {
				return fmt.Sprint(part["text"])
			}
		}
	}
	return ""
}

func TestTraeGatewayForwardsAllInboundProtocols(t *testing.T) {
	ts := newTraeGatewayTestServer(t)
	svc := newTraeGatewayTestService(ts)
	account := traeGatewayTestAccount(ts.server.URL)

	t.Run("chat completions", func(t *testing.T) {
		body := []byte(`{"model":"test-model","messages":[{"role":"user","content":"hello"}],"stream":false}`)
		c, recorder := newTraeGatewayTestContext("/v1/chat/completions", body)
		result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, recorder.Code)
		require.Equal(t, "test-model-real", result.UpstreamModel)
		require.Equal(t, 10, result.Usage.InputTokens)
		require.Equal(t, 5, result.Usage.OutputTokens)

		var response map[string]any
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
		choices, _ := response["choices"].([]any)
		require.Len(t, choices, 1)
		message, _ := choices[0].(map[string]any)["message"].(map[string]any)
		require.Equal(t, "Hello world", message["content"])
		assertTraeUpstreamRequest(t, ts)
	})

	t.Run("anthropic messages", func(t *testing.T) {
		body := []byte(`{"model":"test-model","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
		c, recorder := newTraeGatewayTestContext("/v1/messages", body)
		result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, recorder.Code)
		require.Equal(t, 10, result.Usage.InputTokens)
		require.Equal(t, 5, result.Usage.OutputTokens)

		var response map[string]any
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
		require.Equal(t, "message", response["type"])
		content, _ := response["content"].([]any)
		require.NotEmpty(t, content)
		foundThinking := false
		foundText := false
		for _, rawBlock := range content {
			block, _ := rawBlock.(map[string]any)
			switch block["type"] {
			case "thinking":
				foundThinking = true
				require.Equal(t, "think", block["thinking"])
			case "text":
				foundText = true
				require.Equal(t, "Hello world", block["text"])
			}
		}
		require.True(t, foundThinking)
		require.True(t, foundText)
		assertTraeUpstreamRequest(t, ts)
	})

	t.Run("responses", func(t *testing.T) {
		body := []byte(`{"model":"test-model","input":"hello","max_output_tokens":32,"stream":false}`)
		c, recorder := newTraeGatewayTestContext("/v1/responses", body)
		result, err := svc.Forward(context.Background(), c, account, body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, recorder.Code)
		require.Equal(t, 10, result.Usage.InputTokens)
		require.Equal(t, 5, result.Usage.OutputTokens)

		var response map[string]any
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
		output, _ := response["output"].([]any)
		require.NotEmpty(t, output)
		var text strings.Builder
		for _, rawItem := range output {
			item, _ := rawItem.(map[string]any)
			content, _ := item["content"].([]any)
			for _, rawPart := range content {
				part, _ := rawPart.(map[string]any)
				if part["type"] == "output_text" {
					text.WriteString(fmt.Sprint(part["text"]))
				}
			}
		}
		require.Equal(t, "Hello world", text.String())
		assertTraeUpstreamRequest(t, ts)
	})
}

func TestTraeGatewayRejectsImageForNonMultimodalModel(t *testing.T) {
	ts := newTraeGatewayTestServer(t)
	svc := newTraeGatewayTestService(ts)
	account := traeGatewayTestAccount(ts.server.URL)
	// 注册表中的 text-only-model 为非多模态，验证网关在上游请求前返回 400。
	body := []byte(`{"model":"text-only-model","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}],"stream":false}`)
	c, recorder := newTraeGatewayTestContext("/v1/chat/completions", body)
	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "does not support image input")
}

func TestTraeGatewayStreamsChatCompletions(t *testing.T) {
	ts := newTraeGatewayTestServer(t)
	svc := newTraeGatewayTestService(ts)
	account := traeGatewayTestAccount(ts.server.URL)
	body := []byte(`{"model":"test-model","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c, recorder := newTraeGatewayTestContext("/v1/chat/completions", body)

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.True(t, result.Stream)
	require.Equal(t, http.StatusOK, recorder.Code)
	stream := recorder.Body.String()
	require.Contains(t, stream, `"content":"Hello"`)
	require.Contains(t, stream, `"content":" world"`)
	require.Contains(t, stream, `"prompt_tokens":10`)
	require.Contains(t, stream, "data: [DONE]")
	assertTraeUpstreamRequest(t, ts)
}

func TestTraeGatewayAcceptsResponsesShapedBodyOnChatEndpoint(t *testing.T) {
	ts := newTraeGatewayTestServer(t)
	svc := newTraeGatewayTestService(ts)
	account := traeGatewayTestAccount(ts.server.URL)
	body := []byte(`{"model":"test-model","input":"hello","max_output_tokens":32,"stream":false}`)
	c, recorder := newTraeGatewayTestContext("/v1/chat/completions", body)

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.NotEmpty(t, result.RequestID)

	var response map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	choices, _ := response["choices"].([]any)
	require.Len(t, choices, 1)
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	require.Equal(t, "Hello world", message["content"])
	assertTraeUpstreamRequest(t, ts)
}
