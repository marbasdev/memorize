package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"memorize/internal/domain"
)

type RemoteBindingsFile struct {
	Default *domain.RemoteTargetConfig           `json:"default,omitempty"`
	Repos   map[string]domain.RemoteTargetConfig `json:"repos,omitempty"`
}

func RemotesFilePath(dataDir string) string {
	dir := strings.TrimSpace(dataDir)
	if dir == "" {
		dir = DefaultDataDir()
	}
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "remotes.json")
}

func LoadRemoteBindings(dataDir string) RemoteBindingsFile {
	p := RemotesFilePath(dataDir)
	if p == "" {
		return RemoteBindingsFile{}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return RemoteBindingsFile{}
	}
	var out RemoteBindingsFile
	if json.Unmarshal(b, &out) != nil {
		return RemoteBindingsFile{}
	}
	return out
}

func saveRemoteBindings(dataDir string, f RemoteBindingsFile) error {
	p := RemotesFilePath(dataDir)
	if p == "" {
		return fmt.Errorf("cannot resolve memorize data directory")
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic0600(p, append(b, '\n'))
}

func NormalizeRemoteURL(raw string) string {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	return s
}

func SaveRemoteBinding(dataDir, repoRoot string, global bool, target domain.RemoteTargetConfig) error {
	target.URL = NormalizeRemoteURL(target.URL)
	target.Token = strings.TrimSpace(target.Token)
	if target.URL == "" || target.Token == "" {
		return fmt.Errorf("remote url and token are required")
	}
	cur := LoadRemoteBindings(dataDir)
	if cur.Repos == nil {
		cur.Repos = make(map[string]domain.RemoteTargetConfig)
	}
	if global || strings.TrimSpace(repoRoot) == "" {
		cp := target
		cur.Default = &cp
	} else {
		cleanRepo, err := filepath.Abs(strings.TrimSpace(repoRoot))
		if err != nil {
			cleanRepo = filepath.Clean(strings.TrimSpace(repoRoot))
		}
		cur.Repos[cleanRepo] = target
	}
	return saveRemoteBindings(dataDir, cur)
}

func RemoveRemoteBinding(dataDir, repoRoot string, global bool) (bool, error) {
	cur := LoadRemoteBindings(dataDir)
	removed := false
	if global || strings.TrimSpace(repoRoot) == "" {
		if cur.Default != nil {
			cur.Default = nil
			removed = true
		}
	} else if cur.Repos != nil {
		cleanRepo, err := filepath.Abs(strings.TrimSpace(repoRoot))
		if err != nil {
			cleanRepo = filepath.Clean(strings.TrimSpace(repoRoot))
		}
		if _, ok := cur.Repos[cleanRepo]; ok {
			delete(cur.Repos, cleanRepo)
			removed = true
		}
	}
	if !removed {
		return false, nil
	}
	return true, saveRemoteBindings(dataDir, cur)
}

func ResolveRemoteTarget(dataDir, cwd string) (domain.RemoteTargetConfig, bool) {
	if envURL := NormalizeRemoteURL(os.Getenv("MEMORY_REMOTE_URL")); envURL != "" {
		return domain.RemoteTargetConfig{
			URL:   envURL,
			Token: strings.TrimSpace(os.Getenv("MEMORY_REMOTE_TOKEN")),
		}, true
	}
	bindings := LoadRemoteBindings(dataDir)
	if cwdClean := strings.TrimSpace(cwd); cwdClean != "" {
		if abs, err := filepath.Abs(cwdClean); err == nil {
			cwdClean = filepath.Clean(abs)
		} else {
			cwdClean = filepath.Clean(cwdClean)
		}
		bestLen := -1
		var best domain.RemoteTargetConfig
		for repoKey, target := range bindings.Repos {
			rk := filepath.Clean(repoKey)
			if cwdClean == rk || strings.HasPrefix(cwdClean, rk+string(filepath.Separator)) {
				if len(rk) > bestLen && NormalizeRemoteURL(target.URL) != "" {
					bestLen = len(rk)
					best = target
				}
			}
		}
		if bestLen >= 0 {
			best.URL = NormalizeRemoteURL(best.URL)
			return best, true
		}
	}
	if bindings.Default != nil && NormalizeRemoteURL(bindings.Default.URL) != "" {
		def := *bindings.Default
		def.URL = NormalizeRemoteURL(def.URL)
		return def, true
	}
	return domain.RemoteTargetConfig{}, false
}

type ClientTurnEditEntry struct {
	Scope           string `json:"scope"`
	FilePath        string `json:"file_path"`
	DisplayPath     string `json:"display_path"`
	BaselineExists  int    `json:"baseline_exists"`
	BaselineContent string `json:"baseline_content,omitempty"`
	UpdatedAt       int64  `json:"updated_at"`
}

func clientTurnStatePath(dataDir, sessionID string) string {
	dir := strings.TrimSpace(dataDir)
	if dir == "" {
		dir = DefaultDataDir()
	}
	if dir == "" || strings.TrimSpace(sessionID) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(sessionID)))
	return filepath.Join(dir, "client-turns", hex.EncodeToString(sum[:12])+".json")
}

