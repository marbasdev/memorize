package providers

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"memorize/internal/domain"
)

type GGUFRuntimeProbe struct {
	Supported        bool
	ServerRunning    bool
	RuntimeInstalled bool
	InstalledCount   int
	DownloadActive   bool
	DownloadTarget   string
}

type DriverBuildInput struct {
	Config        map[string]string
	ModelOverride string
	RawActiveProv string
	GGUFSupported bool
	GGUFProbe     GGUFRuntimeProbe
	WakeGGUF      func(context.Context, string) (string, error)
	EnsureGGUF    func(context.Context) error
}

type ProviderDriver struct {
	ID             string
	CacheKey       func(in DriverBuildInput) string
	Build          func(in DriverBuildInput) domain.Evaluator
	Startup        func(rawProv string, in DriverBuildInput) domain.Evaluator
	CheckReadiness func(ctx context.Context, in DriverBuildInput) (bool, string)
}

var (
	driversMu sync.RWMutex
	drivers   = make(map[string]ProviderDriver)
)

func RegisterDriver(drv ProviderDriver) {
	id := domain.NormalizeProviderID(drv.ID)
	if id == "" {
		return
	}
	drv.ID = id
	driversMu.Lock()
	drivers[id] = drv
	driversMu.Unlock()
}

func LookupDriver(provider string) (ProviderDriver, bool) {
	id := domain.NormalizeProviderID(provider)
	if id == "" {
		return ProviderDriver{}, false
	}
	driversMu.RLock()
	drv, ok := drivers[id]
	driversMu.RUnlock()
	return drv, ok
}

func StartupEvaluator(rawProv string, in DriverBuildInput) domain.Evaluator {
	if drv, ok := LookupDriver(rawProv); ok && drv.Startup != nil {
		return drv.Startup(rawProv, in)
	}
	drv, _ := LookupDriver("gguf")
	return drv.Startup(rawProv, in)
}

var setupProbeClient = &http.Client{Timeout: 200 * time.Millisecond}

func ProbeHTTPAlive(ctx context.Context, rawURL string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return false
	}
	resp, err := setupProbeClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode < 500
}

func bearerAuth(apiKey string) string {
	if strings.TrimSpace(apiKey) == "" {
		return ""
	}
	return "Bearer " + apiKey
}

func postJSONRaw(ctx context.Context, client *http.Client, endpoint string, payload []byte, hdrName, hdrVal string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if hdrName != "" && hdrVal != "" {
		req.Header.Set(hdrName, hdrVal)
	}
	return client.Do(req)
}

func resolveDriverModel(provider string, in DriverBuildInput) string {
	if mod := strings.TrimSpace(in.ModelOverride); mod != "" {
		return mod
	}
	return domain.ProviderModelFromConfig(in.Config, provider)
}

func cfgVal(cfg map[string]string, key string) string {
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg[key])
}

