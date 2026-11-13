package adapters

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"strings"
	"sync"

	"memorize/internal/memory"
)

func localmodelSupported() bool {
	return runtime.GOOS == "darwin" || runtime.GOOS == "linux"
}

var jsonSyntaxOrSchemaKeys = map[string]bool{
	"items": true, "id": true, "score": true, "confidence": true, "category": true, "sub_category": true, "tags": true,
	"is_permanent": true, "is_global_invariant": true, "action": true,
	"conflict_fact_ids": true, "a_verdict": true, "violations": true, "probability": true,
	"groups": true, "source_ids": true, "merged_content": true, "reason": true, "sha": true, "summary": true,
	"new_category": true, "new_sub_category": true,
}

func clampProb(v float64) float64 {
	return math.Min(1.0, math.Max(0.0, v))
}

func isSemanticDecisionToken(tok string) bool {
	clean := strings.Trim(tok, " \t\r\n\"{}[]:,")
	return clean != "" && !jsonSyntaxOrSchemaKeys[clean]
}

type tokenLogprob struct {
	Token   string
	Logprob float64
}

// semanticConfidence computes debiased probability [0, 1] from semantic decision tokens,
// filtering out zero-entropy JSON punctuation and schema keys.
func semanticConfidence(tokens []tokenLogprob, avgFallback float64) float64 {
	var sumLP float64
	var cntLP int
	for _, tk := range tokens {
		if isSemanticDecisionToken(tk.Token) {
			sumLP += tk.Logprob
			cntLP++
		}
	}
	if cntLP > 0 {
		return math.Min(1.0, math.Max(0.0, math.Exp(sumLP/float64(cntLP))))
	}
	if avgFallback < 0 {
		// Debias JSON structural token inflation (~60-70% zero-entropy punctuation/keys)
		return math.Min(1.0, math.Max(0.0, math.Exp(avgFallback*2.5)))
	}
	return 0.85
}

type schemaDialect struct {
	upperTypes         bool
	strictNoAdditional bool
}

func (d schemaDialect) t(kind string) string {
	if d.upperTypes {
		return strings.ToUpper(kind)
	}
	return kind
}

func (d schemaDialect) obj(props map[string]any, req []string) map[string]any {
	m := map[string]any{
		"type":       d.t("object"),
		"properties": props,
		"required":   req,
	}
	if d.strictNoAdditional {
		m["additionalProperties"] = false
	}
	return m
}

// jsonSchemaTransport abstracts structured JSON generation across cloud (Gemini) and local (GGUF/OpenAI) engines.
type jsonSchemaTransport interface {
	Name() string
	Dialect() schemaDialect
	CallJSON(ctx context.Context, prompt string, schema map[string]any, target any) (float64, error)
}

type schemaEvaluator struct {
	tr jsonSchemaTransport
}

func newSchemaEvaluator(tr jsonSchemaTransport) memory.Evaluator {
	return &schemaEvaluator{tr: tr}
}

func (e *schemaEvaluator) Name() string { return e.tr.Name() }

