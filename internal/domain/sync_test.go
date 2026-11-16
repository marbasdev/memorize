package domain

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseSyncTarget(t *testing.T) {
	valid := []struct {
		raw      string
		wantHost string
		wantPort int
	}{
		{"home-mac", "home-mac", 0},
		{"alice@100.64.1.10", "alice@100.64.1.10", 0},
		{"alice@home.ts.net:2222", "alice@home.ts.net", 2222},
		{"ssh://bob@mac-mini.local", "bob@mac-mini.local", 0},
		{"ssh://bob@100.70.0.5:2200", "bob@100.70.0.5", 2200},
	}
	for _, tc := range valid {
		got, err := ParseSyncTarget(tc.raw)
		if err != nil {
			t.Fatalf("ParseSyncTarget(%q) unexpected error: %v", tc.raw, err)
		}
		if got.HostSpec != tc.wantHost || got.Port != tc.wantPort {
			t.Fatalf("ParseSyncTarget(%q) = %+v, want host=%q port=%d", tc.raw, got, tc.wantHost, tc.wantPort)
		}
	}

	invalid := []string{
		"",
		"   ",
		"-oProxyCommand=id",
		"-F/tmp/evil",
		"host;rm -rf /",
		"host name",
		"alice@host|cat",
		"http://example.com",
		"ssh://user:secret@host",
		"ssh://user@host/path",
		"alice@-badhost",
		"host:99999",
		"@host",
		"user@",
	}
	for _, raw := range invalid {
		if _, err := ParseSyncTarget(raw); err == nil {
			t.Fatalf("ParseSyncTarget(%q) expected error, got nil", raw)
		}
	}
}

func TestValidateRemoteBin(t *testing.T) {
	if got, err := ValidateRemoteBin(""); err != nil || got != "" {
		t.Fatalf("empty remote bin failed: got=%q err=%v", got, err)
	}
	if got, err := ValidateRemoteBin("/Users/home/.memorize/bin/memorize"); err != nil || got == "" {
		t.Fatalf("valid remote bin failed: got=%q err=%v", got, err)
	}
	for _, bad := range []string{"-oFoo", "memorize;id", "/tmp/a b", "$HOME/x"} {
		if _, err := ValidateRemoteBin(bad); err == nil {
			t.Fatalf("ValidateRemoteBin(%q) expected error, got nil", bad)
		}
	}
}

func TestNormalizeSyncScopesAndIdentities(t *testing.T) {
	mode, scopes := NormalizeSyncScopes("ALL", []string{"global"})
	if mode != SyncScopeModeAll || len(scopes) != 0 {
		t.Fatalf("NormalizeSyncScopes all = (%q, %v)", mode, scopes)
	}
	mode, scopes = NormalizeSyncScopes("selected", []string{"Project:Repo", "global", "project:repo", "  "})
	want := []string{"global", "project:repo"}
	if mode != SyncScopeModeSelected || !reflect.DeepEqual(scopes, want) {
		t.Fatalf("NormalizeSyncScopes selected = (%q, %v), want %v", mode, scopes, want)
	}

	id1, id2 := NewSyncID(), NewSyncID()
	if len(id1) != 36 || id1 == id2 {
		t.Fatalf("NewSyncID invalid: id1=%q id2=%q", id1, id2)
	}
	dev := NewDeviceID("Home Mac.local")
	if !strings.HasPrefix(dev, "home-mac-local-") || len(dev) < 16 {
		t.Fatalf("unexpected NewDeviceID: %q", dev)
	}
}

func TestSnapshotsEquivalent(t *testing.T) {
	a := MemorySnapshot{
		SyncID:   "a",
		Scope:    "global",
		Account:  "alice",
		Category: "convention",
		Content:  "Use Go fmt",
		Pinned:   true,
		Active:   true,
	}
	b := a
	b.SyncID = "b"
	b.Content = " Use Go fmt "
	if !SnapshotsEquivalent(a, b) {
		t.Fatalf("expected semantically equivalent snapshots")
	}
	b.Active = false
	if SnapshotsEquivalent(a, b) {
		t.Fatalf("expected active difference to break equivalence")
	}
}
