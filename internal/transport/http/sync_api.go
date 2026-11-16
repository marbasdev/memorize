package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"memorize/internal/domain"
	"memorize/internal/modules/memory"
)

// SyncDaemonRequest carries one JSON-over-HTTP RPC operation to /api/daemon/sync.
type SyncDaemonRequest struct {
	Action         string                 `json:"action"` // handshake | apply | run_peer | test_peer | status | resolve
	Handshake      *domain.SyncWireFrame  `json:"handshake,omitempty"`
	RemoteDeviceID string                 `json:"remote_device_id,omitempty"`
	Entries        []domain.MemoryTxEntry `json:"entries,omitempty"`
	PeerIDOrTarget string                 `json:"peer_id_or_target,omitempty"`
	ScopeMode      string                 `json:"scope_mode,omitempty"`
	Scopes         []string               `json:"scopes,omitempty"`
	RemoteBin      string                 `json:"remote_bin,omitempty"`
	SyncID         string                 `json:"sync_id,omitempty"`
	Decision       string                 `json:"decision,omitempty"`
}

// SyncDaemonResponse returns the result of a /api/daemon/sync RPC call.
type SyncDaemonResponse struct {
	OK          bool                   `json:"ok"`
	Error       string                 `json:"error,omitempty"`
	DeviceID    string                 `json:"device_id,omitempty"`
	DeviceLabel string                 `json:"device_label,omitempty"`
	Ack         *domain.SyncWireFrame  `json:"ack,omitempty"`
	Entries     []domain.MemoryTxEntry `json:"entries,omitempty"`
	Stats       *domain.SyncApplyStats `json:"stats,omitempty"`
	Report      *domain.SyncRunReport  `json:"report,omitempty"`
	Probe       *domain.SyncPeerProbe  `json:"probe,omitempty"`
	Peers       []domain.SyncPeer      `json:"peers,omitempty"`
	Conflicts   []domain.SyncConflict  `json:"conflicts,omitempty"`
	Message     string                 `json:"message,omitempty"`
}

