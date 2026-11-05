package trae

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	DefaultBaseURL = "https://api.enterprise.trae.cn"

	ideVersionCode = "20260206"
	ideVersion     = "99.99.99"
	appID          = "7b3f9dc2-8a4e-5c6d-2f1b-9e4a3c5b7df0"
	upstreamUA     = "Go-http-client/1.1"
)

// Doer performs HTTP requests. The service layer adapts its shared upstream
// client so Trae reuses the account's proxy, concurrency and TLS settings.
type Doer func(*http.Request) (*http.Response, error)

// Client talks to one Trae cluster. It is safe for concurrent use.
type Client struct {
	baseURL string
	do      Doer
}

func NewClient(baseURL string, do Doer) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{baseURL: baseURL, do: do}
}

func (c *Client) BaseURL() string { return c.baseURL }

func (c *Client) doRequest(req *http.Request) (*http.Response, error) {
	if c == nil || c.do == nil {
		return nil, fmt.Errorf("trae http client is not configured")
	}
	return c.do(req)
}

func genRequestID() string {
	buf := make([]byte, 21)
	if _, err := rand.Read(buf); err != nil {
		now := time.Now().UnixNano()
		for i := range buf {
			buf[i] = byte(now >> uint(8*(i%8)))
		}
	}
	return "021" + hex.EncodeToString(buf)
}

func genConversationID() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return "conv-" + hex.EncodeToString(buf)
}

func genSessionID() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return "sess-" + hex.EncodeToString(buf)
}

func baseHeaders(req *http.Request, requestID string) {
	req.Header.Set("User-Agent", upstreamUA)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("X-App-Id", appID)
	req.Header.Set("X-Client-Request-Id", requestID)
	req.Header.Set("X-Tt-Logid", requestID)
	req.Header.Set("X-Ide-Version-Code", ideVersionCode)
}

func readLimitedBody(resp *http.Response, limit int64) (string, error) {
	if resp == nil || resp.Body == nil {
		return "", nil
	}
	defer func() { _ = resp.Body.Close() }()
	if limit <= 0 {
		limit = 1 << 20
	}
	buf, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return "", err
	}
	return string(buf), nil
}

func sanitizeUpstreamDetail(text string) string {
	t := strings.TrimSpace(text)
	if len(t) > 300 {
		t = t[:300]
	}
	lower := strings.ToLower(t)
	if strings.HasPrefix(lower, "<html") || strings.HasPrefix(lower, "<!doctype") || strings.Contains(lower, "<head") {
		return "upstream returned an HTML error page (likely an intermediary proxy failure)"
	}
	return t
}

// ---------------------------------------------------------------------------
// PAT -> Cloud-IDE-JWT exchange
// ---------------------------------------------------------------------------

type tokenEntry struct {
	token    string
	expireAt time.Time
}

type tokenFlight struct {
	done  chan struct{}
	token string
	err   error
}

var tokenCache = struct {
	sync.Mutex
	entries map[string]tokenEntry
	flights map[string]*tokenFlight
}{
	entries: make(map[string]tokenEntry),
	flights: make(map[string]*tokenFlight),
}

func (c *Client) tokenKey(pat string) string {
	return c.baseURL + "\x00" + strings.TrimSpace(pat)
}

// JWT returns a cached token or exchanges the PAT for a new one. Concurrent
// callers for the same PAT share one ExchangeToken request.
func (c *Client) JWT(ctx context.Context, pat string) (string, error) {
	pat = strings.TrimSpace(pat)
	if pat == "" {
		return "", newError(ErrorKindAuth, http.StatusUnauthorized, "Trae PAT is required")
	}
	key := c.tokenKey(pat)

	tokenCache.Lock()
	if entry, ok := tokenCache.entries[key]; ok && time.Now().Before(entry.expireAt.Add(-10*time.Minute)) {
		tokenCache.Unlock()
		return entry.token, nil
	}
	if flight, ok := tokenCache.flights[key]; ok {
		tokenCache.Unlock()
		select {
		case <-flight.done:
			return flight.token, flight.err
		case <-ctx.Done():
			return "", newError(ErrorKindAborted, 499, "client canceled the request")
		}
	}
	flight := &tokenFlight{done: make(chan struct{})}
	tokenCache.flights[key] = flight
	tokenCache.Unlock()

	token, expireAt, err := c.exchangeToken(ctx, pat)

	tokenCache.Lock()
	delete(tokenCache.flights, key)
	if err == nil {
		tokenCache.entries[key] = tokenEntry{token: token, expireAt: expireAt}
	}
	tokenCache.Unlock()

	flight.token, flight.err = token, err
	close(flight.done)
	return token, err
}

// InvalidateJWT drops the cached token for a PAT so the next call re-exchanges it.
func (c *Client) InvalidateJWT(pat string) {
	key := c.tokenKey(pat)
	tokenCache.Lock()
	delete(tokenCache.entries, key)
	tokenCache.Unlock()
}

