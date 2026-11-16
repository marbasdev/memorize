package domain

import "testing"

func TestProviderCatalogAndLookup(t *testing.T) {
	cat := ProviderCatalog()
	if len(cat) < 4 {
		t.Fatalf("expected at least 4 built-in providers, got %d", len(cat))
	}
	spec, ok := FindProviderSpec("flide")
	if !ok || spec.ID != "jev" || !spec.DecisionOnly || spec.GenerativeCapable {
		t.Fatalf("unexpected spec for alias flide: %+v ok=%v", spec, ok)
	}
	if got := NormalizeProviderID("  FLIDE "); got != "jev" {
		t.Fatalf("expected flide -> jev, got %q", got)
	}
	if got := ProviderIDFromEvaluatorName("flide:jev-latest"); got != "jev" {
		t.Fatalf("expected flide: prefix -> jev, got %q", got)
	}
	if got := ProviderIDFromEvaluatorName("local:qwen3:4b"); got != "local" {
		t.Fatalf("expected local: prefix -> local, got %q", got)
	}
	if got := ProviderIDFromEvaluatorName("unknown:model"); got != "" {
		t.Fatalf("expected empty provider for unknown prefix, got %q", got)
	}
	cfg := map[string]string{"local_model_name": " qwen3:8b ", "gemini_model": "gemini-3.5-flash"}
	if got := ProviderModelFromConfig(cfg, "local"); got != "qwen3:8b" {
		t.Fatalf("expected local_model_name qwen3:8b, got %q", got)
	}
	if got := ProviderModelFromConfig(cfg, "unknown"); got != "" {
		t.Fatalf("expected empty model for unknown provider, got %q", got)
	}
}

func TestUtilHelpers(t *testing.T) {
	if got := FirstNonEmpty("", "   ", " alpha ", "beta"); got != "alpha" {
		t.Fatalf("expected alpha, got %q", got)
	}
	if got := FirstNonEmpty("", " "); got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
	if got := Clamp(-0.5, 0, 1); got != 0 {
		t.Fatalf("expected 0, got %v", got)
	}
	if got := Clamp(1.5, 0, 1); got != 1 {
		t.Fatalf("expected 1, got %v", got)
	}
	if got := Clamp(0.42, 0, 1); got != 0.42 {
		t.Fatalf("expected 0.42, got %v", got)
	}
}
