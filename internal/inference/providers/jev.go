package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// httpClient sets a generous safety ceiling while letting caller ctx deadlines
// (e.g. lifecycle hooks vs MCP tool calls) govern per-request timeouts.
var httpClient = &http.Client{Timeout: 30 * time.Second}

const (
	sysOneEndpointPath = "/v1/sys" + "temone"
	sysOneTail         = "/sys" + "temone"
	typesafeCloudHost  = "typesafe.ai"
	defaultFlideBase   = "http://127.0.0.1:8000"
)

// ResolveSystemOneEndpoint normalizes either a base server URL (e.g. http://127.0.0.1:8000 for OmniJev/Flide)
// or a full System 1 endpoint URL into the canonical POST endpoint URL.
func ResolveSystemOneEndpoint(raw string) string {
	u := strings.TrimRight(strings.TrimSpace(raw), "/")
	if u == "" {
		return "https://api." + typesafeCloudHost + sysOneEndpointPath
	}
	low := strings.ToLower(u)
	switch {
	case strings.HasSuffix(low, sysOneTail):
		return u
	case strings.HasSuffix(low, "/v1"):
		return u + sysOneTail
	default:
		return u + sysOneEndpointPath
	}
}

// ResolveSystemOneBaseURL extracts the root server origin/prefix (stripping /v1/systemone or /v1)
// so auxiliary endpoints such as Flide's GET /health and GET /v1/models can be queried cleanly.
func ResolveSystemOneBaseURL(raw string) string {
	ep := ResolveSystemOneEndpoint(raw)
	low := strings.ToLower(ep)
	if strings.HasSuffix(low, sysOneEndpointPath) {
		return ep[:len(ep)-len(sysOneEndpointPath)]
	}
	if strings.HasSuffix(low, sysOneTail) {
		return ep[:len(ep)-len(sysOneTail)]
	}
	return strings.TrimRight(ep, "/")
}

func IsTypesafeCloudURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	return host == "api.typesafe.ai" || host == typesafeCloudHost
}

type JevAdapter struct {
	prefix   string
	endpoint string
	apiKey   string
	model    string
}

func NewJevAdapter() *BatchEvaluator {
	return NewJevAdapterConfig("", "")
}

func NewFlideAdapter(endpoint, apiKey, model string) *BatchEvaluator {
	ep := strings.TrimSpace(endpoint)
	if ep == "" {
		ep = strings.TrimSpace(os.Getenv("FLIDE_API_URL"))
	}
	if ep == "" {
		ep = defaultFlideBase
	}
	return NewJevAdapterConfig(ep, apiKey, model)
}

func NewJevAdapterConfig(endpoint, apiKey string, modelOpt ...string) *BatchEvaluator {
	rawEP := strings.TrimSpace(endpoint)
	if rawEP == "" {
		rawEP = strings.TrimSpace(os.Getenv("JEV_API_URL"))
	}
	if rawEP == "" {
		rawEP = strings.TrimSpace(os.Getenv("FLIDE_API_URL"))
	}
	ep := ResolveSystemOneEndpoint(rawEP)
	isCloud := IsTypesafeCloudURL(ep)

	k := strings.TrimSpace(apiKey)
	if k == "" {
		k = strings.TrimSpace(os.Getenv("TYPESAFE_API_KEY"))
	}
	if k == "" {
		k = strings.TrimSpace(os.Getenv("FLIDE_API_KEY"))
	}

	var m string
	if len(modelOpt) > 0 {
		m = strings.TrimSpace(modelOpt[0])
	}
	if m == "" {
		m = strings.TrimSpace(os.Getenv("JEV_MODEL"))
	}
	if m == "" {
		m = strings.TrimSpace(os.Getenv("FLIDE_MODEL"))
	}
	if m == "" {
		if isCloud {
			m = "jev-1.13.0"
		} else {
			// OmniJev/Flide registers "jev-latest" as a universal alias across all served decision models.
			m = "jev-latest"
		}
	}

	prefix := "flide:"
	chunkSize := 64
	if isCloud {
		prefix = "jev:"
		chunkSize = 25
	}
	raw := &JevAdapter{
		prefix:   prefix,
		endpoint: ep,
		apiKey:   k,
		model:    m,
	}
	return NewBatchEvaluator(prefix+m, &decisionEvaluator{tr: raw}, chunkSize, 6)
}

func NewLocalLayaAdapter() *BatchEvaluator {
	return NewLocalLayaAdapterConfig("", "")
}

func NewLocalLayaAdapterConfig(endpoint, apiKey string) *BatchEvaluator {
	ep := strings.TrimSpace(endpoint)
	if ep == "" {
		ep = os.Getenv("LOCAL_MODEL_URL")
	}
	if ep == "" {
		ep = defaultFlideBase
	}
	return NewJevAdapterConfig(ep, apiKey)
}

func (j *JevAdapter) Name() string { return j.prefix + j.model }

// CallDecisions posts a System 1 evaluation request to OmniJev Flide / TypeSafe Jev.
func (j *JevAdapter) CallDecisions(ctx context.Context, state string, questions map[string]decisionQuestion) (map[string]decisionAnswer, error) {
	body, err := json.Marshal(map[string]any{
		"model":     j.model,
		"state":     state,
		"questions": questions,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if j.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+j.apiKey)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("System 1 endpoint unreachable (%s): %w", j.endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		errBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if msg := strings.TrimSpace(string(errBytes)); msg != "" {
			return nil, fmt.Errorf("System 1 status %d (%s): %s", resp.StatusCode, j.endpoint, msg)
		}
		return nil, fmt.Errorf("System 1 status %d (%s)", resp.StatusCode, j.endpoint)
	}
	var out struct {
		Answers map[string]struct {
			Choice           string  `json:"choice"`
			Score            float64 `json:"score"`
			Noul             float64 `json:"noul"`
			Confidence       float64 `json:"confidence"`
			AnswerConfidence float64 `json:"answer_confidence"`
		} `json:"answers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	res := make(map[string]decisionAnswer, len(out.Answers))
	for k, a := range out.Answers {
		res[k] = decisionAnswer{
			Choice:           a.Choice,
			Score:            a.Score,
			Noul:             a.Noul,
			Confidence:       a.Confidence,
			AnswerConfidence: a.AnswerConfidence,
		}
	}
	return res, nil
}
