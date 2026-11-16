package domain

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode"
)

const (
	SyncOpUpsert = "upsert"
	SyncOpPurge  = "purge"

	SyncScopeModeAll      = "all"
	SyncScopeModeSelected = "selected"

	SyncResolveKeepLocal = "keep_local"
	SyncResolveUseRemote = "use_remote"
	SyncResolveKeepBoth  = "keep_both"

	SyncFramePrefix = "MEMORIZE_SYNC_V1 "
	SyncProtocolVer = 1
)

var (
	// ErrMemorySyncDisabled is returned whenever cross-device sync is gated off.
	ErrMemorySyncDisabled = errors.New("cross-device memory sync is disabled in this release")
	memorySyncEnabled     atomic.Bool
)

// IsMemorySyncEnabled reports whether cross-device memory sync and tx logging are active.
func IsMemorySyncEnabled() bool {
	return memorySyncEnabled.Load()
}

// SetMemorySyncEnabledForTest toggles cross-device sync in tests and returns a cleanup callback.
func SetMemorySyncEnabledForTest(enabled bool) func() {
	prev := memorySyncEnabled.Swap(enabled)
	return func() { memorySyncEnabled.Store(prev) }
}

// SyncWatermarkMap indexes the last applied origin_seq per scope and origin device:
// watermarks[scope][origin_device] = max_origin_seq.
type SyncWatermarkMap map[string]map[string]int64

// MemorySnapshot carries the complete portable state of one memories row for transaction log replay.
type MemorySnapshot struct {
	SyncID             string  `json:"sync_id"`
	Scope              string  `json:"scope"`
	Account            string  `json:"account"`
	Category           string  `json:"category"`
	SubCategory        string  `json:"sub_category,omitempty"`
	Tags               string  `json:"tags,omitempty"`
	ReplacesText       string  `json:"replaces_text,omitempty"`
	Content            string  `json:"content"`
	Confidence         float64 `json:"confidence"`
	Pinned             bool    `json:"pinned"`
	Active             bool    `json:"active"`
	SupersededBySyncID string  `json:"superseded_by_sync_id,omitempty"`
	CreatedAt          string  `json:"created_at,omitempty"`
}

// MemoryTxEntry describes one immutable transaction log record emitted for a memory mutation or purge.
type MemoryTxEntry struct {
	Seq                  int64           `json:"seq,omitempty"`
	OriginDevice         string          `json:"origin_device"`
	OriginSeq            int64           `json:"origin_seq"`
	Op                   string          `json:"op"`
	SyncID               string          `json:"sync_id"`
	BaseOriginDevice     string          `json:"base_origin_device,omitempty"`
	BaseOriginSeq        int64           `json:"base_origin_seq,omitempty"`
	ResolvedOriginDevice string          `json:"resolved_origin_device,omitempty"`
	ResolvedOriginSeq    int64           `json:"resolved_origin_seq,omitempty"`
	Scope                string          `json:"scope"`
	Snapshot             *MemorySnapshot `json:"snapshot,omitempty"`
	CreatedAt            string          `json:"created_at,omitempty"`
}

// SyncPeer configures one remote SSH target and its remembered scope filter and last run status.
type SyncPeer struct {
	PeerID         string   `json:"peer_id"`
	SSHTarget      string   `json:"ssh_target"`
	RemoteBin      string   `json:"remote_bin,omitempty"`
	DeviceLabel    string   `json:"device_label,omitempty"`
	RemoteDeviceID string   `json:"remote_device_id,omitempty"`
	ScopeMode      string   `json:"scope_mode"`
	Scopes         []string `json:"scopes,omitempty"`
	LastSyncAt     string   `json:"last_sync_at,omitempty"`
	LastStatus     string   `json:"last_status,omitempty"`
	LastError      string   `json:"last_error,omitempty"`
}

// SyncConflict stores a concurrent edit/purge conflict awaiting operator selection in the dashboard or CLI.
type SyncConflict struct {
	SyncID             string         `json:"sync_id"`
	Scope              string         `json:"scope"`
	LocalMemoryID      int64          `json:"local_memory_id"`
	LocalOriginDevice  string         `json:"local_origin_device"`
	LocalOriginSeq     int64          `json:"local_origin_seq"`
	LocalSnapshot      MemorySnapshot `json:"local_snapshot"`
	RemoteDeviceID     string         `json:"remote_device_id"`
	RemoteOriginDevice string         `json:"remote_origin_device"`
	RemoteOriginSeq    int64          `json:"remote_origin_seq"`
	RemoteOp           string         `json:"remote_op"`
	RemoteSnapshot     MemorySnapshot `json:"remote_snapshot"`
	Reason             string         `json:"reason"`
	CreatedAt          string         `json:"created_at"`
}

// SyncApplyStats summarizes the outcome of applying incoming delta entries locally.
type SyncApplyStats struct {
	Applied   int `json:"applied"`
	Deduped   int `json:"deduped"`
	Skipped   int `json:"skipped"`
	Conflicts int `json:"conflicts"`
}

