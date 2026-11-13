package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// LocalProbeTarget describes a standard local inference endpoint for protocol probing.
type LocalProbeTarget struct {
	Label    string
	Provider string // "local" or "jev"
	BaseURL  string
}

// DetectedLocalServer represents an already-running local inference server verified via HTTP API.
type DetectedLocalServer struct {
	Label    string
	Provider string
	BaseURL  string
	Models   []string
}

func defaultLocalProbeTargets() []LocalProbeTarget {
	return []LocalProbeTarget{
		{Label: "Ollama", Provider: "local", BaseURL: "http://127.0.0.1:11434/v1"},
		{Label: "LM Studio", Provider: "local", BaseURL: "http://127.0.0.1:1234/v1"},
		{Label: "Flide / OmniJev", Provider: "jev", BaseURL: "http://127.0.0.1:8000"},
	}
}

var localHandshakeClient = &http.Client{
	Timeout: 180 * time.Millisecond,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func parseDiscoveredModelIDs(body []byte) ([]string, bool) {
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false
	}
	if payload.Data == nil && payload.Models == nil {
		return nil, false
	}
	seen := make(map[string]bool)
	var models []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			models = append(models, id)
		}
	}
	for _, d := range payload.Data {
		add(d.ID)
	}
	for _, m := range payload.Models {
		if m.ID != "" {
			add(m.ID)
		} else {
			add(m.Name)
		}
	}
	return models, true
}

func probeSingleLocalTarget(ctx context.Context, target LocalProbeTarget) (DetectedLocalServer, bool) {
	base := strings.TrimRight(strings.TrimSpace(target.BaseURL), "/")
	if base == "" {
		return DetectedLocalServer{}, false
	}
	cctx, cancel := context.WithTimeout(ctx, 180*time.Millisecond)
	defer cancel()

	if target.Provider == "jev" {
		if req, err := http.NewRequestWithContext(cctx, http.MethodGet, base+"/v1/models", nil); err == nil {
			if resp, err := localHandshakeClient.Do(req); err == nil {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 32768))
				_ = resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					if models, ok := parseDiscoveredModelIDs(body); ok {
						return DetectedLocalServer{
							Label: target.Label, Provider: "jev", BaseURL: base, Models: append([]string{"jev-latest"}, models...),
						}, true
					}
				}
			}
		}
		if req, err := http.NewRequestWithContext(cctx, http.MethodGet, base+"/health", nil); err == nil {
			if resp, err := localHandshakeClient.Do(req); err == nil {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
				_ = resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					var h struct {
						Status string `json:"status"`
						Model  string `json:"model"`
					}
					if json.Unmarshal(body, &h) == nil && (h.Status == "ok" || h.Status == "loading") {
						models := []string{"jev-latest"}
						if m := strings.TrimSpace(h.Model); m != "" {
							models = append(models, m)
						}
						return DetectedLocalServer{
							Label: target.Label, Provider: "jev", BaseURL: base, Models: models,
						}, true
					}
				}
			}
		}
		return DetectedLocalServer{}, false
	}

	modelsURL := base
	if !strings.HasSuffix(strings.ToLower(modelsURL), "/v1") {
		modelsURL += "/v1"
	}
	modelsURL += "/models"
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return DetectedLocalServer{}, false
	}
	resp, err := localHandshakeClient.Do(req)
	if err != nil {
		return DetectedLocalServer{}, false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32768))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return DetectedLocalServer{}, false
	}
	models, ok := parseDiscoveredModelIDs(body)
	if !ok {
		return DetectedLocalServer{}, false
	}
	return DetectedLocalServer{
		Label:    target.Label,
		Provider: "local",
		BaseURL:  base,
		Models:   models,
	}, true
}

// ProbeLocalInferenceServers sends parallel HTTP API handshakes to standard local inference
// ports (or explicit test targets) and returns only actively responding servers.
func ProbeLocalInferenceServers(ctx context.Context, targets ...LocalProbeTarget) []DetectedLocalServer {
	list := targets
	if len(list) == 0 {
		list = defaultLocalProbeTargets()
	}
	results := make([]DetectedLocalServer, len(list))
	found := make([]bool, len(list))
	var wg sync.WaitGroup
	for i, tgt := range list {
		wg.Add(1)
		go func(idx int, t LocalProbeTarget) {
			defer wg.Done()
			if det, ok := probeSingleLocalTarget(ctx, t); ok {
				results[idx] = det
				found[idx] = true
			}
		}(i, tgt)
	}
	wg.Wait()

	var out []DetectedLocalServer
	for i, ok := range found {
		if ok {
			out = append(out, results[i])
		}
	}
	return out
}