func RecordClientTurnBaseline(dataDir, sessionID, scope, filePath, displayPath string) {
	p := clientTurnStatePath(dataDir, sessionID)
	if p == "" || strings.TrimSpace(filePath) == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	entries := loadClientTurnBaselines(p)
	clean := filepath.Clean(filePath)
	now := time.Now().Unix()

	for _, e := range entries {
		if e.FilePath == clean && e.Scope == scope {
			return
		}
	}

	const maxBytes = 128 * 1024
	exists := -1
	content := ""
	if data, err := os.ReadFile(clean); os.IsNotExist(err) {
		exists = 0
	} else if err == nil && len(data) <= maxBytes && utf8.Valid(data) && bytes.IndexByte(data, 0) < 0 {
		exists = 1
		content = string(data)
	}
	entries = append(entries, ClientTurnEditEntry{
		Scope:           scope,
		FilePath:        clean,
		DisplayPath:     displayPath,
		BaselineExists:  exists,
		BaselineContent: content,
		UpdatedAt:       now,
	})
	if b, err := json.Marshal(entries); err == nil {
		_ = writeFileAtomic0600(p, b)
	}
}

func loadClientTurnBaselines(path string) []ClientTurnEditEntry {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var raw []ClientTurnEditEntry
	if json.Unmarshal(b, &raw) != nil {
		return nil
	}
	cutoff := time.Now().Add(-domain.TurnBaselineTTL).Unix()
	var fresh []ClientTurnEditEntry
	for _, e := range raw {
		if e.UpdatedAt >= cutoff {
			fresh = append(fresh, e)
		}
	}
	return fresh
}

func ConsumeClientTurnDiffs(dataDir, sessionID string, clear bool) []domain.HookDiffSection {
	p := clientTurnStatePath(dataDir, sessionID)
	if p == "" {
		return nil
	}
	entries := loadClientTurnBaselines(p)
	if clear {
		_ = os.Remove(p)
	}
	if len(entries) == 0 {
		return nil
	}
	var out []domain.HookDiffSection
	for _, e := range entries {
		data, err := os.ReadFile(e.FilePath)
		if os.IsNotExist(err) {
			if e.BaselineExists == 1 && strings.TrimSpace(e.BaselineContent) != "" {
				out = append(out, domain.HookDiffSection{
					Scope: e.Scope,
					Label: e.DisplayPath,
					Body:  fmt.Sprintf("File: %s (deleted)\nRemoved:\n%s", e.DisplayPath, domain.CompactDiffLine(e.BaselineContent, 2000)),
				})
			}
			continue
		}
		if err != nil {
			continue
		}
		if e.BaselineExists == -1 || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			out = append(out, domain.HookDiffSection{
				Scope: e.Scope,
				Label: e.DisplayPath,
				Body:  fmt.Sprintf("File: %s (modified)", e.DisplayPath),
			})
			continue
		}
		cur := string(data)
		if e.BaselineExists == 1 && cur == e.BaselineContent {
			continue
		}
		oldText := e.BaselineContent
		if e.BaselineExists == 0 {
			oldText = ""
		}
		diff := domain.FormatChangedLineHunks(oldText, cur, 0)
		if strings.TrimSpace(diff) == "" {
			continue
		}
		body := fmt.Sprintf("File: %s\n%s", e.DisplayPath, diff)
		if diff == "(whitespace/formatting only)" {
			body = fmt.Sprintf("File: %s %s", e.DisplayPath, diff)
		}
		out = append(out, domain.HookDiffSection{
			Scope: e.Scope,
			Label: e.DisplayPath,
			Body:  body,
		})
	}
	return out
}
