package localmodel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

type HardwareSpec struct {
	Platform     string  `json:"platform"`
	RAMGB        int     `json:"ram_gb"`
	SafeBudgetGB float64 `json:"safe_budget_gb"`
}

var (
	hwOnce   sync.Once
	cachedHW HardwareSpec
)

func DetectHardware() HardwareSpec {
	hwOnce.Do(func() {
		ramBytes := int64(16) * 1024 * 1024 * 1024 // fallback 16GB
		switch runtime.GOOS {
		case "darwin":
			if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
				if v, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil && v > 0 {
					ramBytes = v
				}
			}
		case "linux":
			if b, err := os.ReadFile("/proc/meminfo"); err == nil {
				for _, line := range strings.Split(string(b), "\n") {
					if strings.HasPrefix(line, "MemTotal:") {
						fields := strings.Fields(line)
						if len(fields) >= 2 {
							if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil && kb > 0 {
								ramBytes = kb * 1024
							}
						}
						break
					}
				}
			}
		}
		gb := int((ramBytes + 512*1024*1024) / (1024 * 1024 * 1024))
		if gb < 4 {
			gb = 4
		}
		cachedHW = HardwareSpec{
			Platform:     runtime.GOOS + "/" + runtime.GOARCH,
			RAMGB:        gb,
			SafeBudgetGB: float64(gb) * 0.32, // ~32% of RAM leaves comfortable headroom for IDE + OS
		}
	})
	return cachedHW
}

type Preset struct {
	ID          string `json:"id"`
	Tier        string `json:"tier"` // "speed", "balanced", "accuracy", "heavy"
	Name        string `json:"name"`
	Repo        string `json:"repo"`
	FileName    string `json:"file_name"`
	SizeBytes   int64  `json:"size_bytes"`
	SizeHint    string `json:"size_hint"`
	UpdatedAt   string `json:"updated_at"`
	Recommended bool   `json:"recommended"`
	FitStatus   string `json:"fit_status"` // "optimal", "tight", "heavy"
	FitBadge    string `json:"fit_badge"`
	Desc        string `json:"desc"`
	URL         string `json:"url"`
	Downloads   int64  `json:"downloads,omitempty"`
	Live        bool   `json:"live,omitempty"`
}

func decorateFit(p Preset, hw HardwareSpec) Preset {
	sizeGB := float64(p.SizeBytes) / (1024 * 1024 * 1024)
	if p.SizeHint == "" && p.SizeBytes > 0 {
		if sizeGB >= 1.0 {
			p.SizeHint = fmt.Sprintf("%.2f GB", sizeGB)
		} else {
			p.SizeHint = fmt.Sprintf("%.0f MB", float64(p.SizeBytes)/(1024*1024))
		}
	}
	if p.Tier == "" {
		switch {
		case sizeGB <= 1.7:
			p.Tier = "speed"
		case sizeGB <= 4.8:
			p.Tier = "balanced"
		case sizeGB <= 8.0:
			p.Tier = "accuracy"
		default:
			p.Tier = "heavy"
		}
	}
	switch {
	case sizeGB <= hw.SafeBudgetGB:
		p.FitStatus = "optimal"
		p.FitBadge = fmt.Sprintf("Optimal (%dGB RAM)", hw.RAMGB)
	case sizeGB <= float64(hw.RAMGB)*0.58:
		p.FitStatus = "tight"
		p.FitBadge = fmt.Sprintf("Fits (%dGB RAM)", hw.RAMGB)
	default:
		p.FitStatus = "heavy"
		p.FitBadge = fmt.Sprintf("Heavy (%dGB RAM)", hw.RAMGB)
	}
	return p
}

