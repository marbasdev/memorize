package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"memorize/internal/memory"
)

var scoreRubric = []string{
	"0: Unrelated to the task.",
	"1: Tangentially related domain.",
	"2: Weakly related background context.",
	"3: Moderately relevant context.",
	"4: Directly relevant guideline for the task.",
	"5: Critical architectural/convention constraint governing the task.",
}

type decisionQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type decisionAnswer struct {
	Choice           string
	Score            float64
	Noul             float64
	Confidence       float64
	AnswerConfidence float64
}

func (a decisionAnswer) conf() float64 {
	if a.Confidence > 0 {
		return a.Confidence
	}
	if a.AnswerConfidence > 0 {
		return a.AnswerConfidence
	}
	// Winning Bernoulli class probability max(p, 1-p) in [0.5, 1.0]:
	// 0.50 at total uncertainty (noul=0.5), 0.95 at high certainty (noul=0.95 or 0.05).
	return 0.5 + abs(a.Noul-0.5)
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// decisionTransport abstracts closed-set probability decision endpoints such as
// OmniJev Flide (/v1/systemone) and Google Gemini Decisions API (/v1beta/decisions).
type decisionTransport interface {
	Name() string
	CallDecisions(ctx context.Context, input string, questions map[string]decisionQuestion) (map[string]decisionAnswer, error)
}

// decisionEvaluator implements memorization scoring, mutation gating, guardrail verification,
// and 5-band confidence calibration using a non-generative decisionTransport (0 output tokens).
type decisionEvaluator struct {
	tr decisionTransport
}

func (d *decisionEvaluator) Name() string { return d.tr.Name() }

func (d *decisionEvaluator) ScoreRelevance(ctx context.Context, currentTask string, facts []memory.Fact) ([]memory.ScoredFact, error) {
	if len(facts) == 0 {
		return nil, nil
	}
	// Place stable Active Facts at byte 0 and per-turn Current Task at the end so prefix/context
	// caching hits on repeated queries across turns.
	var sb strings.Builder
	sb.WriteString("Active Facts:\n")
	q := make(map[string]decisionQuestion, len(facts))
	for _, f := range facts {
		fmt.Fprintf(&sb, "[#%d] (%s) %s\n", f.ID, f.Category, f.Content)
		q[fmt.Sprintf("f_%d", f.ID)] = decisionQuestion{
			Type:         "score",
			Instructions: fmt.Sprintf("Rate relevance of Fact #%d to Current Task.", f.ID),
			Criteria:     scoreRubric,
		}
	}
	fmt.Fprintf(&sb, "\nCurrent Task: %s\n", currentTask)

	ans, err := d.tr.CallDecisions(ctx, sb.String(), q)
	if err != nil {
		return nil, err
	}
	res := make([]memory.ScoredFact, 0, len(facts))
	for _, f := range facts {
		a := ans[fmt.Sprintf("f_%d", f.ID)]
		res = append(res, memory.ScoredFact{Fact: f, Score: a.Score, Confidence: a.conf()})
	}
	return res, nil
}

func buildCategoryCriteria(ctx context.Context) map[string]string {
	cats := map[string]string{
		"architecture": "System structure, module boundaries, runtime constraints.",
		"security":     "Auth, tokens, sessions, secrets, permissions.",
		"database":     "Database, SQL, schema, queries, transactions, storage.",
		"convention":   "Coding style, naming, error handling, formatting.",
		"dependency":   "Libraries, packages, SDKs, versions.",
		"testing":      "Tests, verification, quality gates.",
		"workflow":     "Build, CI/CD, git, operations.",
		"preference":   "Soft preferences, favored languages/tools, personal tastes.",
		"profile":      "Identity, residence, role, background.",
		"health":       "Allergies, medical/diet constraints.",
		"routine":      "Schedules, habits, recurring routines.",
		"goal":         "Long-term goals and targets.",
	}
	var custom map[string][]string
	if err := json.Unmarshal([]byte(memory.PresetCategoriesFromContext(ctx)), &custom); err == nil {
		for k, subs := range custom {
			k = strings.ToLower(strings.TrimSpace(k))
			if k == "" || len(cats) >= 64 {
				continue
			}
			if _, exists := cats[k]; !exists {
				if len(subs) > 0 {
					cats[k] = "Domain category (" + strings.Join(subs, ", ") + ")."
				} else {
					cats[k] = "Custom domain category " + k + "."
				}
			}
		}
	}
	return cats
}

func (d *decisionEvaluator) EvaluateMutation(ctx context.Context, scope, candidate string, existing []memory.Fact) (memory.MutationDecision, error) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Scope: %s\nExisting Facts:\n", scope)
	q := map[string]decisionQuestion{
		"perm": {Type: "noul", Instructions: "Is Candidate a lasting rule/state/preference worth keeping long-term (not a temporary log or transient chatter)?"},
		"inv":  {Type: "noul", Instructions: "Is Candidate a strict non-negotiable invariant or constraint (e.g. constitutional rule, allergy, hard boundary) that must always be surfaced?"},
		"tier": {Type: "choice", Instructions: "Classify Candidate strength/tier.", Criteria: map[string]string{
			"invariant":  "Strict non-negotiable prohibition or safety boundary.",
			"rule":       "Standard architectural decision, convention, or current state.",
			"preference": "Soft preference, favored language/tool, or personal taste (non-binding hint).",
			"transient":  "Temporary debug log or ephemeral chatter.",
		}},
		"card": {Type: "choice", Instructions: "Can multiple items coexist for this subject, or is it mutually exclusive?", Criteria: map[string]string{
			"exclusive": "Only one value/state can hold at a time, or Candidate explicitly replaces/negates a previous state.",
			"additive":  "Multiple items/preferences can coexist simultaneously (e.g. liking multiple languages, tools, or foods).",
		}},
		"cat": {Type: "choice", Instructions: "Classify Candidate into a domain.", Criteria: buildCategoryCriteria(ctx)},
	}
	relCriteria := map[string]string{
		"INDEPENDENT": "Unrelated, complementary, or coexisting additive preference (e.g. liking multiple languages/tools/foods).",
		"OVERWRITE":   "Candidate explicitly contradicts, replaces, or negates this existing fact (mutually exclusive state).",
		"DUPLICATE":   "Candidate already means the exact same thing as this existing fact.",
	}
	for _, f := range existing {
		fmt.Fprintf(&sb, "[#%d] (%s) %s\n", f.ID, f.Category, f.Content)
		q[fmt.Sprintf("r_%d", f.ID)] = decisionQuestion{
			Type:         "choice",
			Instructions: fmt.Sprintf("Relation of Candidate Fact to Existing Fact #%d.", f.ID),
			Criteria:     relCriteria,
		}
	}
	fmt.Fprintf(&sb, "\nCandidate Fact: %s\n", candidate)

	ans, err := d.tr.CallDecisions(ctx, sb.String(), q)
	if err != nil {
		return memory.MutationDecision{}, err
	}
	perm, inv, cat := ans["perm"], ans["inv"], ans["cat"]
	dec := memory.MutationDecision{
		Category:          cat.Choice,
		Tier:              ans["tier"].Choice,
		Cardinality:       ans["card"].Choice,
		IsPermanent:       perm.Noul,
		IsGlobalInvariant: inv.Noul,
		Action:            "INSERT",
		Confidence:        perm.conf(),
	}
	var overwrites, duplicates []int64
	var bestConf float64
	for _, f := range existing {
		a := ans[fmt.Sprintf("r_%d", f.ID)]
		c := a.conf()
		switch a.Choice {
		case "DUPLICATE":
			duplicates = append(duplicates, f.ID)
			if c > bestConf {
				bestConf = c
			}
		case "OVERWRITE":
			overwrites = append(overwrites, f.ID)
			if c >= bestConf {
				bestConf = c
				dec.ConflictFactID = f.ID
			}
		}
	}
	if len(duplicates) > 0 {
		dec.Action = "IGNORE"
		dec.ConflictFactID = duplicates[0]
		dec.ConflictFactIDs = duplicates
		dec.Confidence = bestConf
	} else if len(overwrites) > 0 {
		dec.Action = "OVERWRITE"
		dec.ConflictFactIDs = overwrites
		dec.Confidence = bestConf
	}
	memory.ApplyDecisionSemantics(&dec)
	return dec, nil
}