func (c *Client) exchangeToken(ctx context.Context, pat string) (string, time.Time, error) {
	requestID := genRequestID()
	body, _ := json.Marshal(map[string]string{"RefreshToken": pat})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/cloudide/api/v3/trae/oauth/ExchangeToken", bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, wrapError(ErrorKindFatal, 0, "build Trae token request", err)
	}
	req.Header.Set("Content-Type", "application/json")
	baseHeaders(req, requestID)
	req.Header.Set("X-Cloudide-Token", "")

	resp, err := c.doRequest(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", time.Time{}, newError(ErrorKindAborted, 499, "client canceled the request")
		}
		return "", time.Time{}, wrapError(ErrorKindTransient, 0, "Trae token exchange failed: "+err.Error(), err)
	}
	text, readErr := readLimitedBody(resp, 1<<20)
	if readErr != nil {
		return "", time.Time{}, wrapError(ErrorKindTransient, resp.StatusCode, "read Trae token response", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		kind := ErrorKindTransient
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			kind = ErrorKindAuth
		}
		return "", time.Time{}, newError(kind, resp.StatusCode, fmt.Sprintf("Trae token exchange failed (HTTP %d)", resp.StatusCode)).withDetail(sanitizeUpstreamDetail(text))
	}

	var parsed struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
		Data    struct {
			Token         string `json:"Token"`
			TokenExpireAt int64  `json:"TokenExpireAt"`
		} `json:"Data"`
	}
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return "", time.Time{}, newError(ErrorKindProtocol, resp.StatusCode, "Trae token exchange returned invalid JSON").withDetail(sanitizeUpstreamDetail(text))
	}
	if strings.TrimSpace(parsed.Data.Token) == "" {
		code := strings.TrimSpace(string(parsed.Code))
		message := strings.TrimSpace(parsed.Message)
		detail := sanitizeUpstreamDetail(text)
		msg := "Trae token exchange returned no token"
		if code != "" && code != "null" {
			msg += " (code=" + code + ")"
		}
		if message != "" {
			msg += ": " + message
		}
		return "", time.Time{}, newError(ErrorKindAuth, resp.StatusCode, msg).withDetail(detail)
	}
	expireAt := time.Now().Add(6 * 24 * time.Hour)
	if parsed.Data.TokenExpireAt > 0 {
		expireAt = time.UnixMilli(parsed.Data.TokenExpireAt)
	}
	return parsed.Data.Token, expireAt, nil
}

// ---------------------------------------------------------------------------
// Dynamic model registry
// ---------------------------------------------------------------------------

type registryEntry struct {
	models    []ModelInfo
	byConfig  map[string]ModelInfo
	fetchedAt time.Time
}

type registryFlight struct {
	done   chan struct{}
	models []ModelInfo
	err    error
}

var registryCache = struct {
	sync.Mutex
	entries map[string]registryEntry
	flights map[string]*registryFlight
}{
	entries: make(map[string]registryEntry),
	flights: make(map[string]*registryFlight),
}

func (c *Client) registryKey(pat string) string {
	return c.baseURL + "\x00" + strings.TrimSpace(pat)
}

// ListModels fetches the live config_info_list for this PAT.
func (c *Client) ListModels(ctx context.Context, pat string) ([]ModelInfo, error) {
	token, err := c.JWT(ctx, pat)
	if err != nil {
		return nil, err
	}
	models, err := c.listModelsWithToken(ctx, pat, token)
	if e, ok := err.(*Error); ok && e.Kind == ErrorKindAuth {
		c.InvalidateJWT(pat)
		token, err = c.JWT(ctx, pat)
		if err != nil {
			return nil, err
		}
		return c.listModelsWithToken(ctx, pat, token)
	}
	return models, err
}

func (c *Client) listModelsWithToken(ctx context.Context, pat, token string) ([]ModelInfo, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, newError(ErrorKindAborted, 499, "client canceled the request")
			case <-time.After(1500 * time.Millisecond):
			}
		}
		models, err := c.fetchModelList(ctx, token)
		if err == nil && len(models) > 0 {
			return models, nil
		}
		if err != nil {
			lastErr = err
			if e, ok := err.(*Error); ok && e.Kind == ErrorKindAuth {
				return nil, err
			}
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, newError(ErrorKindTransient, http.StatusBadGateway, "Trae model registry returned an empty response")
}