func Presets() []Preset {
	hw := DetectHardware()
	raw := []Preset{
		// Tier 1: speed (<= 2B, Ultra-low latency System 1)
		{
			ID:          "qwen3.5-2b-q4",
			Tier:        "speed",
			Name:        "Qwen 3.5 2B (Q4_K_M)",
			Repo:        "unsloth/Qwen3.5-2B-GGUF",
			FileName:    "Qwen3.5-2B-Q4_K_M.gguf",
			SizeBytes:   1280835840,
			UpdatedAt:   "2026-03",
			Recommended: true,
			Desc:        "Low-latency top pick for structured JSON classification and relevance scoring",
			URL:         "https://huggingface.co/unsloth/Qwen3.5-2B-GGUF/resolve/main/Qwen3.5-2B-Q4_K_M.gguf",
		},
		{
			ID:        "lfm3-1.7b-q4",
			Tier:      "speed",
			Name:      "LiquidAI LFM3 1.7B (Q4_K_M)",
			Repo:      "LiquidAI/LFM3-1.7B-GGUF",
			FileName:  "LFM3-1.7B-Q4_K_M.gguf",
			SizeBytes: 1099627808,
			UpdatedAt: "2026-10",
			Desc:      "Hybrid LFM3 architecture optimized for fast full-context batch evaluation",
			URL:       "https://huggingface.co/LiquidAI/LFM3-1.7B-GGUF/resolve/main/LFM3-1.7B-Q4_K_M.gguf",
		},
		{
			ID:        "qwen3.5-0.8b-q8",
			Tier:      "speed",
			Name:      "Qwen 3.5 0.8B (Q8_0)",
			Repo:      "unsloth/Qwen3.5-0.8B-GGUF",
			FileName:  "Qwen3.5-0.8B-Q8_0.gguf",
			SizeBytes: 811843840,
			UpdatedAt: "2026-03",
			Desc:      "Sub-1GB 8-bit model suited for 8GB RAM laptops and always-on background use",
			URL:       "https://huggingface.co/unsloth/Qwen3.5-0.8B-GGUF/resolve/main/Qwen3.5-0.8B-Q8_0.gguf",
		},

		// Tier 2: balanced (2B ~ 4B, Multilingual + Architecture Reasoning / Latency Balance)
		{
			ID:          "qwen3.5-4b-q4",
			Tier:        "balanced",
			Name:        "Qwen 3.5 4B (Q4_K_M)",
			Repo:        "unsloth/Qwen3.5-4B-GGUF",
			FileName:    "Qwen3.5-4B-Q4_K_M.gguf",
			SizeBytes:   2740937888,
			UpdatedAt:   "2026-03",
			Recommended: true,
			Desc:        "Balanced top pick for multilingual rule conflict detection and state mutation",
			URL:         "https://huggingface.co/unsloth/Qwen3.5-4B-GGUF/resolve/main/Qwen3.5-4B-Q4_K_M.gguf",
		},
		{
			ID:        "gemma-4-e2b-q4",
			Tier:      "balanced",
			Name:      "Google Gemma 4 E2B Instruct (Q4_0)",
			Repo:      "ggml-org/gemma-4-E2B-it-GGUF",
			FileName:  "gemma-4-E2B-it-Q4_0.gguf",
			SizeBytes: 2841481184,
			UpdatedAt: "2026-10",
			Desc:      "Official Gemma 4 E2B GGUF with reliable instruction following and JSON output",
			URL:       "https://huggingface.co/ggml-org/gemma-4-E2B-it-GGUF/resolve/main/gemma-4-E2B-it-Q4_0.gguf",
		},
		{
			ID:        "gemma-4-e4b-q4",
			Tier:      "balanced",
			Name:      "Google Gemma 4 E4B Instruct (Q4_0)",
			Repo:      "ggml-org/gemma-4-E4B-it-GGUF",
			FileName:  "gemma-4-E4B-it-Q4_0.gguf",
			SizeBytes: 4590807392,
			UpdatedAt: "2026-10",
			Desc:      "Official Gemma 4 E4B GGUF with stronger domain taxonomy classification",
			URL:       "https://huggingface.co/ggml-org/gemma-4-E4B-it-GGUF/resolve/main/gemma-4-E4B-it-Q4_0.gguf",
		},

		// Tier 3: accuracy (8B ~ 12B, High-Precision Plan Verification for 24GB+ RAM)
		{
			ID:          "qwen3.5-9b-q4",
			Tier:        "accuracy",
			Name:        "Qwen 3.5 9B (Q4_K_M)",
			Repo:        "unsloth/Qwen3.5-9B-GGUF",
			FileName:    "Qwen3.5-9B-Q4_K_M.gguf",
			SizeBytes:   5680522464,
			UpdatedAt:   "2026-03",
			Recommended: true,
			Desc:        "High-accuracy top pick for multi-rule supersession and plan guardrail checks",
			URL:         "https://huggingface.co/unsloth/Qwen3.5-9B-GGUF/resolve/main/Qwen3.5-9B-Q4_K_M.gguf",
		},
		{
			ID:        "gemma-4-12b-q4",
			Tier:      "accuracy",
			Name:      "Google Gemma 4 12B Instruct (Q4_0)",
			Repo:      "ggml-org/gemma-4-12B-it-GGUF",
			FileName:  "gemma-4-12B-it-Q4_0.gguf",
			SizeBytes: 7219673216,
			UpdatedAt: "2026-11",
			Desc:      "Official Gemma 4 12B GGUF for deep constraint verification on 24GB+ RAM hosts",
			URL:       "https://huggingface.co/ggml-org/gemma-4-12B-it-GGUF/resolve/main/gemma-4-12B-it-Q4_0.gguf",
		},
	}
	out := make([]Preset, len(raw))
	for i, p := range raw {
		out[i] = decorateFit(p, hw)
	}
	return out
}

