package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/trae"
	"github.com/gin-gonic/gin"
)

// defaultTraeModelIDs is the offline fallback for /v1/models and group model
// candidates. The authoritative list always comes from get_config_list because
// Trae's config_name values change with staged rollouts.
var defaultTraeModelIDs = []string{
	"glm-5.3-flash",
	"deepseek-V4-Pro",
	"DeepSeek-V4-Flash",
	"DeepSeek-V4-Flash-Official",
	"DeepSeek-V4-Pro-Official",
	"Doubao_1_6",
	"Doubao-Seed-2.0-Code",
	"Doubao-Seed-2.1-pro",
	"Doubao-Seed-2.1-turbo",
	"Doubao-Seed-Evolving",
	"glm-5.1",
	"glm-5.2",
	"glm-5.3",
	"glm-5v-turbo",
	"kimi-k2.7-code",
	"kimi-k3",
	"minimax-m2.7",
	"minimax-m3",
	"qwen-3.7-plus",
	"qwen3.8-max",
}

func DefaultTraeModelIDs() []string {
	return append([]string(nil), defaultTraeModelIDs...)
}

func (s *AccountTestService) fetchTraeUpstreamModels(ctx context.Context, account *Account) ([]string, []byte, error) {
	proxyURL := ""
	if account != nil && account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	client := trae.NewClient(traeBaseURL(account), func(req *http.Request) (*http.Response, error) {
		if s == nil || s.httpUpstream == nil {
			return nil, fmt.Errorf("upstream HTTP client is not configured")
		}
		return s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	})
	models, err := client.ListModels(ctx, traePAT(account))
	if err != nil {
		return nil, nil, newUpstreamModelSyncUpstreamError("Failed to fetch Trae models: "+traeClientErrorMessage(err), err)
	}
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if strings.TrimSpace(model.ConfigName) == "" {
			continue
		}
		ids = append(ids, model.ConfigName)
	}
	body, _ := json.Marshal(map[string]any{
		"data": func() []map[string]string {
			out := make([]map[string]string, 0, len(ids))
			for _, id := range ids {
				out = append(out, map[string]string{"id": id, "object": "model", "owned_by": "trae"})
			}
			return out
		}(),
	})
	return ids, body, nil
}

func (s *AccountTestService) testTraeAccountConnection(c *gin.Context, account *Account, modelID, prompt string) error {
	ctx := c.Request.Context()
	client := s.traeClientForTest(account)
	if _, err := client.JWT(ctx, traePAT(account)); err != nil {
		return s.sendErrorAndEnd(c, traeClientErrorMessage(err))
	}
	models, err := client.ListModels(ctx, traePAT(account))
	if err != nil {
		return s.sendErrorAndEnd(c, traeClientErrorMessage(err))
	}
	testModel := strings.TrimSpace(modelID)
	if testModel == "" {
		testModel = account.providerDefaultTestModel()
	}
	testModel = account.GetMappedModel(testModel)
	found := false
	for _, model := range models {
		if strings.EqualFold(model.ConfigName, testModel) {
			testModel = model.ConfigName
			found = true
			break
		}
	}
	if !found && len(models) > 0 {
		testModel = models[0].ConfigName
	}

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()
	s.sendEvent(c, TestEvent{Type: "test_start", Model: testModel})
	s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("Trae PAT is valid. %d models available.", len(models))})
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

func (s *AccountTestService) traeClientForTest(account *Account) *trae.Client {
	proxyURL := ""
	if account != nil && account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	return trae.NewClient(traeBaseURL(account), func(req *http.Request) (*http.Response, error) {
		if s == nil || s.httpUpstream == nil {
			return nil, fmt.Errorf("upstream HTTP client is not configured")
		}
		return s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	})
}
