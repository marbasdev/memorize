package providers

import (
	"context"
	"strings"
	"testing"

	"memorize/internal/domain"
)

func TestDriverRegistryBuiltinsAndAlias(t *testing.T) {
	for _, id := range []string{"gguf", "gemini", "jev", "local"} {
		drv, ok := LookupDriver(id)
		if !ok || drv.ID != id {
			t.Fatalf("expected driver %q registered, got ok=%v drv=%+v", id, ok, drv)
		}
		if drv.CacheKey == nil || drv.Build == nil || drv.CheckReadiness == nil {
			t.Fatalf("driver %q missing required hooks", id)
		}
	}
	drv, ok := LookupDriver("flide")
	if !ok || drv.ID != "jev" {
		t.Fatalf("expected flide alias to resolve to jev driver, got %+v ok=%v", drv, ok)
	}
	if ev := StartupEvaluator("flide", DriverBuildInput{GGUFSupported: true}); ev == nil || !strings.HasPrefix(ev.Name(), "flide:") {
		t.Fatalf("expected flide: startup evaluator, got %v", ev)
	}
}

func TestRegisterCustomDriver(t *testing.T) {
	RegisterDriver(ProviderDriver{
		ID:       "custom-test-drv",
		CacheKey: func(in DriverBuildInput) string { return "custom|" + in.ModelOverride },
		Build:    func(_ DriverBuildInput) domain.Evaluator { return nil },
		CheckReadiness: func(_ context.Context, _ DriverBuildInput) (bool, string) {
			return true, ""
		},
	})
	t.Cleanup(func() {
		driversMu.Lock()
		delete(drivers, "custom-test-drv")
		driversMu.Unlock()
	})
	drv, ok := LookupDriver("custom-test-drv")
	if !ok || drv.CacheKey(DriverBuildInput{ModelOverride: "m1"}) != "custom|m1" {
		t.Fatalf("unexpected custom driver state: ok=%v drv=%+v", ok, drv)
	}
}
