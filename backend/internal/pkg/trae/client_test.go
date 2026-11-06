package trae

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testTraeClient(server *httptest.Server) *Client {
	return NewClient(server.URL, func(req *http.Request) (*http.Response, error) {
		return http.DefaultClient.Do(req)
	})
}

func writeTraeToken(w http.ResponseWriter, token string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code": 0,
		"Data": map[string]any{
			"Token":         token,
			"TokenExpireAt": time.Now().Add(time.Hour).UnixMilli(),
		},
	})
}

func writeTraeModels(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprint(w, `{"code":0,"config_info_list":[
		{"config_name":"chat-model","usage":"chat_completion","model_detail_list":[{"model_name":"chat-model__v2","max_tokens":8192,"prompt_max_tokens":128000}],"display_config":{"multimodal":true}},
		{"config_name":"embedding-model","usage":"embedding","model_detail_list":[{"model_name":"embedding__v1"}]},
		{"config_name":"missing-detail","usage":"chat_completion","model_detail_list":[]}
	]}`)
}

func TestJWTCachesPATExchange(t *testing.T) {
	var exchanges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cloudide/api/v3/trae/oauth/ExchangeToken":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode token request: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if body["RefreshToken"] != "pat-1" {
				t.Errorf("unexpected refresh token: %q", body["RefreshToken"])
			}
			n := exchanges.Add(1)
			writeTraeToken(w, fmt.Sprintf("jwt-%d", n))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := testTraeClient(server)
	token, err := client.JWT(context.Background(), "pat-1")
	if err != nil || token != "jwt-1" {
		t.Fatalf("first JWT = %q, %v", token, err)
	}
	token, err = client.JWT(context.Background(), "pat-1")
	if err != nil || token != "jwt-1" {
		t.Fatalf("cached JWT = %q, %v", token, err)
	}
	if got := exchanges.Load(); got != 1 {
		t.Fatalf("exchange count = %d, want 1", got)
	}

	client.InvalidateJWT("pat-1")
	token, err = client.JWT(context.Background(), "pat-1")
	if err != nil || token != "jwt-2" {
		t.Fatalf("refreshed JWT = %q, %v", token, err)
	}
}

func TestListModelsUsesLiveRegistryAndFiltersChatModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cloudide/api/v3/trae/oauth/ExchangeToken":
			writeTraeToken(w, "jwt-models")
		case "/api/ide/v1/cli/get_config_list":
			if got := r.Header.Get("Authorization"); got != "Cloud-IDE-JWT jwt-models" {
				t.Errorf("Authorization = %q", got)
			}
			writeTraeModels(w)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	models, err := testTraeClient(server).ListModels(context.Background(), "pat-models")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("models = %#v, want one chat model", models)
	}
	if models[0].ConfigName != "chat-model" || models[0].ModelName != "chat-model__v2" || !models[0].Multimodal {
		t.Fatalf("unexpected model: %#v", models[0])
	}
}

