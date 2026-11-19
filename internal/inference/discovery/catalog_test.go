package discovery_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"memorize/internal/domain"
	"memorize/internal/inference/discovery"
	"memorize/internal/inference/localmodel"
)

func TestModelRecommendationCriteriaAndLiveDiscovery(t *testing.T) {
	var gotGeminiHeader string
	mockCloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/models":
			_, _ = w.Write([]byte(`[
				{"id":"kokoro/kokoro-tts-GGUF","downloads":99999,"lastModified":"2026-11-01T10:00:00Z"},
				{"id":"ggml-org/gemma-4-E2B-it-GGUF","downloads":54321,"lastModified":"2026-10-28T12:00:00Z"}
			]`))
		case "/api/models/ggml-org/gemma-4-E2B-it-GGUF":
			_, _ = w.Write([]byte(`{
				"id":"ggml-org/gemma-4-E2B-it-GGUF",
				"downloads":54321,
				"lastModified":"2026-10-28T12:00:00Z",
				"siblings":[
					{"rfilename":"mtp-gemma-4-E2B.gguf","size":500000000},
					{"rfilename":"mmproj-F16.gguf","size":600000000},
					{"rfilename":"gemma-4-E2B-it-Q8_0.gguf","size":5100000000},
					{"rfilename":"gemma-4-E2B-it-Q4_K_M.gguf","size":2750000000}
				]
			}`))
		case "/v1beta/models":
			gotGeminiHeader = r.Header.Get("x-goog-api-key")
			_, _ = w.Write([]byte(`{
				"models":[
					{"name":"models/gemini-1.5-flash","displayName":"Legacy 1.5","supportedGenerationMethods":["generateContent"]},
					{"name":"models/gemini-3.5-flash-preview-tts","displayName":"TTS","supportedGenerationMethods":["generateContent"]},
					{"name":"models/gemini-3.5-flash-lite-001","displayName":"Gemini 3.5 Flash-Lite 001","supportedGenerationMethods":["generateContent"]},
					{"name":"models/gemini-3.5-pro","displayName":"Gemini 3.5 Pro","supportedGenerationMethods":["generateContent"]},
					{"name":"models/gemini-3.8-flash","displayName":"Gemini 3.8 Flash","supportedGenerationMethods":["generateContent"]},
					{"name":"models/gemini-3.5-flash-lite","displayName":"Gemini 3.5 Flash-Lite","supportedGenerationMethods":["generateContent"]}
				]
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockCloud.Close()

	t.Setenv("HF_API_BASE", mockCloud.URL)
	t.Setenv("GEMINI_API_BASE", mockCloud.URL)
	t.Setenv("GEMINI_API_KEY", "")

	mgr := localmodel.NewManager(t.TempDir())
	st := mgr.Status(localmodel.DefaultPort)
	if st.Hardware.RAMGB < 4 || st.Hardware.SafeBudgetGB <= 0 {
		t.Fatalf("expected valid hardware detection, got %+v", st.Hardware)
	}
	wantSupported := localmodel.IsSupportedPlatform()
	if st.Supported != wantSupported {
		t.Fatalf("expected Status.Supported=%v, got %v", wantSupported, st.Supported)
	}
	if wantSupported {
		if len(st.Presets) != 8 {
			t.Fatalf("expected 8 tiered late-2026 presets, got %d", len(st.Presets))
		}
	} else if len(st.Presets) != 0 {
		t.Fatalf("expected empty Manager.Status().Presets on unsupported platform %s, got %d", st.OS, len(st.Presets))
	}

	presets := localmodel.Presets()
	if len(presets) != 8 {
		t.Fatalf("expected 8 tiered late-2026 presets from localmodel.Presets(), got %d", len(presets))
	}
	tiers := map[string]int{}
	for _, p := range presets {
		tiers[p.Tier]++
		if p.FitBadge == "" || p.SizeHint == "" {
			t.Fatalf("preset %s missing fit badge or size hint: %+v", p.ID, p)
		}
	}
	if tiers["speed"] != 3 || tiers["balanced"] != 3 || tiers["accuracy"] != 2 {
		t.Fatalf("unexpected tier distribution: %+v", tiers)
	}

	hfModels, err := localmodel.DiscoverHuggingFaceModels(context.Background(), "latest", "gemma", true)
	if err != nil || len(hfModels) != 1 || hfModels[0].FileName != "gemma-4-E2B-it-Q4_K_M.gguf" || hfModels[0].Tier != "balanced" {
		t.Fatalf("expected gemma-4-E2B-it-Q4_K_M.gguf (balanced), got %+v (%v)", hfModels, err)
	}

	optsBuiltin, srcBuiltin, _ := discovery.DiscoverGeminiModels(context.Background(), map[string]string{}, "")
	if srcBuiltin != "builtin" || len(optsBuiltin) == 0 {
		t.Fatalf("expected builtin gemini defaults, got src=%q len=%d", srcBuiltin, len(optsBuiltin))
	}

	_, _, _ = discovery.DiscoverGeminiModels(context.Background(), map[string]string{}, "typed-unsaved-key")
	if gotGeminiHeader != "typed-unsaved-key" {
		t.Fatalf("expected typed override key header, got %q", gotGeminiHeader)
	}

	optsLive, srcLive, errMsg := discovery.DiscoverGeminiModels(context.Background(), map[string]string{"gemini_api_key": "test-live-gem-key"}, "")
	if srcLive != "live_api" || errMsg != "" {
		t.Fatalf("expected live gemini catalog without error, got src=%q err=%q", srcLive, errMsg)
	}
	if gotGeminiHeader != "test-live-gem-key" {
		t.Fatalf("expected x-goog-api-key header, got %q", gotGeminiHeader)
	}
	pos := map[string]int{}
	for i, o := range optsLive {
		pos[o.ID] = i
	}
	for _, excluded := range []string{"gemini-1.5-flash", "gemini-3.5-flash-preview-tts"} {
		if _, ok := pos[excluded]; ok {
			t.Fatalf("expected filtered model %q to be excluded", excluded)
		}
	}
	i0, ok0 := pos["gemini-3.5-flash-lite"]
	i1, ok1 := pos["gemini-3.5-flash-lite-001"]
	i2, ok2 := pos["gemini-3.8-flash"]
	i3, ok3 := pos["gemini-3.5-pro"]
	if !ok0 || !ok1 || !ok2 || !ok3 || !(i0 < i1 && i1 < i2 && i2 < i3) {
		t.Fatalf("expected ranked Gemini models (flash-lite < flash-lite-001 < 3.8-flash < 3.5-pro), pos=%v", pos)
	}
}

func TestDiscoveryConfigRoutingLocalProbeAndSystemOneSSRF(t *testing.T) {
	t.Run("preset_categories_mask_and_public_config", func(t *testing.T) {
		if got := discovery.NormalizePresetCategoriesJSON("invalid-json"); got != domain.DefaultPresetCategoriesJSON {
			t.Fatalf("expected default preset JSON on invalid input")
		}
		norm := discovery.NormalizePresetCategoriesJSON(`{"Architecture":["Build","api","build"]}`)
		if norm != `{"architecture":["api","build"]}` {
			t.Fatalf("unexpected normalized presets: %s", norm)
		}
		if masked := discovery.MaskSecret("sk-1234567890abcdef", "UNUSED_ENV"); masked != "Stored in DB (sk-1••••cdef)" {
			t.Fatalf("unexpected MaskSecret: %q", masked)
		}
		pub := discovery.PublicConfigWithGGUFPort(map[string]string{
			"active_provider": "flide",
			"gemini_api_key":  "AIza1234567890abcdef",
		}, "", 18088)
		if pub["effective_provider"] != "jev" || pub["gguf_endpoint"] != "http://127.0.0.1:18088" {
			t.Fatalf("unexpected PublicConfigWithGGUFPort: %+v", pub)
		}
	})

	t.Run("validate_and_apply_routing_pairs", func(t *testing.T) {
		pairs := map[string]string{}
		if err := discovery.ValidateAndApplyRoutingPairs("jev", "jev-latest", true, nil, pairs); err == nil {
			t.Fatalf("expected decision-only generative companion to fail")
		}
		if err := discovery.ValidateAndApplyRoutingPairs("gemini", "gemini-3.5-flash-lite", true, map[domain.EvalTask][2]string{
			domain.TaskVerify: {"gemini", "gemini-decision-1"},
		}, pairs); err != nil {
			t.Fatalf("expected valid routing pairs, got %v", err)
		}
		if pairs["gen_provider"] != "gemini" || pairs["task_verify_model"] != "gemini-decision-1" {
			t.Fatalf("unexpected applied pairs: %+v", pairs)
		}
	})

	t.Run("systemone_ssrf_and_local_probe", func(t *testing.T) {
		var savedHostAuth, foreignHostAuth string
		savedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			savedHostAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/health":
				_, _ = w.Write([]byte(`{"status":"ok","model":"OmniJev/OneJev1.6-0.8B"}`))
			case "/v1/models":
				_, _ = w.Write([]byte(`{"data":[{"id":"OmniJev/OneJev1.6-0.8B"}]}`))
			default:
				http.NotFound(w, r)
			}
		}))
		defer savedSrv.Close()

		foreignSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			foreignHostAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		}))
		defer foreignSrv.Close()

		cfg := map[string]string{
			"jev_api_url":      savedSrv.URL,
			"typesafe_api_key": "secret-ts-key",
		}
		resSaved := discovery.DiscoverSystemOneEndpoint(context.Background(), cfg, savedSrv.URL, "")
		if resSaved.Source != "live_flide" || savedHostAuth != "Bearer secret-ts-key" {
			t.Fatalf("expected saved host to receive key and discover live_flide, got %+v auth=%q", resSaved, savedHostAuth)
		}
		// Different port on loopback has same Hostname() ("127.0.0.1"), so test an unreachable non-matching hostname to confirm SSRF guard without leaking key.
		_ = discovery.DiscoverSystemOneEndpoint(context.Background(), map[string]string{
			"jev_api_url":      "https://saved.example.invalid",
			"typesafe_api_key": "secret-ts-key",
		}, foreignSrv.URL, "")
		if foreignHostAuth != "" {
			t.Fatalf("SSRF guard leaked saved API key to foreign host: %q", foreignHostAuth)
		}

		resCloud := discovery.DiscoverSystemOneEndpoint(context.Background(), map[string]string{}, "https://api.typesafe.ai/v1/systemone", "")
		if resCloud.Source != "typesafe_cloud" || len(resCloud.Models) == 0 {
			t.Fatalf("expected typesafe_cloud discovery, got %+v", resCloud)
		}

		detected := discovery.ProbeLocalInferenceServers(context.Background(),
			discovery.LocalProbeTarget{Label: "Flide", Provider: "jev", BaseURL: savedSrv.URL},
			discovery.LocalProbeTarget{Label: "OpenAI", Provider: "local", BaseURL: savedSrv.URL},
		)
		if len(detected) != 2 {
			t.Fatalf("expected 2 detected local servers, got %+v", detected)
		}
	})
}