// SelectBootstrapPreset chooses the best zero-config speed-tier GGUF preset that fits
// comfortably within the host machine's SafeBudgetGB.
func SelectBootstrapPreset() Preset {
	presets := Presets()
	for _, p := range presets {
		if p.Tier == "speed" && p.FitStatus == "optimal" && p.Recommended {
			return p
		}
	}
	for _, p := range presets {
		if p.Tier == "speed" && p.FitStatus == "optimal" {
			return p
		}
	}
	for _, p := range presets {
		if p.FitStatus == "optimal" {
			return p
		}
	}
	return presets[0]
}

type hfSibling struct {
	RFilename string `json:"rfilename"`
	Size      int64  `json:"size"`
}

func quantRank(fname string) int {
	up := strings.ToUpper(fname)
	if !strings.HasSuffix(up, ".GGUF") {
		return -1
	}
	for _, skip := range []string{"MMPROJ", "MTP-", "DFLASH", "DSPARK", "DRAFT", "IMATRIX", "-00001-OF-", "-00002-OF-"} {
		if strings.Contains(up, skip) {
			return -1
		}
	}
	if strings.Contains(up, "/") {
		return -1 // skip nested shard directories
	}
	switch {
	case strings.Contains(up, "Q4_K_M"):
		return 100
	case strings.Contains(up, "Q4_K_S"), strings.Contains(up, "Q4_0"):
		return 92
	case strings.Contains(up, "Q5_K_M"):
		return 86
	case strings.Contains(up, "Q8_0"):
		return 80
	case strings.Contains(up, "Q6_K"):
		return 75
	default:
		return 40
	}
}

func pickBestGGUF(siblings []hfSibling) (string, int64) {
	bestRank := -1
	var bestFile string
	var bestSize int64
	for _, s := range siblings {
		r := quantRank(s.RFilename)
		if r > bestRank {
			bestRank = r
			bestFile = s.RFilename
			bestSize = s.Size
		}
	}
	return bestFile, bestSize
}

func isNonChatRepo(repoID string) bool {
	low := strings.ToLower(repoID)
	for _, skip := range []string{
		"flux", "image", "embed", "whisper", "tts", "audio", "kokoro",
		"rerank", "bge-", "nomic", "ocr", "canary", "parakeet", "imatrix",
		"dflash", "dspark", "guard", "abliterated", "uncensored", "heretic",
		"test-model", "models-moved", "spec-for-testing",
	} {
		if strings.Contains(low, skip) {
			return true
		}
	}
	return false
}

var oversizedParamRe = regexp.MustCompile(`(?i)[-_](2[4-9]|[3-9]\d|\d{3,})b(?:[-_.]|$)`)

func isOversizedRepo(repoID string) bool {
	return oversizedParamRe.MatchString(repoID)
}

