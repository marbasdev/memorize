package discovery

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"praxis/internal/inference/providers"
)

var systemOneClient = &http.Client{
	Timeout: 2500 * time.Millisecond,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func sameURLHost(rawA, rawB string) bool {
	uA, errA := url.Parse(strings.TrimSpace(rawA))
	uB, errB := url.Parse(strings.TrimSpace(rawB))
	if errA != nil || errB != nil || uA.Hostname() == "" || uB.Hostname() == "" {
		return false
	}
	return strings.EqualFold(uA.Hostname(), uB.Hostname())
}

type SystemOneModelOption struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Engine      string `json:"engine"` // "flide" or "jev"
	Recommended bool   `json:"recommended"`
	Desc        string `json:"desc"`
}

type SystemOneDiscovery struct {
	Source       string                 `json:"source"`        // "live_flide", "typesafe_cloud", "builtin"
	Engine       string                 `json:"engine"`        // "flide" or "jev"
	HealthStatus string                 `json:"health_status"` // "ok", "loading", "cloud", "offline"
	ServedModel  string                 `json:"served_model,omitempty"`
	Format       string                 `json:"format,omitempty"`
	Version      string                 `json:"version,omitempty"`
	Models       []SystemOneModelOption `json:"models"`
	APIError     string                 `json:"api_error,omitempty"`
}

func DefaultFlideModels() []SystemOneModelOption {
	return []SystemOneModelOption{
		{
			ID: "jev-latest", Name: "jev-latest (Universal Alias)", Engine: "flide", Recommended: true,
			Desc: "Universal Flide alias — automatically routes to whichever model the Flide server has loaded",
		},
		{
			ID: "OmniJev/OneJev1.6-0.8B", Name: "OmniJev/OneJev1.6-0.8B", Engine: "flide", Recommended: true,
			Desc: "Dedicated 0.8B System 1 decision engine (~1.6 GB VRAM, sub-10ms single-token probability)",
		},
		{
			ID: "perplexity-ai/pplx-decider-flash-v1.2", Name: "pplx-decider-flash-v1.2", Engine: "flide",
			Desc: "High-precision System 1 decider model supported by OmniJev/Flide",
		},
	}
}

func DefaultTypesafeJevModels() []SystemOneModelOption {
	return []SystemOneModelOption{
		{
			ID: "jev-1.13.0", Name: "TypeSafe Jev 1.13.0", Engine: "jev", Recommended: true,
			Desc: "Managed TypeSafe Cloud System 1 decision engine",
		},
		{
			ID: "jev-latest", Name: "jev-latest", Engine: "jev",
			Desc: "Rolling latest System 1 model alias",
		},
	}
}

// DiscoverSystemOneEndpoint probes Flide/OmniJev or returns TypeSafe Cloud models without SSRF key leaks.
func DiscoverSystemOneEndpoint(ctx context.Context, rawCfg map[string]string, overrideURL, overrideKey string) SystemOneDiscovery {
	defaultCloud := "https://api.typesafe.ai/v1/sys" + "temone"
	savedURL := EnvOrDefault(rawCfg["jev_api_url"], "JEV_API_URL", "")
	targetURL := strings.TrimSpace(overrideURL)
	if targetURL == "" {
		targetURL = EnvOrDefault(savedURL, "", defaultCloud)
	}
	apiKey := strings.TrimSpace(overrideKey)
	if apiKey == "" && (overrideURL == "" || providers.IsTypesafeCloudURL(targetURL) || (savedURL != "" && sameURLHost(targetURL, savedURL))) {
		apiKey = EnvOrDefault(rawCfg["typesafe_api_key"], "TYPESAFE_API_KEY", "")
	}

	u, err := url.Parse(targetURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return SystemOneDiscovery{
			Source: "builtin", Engine: "flide", HealthStatus: "offline",
			Models: DefaultFlideModels(), APIError: "invalid System 1 API URL",
		}
	}

	if providers.IsTypesafeCloudURL(targetURL) {
		return SystemOneDiscovery{
			Source:       "typesafe_cloud",
			Engine:       "jev",
			HealthStatus: "cloud",
			Models:       DefaultTypesafeJevModels(),
		}
	}

	base := providers.ResolveSystemOneBaseURL(targetURL)
	out := SystemOneDiscovery{
		Source:       "builtin",
		Engine:       "flide",
		HealthStatus: "offline",
	}

	cctx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()

	bearerVal := ""
	if apiKey != "" {
		bearerVal = "Bearer " + apiKey
	}

	var probeErr string
	if code, body, err := httpGetBody(cctx, systemOneClient, base+"/health", "Authorization", bearerVal, 8192); err == nil {
		var h struct {
			Status  string `json:"status"`
			Model   string `json:"model"`
			Format  string `json:"format"`
			Engine  string `json:"engine"`
			Version string `json:"version"`
		}
		_ = json.Unmarshal(body, &h)
		if h.Status == "ok" || h.Status == "loading" {
			out.HealthStatus = h.Status
			out.ServedModel = strings.TrimSpace(h.Model)
			out.Format = strings.TrimSpace(h.Format)
			out.Version = strings.TrimSpace(h.Version)
			out.Source = "live_flide"
		} else if code >= 200 && code < 300 {
			out.HealthStatus = "ok"
			out.Source = "live_flide"
		}
	} else {
		probeErr = err.Error()
	}

	var liveModels []SystemOneModelOption
	seen := map[string]bool{}
	addModel := func(opt SystemOneModelOption) {
		key := strings.TrimSpace(opt.ID)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		liveModels = append(liveModels, opt)
	}

	if code, body, err := httpGetBody(cctx, systemOneClient, base+"/v1/models", "Authorization", bearerVal, 32768); err == nil && code >= 200 && code < 300 {
		var payload struct {
			Models []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"models"`
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &payload) == nil {
			out.Source = "live_flide"
			if out.HealthStatus == "offline" {
				out.HealthStatus = "ok"
			}
			addModel(SystemOneModelOption{
				ID: "jev-latest", Name: "jev-latest (Universal Alias)", Engine: "flide", Recommended: true,
				Desc: "Universal Flide alias — routes to currently served model",
			})
			for _, m := range payload.Models {
				desc := strings.TrimSpace(m.Description)
				if desc == "" {
					desc = "Currently served by Flide (/v1/models)"
				}
				addModel(SystemOneModelOption{
					ID: m.Name, Name: m.Name, Engine: "flide", Recommended: true, Desc: desc,
				})
				if out.ServedModel == "" {
					out.ServedModel = strings.TrimSpace(m.Name)
				}
			}
			for _, d := range payload.Data {
				addModel(SystemOneModelOption{
					ID: d.ID, Name: d.ID, Engine: "flide", Recommended: d.ID == out.ServedModel,
					Desc: "Discovered from /v1/models",
				})
			}
		}
	} else if err != nil && probeErr == "" {
		probeErr = err.Error()
	}

	if out.ServedModel != "" && !seen[out.ServedModel] {
		addModel(SystemOneModelOption{
			ID: "jev-latest", Name: "jev-latest (Universal Alias)", Engine: "flide", Recommended: true,
			Desc: "Universal Flide alias — routes to " + out.ServedModel,
		})
		addModel(SystemOneModelOption{
			ID: out.ServedModel, Name: out.ServedModel, Engine: "flide", Recommended: true,
			Desc: "Currently loaded on Flide server",
		})
	}
	for _, def := range DefaultFlideModels() {
		addModel(def)
	}
	out.Models = liveModels
	if out.HealthStatus == "offline" && probeErr != "" {
		out.APIError = probeErr
	}
	return out
}