func (e *schemaEvaluator) ScoreRelevance(ctx context.Context, currentTask string, facts []memory.Fact) ([]memory.ScoredFact, error) {
	if len(facts) == 0 {
		return nil, nil
	}
	// Prefix-stable ordering: static instructions + sorted Facts first, dynamic Current Task last
	// so local llama-server (KV cache) and cloud Context Caching reuse the entire Facts prefix.
	var sb strings.Builder
	sb.WriteString(`Evaluate each fact individually against the Current Task and return per-item "score" and "confidence":
- score (0.0 to 5.0): 4.5-5.0 critical constraint directly governing the task; 3.5-4.4 directly applicable rule/context; 2.0-3.4 indirect background; 0.0-1.9 unrelated.
- confidence (0.00 to 1.00, 5-band certainty scale per item):
  * 0.95-1.00: unambiguous, explicit non-negotiable invariant or exact task match/non-match.
  * 0.90-0.94: clear, concrete architectural/domain applicability.
  * 0.85-0.89: standard convention or clear contextual match.
  * 0.80-0.84: soft preference or situational match.
  * <0.80: vague task phrasing or uncertain/speculative match.
Facts:
`)
	factMap := make(map[int64]memory.Fact, len(facts))
	for _, f := range facts {
		factMap[f.ID] = f
		fmt.Fprintf(&sb, "[#%d] (%s) %s\n", f.ID, f.Category, f.Content)
	}
	fmt.Fprintf(&sb, "\nCurrent Task: %q\n", currentTask)

	d := e.tr.Dialect()
	itemSchema := d.obj(map[string]any{
		"id":         map[string]any{"type": d.t("integer")},
		"score":      map[string]any{"type": d.t("number")},
		"confidence": map[string]any{"type": d.t("number")},
	}, []string{"id", "score", "confidence"})
	schema := d.obj(map[string]any{
		"items": map[string]any{"type": d.t("array"), "items": itemSchema},
	}, []string{"items"})

	var parsed struct {
		Items []struct {
			ID         int64   `json:"id"`
			Score      float64 `json:"score"`
			Confidence float64 `json:"confidence"`
		} `json:"items"`
	}
	fallbackConf, err := e.tr.CallJSON(ctx, sb.String(), schema, &parsed)
	if err != nil {
		return nil, err
	}
	res := make([]memory.ScoredFact, 0, len(parsed.Items))
	for _, it := range parsed.Items {
		if f, ok := factMap[it.ID]; ok {
			c := clampProb(it.Confidence)
			if it.Confidence <= 0 {
				c = fallbackConf
			}
			res = append(res, memory.ScoredFact{Fact: f, Score: it.Score, Confidence: c})
		}
	}
	return res, nil
}

func (e *schemaEvaluator) EvaluateMutation(ctx context.Context, scope, candidate string, existing []memory.Fact) (memory.MutationDecision, error) {
	presets := memory.PresetCategoriesFromContext(ctx)
	var sb strings.Builder
	fmt.Fprintf(&sb, `Scope: %s
Preset Taxonomy (category -> sub_categories): %s
Evaluate Candidate Fact against Existing Facts using semantic reasoning:
- category: concise lowercase domain slug. Reuse an existing Preset Taxonomy category when applicable; if none fits, introduce a concise new category slug.
- sub_category: concise lowercase sub-category slug. Reuse an existing sub-category under that category when applicable, or introduce a concise new sub-category slug when a clear sub-topic exists ("" only if genuinely broad).
- tags: 1 to 3 concise lowercase sub-topic keywords.
- tier:
  * 'invariant': strict non-negotiable prohibition/safety constraint that must never be violated (is_global_invariant >= 0.88).
  * 'rule': standard architectural decision, convention, or current factual state (is_permanent >= 0.80).
  * 'preference': soft preference, favored option, or personal taste — non-binding hint (category='preference', is_global_invariant <= 0.20).
  * 'transient': temporary debug log, scratch note, or ephemeral chatter (is_permanent <= 0.30).
- cardinality:
  * 'exclusive': only ONE state/value can hold at a time for this subject, OR Candidate explicitly replaces, negates, or switches away from an Existing fact.
  * 'additive': MULTIPLE items/values can coexist simultaneously for this subject (e.g. preferring multiple programming languages, tools, foods, or adding complementary rules).
- action:
  * 'IGNORE' with matching conflict_fact_ids ONLY if an Existing fact already states the exact same meaning as Candidate.
  * 'OVERWRITE' with conflict_fact_ids ONLY when cardinality is 'exclusive' and Candidate contradicts or supersedes those Existing fact(s).
  * 'INSERT' with [] when Candidate is new OR when cardinality is 'additive' (coexisting items).
- confidence (0.00 to 1.00): individual certainty and explicitness of this Candidate and action:
  * 0.95-1.00: explicit non-negotiable invariant, hard prohibition, or strict security/safety mandate.
  * 0.90-0.94: unambiguous architectural, schema, API, or workflow decision, or clear exclusive replacement.
  * 0.85-0.89: standard coding/UI convention or agreed implementation guideline.
  * 0.80-0.84: soft preference, stylistic taste, or narrow situational preference.
  * <0.80: vague, speculative, incomplete, or uncertain statement.
Existing:
`, scope, presets)

	validIDs := make(map[int64]bool, len(existing))
	for _, f := range existing {
		validIDs[f.ID] = true
		fmt.Fprintf(&sb, "[#%d] (%s) %s\n", f.ID, f.Category, f.Content)
	}
	fmt.Fprintf(&sb, "\nCandidate: %q\n", candidate)
	d := e.tr.Dialect()
	schema := d.obj(map[string]any{
		"category":            map[string]any{"type": d.t("string")},
		"sub_category":        map[string]any{"type": d.t("string")},
		"tags":                map[string]any{"type": d.t("array"), "items": map[string]any{"type": d.t("string")}},
		"tier":                map[string]any{"type": d.t("string"), "enum": []string{"invariant", "rule", "preference", "transient"}},
		"cardinality":         map[string]any{"type": d.t("string"), "enum": []string{"exclusive", "additive"}},
		"is_permanent":        map[string]any{"type": d.t("number")},
		"is_global_invariant": map[string]any{"type": d.t("number")},
		"action":              map[string]any{"type": d.t("string"), "enum": []string{"INSERT", "OVERWRITE", "IGNORE"}},
		"conflict_fact_ids":   map[string]any{"type": d.t("array"), "items": map[string]any{"type": d.t("integer")}},
		"confidence":          map[string]any{"type": d.t("number")},
	}, []string{"category", "sub_category", "tags", "tier", "cardinality", "is_permanent", "is_global_invariant", "action", "conflict_fact_ids", "confidence"})

	var dec memory.MutationDecision
	conf, err := e.tr.CallJSON(ctx, sb.String(), schema, &dec)
	if err != nil {
		return memory.MutationDecision{}, err
	}
	if dec.Confidence > 0 {
		dec.Confidence = clampProb(dec.Confidence)
	} else {
		dec.Confidence = conf
	}
	memory.ApplyDecisionSemantics(&dec)

	var cleanIDs []int64
	seen := make(map[int64]bool)
	for _, id := range dec.ConflictFactIDs {
		if validIDs[id] && !seen[id] {
			seen[id] = true
			cleanIDs = append(cleanIDs, id)
		}
	}
	dec.ConflictFactIDs = cleanIDs
	if len(cleanIDs) > 0 {
		dec.ConflictFactID = cleanIDs[0]
	} else if dec.Action == "OVERWRITE" || (dec.Action == "IGNORE" && dec.IsPermanent >= 0.75) {
		dec.Action = "INSERT"
	}
	return dec, nil
}

