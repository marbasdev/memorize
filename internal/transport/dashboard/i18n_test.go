package dashboard

import "testing"

func TestI18nENKOParity(t *testing.T) {
	for k, v := range enTranslations {
		if kv, ok := koTranslations[k]; !ok || kv == "" {
			t.Errorf("en key %q (=%q) missing or empty in koTranslations", k, v)
		}
	}
	for k, v := range koTranslations {
		if ev, ok := enTranslations[k]; !ok || ev == "" {
			t.Errorf("ko key %q (=%q) missing or empty in enTranslations", k, v)
		}
	}
}
