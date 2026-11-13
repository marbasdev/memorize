package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const defaultGeminiDecisionModel = "gemini-decision-1"

// GeminiDecisionsAdapter implements non-generative closed-set decision evaluation
// via Google's Gemini Decisions API (/v1beta/decisions, 0 output tokens).
type GeminiDecisionsAdapter struct {
	apiKey string
	model  string
}

// NewGeminiDecisionsAdapter returns a BatchEvaluator backed by gemini-decision-1.
func NewGeminiDecisionsAdapter() *BatchEvaluator {
	return NewGeminiDecisionsAdapterConfig("", "")
}

// NewGeminiDecisionsAdapterConfig configures a Gemini Decisions API evaluator with
// explicit API key and model overrides (defaulting to gemini-decision-1).
func NewGeminiDecisionsAdapterConfig(apiKey, model string) *BatchEvaluator {
	k := strings.TrimSpace(apiKey)
	if k == "" {
		k = strings.TrimSpace(os.Getenv("GEMINI_API_KEY"))
	}
	m := strings.TrimSpace(model)
	if m == "" {
		m = strings.TrimSpace(os.Getenv("GEMINI_MODEL"))
	}
	if m == "" {
		m = defaultGeminiDecisionModel
	}
	tr := &GeminiDecisionsAdapter{apiKey: k, model: m}
	return NewBatchEvaluator("gemini:"+m, &decisionEvaluator{tr: tr}, 100, 4)
}

func (g *GeminiDecisionsAdapter) Name() string { return "gemini:" + g.model }

type geminiDecisionAnswerWire struct {
	Noul             float64            `json:"noul"`
	Choice           string             `json:"choice"`
	SelectedOption   string             `json:"selectedOption"`
	SelectedOptionSn string             `json:"selected_option"`
	Score            float64            `json:"score"`
	ExpectedScore    *float64           `json:"expectedScore"`
	ExpectedScoreSn  *float64           `json:"expected_score"`
	Confidence       float64            `json:"confidence"`
	AnswerConfidence float64            `json:"answer_confidence"`
	Probabilities    map[string]float64 `json:"probabilities"`
}

func (w geminiDecisionAnswerWire) toDecisionAnswer() decisionAnswer {
	choice := strings.TrimSpace(w.SelectedOption)
	if choice == "" {
		choice = strings.TrimSpace(w.SelectedOptionSn)
	}
	if choice == "" {
		choice = strings.TrimSpace(w.Choice)
	}
	maxProb := 0.0
	numProbs := make(map[int]float64, len(w.Probabilities))
	numSum := 0.0
	if len(w.Probabilities) > 0 {
		var bestKey string
		for k, p := range w.Probabilities {
			if p > maxProb {
				maxProb = p
				bestKey = k
			}
			if lv, err := strconv.Atoi(strings.TrimSpace(k)); err == nil {
				numProbs[lv] = p
				numSum += p
			}
		}
		if choice == "" {
			choice = bestKey
		}
	}

	var score float64
	isScore := false
	switch {
	case w.ExpectedScore != nil:
		score = *w.ExpectedScore
		isScore = true
	case w.ExpectedScoreSn != nil:
		score = *w.ExpectedScoreSn
		isScore = true
	case w.Score != 0:
		score = w.Score
		isScore = true
	case len(w.Probabilities) > 0:
		for k, p := range w.Probabilities {
			if lv, err := strconv.ParseFloat(k, 64); err == nil {
				score += lv * p
			}
		}
	}

	conf := w.Confidence
	if conf <= 0 {
		conf = w.AnswerConfidence
	}
	if conf <= 0 && maxProb > 0 {
		conf = maxProb
	}
	// On discrete ordinal rubrics (0..5), Google's API sets confidence = max_k P(k),
	// which under-reports certainty when probability mass straddles two adjacent levels
	// around the expected score (e.g. P(4)=0.29, P(5)=0.51 -> score 4.25).
	if isScore && len(numProbs) >= 3 && numSum > 0 {
		r := int(math.Round(score))
		adjConf := math.Max(numProbs[r]+numProbs[r-1], numProbs[r]+numProbs[r+1])
		if adjConf > 1 {
			adjConf = 1
		}
		if adjConf > conf {
			conf = adjConf
		}
	}

	return decisionAnswer{
		Choice:           choice,
		Score:            score,
		Noul:             w.Noul,
		Confidence:       conf,
		AnswerConfidence: w.AnswerConfidence,
	}
}

// CallDecisions posts a single forward-pass decision batch to /v1beta/decisions.
func (g *GeminiDecisionsAdapter) CallDecisions(ctx context.Context, input string, questions map[string]decisionQuestion) (map[string]decisionAnswer, error) {
	endpoint := geminiBaseURL() + "/v1beta/decisions"
	body, err := json.Marshal(map[string]any{
		"model":     g.model,
		"input":     input,
		"questions": questions,
	})
	if err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt*350) * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if g.apiKey != "" {
			req.Header.Set("x-goog-api-key", g.apiKey)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("gemini decisions endpoint unreachable: %w", err)
		}

		if resp.StatusCode >= 300 {
			errBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			_ = resp.Body.Close()
			msg := geminiErrorMessage(errBytes)
			if msg != "" {
				lastErr = fmt.Errorf("gemini decisions status %d: %s", resp.StatusCode, msg)
			} else {
				lastErr = fmt.Errorf("gemini decisions status %d", resp.StatusCode)
			}
			if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests {
				continue
			}
			return nil, lastErr
		}

		var wire struct {
			Answers map[string]geminiDecisionAnswerWire `json:"answers"`
		}
		decErr := json.NewDecoder(resp.Body).Decode(&wire)
		_ = resp.Body.Close()
		if decErr != nil {
			return nil, decErr
		}
		out := make(map[string]decisionAnswer, len(wire.Answers))
		for k, v := range wire.Answers {
			out[k] = v.toDecisionAnswer()
		}
		return out, nil
	}
	return nil, lastErr
}