func init() {
	RegisterDriver(ProviderDriver{
		ID: "gguf",
		CacheKey: func(in DriverBuildInput) string {
			mod := resolveDriverModel("gguf", in)
			if !in.GGUFSupported {
				return strings.Join([]string{"local", cfgVal(in.Config, "local_model_url"), mod, cfgVal(in.Config, "local_api_key")}, "|")
			}
			return strings.Join([]string{"gguf", cfgVal(in.Config, "gguf_endpoint"), mod}, "|")
		},
		Build: func(in DriverBuildInput) domain.Evaluator {
			mod := resolveDriverModel("gguf", in)
			if !in.GGUFSupported {
				return NewOpenAICompatibleAdapter(cfgVal(in.Config, "local_model_url"), mod, cfgVal(in.Config, "local_api_key"))
			}
			ep := cfgVal(in.Config, "gguf_endpoint")
			if in.WakeGGUF != nil {
				targetMod := mod
				return newGGUFAdapter(ep, mod, nil, func(ctx context.Context) (string, error) {
					return in.WakeGGUF(ctx, targetMod)
				})
			}
			return NewGGUFAdapter(ep, mod)
		},
		Startup: func(_ string, in DriverBuildInput) domain.Evaluator {
			if domain.DefaultProvider() == "local" || !in.GGUFSupported {
				return NewOpenAICompatibleAdapter("", "", "")
			}
			return NewGGUFAdapter("", "", in.EnsureGGUF)
		},
		CheckReadiness: func(ctx context.Context, in DriverBuildInput) (bool, string) {
			if envEP := strings.TrimSpace(os.Getenv("LOCAL_GGUF_URL")); envEP != "" {
				if ProbeHTTPAlive(ctx, strings.TrimRight(envEP, "/")+"/health") {
					return true, ""
				}
				return false, "LOCAL_GGUF_URL unreachable (" + envEP + ")"
			}
			p := in.GGUFProbe
			if p.ServerRunning || (p.InstalledCount > 0 && p.RuntimeInstalled) {
				return true, ""
			}
			if p.DownloadActive {
				return false, "downloading local GGUF model (" + p.DownloadTarget + ")"
			}
			if p.InstalledCount > 0 && !p.RuntimeInstalled {
				return false, "llama-server runtime not installed"
			}
			return false, "no local GGUF model installed"
		},
	})

	RegisterDriver(ProviderDriver{
		ID: "gemini",
		CacheKey: func(in DriverBuildInput) string {
			mod := resolveDriverModel("gemini", in)
			return strings.Join([]string{"gemini", cfgVal(in.Config, "gemini_api_key"), mod}, "|")
		},
		Build: func(in DriverBuildInput) domain.Evaluator {
			return NewGeminiAdapterConfig(cfgVal(in.Config, "gemini_api_key"), resolveDriverModel("gemini", in))
		},
		Startup: func(_ string, _ DriverBuildInput) domain.Evaluator {
			return NewGeminiAdapterConfig("", "")
		},
		CheckReadiness: func(_ context.Context, in DriverBuildInput) (bool, string) {
			if domain.FirstNonEmpty(cfgVal(in.Config, "gemini_api_key"), os.Getenv("GEMINI_API_KEY")) == "" {
				return false, "GEMINI_API_KEY not set"
			}
			return true, ""
		},
	})

	RegisterDriver(ProviderDriver{
		ID: "local",
		CacheKey: func(in DriverBuildInput) string {
			mod := resolveDriverModel("local", in)
			locKey := domain.FirstNonEmpty(cfgVal(in.Config, "local_api_key"), cfgVal(in.Config, "typesafe_api_key"))
			return strings.Join([]string{"local", cfgVal(in.Config, "local_model_url"), mod, locKey}, "|")
		},
		Build: func(in DriverBuildInput) domain.Evaluator {
			mod := resolveDriverModel("local", in)
			locKey := domain.FirstNonEmpty(cfgVal(in.Config, "local_api_key"), cfgVal(in.Config, "typesafe_api_key"))
			return NewOpenAICompatibleAdapter(cfgVal(in.Config, "local_model_url"), mod, locKey)
		},
		Startup: func(_ string, _ DriverBuildInput) domain.Evaluator {
			return NewOpenAICompatibleAdapter("", "", "")
		},
		CheckReadiness: func(ctx context.Context, in DriverBuildInput) (bool, string) {
			locURL := domain.FirstNonEmpty(cfgVal(in.Config, "local_model_url"), os.Getenv("LOCAL_MODEL_URL"), "http://127.0.0.1:11434/v1")
			locKey := domain.FirstNonEmpty(cfgVal(in.Config, "local_api_key"), os.Getenv("LOCAL_API_KEY"), os.Getenv("OPENAI_API_KEY"))
			if locKey != "" && domain.FirstNonEmpty(cfgVal(in.Config, "local_model_url"), os.Getenv("LOCAL_MODEL_URL")) != "" {
				return true, ""
			}
			modelsURL := strings.TrimRight(locURL, "/")
			if !strings.HasSuffix(modelsURL, "/v1") {
				modelsURL += "/v1"
			}
			modelsURL += "/models"
			if ProbeHTTPAlive(ctx, modelsURL) {
				return true, ""
			}
			return false, "local OpenAI-compatible server unreachable (" + locURL + ")"
		},
	})

	RegisterDriver(ProviderDriver{
		ID: "jev",
		CacheKey: func(in DriverBuildInput) string {
			mod := resolveDriverModel("jev", in)
			return strings.Join([]string{
				strings.ToLower(strings.TrimSpace(in.RawActiveProv)),
				"jev",
				cfgVal(in.Config, "jev_api_url"),
				mod,
				cfgVal(in.Config, "typesafe_api_key"),
			}, "|")
		},
		Build: func(in DriverBuildInput) domain.Evaluator {
			mod := resolveDriverModel("jev", in)
			jevURL := cfgVal(in.Config, "jev_api_url")
			tsKey := cfgVal(in.Config, "typesafe_api_key")
			if strings.ToLower(strings.TrimSpace(in.RawActiveProv)) == "flide" && jevURL == "" {
				return NewFlideAdapter("", tsKey, mod)
			}
			return NewJevAdapterConfig(jevURL, tsKey, mod)
		},
		Startup: func(rawProv string, _ DriverBuildInput) domain.Evaluator {
			if strings.ToLower(strings.TrimSpace(rawProv)) == "flide" {
				return NewFlideAdapter("", "", "")
			}
			return NewJevAdapterConfig("", "")
		},
		CheckReadiness: func(ctx context.Context, in DriverBuildInput) (bool, string) {
			jevURL := domain.FirstNonEmpty(cfgVal(in.Config, "jev_api_url"), os.Getenv("JEV_API_URL"), "https://api.typesafe.ai/v1/sys"+"temone")
			if IsTypesafeCloudURL(jevURL) {
				if domain.FirstNonEmpty(cfgVal(in.Config, "typesafe_api_key"), os.Getenv("TYPESAFE_API_KEY"), os.Getenv("FLIDE_API_KEY")) == "" {
					return false, "TYPESAFE_API_KEY not set"
				}
				return true, ""
			}
			base := ResolveSystemOneBaseURL(jevURL)
			if ProbeHTTPAlive(ctx, base+"/health") || ProbeHTTPAlive(ctx, base+"/v1/models") {
				return true, ""
			}
			return false, "System 1 endpoint unreachable (" + jevURL + ")"
		},
	})
}