func (c *Client) fetchModelList(ctx context.Context, token string) ([]ModelInfo, error) {
	requestID := genRequestID()
	body := strings.NewReader(`{"function":"chat"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/ide/v1/cli/get_config_list", body)
	if err != nil {
		return nil, wrapError(ErrorKindFatal, 0, "build Trae model list request", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+token)
	baseHeaders(req, requestID)

	resp, err := c.doRequest(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, newError(ErrorKindAborted, 499, "client canceled the request")
		}
		return nil, wrapError(ErrorKindTransient, 0, "Trae model list request failed: "+err.Error(), err)
	}
	text, readErr := readLimitedBody(resp, 4<<20)
	if readErr != nil {
		return nil, wrapError(ErrorKindTransient, resp.StatusCode, "read Trae model list response", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		kind := ErrorKindTransient
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			kind = ErrorKindAuth
		}
		return nil, newError(kind, resp.StatusCode, fmt.Sprintf("Trae model list failed (HTTP %d)", resp.StatusCode)).withDetail(sanitizeUpstreamDetail(text))
	}

	var parsed struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		List    []struct {
			ConfigName string `json:"config_name"`
			Usage      string `json:"usage"`
			Details    []struct {
				ModelName       string `json:"model_name"`
				MaxTokens       int    `json:"max_tokens"`
				PromptMaxTokens int    `json:"prompt_max_tokens"`
			} `json:"model_detail_list"`
			Display struct {
				Multimodal bool `json:"multimodal"`
				Invisible  bool `json:"is_invisible_to_user"`
			} `json:"display_config"`
		} `json:"config_info_list"`
	}
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return nil, newError(ErrorKindProtocol, resp.StatusCode, "Trae model list returned invalid JSON").withDetail(sanitizeUpstreamDetail(text))
	}
	if len(parsed.List) == 0 {
		return nil, newError(ErrorKindTransient, http.StatusBadGateway, fmt.Sprintf("Trae model list was empty (code=%d %s)", parsed.Code, parsed.Message))
	}
	models := make([]ModelInfo, 0, len(parsed.List))
	for _, item := range parsed.List {
		if strings.TrimSpace(item.ConfigName) == "" || item.Usage != "chat_completion" || len(item.Details) == 0 {
			continue
		}
		modelName := strings.TrimSpace(item.Details[0].ModelName)
		if modelName == "" {
			continue
		}
		models = append(models, ModelInfo{
			ConfigName:      item.ConfigName,
			ModelName:       modelName,
			PromptMaxTokens: item.Details[0].PromptMaxTokens,
			MaxTokens:       item.Details[0].MaxTokens,
			Multimodal:      item.Display.Multimodal,
			Invisible:       item.Display.Invisible,
		})
	}
	if len(models) == 0 {
		return nil, newError(ErrorKindTransient, http.StatusBadGateway, "Trae model list contained no chat_completion models")
	}
	return models, nil
}

func (c *Client) refreshRegistry(ctx context.Context, pat, token string) ([]ModelInfo, error) {
	key := c.registryKey(pat)

	registryCache.Lock()
	if flight, ok := registryCache.flights[key]; ok {
		registryCache.Unlock()
		select {
		case <-flight.done:
			return flight.models, flight.err
		case <-ctx.Done():
			return nil, newError(ErrorKindAborted, 499, "client canceled the request")
		}
	}
	flight := &registryFlight{done: make(chan struct{})}
	registryCache.flights[key] = flight
	registryCache.Unlock()

	models, err := c.listModelsWithToken(ctx, pat, token)

	registryCache.Lock()
	delete(registryCache.flights, key)
	if err == nil {
		byConfig := make(map[string]ModelInfo, len(models))
		for _, model := range models {
			byConfig[strings.ToLower(model.ConfigName)] = model
		}
		registryCache.entries[key] = registryEntry{models: models, byConfig: byConfig, fetchedAt: time.Now()}
	}
	registryCache.Unlock()

	flight.models, flight.err = models, err
	close(flight.done)
	return models, err
}

func (c *Client) registryState(pat string) (registryEntry, bool) {
	key := c.registryKey(pat)
	registryCache.Lock()
	defer registryCache.Unlock()
	entry, ok := registryCache.entries[key]
	return entry, ok
}

func (c *Client) ResolveModel(ctx context.Context, pat, requested string) (ModelInfo, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return ModelInfo{}, newError(ErrorKindBadRequest, http.StatusBadRequest, "model is required")
	}
	token, err := c.JWT(ctx, pat)
	if err != nil {
		return ModelInfo{}, err
	}
	refresh := func() error {
		_, err := c.refreshRegistry(ctx, pat, token)
		if e, ok := err.(*Error); ok && e.Kind == ErrorKindAuth {
			// Cached JWT can be revoked before its local expiry. Exchange once
			// more only when the problem is upstream auth, not a bad PAT.
			c.InvalidateJWT(pat)
			token, err = c.JWT(ctx, pat)
			if err != nil {
				return err
			}
			_, err = c.refreshRegistry(ctx, pat, token)
		}
		return err
	}
	entry, ok := c.registryState(pat)
	if !ok || time.Since(entry.fetchedAt) > 5*time.Minute {
		if err := refresh(); err != nil {
			return ModelInfo{}, err
		}
	}
	entry, _ = c.registryState(pat)
	if info, ok := entry.byConfig[strings.ToLower(requested)]; ok {
		return info, nil
	}
	// Miss may be a newly rolled-out config_name; force one refresh before failing.
	if err := refresh(); err != nil {
		return ModelInfo{}, err
	}
	entry, _ = c.registryState(pat)
	if info, ok := entry.byConfig[strings.ToLower(requested)]; ok {
		return info, nil
	}
	available := make([]string, 0, len(entry.models))
	for _, model := range entry.models {
		available = append(available, model.ConfigName)
	}
	return ModelInfo{}, newError(ErrorKindModelNotFound, http.StatusNotFound,
		fmt.Sprintf("model %s is not available on Trae. Available models: %s", requested, strings.Join(available, ", ")))
}