// SyncRunReport summarizes a completed bidirectional (Pull + Push) SSH sync session.
type SyncRunReport struct {
	PeerID          string   `json:"peer_id,omitempty"`
	SSHTarget       string   `json:"ssh_target"`
	LocalDeviceID   string   `json:"local_device_id"`
	RemoteDeviceID  string   `json:"remote_device_id"`
	MatchedScopes   []string `json:"matched_scopes"`
	PulledApplied   int      `json:"pulled_applied"`
	PulledDeduped   int      `json:"pulled_deduped"`
	PulledSkipped   int      `json:"pulled_skipped"`
	PullConflicts   int      `json:"pull_conflicts"`
	PushedSent      int      `json:"pushed_sent"`
	PushedApplied   int      `json:"pushed_applied"`
	PushedConflicts int      `json:"pushed_conflicts"`
	ElapsedMs       int64    `json:"elapsed_ms"`
}

// SyncPeerProbe verifies non-interactive SSH connectivity to a peer and returns its remote identity.
type SyncPeerProbe struct {
	OK              bool   `json:"ok"`
	SSHTarget       string `json:"ssh_target"`
	RemoteDeviceID  string `json:"remote_device_id,omitempty"`
	RemoteLabel     string `json:"remote_label,omitempty"`
	ProtocolVersion int    `json:"protocol_version,omitempty"`
	ElapsedMs       int64  `json:"elapsed_ms"`
	Error           string `json:"error,omitempty"`
}

// SyncTargetSpec represents a validated SSH destination parsed from user input or ssh:// URL.
type SyncTargetSpec struct {
	HostSpec string // host or user@host passed after '--' to ssh
	Port     int    // optional SSH port (>0 -> pass -p <port>)
}

// SyncWireFrame transports one NDJSON protocol message over SSH stdin/stdout pipes.
type SyncWireFrame struct {
	Type            string           `json:"type"` // hello | hello_ack | entries | batch_done | push_ack | error
	ProtocolVersion int              `json:"protocol_version,omitempty"`
	DeviceID        string           `json:"device_id,omitempty"`
	DeviceLabel     string           `json:"device_label,omitempty"`
	ScopeMode       string           `json:"scope_mode,omitempty"`
	Scopes          []string         `json:"scopes,omitempty"`
	MatchedScopes   []string         `json:"matched_scopes,omitempty"`
	Watermarks      SyncWatermarkMap `json:"watermarks,omitempty"`
	Entries         []MemoryTxEntry  `json:"entries,omitempty"`
	Stats           *SyncApplyStats  `json:"stats,omitempty"`
	ProbeOnly       bool             `json:"probe_only,omitempty"`
	Error           string           `json:"error,omitempty"`
}

const forbiddenShellRunes = ";|&$`<>(){}[]!\\'\"\n\r\t "

func containsForbiddenShellRune(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(forbiddenShellRunes, r) {
			return true
		}
	}
	return false
}

// ParseSyncTarget validates raw as an SSH destination (alias, host, user@host, or ssh://user@host[:port])
// and rejects flags or shell metacharacters to prevent SSH option injection.
func ParseSyncTarget(raw string) (SyncTargetSpec, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return SyncTargetSpec{}, errors.New("ssh target is required")
	}
	if strings.HasPrefix(s, "-") {
		return SyncTargetSpec{}, errors.New("ssh target cannot start with '-'")
	}
	if containsForbiddenShellRune(s) {
		return SyncTargetSpec{}, errors.New("ssh target contains invalid whitespace or shell characters")
	}
	if strings.HasPrefix(strings.ToLower(s), "ssh://") {
		u, err := url.Parse(s)
		if err != nil {
			return SyncTargetSpec{}, fmt.Errorf("invalid ssh url: %w", err)
		}
		if u.Path != "" && u.Path != "/" {
			return SyncTargetSpec{}, errors.New("ssh url must not include a path")
		}
		if u.RawQuery != "" || u.Fragment != "" {
			return SyncTargetSpec{}, errors.New("ssh url must not include query or fragment")
		}
		host := strings.TrimSpace(u.Hostname())
		if host == "" || strings.HasPrefix(host, "-") || containsForbiddenShellRune(host) {
			return SyncTargetSpec{}, errors.New("invalid ssh host")
		}
		userName := ""
		if u.User != nil {
			if _, hasPass := u.User.Password(); hasPass {
				return SyncTargetSpec{}, errors.New("passwords in ssh urls are not supported; use ssh keys")
			}
			userName = strings.TrimSpace(u.User.Username())
			if strings.HasPrefix(userName, "-") || containsForbiddenShellRune(userName) {
				return SyncTargetSpec{}, errors.New("invalid ssh username")
			}
		}
		port := 0
		if ps := strings.TrimSpace(u.Port()); ps != "" {
			p, perr := strconv.Atoi(ps)
			if perr != nil || p < 1 || p > 65535 {
				return SyncTargetSpec{}, errors.New("invalid ssh port")
			}
			port = p
		}
		hostSpec := host
		if userName != "" {
			hostSpec = userName + "@" + host
		}
		return SyncTargetSpec{HostSpec: hostSpec, Port: port}, nil
	}
	if strings.Contains(s, "://") {
		return SyncTargetSpec{}, errors.New("only ssh:// urls or host aliases are supported")
	}
	if strings.Count(s, ":") == 1 && !strings.Contains(s, "[") {
		if h, pStr, err := net.SplitHostPort(s); err == nil {
			p, perr := strconv.Atoi(pStr)
			if perr != nil || p < 1 || p > 65535 {
				return SyncTargetSpec{}, errors.New("invalid ssh port")
			}
			if err := validateUserHostPart(h); err != nil {
				return SyncTargetSpec{}, err
			}
			return SyncTargetSpec{HostSpec: h, Port: p}, nil
		}
	}
	if err := validateUserHostPart(s); err != nil {
		return SyncTargetSpec{}, err
	}
	return SyncTargetSpec{HostSpec: s}, nil
}