func (d *decisionEvaluator) VerifyPlan(ctx context.Context, proposedPlan string, rules []memory.Fact) ([]memory.PlanViolation, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	// Place stable Project Rules at byte 0 and per-call Proposed Plan at the end for prefix cache hits.
	var sb strings.Builder
	sb.WriteString("Project Rules:\n")
	q := make(map[string]decisionQuestion, len(rules))
	for _, r := range rules {
		fmt.Fprintf(&sb, "[#%d] (%s) %s\n", r.ID, r.Category, r.Content)
		q[fmt.Sprintf("v_%d", r.ID)] = decisionQuestion{
			Type:         "noul",
			Instructions: fmt.Sprintf("Does the Proposed Plan violate or contradict Rule #%d?", r.ID),
		}
	}
	fmt.Fprintf(&sb, "\nProposed Plan:\n%s\n", proposedPlan)

	ans, err := d.tr.CallDecisions(ctx, sb.String(), q)
	if err != nil {
		return nil, err
	}
	var out []memory.PlanViolation
	for _, r := range rules {
		a := ans[fmt.Sprintf("v_%d", r.ID)]
		if a.Noul >= 0.5 {
			out = append(out, memory.PlanViolation{Fact: r, Probability: a.Noul, Confidence: a.conf()})
		}
	}
	return out, nil
}

func (d *decisionEvaluator) CalibrateConfidence(ctx context.Context, scope string, facts []memory.Fact) (map[int64]float64, error) {
	if len(facts) == 0 {
		return map[int64]float64{}, nil
	}
	bandCriteria := map[string]string{
		"band_95": "0.95-1.00: strict non-negotiable invariant, hard prohibition, security/safety boundary, or core system contract.",
		"band_90": "0.90-0.94: explicit, concrete architectural, database, API, or operational rule with unambiguous conditions.",
		"band_85": "0.85-0.89: standard coding, UI layout, or workflow convention.",
		"band_80": "0.80-0.84: soft preference, stylistic taste, or situational hint.",
		"band_75": "<0.80: vague, overly broad, or underspecified note.",
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Scope: %s\nMemories:\n", scope)
	q := make(map[string]decisionQuestion, len(facts))
	for _, f := range facts {
		fmt.Fprintf(&sb, "[#%d] (%s) %s\n", f.ID, f.Category, f.Content)
		q[fmt.Sprintf("c_%d", f.ID)] = decisionQuestion{
			Type:         "choice",
			Instructions: fmt.Sprintf("Classify semantic certainty band for Memory #%d.", f.ID),
			Criteria:     bandCriteria,
		}
	}
	ans, err := d.tr.CallDecisions(ctx, sb.String(), q)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]float64, len(facts))
	for _, f := range facts {
		a := ans[fmt.Sprintf("c_%d", f.ID)]
		var base float64
		switch a.Choice {
		case "band_95":
			base = 0.96
		case "band_90":
			base = 0.92
		case "band_85":
			base = 0.87
		case "band_80":
			base = 0.82
		default:
			base = 0.76
		}
		out[f.ID] = clampProb(base)
	}
	return out, nil
}