func isTrustedGGUFAuthor(repoID string) bool {
	low := strings.ToLower(repoID)
	for _, org := range []string{"ggml-org/", "unsloth/", "qwen/", "liquidai/", "bartowski/", "lmstudio-community/"} {
		if strings.HasPrefix(low, org) {
			return true
		}
	}
	return false
}

type hfCacheEntry struct {
	at    time.Time
	items []Preset
}

var (
	hfCacheMu sync.Mutex
	hfCache   = map[string]hfCacheEntry{}
)

// DiscoverHuggingFaceModels queries Hugging Face Hub API live for the newest / trending GGUF models
// across speed, balanced, and accuracy tiers, automatically selecting optimal single-file quantizations.
func DiscoverHuggingFaceModels(ctx context.Context, sortMode, searchQuery string, forceRefresh ...bool) ([]Preset, error) {
	base := strings.TrimRight(os.Getenv("HF_API_BASE"), "/")
	if base == "" {
		base = "https://huggingface.co"
	}
	if sortMode == "" {
		sortMode = "latest"
	}
	q := strings.TrimSpace(searchQuery)
	cacheKey := base + "|" + sortMode + "|" + strings.ToLower(q)
	refresh := len(forceRefresh) > 0 && forceRefresh[0]

	if !refresh {
		hfCacheMu.Lock()
		if ent, ok := hfCache[cacheKey]; ok && time.Since(ent.at) < 3*time.Minute && len(ent.items) > 0 {
			out := make([]Preset, len(ent.items))
			copy(out, ent.items)
			hfCacheMu.Unlock()
			return out, nil
		}
		hfCacheMu.Unlock()
	}

	cctx, cancel := context.WithTimeout(ctx, 9*time.Second)
	defer cancel()

	sortField := "trendingScore"
	if sortMode == "latest" {
		sortField = "lastModified"
	}

	type repoSummary struct {
		ID           string `json:"id"`
		Downloads    int64  `json:"downloads"`
		LastModified string `json:"lastModified"`
	}

	var candidates []repoSummary
	if q != "" {
		apiURL := fmt.Sprintf("%s/api/models?filter=gguf&pipeline_tag=text-generation&search=%s&sort=%s&direction=-1&limit=16",
			base, url.QueryEscape(q), sortField)
		req, _ := http.NewRequestWithContext(cctx, http.MethodGet, apiURL, nil)
		req.Header.Set("User-Agent", "memorize-recommender")
		resp, err := apiClient.Do(req)
		if err != nil {
			return nil, err
		}
		_ = json.NewDecoder(resp.Body).Decode(&candidates)
		_ = resp.Body.Close()
	} else {
		// Fetch live feeds concurrently across official & trusted GGUF maintainers
		urls := []string{
			fmt.Sprintf("%s/api/models?author=ggml-org&filter=gguf&sort=%s&direction=-1&limit=12", base, sortField),
			fmt.Sprintf("%s/api/models?author=unsloth&filter=gguf&sort=%s&direction=-1&limit=12", base, sortField),
			fmt.Sprintf("%s/api/models?author=unsloth&filter=gguf&search=4B&sort=%s&direction=-1&limit=6", base, sortField),
			fmt.Sprintf("%s/api/models?author=ggml-org&filter=gguf&search=gemma-4&sort=%s&direction=-1&limit=6", base, sortField),
			fmt.Sprintf("%s/api/models?author=LiquidAI&filter=gguf&sort=%s&direction=-1&limit=8", base, sortField),
			fmt.Sprintf("%s/api/models?author=bartowski&filter=gguf&sort=%s&direction=-1&limit=10", base, sortField),
			fmt.Sprintf("%s/api/models?author=lmstudio-community&filter=gguf&sort=%s&direction=-1&limit=8", base, sortField),
		}
		buckets := make([][]repoSummary, len(urls))
		fg, fctx := errgroup.WithContext(cctx)
		for idx, u := range urls {
			i, feedURL := idx, u
			fg.Go(func() error {
				req, err := http.NewRequestWithContext(fctx, http.MethodGet, feedURL, nil)
				if err != nil {
					return nil
				}
				req.Header.Set("User-Agent", "memorize-recommender")
				resp, err := apiClient.Do(req)
				if err != nil {
					return nil
				}
				defer resp.Body.Close()
				if resp.StatusCode < 300 {
					_ = json.NewDecoder(resp.Body).Decode(&buckets[i])
				}
				return nil
			})
		}
		_ = fg.Wait()

		// Round-robin interleave across official maintainers so speed, balanced, and accuracy tiers are well represented
		seen := make(map[string]bool)
		for pos := 0; pos < 14; pos++ {
			for b := range buckets {
				if pos >= len(buckets[b]) {
					continue
				}
				it := buckets[b][pos]
				if it.ID == "" || seen[it.ID] || isNonChatRepo(it.ID) || isOversizedRepo(it.ID) {
					continue
				}
				seen[it.ID] = true
				candidates = append(candidates, it)
			}
		}
	}

	var filtered []repoSummary
	for _, c := range candidates {
		if !isNonChatRepo(c.ID) && (q != "" || !isOversizedRepo(c.ID)) {
			filtered = append(filtered, c)
		}
		if len(filtered) >= 22 {
			break
		}
	}

	hw := DetectHardware()
	var mu sync.Mutex
	var results []Preset

	g, gctx := errgroup.WithContext(cctx)
	g.SetLimit(6)
	for _, item := range filtered {
		repo := item
		g.Go(func() error {
			detailURL := fmt.Sprintf("%s/api/models/%s?blobs=true", base, repo.ID)
			req, err := http.NewRequestWithContext(gctx, http.MethodGet, detailURL, nil)
			if err != nil {
				return nil
			}
			req.Header.Set("User-Agent", "memorize-recommender")
			resp, err := apiClient.Do(req)
			if err != nil {
				return nil
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 300 {
				return nil
			}
			var detail struct {
				ID           string      `json:"id"`
				Downloads    int64       `json:"downloads"`
				LastModified string      `json:"lastModified"`
				Siblings     []hfSibling `json:"siblings"`
			}
			if json.NewDecoder(resp.Body).Decode(&detail) != nil {
				return nil
			}
			fname, size := pickBestGGUF(detail.Siblings)
			if fname == "" || !safeFileRe.MatchString(fname) {
				return nil
			}
			modDate := detail.LastModified
			if len(modDate) >= 10 {
				modDate = modDate[:10]
			}
			p := decorateFit(Preset{
				ID:        "hf:" + detail.ID,
				Name:      detail.ID,
				Repo:      detail.ID,
				FileName:  fname,
				SizeBytes: size,
				UpdatedAt: modDate,
				Downloads: detail.Downloads,
				Live:      true,
				Desc:      fmt.Sprintf("HuggingFace live · %s · %d downloads", fname, detail.Downloads),
				URL:       fmt.Sprintf("%s/%s/resolve/main/%s", base, detail.ID, fname),
			}, hw)
			mu.Lock()
			results = append(results, p)
			mu.Unlock()
			return nil
		})
	}
	_ = g.Wait()

	sort.Slice(results, func(i, j int) bool {
		fitI := results[i].FitStatus == "optimal"
		fitJ := results[j].FitStatus == "optimal"
		if fitI != fitJ {
			return fitI
		}
		trI := isTrustedGGUFAuthor(results[i].Repo)
		trJ := isTrustedGGUFAuthor(results[j].Repo)
		if sortMode == "latest" {
			if results[i].UpdatedAt != results[j].UpdatedAt {
				return results[i].UpdatedAt > results[j].UpdatedAt
			}
			if trI != trJ {
				return trI
			}
		} else {
			if results[i].Downloads != results[j].Downloads {
				return results[i].Downloads > results[j].Downloads
			}
		}
		return results[i].Downloads > results[j].Downloads
	})

	// Mark the top (#1) optimal live model in each tier as Recommended
	topTier := map[string]bool{}
	for i := range results {
		t := results[i].Tier
		if !topTier[t] && results[i].FitStatus == "optimal" {
			results[i].Recommended = true
			topTier[t] = true
		}
	}

	if len(results) > 0 {
		hfCacheMu.Lock()
		copied := make([]Preset, len(results))
		copy(copied, results)
		hfCache[cacheKey] = hfCacheEntry{at: time.Now(), items: copied}
		hfCacheMu.Unlock()
	}
	return results, nil
}