func TestChatConsumesTraeSSE(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cloudide/api/v3/trae/oauth/ExchangeToken":
			writeTraeToken(w, "jwt-chat")
		case "/api/ide/v1/cli/get_config_list":
			writeTraeModels(w)
		case "/api/ide/v2/llm_raw_chat":
			if got := r.Header.Get("Authorization"); got != "Cloud-IDE-JWT jwt-chat" {
				t.Errorf("Authorization = %q", got)
			}
			if got := r.Header.Get("X-Ide-Function"); got != "chat" {
				t.Errorf("X-Ide-Function = %q", got)
			}
			var extra map[string]any
			if err := json.Unmarshal([]byte(r.Header.Get("Extra")), &extra); err != nil {
				t.Errorf("decode Extra: %v", err)
			}
			if extra["model_name"] != "chat-model__v2" {
				t.Errorf("Extra model_name = %v", extra["model_name"])
			}
			var payload chatRequest
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode chat request: %v", err)
			}
			if payload.ConfigName != "chat-model" || payload.ModelName != "chat-model__v2" || len(payload.Tools) != 0 {
				t.Errorf("unexpected chat payload: %#v", payload)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: metadata\ndata: {}\n\n")
			_, _ = fmt.Fprint(w, "event: output\ndata: {\"response\":\"hello\"}\n\n")
			_, _ = fmt.Fprint(w, "event: output\ndata: {\"reasoning_content\":\"think\"}\n\n")
			_, _ = fmt.Fprint(w, "event: token_usage\ndata: {\"prompt_tokens\":10,\"completion_tokens\":4,\"reasoning_tokens\":2,\"cache_read_input_tokens\":3}\n\n")
			_, _ = fmt.Fprint(w, "event: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var textParts, reasoningParts []string
	outcome, err := testTraeClient(server).Chat(context.Background(), "pat-chat", "chat-model", []Message{{
		Role: "user", Content: []ContentPart{{Type: "text", Text: "hi"}},
	}}, ChatOptions{}, func(text, reasoning string) error {
		if text != "" {
			textParts = append(textParts, text)
		}
		if reasoning != "" {
			reasoningParts = append(reasoningParts, reasoning)
		}
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if outcome.Text != "hello" || outcome.Reasoning != "think" || outcome.ModelName != "chat-model__v2" {
		t.Fatalf("unexpected outcome: %#v", outcome)
	}
	if strings.Join(textParts, "") != "hello" || strings.Join(reasoningParts, "") != "think" {
		t.Fatalf("deltas = %v / %v", textParts, reasoningParts)
	}
	if outcome.Usage.PromptTokens != 10 || outcome.Usage.CompletionTokens != 4 || outcome.Usage.TotalTokens != 14 || outcome.Usage.ReasoningTokens != 2 || outcome.Usage.CacheReadInputTokens != 3 {
		t.Fatalf("unexpected usage: %#v", outcome.Usage)
	}
}

func TestChatForwardsToolsAndAggregatesToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cloudide/api/v3/trae/oauth/ExchangeToken":
			writeTraeToken(w, "jwt-tools")
		case "/api/ide/v1/cli/get_config_list":
			writeTraeModels(w)
		case "/api/ide/v2/llm_raw_chat":
			var payload chatRequest
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode chat request: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if len(payload.Tools) != 1 {
				t.Errorf("tools = %#v, want one entry", payload.Tools)
			} else {
				tool, _ := payload.Tools[0].(map[string]any)
				fn, _ := tool["function"].(map[string]any)
				if tool["type"] != "function" || fn["name"] != "get_weather" {
					t.Errorf("unexpected tool payload: %#v", payload.Tools[0])
				}
				// The official client serializes parameters as a JSON string.
				if _, ok := fn["parameters"].(string); !ok {
					t.Errorf("parameters = %#v, want JSON string", fn["parameters"])
				}
			}
			if string(payload.ToolChoice) != `"required"` {
				t.Errorf("tool_choice = %s, want \"required\"", payload.ToolChoice)
			}
			if payload.ReasoningEffort != "high" {
				t.Errorf("reasoning_effort = %q, want high", payload.ReasoningEffort)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: output\ndata: {\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"function_call\":{\"name\":\"get_weather\",\"partial_arguments\":\"{\\\"ci\"}}]}\n\n")
			_, _ = fmt.Fprint(w, "event: output\ndata: {\"tool_calls\":[{\"index\":0,\"function_call\":{\"partial_arguments\":\"ty\\\":\\\"SF\\\"}\"}}]}\n\n")
			// A retry replays the whole call; the aggregator must overwrite, not append.
			_, _ = fmt.Fprint(w, "event: output\ndata: {\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"function_call\":{\"arguments\":\"{\\\"city\\\":\\\"SF\\\"}\"}}]}\n\n")
			_, _ = fmt.Fprint(w, "event: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var deltas []ToolCallDelta
	outcome, err := testTraeClient(server).Chat(context.Background(), "pat-tools", "chat-model", []Message{{
		Role: "user", Content: []ContentPart{{Type: "text", Text: "weather?"}},
	}}, ChatOptions{
		Tools: []ToolDefinition{{
			Name:        "get_weather",
			Description: "Look up the weather",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
		ToolChoice:      json.RawMessage(`"required"`),
		ReasoningEffort: "high",
		Stream:          true,
	}, nil, func(delta ToolCallDelta) error {
		deltas = append(deltas, delta)
		return nil
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !outcome.ToolUseSeen {
		t.Fatalf("ToolUseSeen = false, want true")
	}
	if len(outcome.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v, want one", outcome.ToolCalls)
	}
	call := outcome.ToolCalls[0]
	if call.ID != "call_a" || call.Name != "get_weather" || call.Arguments != `{"city":"SF"}` {
		t.Fatalf("unexpected tool call: %#v", call)
	}
	if len(deltas) != 3 {
		t.Fatalf("deltas = %d, want 3", len(deltas))
	}
	if deltas[0].Index != 0 || deltas[0].Name != "get_weather" {
		t.Fatalf("first delta = %#v", deltas[0])
	}
}

func TestChatRetriesExpiredJWTAuthFailure(t *testing.T) {
	var exchanges, chats atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cloudide/api/v3/trae/oauth/ExchangeToken":
			n := exchanges.Add(1)
			writeTraeToken(w, fmt.Sprintf("jwt-auth-%d", n))
		case "/api/ide/v1/cli/get_config_list":
			writeTraeModels(w)
		case "/api/ide/v2/llm_raw_chat":
			if chats.Add(1) == 1 {
				http.Error(w, "expired", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: output\ndata: {\"response\":\"recovered\"}\n\n")
			_, _ = fmt.Fprint(w, "event: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	outcome, err := testTraeClient(server).Chat(context.Background(), "pat-auth", "chat-model", []Message{{
		Role: "user", Content: []ContentPart{{Type: "text", Text: "hi"}},
	}}, ChatOptions{}, nil, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if outcome.Text != "recovered" || chats.Load() != 2 || exchanges.Load() != 2 {
		t.Fatalf("outcome=%#v chats=%d exchanges=%d", outcome, chats.Load(), exchanges.Load())
	}
}

func TestChatRefreshesJWTWhenModelRegistryRejectsCachedToken(t *testing.T) {
	var exchanges, registryCalls, chats atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cloudide/api/v3/trae/oauth/ExchangeToken":
			n := exchanges.Add(1)
			writeTraeToken(w, fmt.Sprintf("jwt-registry-%d", n))
		case "/api/ide/v1/cli/get_config_list":
			if registryCalls.Add(1) == 1 {
				http.Error(w, "revoked", http.StatusUnauthorized)
				return
			}
			if got := r.Header.Get("Authorization"); got != "Cloud-IDE-JWT jwt-registry-2" {
				t.Errorf("registry Authorization = %q", got)
			}
			writeTraeModels(w)
		case "/api/ide/v2/llm_raw_chat":
			chats.Add(1)
			if got := r.Header.Get("Authorization"); got != "Cloud-IDE-JWT jwt-registry-2" {
				t.Errorf("chat Authorization = %q", got)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: output\ndata: {\"response\":\"recovered\"}\n\n")
			_, _ = fmt.Fprint(w, "event: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	outcome, err := testTraeClient(server).Chat(context.Background(), "pat-registry-auth", "chat-model", []Message{{
		Role: "user", Content: []ContentPart{{Type: "text", Text: "hi"}},
	}}, ChatOptions{}, nil, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if outcome.Text != "recovered" || exchanges.Load() != 2 || registryCalls.Load() != 2 || chats.Load() != 1 {
		t.Fatalf("outcome=%#v exchanges=%d registry=%d chats=%d", outcome, exchanges.Load(), registryCalls.Load(), chats.Load())
	}
}

func TestChatRejectsImagesForNonMultimodalModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cloudide/api/v3/trae/oauth/ExchangeToken":
			writeTraeToken(w, "jwt-image")
		case "/api/ide/v1/cli/get_config_list":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"code":0,"config_info_list":[{"config_name":"text-only","usage":"chat_completion","model_detail_list":[{"model_name":"text-only__v1"}],"display_config":{"multimodal":false}}]}`)
		case "/api/ide/v2/llm_raw_chat":
			t.Error("llm_raw_chat must not be called for a rejected image")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	_, err := testTraeClient(server).Chat(context.Background(), "pat-image", "text-only", []Message{{
		Role: "user", Content: []ContentPart{
			{Type: "text", Text: "what is this?"},
			{Type: "image_url", ImageURL: &ImageURL{URL: "data:image/png;base64,AA=="}},
		},
	}}, ChatOptions{}, nil, nil)
	e, ok := err.(*Error)
	if !ok || e.Kind != ErrorKindBadRequest {
		t.Fatalf("error = %#v, want bad_request", err)
	}
}