func validateUserHostPart(h string) error {
	if h == "" || strings.HasPrefix(h, "-") {
		return errors.New("invalid ssh host")
	}
	if strings.HasSuffix(h, "@") || strings.HasPrefix(h, "@") || strings.Count(h, "@") > 1 {
		return errors.New("invalid user@host format")
	}
	if _, hostPart, hasAt := strings.Cut(h, "@"); hasAt {
		if hostPart == "" || strings.HasPrefix(hostPart, "-") {
			return errors.New("invalid ssh host")
		}
	}
	return nil
}

// ValidateRemoteBin verifies an optional remote binary path override is safe to invoke over SSH.
func ValidateRemoteBin(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if strings.HasPrefix(s, "-") {
		return "", errors.New("remote binary path cannot start with '-'")
	}
	if containsForbiddenShellRune(s) {
		return "", errors.New("remote binary path contains forbidden characters or spaces")
	}
	return s, nil
}

// NormalizeSyncScopes validates scope mode ("all" | "selected") and canonicalizes explicit scope names.
func NormalizeSyncScopes(mode string, scopes []string) (string, []string) {
	m := strings.ToLower(strings.TrimSpace(mode))
	if m == SyncScopeModeAll {
		return SyncScopeModeAll, nil
	}
	seen := make(map[string]bool, len(scopes))
	out := make([]string, 0, len(scopes))
	for _, raw := range scopes {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		norm := NormalizeScope(trimmed)
		if !seen[norm] {
			seen[norm] = true
			out = append(out, norm)
		}
	}
	sort.Strings(out)
	return SyncScopeModeSelected, out
}

// NewSyncID generates a random RFC 4122 v4 UUID string suitable as a global memory identifier.
func NewSyncID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// NewDeviceID generates a compact human-readable device identifier from hostname plus a random suffix.
func NewDeviceID(hostname string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(hostname)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		} else if r == '-' || r == '_' || r == '.' || unicode.IsSpace(r) {
			if sb.Len() > 0 && sb.String()[sb.Len()-1] != '-' {
				sb.WriteByte('-')
			}
		}
		if sb.Len() >= 20 {
			break
		}
	}
	prefix := strings.Trim(sb.String(), "-")
	if prefix == "" {
		prefix = "device"
	}
	var suffix [3]byte
	_, _ = rand.Read(suffix[:])
	return prefix + "-" + hex.EncodeToString(suffix[:])
}

// NormalizeDedupKey builds a canonical key for exact-content deduplication across pre-existing databases.
func NormalizeDedupKey(scope, account, content string) string {
	collapsed := strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(content))), " ")
	return NormalizeScope(scope) + "|" + NormalizeAccount(account) + "|" + collapsed
}

// SnapshotsEquivalent reports whether two MemorySnapshot states carry identical semantic fields.
func SnapshotsEquivalent(a, b MemorySnapshot) bool {
	return NormalizeScope(a.Scope) == NormalizeScope(b.Scope) &&
		NormalizeAccount(a.Account) == NormalizeAccount(b.Account) &&
		strings.TrimSpace(a.Category) == strings.TrimSpace(b.Category) &&
		strings.TrimSpace(a.SubCategory) == strings.TrimSpace(b.SubCategory) &&
		strings.TrimSpace(a.Tags) == strings.TrimSpace(b.Tags) &&
		strings.TrimSpace(a.ReplacesText) == strings.TrimSpace(b.ReplacesText) &&
		strings.TrimSpace(a.Content) == strings.TrimSpace(b.Content) &&
		a.Pinned == b.Pinned &&
		a.Active == b.Active &&
		strings.TrimSpace(a.SupersededBySyncID) == strings.TrimSpace(b.SupersededBySyncID)
}