func registerSyncDaemonRoutes(mux *http.ServeMux, mem *memory.Service) {
	mux.HandleFunc("/api/daemon/sync", func(w http.ResponseWriter, r *http.Request) {
		if !IsLocalJSONPost(r) {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		if mem == nil {
			http.Error(w, `{"error":"memory service unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		if !domain.IsMemorySyncEnabled() {
			WriteJSON(w, http.StatusForbidden, SyncDaemonResponse{Error: domain.ErrMemorySyncDisabled.Error()})
			return
		}
		var req SyncDaemonRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&req); err != nil {
			WriteJSON(w, http.StatusBadRequest, SyncDaemonResponse{Error: "invalid json body"})
			return
		}
		ctx := domain.WithAdminBypass(r.Context())
		switch strings.TrimSpace(req.Action) {
		case "handshake":
			if req.Handshake == nil {
				WriteJSON(w, http.StatusBadRequest, SyncDaemonResponse{Error: "handshake payload required"})
				return
			}
			ack, entries, err := mem.HandleSyncHandshake(ctx, *req.Handshake)
			if err != nil {
				WriteJSON(w, http.StatusOK, SyncDaemonResponse{Error: err.Error()})
				return
			}
			WriteJSON(w, http.StatusOK, SyncDaemonResponse{OK: true, Ack: &ack, Entries: entries})

		case "apply":
			stats, err := mem.ApplySyncDeltas(ctx, req.RemoteDeviceID, req.Entries)
			if err != nil {
				WriteJSON(w, http.StatusOK, SyncDaemonResponse{Error: err.Error()})
				return
			}
			WriteJSON(w, http.StatusOK, SyncDaemonResponse{OK: true, Stats: &stats})

		case "run_peer":
			rep, err := runDaemonPeerSync(ctx, mem, req)
			if err != nil {
				WriteJSON(w, http.StatusOK, SyncDaemonResponse{Error: err.Error()})
				return
			}
			WriteJSON(w, http.StatusOK, SyncDaemonResponse{OK: true, Report: &rep})

		case "test_peer":
			target := strings.TrimSpace(req.PeerIDOrTarget)
			bin := strings.TrimSpace(req.RemoteBin)
			if p, err := mem.GetSyncPeer(ctx, target); err == nil && p != nil {
				target = p.SSHTarget
				if bin == "" {
					bin = p.RemoteBin
				}
			}
			probe := mem.TestSyncTarget(ctx, target, bin)
			WriteJSON(w, http.StatusOK, SyncDaemonResponse{OK: probe.OK, Probe: &probe, Error: probe.Error})

		case "status":
			devID, devLabel := mem.SelfDeviceID(ctx)
			peers, _ := mem.ListSyncPeers(ctx)
			conflicts, _ := mem.ListSyncConflicts(ctx)
			WriteJSON(w, http.StatusOK, SyncDaemonResponse{
				OK:          true,
				DeviceID:    devID,
				DeviceLabel: devLabel,
				Peers:       peers,
				Conflicts:   conflicts,
			})

		case "resolve":
			msg, err := mem.ResolveSyncConflict(ctx, req.SyncID, req.Decision)
			if err != nil {
				WriteJSON(w, http.StatusOK, SyncDaemonResponse{Error: err.Error()})
				return
			}
			WriteJSON(w, http.StatusOK, SyncDaemonResponse{OK: true, Message: msg})

		default:
			WriteJSON(w, http.StatusBadRequest, SyncDaemonResponse{Error: fmt.Sprintf("unknown sync action %q", req.Action)})
		}
	})
}

func runDaemonPeerSync(ctx context.Context, mem *memory.Service, req SyncDaemonRequest) (domain.SyncRunReport, error) {
	key := strings.TrimSpace(req.PeerIDOrTarget)
	if key == "" {
		return domain.SyncRunReport{}, errors.New("peer_id or ssh target is required")
	}
	peerPtr, err := mem.GetSyncPeer(ctx, key)
	var peer domain.SyncPeer
	if err != nil || peerPtr == nil {
		spec, perr := domain.ParseSyncTarget(key)
		if perr != nil {
			return domain.SyncRunReport{}, perr
		}
		mode, scopes := domain.NormalizeSyncScopes(req.ScopeMode, req.Scopes)
		saved, uerr := mem.UpsertSyncPeer(ctx, domain.SyncPeer{
			PeerID:    spec.HostSpec,
			SSHTarget: key,
			RemoteBin: req.RemoteBin,
			ScopeMode: mode,
			Scopes:    scopes,
		})
		if uerr != nil {
			return domain.SyncRunReport{}, uerr
		}
		peer = saved
	} else {
		peer = *peerPtr
		if strings.TrimSpace(req.ScopeMode) != "" || len(req.Scopes) > 0 {
			peer.ScopeMode, peer.Scopes = domain.NormalizeSyncScopes(req.ScopeMode, req.Scopes)
		}
		if strings.TrimSpace(req.RemoteBin) != "" {
			cleanBin, berr := domain.ValidateRemoteBin(req.RemoteBin)
			if berr != nil {
				return domain.SyncRunReport{}, berr
			}
			peer.RemoteBin = cleanBin
		}
	}
	return mem.SyncWithPeerSpec(ctx, peer)
}

// PostDaemonSync invokes POST /api/daemon/sync on the local daemon listening at addr.
func PostDaemonSync(ctx context.Context, addr string, req SyncDaemonRequest) (SyncDaemonResponse, error) {
	var out SyncDaemonResponse
	body, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 75*time.Second)
		defer cancel()
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/api/daemon/sync", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := daemonHookHTTP.Do(httpReq)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("decode sync response (HTTP %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode >= 400 && out.Error == "" {
		return out, fmt.Errorf("daemon sync HTTP %d", resp.StatusCode)
	}
	if !out.OK && out.Error != "" {
		return out, errors.New(out.Error)
	}
	return out, nil
}

type daemonSyncBackend struct {
	addr string
}

func (b *daemonSyncBackend) HandleSyncHandshake(ctx context.Context, req domain.SyncWireFrame) (domain.SyncWireFrame, []domain.MemoryTxEntry, error) {
	resp, err := PostDaemonSync(ctx, b.addr, SyncDaemonRequest{
		Action:    "handshake",
		Handshake: &req,
	})
	if err != nil {
		return domain.SyncWireFrame{}, nil, err
	}
	if resp.Ack == nil {
		return domain.SyncWireFrame{}, nil, errors.New("daemon returned empty handshake ack")
	}
	return *resp.Ack, resp.Entries, nil
}

func (b *daemonSyncBackend) ApplySyncDeltas(ctx context.Context, remoteDevID string, entries []domain.MemoryTxEntry) (domain.SyncApplyStats, error) {
	resp, err := PostDaemonSync(ctx, b.addr, SyncDaemonRequest{
		Action:         "apply",
		RemoteDeviceID: remoteDevID,
		Entries:        entries,
	})
	if err != nil {
		return domain.SyncApplyStats{}, err
	}
	if resp.Stats == nil {
		return domain.SyncApplyStats{}, nil
	}
	return *resp.Stats, nil
}

// ServeSyncStreamOverDaemon runs the passive SSH stdio sync protocol against the live local daemon.
func ServeSyncStreamOverDaemon(ctx context.Context, addr string, r io.Reader, w io.Writer) error {
	if !domain.IsMemorySyncEnabled() {
		return domain.ErrMemorySyncDisabled
	}
	return memory.ServeSyncStream(ctx, &daemonSyncBackend{addr: addr}, r, w)
}