func (e *schemaEvaluator) VerifyPlan(ctx context.Context, proposedPlan string, rules []memory.Fact) ([]memory.PlanViolation, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	var sb strings.Builder
	sb.WriteString(`Check whether the Proposed Plan directly and explicitly violates any of the Rules below.
- First set "a_verdict" to one concise sentence stating "COMPLIES: ..." or "VIOLATES #<id>: ...".
- Return an EMPTY array ("violations": []) whenever the Proposed Plan complies with the rules, implements a valid part/slice of a rule (e.g. backend code while UI lives in another file, or vice versa), is unrelated to the rules, or is a normal read/test/query/build command.
- NEVER flag a rule as a violation merely because the Proposed Plan touches the same file or topic while following or implementing part of the rule.
- Only include a rule in "violations" if the Proposed Plan actively introduces behavior or values that directly contradict what that rule requires or forbids: set "probability" (0.80-1.00 violation likelihood) and "confidence" (0.00-1.00 5-band certainty of that violation).
Rules:
`)
	ruleMap := make(map[int64]memory.Fact, len(rules))
	for _, r := range rules {
		ruleMap[r.ID] = r
		fmt.Fprintf(&sb, "[#%d] (%s) %s\n", r.ID, r.Category, r.Content)
	}
	fmt.Fprintf(&sb, "\nProposed Plan:\n%s\n", proposedPlan)
	d := e.tr.Dialect()
	itemSchema := d.obj(map[string]any{
		"id":          map[string]any{"type": d.t("integer")},
		"probability": map[string]any{"type": d.t("number")},
		"confidence":  map[string]any{"type": d.t("number")},
	}, []string{"id", "probability", "confidence"})
	schema := d.obj(map[string]any{
		"a_verdict":  map[string]any{"type": d.t("string")},
		"violations": map[string]any{"type": d.t("array"), "items": itemSchema},
	}, []string{"a_verdict", "violations"})

	var parsed struct {
		Verdict    string `json:"a_verdict"`
		Violations []struct {
			ID          int64   `json:"id"`
			Probability float64 `json:"probability"`
			Confidence  float64 `json:"confidence"`
		} `json:"violations"`
	}
	conf, err := e.tr.CallJSON(ctx, sb.String(), schema, &parsed)
	if err != nil {
		return nil, err
	}
	var out []memory.PlanViolation
	for _, v := range parsed.Violations {
		if r, ok := ruleMap[v.ID]; ok && v.Probability >= 0.5 {
			vConf := conf
			if v.Confidence > 0 {
				vConf = clampProb(v.Confidence)
			}
			out = append(out, memory.PlanViolation{Fact: r, Probability: v.Probability, Confidence: vConf})
		}
	}
	return out, nil
}

func (e *schemaEvaluator) CalibrateConfidence(ctx context.Context, scope string, facts []memory.Fact) (map[int64]float64, error) {
	if len(facts) == 0 {
		return map[int64]float64{}, nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, `Scope: %s
Assign an individual semantic confidence (0.00 to 1.00) to EACH stored memory below using this exact rubric (differentiate each item accurately according to its specificity and binding strength):
- 0.95-1.00: strict non-negotiable invariant, hard prohibition, security/safety boundary, or core system contract.
- 0.90-0.94: explicit, concrete architectural, database, API, or operational rule with unambiguous conditions.
- 0.85-0.89: standard coding, UI layout, or workflow convention.
- 0.80-0.84: soft preference, stylistic taste, or situational hint.
- <0.80: vague, overly broad, or underspecified note.

Memories:
`, scope)
	valid := make(map[int64]bool, len(facts))
	for _, f := range facts {
		valid[f.ID] = true
		pinTag := ""
		if f.Pinned {
			pinTag = ", pinned"
		}
		fmt.Fprintf(&sb, "[#%d] (%s%s) %s\n", f.ID, f.Category, pinTag, f.Content)
	}
	d := e.tr.Dialect()
	itemSchema := d.obj(map[string]any{
		"id":         map[string]any{"type": d.t("integer")},
		"confidence": map[string]any{"type": d.t("number")},
	}, []string{"id", "confidence"})
	schema := d.obj(map[string]any{
		"items": map[string]any{"type": d.t("array"), "items": itemSchema},
	}, []string{"items"})

	var parsed struct {
		Items []struct {
			ID         int64   `json:"id"`
			Confidence float64 `json:"confidence"`
		} `json:"items"`
	}
	if _, err := e.tr.CallJSON(ctx, sb.String(), schema, &parsed); err != nil {
		return nil, err
	}
	out := make(map[int64]float64, len(parsed.Items))
	for _, it := range parsed.Items {
		if valid[it.ID] && it.Confidence > 0 {
			out[it.ID] = clampProb(it.Confidence)
		}
	}
	return out, nil
}

func (e *schemaEvaluator) ConsolidateFacts(ctx context.Context, scope, category string, facts []memory.Fact) (string, error) {
	if len(facts) == 0 {
		return "", nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Scope: %s\nCategory: %s\nMerge the following related/overlapping rules into ONE ultra-concise single-sentence rule (target 90-145 characters, strictly <= 165 characters max) preserving core constraints while removing verbose phrasing.\nCRITICAL: Write merged_content in the exact same language as the input facts (e.g., if the input facts are in Korean, output Korean; never translate into English):\n", scope, category)
	for _, f := range facts {
		fmt.Fprintf(&sb, "- [#%d] %s\n", f.ID, f.Content)
	}
	d := e.tr.Dialect()
	schema := d.obj(map[string]any{
		"merged_content": map[string]any{"type": d.t("string")},
	}, []string{"merged_content"})

	var parsed struct {
		MergedContent string `json:"merged_content"`
	}
	if _, err := e.tr.CallJSON(ctx, sb.String(), schema, &parsed); err != nil {
		return "", err
	}
	out := strings.TrimSpace(parsed.MergedContent)
	if len([]rune(out)) > memory.MaxConsolidatedRunes {
		shortPrompt := fmt.Sprintf("Shorten this rule to strictly <= 155 characters in the exact same language while preserving its core constraints:\n%s", out)
		var retry struct {
			MergedContent string `json:"merged_content"`
		}
		if _, rerr := e.tr.CallJSON(ctx, shortPrompt, schema, &retry); rerr == nil && strings.TrimSpace(retry.MergedContent) != "" {
			out = strings.TrimSpace(retry.MergedContent)
		}
	}
	return out, nil
}

func (e *schemaEvaluator) SuggestConsolidations(ctx context.Context, scope string, facts []memory.Fact) ([]memory.ConsolidationProposal, error) {
	if len(facts) < 2 {
		return nil, nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, `Scope: %s
Analyze the active memories below and recommend groups of 2 to 4 overlapping, duplicate, or closely related memories (covering the same feature, UI tab, workflow, or subsystem constraint) that can be consolidated into ONE tight rule.
Rules:
1. Group memories that address the same narrow feature/subsystem or partially repeat each other (e.g., multiple rules about the same UI modal, tab behavior, onboarding step, or category feature). Do NOT merge completely unrelated domains. If truly no pair is related, return {"groups": []}.
2. Keep [pinned] core invariants separate from [general] unpinned items unless they state the same invariant.
3. Each memory #ID may appear in AT MOST ONE group.
4. merged_content MUST be compact and dense: target 90-145 characters (strictly <= 165 characters max), written in the EXACT SAME LANGUAGE as the source memories (e.g., Korean input -> Korean output; never translate to English).
5. reason MUST be a short phrase (<= 40 characters, same language) naming the shared topic being consolidated.

Active Memories:
`, scope)
	for _, f := range facts {
		tier := "general"
		if f.Pinned {
			tier = "pinned"
		}
		cat := f.Category
		if f.SubCategory != "" {
			cat += "/" + f.SubCategory
		}
		fmt.Fprintf(&sb, "- [#%d] (%s | %s) %s\n", f.ID, cat, tier, f.Content)
	}

	d := e.tr.Dialect()
	groupSchema := d.obj(map[string]any{
		"source_ids":     map[string]any{"type": d.t("array"), "items": map[string]any{"type": d.t("integer")}},
		"category":       map[string]any{"type": d.t("string")},
		"merged_content": map[string]any{"type": d.t("string")},
		"reason":         map[string]any{"type": d.t("string")},
	}, []string{"source_ids", "category", "merged_content", "reason"})
	schema := d.obj(map[string]any{
		"groups": map[string]any{"type": d.t("array"), "items": groupSchema},
	}, []string{"groups"})

	var parsed struct {
		Groups []struct {
			SourceIDs     []int64 `json:"source_ids"`
			Category      string  `json:"category"`
			MergedContent string  `json:"merged_content"`
			Reason        string  `json:"reason"`
		} `json:"groups"`
	}
	if _, err := e.tr.CallJSON(ctx, sb.String(), schema, &parsed); err != nil {
		return nil, err
	}
	out := make([]memory.ConsolidationProposal, 0, len(parsed.Groups))
	for _, g := range parsed.Groups {
		out = append(out, memory.ConsolidationProposal{
			SourceIDs:     g.SourceIDs,
			Category:      strings.TrimSpace(g.Category),
			MergedContent: strings.TrimSpace(g.MergedContent),
			Reason:        strings.TrimSpace(g.Reason),
		})
	}
	return out, nil
}

// SummarizeCommitProgress categorizes incremental commits strictly for the separated project_progress table
// (human work-progress sync; never stored in memories or lifecycle hooks).
func (e *schemaEvaluator) SummarizeCommitProgress(ctx context.Context, scope, lang string, commits []memory.GitCommitBrief) ([]memory.CommitProgressItem, error) {
	if len(commits) == 0 {
		return nil, nil
	}
	presets := memory.PresetCategoriesFromContext(ctx)
	langRule := "Keep the original language (or concise English if mixed)."
	if strings.EqualFold(strings.TrimSpace(lang), "ko") {
		langRule = "Write summary in concise Korean."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, `Scope: %s
Preset Taxonomy (category -> sub_categories): %s
Map each incremental change into a human-friendly work stream "track" (e.g. a concise feature/module topic in the requested language so related commits share the same track name), a category/sub_category, and a clear human-readable progress summary.
Language: %s
Changes:
`, scope, presets, langRule)
	for _, c := range commits {
		files := strings.Join(c.Files, ", ")
		fmt.Fprintf(&sb, "- [%s] (%s) %s | files: %s\n", c.SHA, c.Date, c.Subject, files)
	}

	d := e.tr.Dialect()
	itemSchema := d.obj(map[string]any{
		"sha":          map[string]any{"type": d.t("string")},
		"track":        map[string]any{"type": d.t("string")},
		"category":     map[string]any{"type": d.t("string")},
		"sub_category": map[string]any{"type": d.t("string")},
		"summary":      map[string]any{"type": d.t("string")},
	}, []string{"sha", "track", "category", "sub_category", "summary"})
	schema := d.obj(map[string]any{
		"items": map[string]any{"type": d.t("array"), "items": itemSchema},
	}, []string{"items"})

	var parsed struct {
		Items []memory.CommitProgressItem `json:"items"`
	}
	if _, err := e.tr.CallJSON(ctx, sb.String(), schema, &parsed); err != nil {
		return nil, err
	}
	return parsed.Items, nil
}

func providerDefaultModel(prov string, cfg map[string]string) string {
	switch memory.NormalizeProviderID(prov) {
	case "gguf":
		return strings.TrimSpace(cfg["gguf_model"])
	case "gemini":
		return strings.TrimSpace(cfg["gemini_model"])
	case "jev":
		return strings.TrimSpace(cfg["jev_model"])
	case "local":
		return strings.TrimSpace(cfg["local_model_name"])
	default:
		return ""
	}
}

func safeGenerativeModel(prov, rawMod string, cfg map[string]string) string {
	mod := strings.TrimSpace(rawMod)
	if mod == "" {
		mod = providerDefaultModel(prov, cfg)
	}
	if memory.NormalizeProviderID(prov) == "gemini" && memory.IsDecisionOnlySpec("gemini", mod) {
		return "gemini-3.5-flash-lite"
	}
	return mod
}

// NewTaskResolver builds a cached task-aware Evaluator resolver backed by SQLite config.
// It supports both bulk engine assignment and per-feature model routing while ensuring
// non-generative decision engines (gemini-decision-1, jev, flide) are never asked to generate text.
func NewTaskResolver(autoWake ...func(ctx context.Context, model string) error) func(map[string]string, memory.EvalTask) (memory.Evaluator, bool) {
	var wakeFn func(context.Context, string) (string, error)
	if len(autoWake) > 0 && autoWake[0] != nil {
		raw := autoWake[0]
		wakeFn = func(ctx context.Context, model string) (string, error) {
			return "", raw(ctx, model)
		}
	}
	return NewTaskResolverWithEndpoint(wakeFn)
}

// NewTaskResolverWithEndpoint behaves like NewTaskResolver while propagating the runtime
// GGUF endpoint returned by the auto-wake callback across port changes or fallbacks.
func NewTaskResolverWithEndpoint(wakeFn func(ctx context.Context, model string) (string, error)) func(map[string]string, memory.EvalTask) (memory.Evaluator, bool) {
	var mu sync.Mutex
	cache := make(map[string]memory.Evaluator)

	getOrBuild := func(prov, modelOverride string, cfg map[string]string) memory.Evaluator {
		prov = memory.NormalizeProviderID(prov)
		if prov == "" {
			return nil
		}
		ep := strings.TrimSpace(cfg["gguf_endpoint"])
		mod := strings.TrimSpace(modelOverride)
		if mod == "" {
			mod = providerDefaultModel(prov, cfg)
		}
		gemKey := strings.TrimSpace(cfg["gemini_api_key"])
		jevURL := strings.TrimSpace(cfg["jev_api_url"])
		tsKey := strings.TrimSpace(cfg["typesafe_api_key"])
		locURL := strings.TrimSpace(cfg["local_model_url"])
		locKey := strings.TrimSpace(cfg["local_api_key"])

		var key string
		switch prov {
		case "gguf":
			if !localmodelSupported() {
				key = strings.Join([]string{"local", locURL, mod, locKey}, "|")
			} else {
				key = strings.Join([]string{"gguf", ep, mod}, "|")
			}
		case "gemini":
			key = strings.Join([]string{"gemini", gemKey, mod}, "|")
		case "local":
			if locKey == "" {
				locKey = tsKey
			}
			key = strings.Join([]string{"local", locURL, mod, locKey}, "|")
		case "jev":
			key = strings.Join([]string{strings.ToLower(strings.TrimSpace(cfg["active_provider"])), "jev", jevURL, mod, tsKey}, "|")
		default:
			return nil
		}

		mu.Lock()
		defer mu.Unlock()
		if ev, ok := cache[key]; ok && ev != nil {
			return ev
		}
		var ev memory.Evaluator
		switch prov {
		case "gguf":
			if !localmodelSupported() {
				ev = NewOpenAICompatibleAdapter(locURL, mod, locKey)
				break
			}
			if wakeFn != nil {
				targetMod := mod
				ev = NewGGUFAdapterWithEndpoint(ep, mod, func(ctx context.Context) (string, error) {
					return wakeFn(ctx, targetMod)
				})
			} else {
				ev = NewGGUFAdapter(ep, mod)
			}
		case "gemini":
			ev = NewGeminiAdapterConfig(gemKey, mod)
		case "local":
			ev = NewOpenAICompatibleAdapter(locURL, mod, locKey)
		case "jev":
			rawAct := strings.ToLower(strings.TrimSpace(cfg["active_provider"]))
			if rawAct == "flide" && jevURL == "" {
				ev = NewFlideAdapter("", tsKey, mod)
			} else {
				ev = NewJevAdapterConfig(jevURL, tsKey, mod)
			}
		default:
			return nil
		}
		if len(cache) >= 64 {
			cache = make(map[string]memory.Evaluator)
		}
		cache[key] = ev
		return ev
	}

	return func(cfg map[string]string, task memory.EvalTask) (memory.Evaluator, bool) {
		bulkProv := memory.NormalizeProviderID(cfg["active_provider"])
		bulkMod := providerDefaultModel(bulkProv, cfg)

		spec, hasSpec := memory.FindEvalTask(task)
		if hasSpec {
			ovProv := memory.NormalizeProviderID(cfg[spec.ProviderKey])
			ovMod := strings.TrimSpace(cfg[spec.ModelKey])
			if ovProv == "" && ovMod != "" {
				ovProv = bulkProv
			}
			if ovProv != "" {
				if memory.ValidateTaskRoute(task, ovProv, ovMod) == nil {
					effMod := ovMod
					if effMod == "" && spec.RequiresGenerative {
						effMod = safeGenerativeModel(ovProv, "", cfg)
					}
					if ev := getOrBuild(ovProv, effMod, cfg); ev != nil {
						return ev, true
					}
				}
			}
		}

		if bulkProv == "" {
			return nil, false
		}
		if hasSpec && spec.RequiresGenerative && memory.IsDecisionOnlySpec(bulkProv, bulkMod) {
			genProv := memory.NormalizeProviderID(cfg["gen_provider"])
			genMod := strings.TrimSpace(cfg["gen_model"])
			if genProv != "" && !memory.IsDecisionOnlySpec(genProv, genMod) {
				effMod := safeGenerativeModel(genProv, genMod, cfg)
				if ev := getOrBuild(genProv, effMod, cfg); ev != nil {
					return ev, true
				}
			}
			return nil, true
		}

		ev := getOrBuild(bulkProv, bulkMod, cfg)
		return ev, ev != nil
	}
}

// NewDynamicResolver builds a cached hot-swappable Evaluator resolver backed by SQLite config.
func NewDynamicResolver(autoWake ...func(ctx context.Context, model string) error) func(map[string]string) memory.Evaluator {
	taskRes := NewTaskResolver(autoWake...)
	return func(cfg map[string]string) memory.Evaluator {
		ev, _ := taskRes(cfg, "")
		return ev
	}
}
