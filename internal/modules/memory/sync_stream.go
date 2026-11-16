package memory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"memorize/internal/domain"
	issh "memorize/internal/infra/ssh"
)

const syncBatchSize = 200

// SyncStreamBackend abstracts the storage operations needed by the SSH stdio server handler
// so both direct in-process *memory.Service and HTTP loopback daemon bridges share one protocol loop.
type SyncStreamBackend interface {
	HandleSyncHandshake(ctx context.Context, req domain.SyncWireFrame) (domain.SyncWireFrame, []domain.MemoryTxEntry, error)
	ApplySyncDeltas(ctx context.Context, remoteDevID string, entries []domain.MemoryTxEntry) (domain.SyncApplyStats, error)
}

// ServeSyncStream runs the passive peer side of the bidirectional NDJSON sync protocol over r and w.
func ServeSyncStream(ctx context.Context, backend SyncStreamBackend, r io.Reader, w io.Writer) error {
	if !domain.IsMemorySyncEnabled() {
		_ = issh.WriteSyncFrame(w, domain.SyncWireFrame{Type: "error", Error: domain.ErrMemorySyncDisabled.Error()})
		return domain.ErrMemorySyncDisabled
	}
	sc := issh.NewFrameScanner(r)
	hello, err := issh.ReadSyncFrame(sc)
	if err != nil {
		return err
	}
	if hello.Type != "hello" {
		_ = issh.WriteSyncFrame(w, domain.SyncWireFrame{Type: "error", Error: fmt.Sprintf("expected hello, got %q", hello.Type)})
		return fmt.Errorf("expected hello, got %q", hello.Type)
	}

	ack, pullEntries, err := backend.HandleSyncHandshake(ctx, hello)
	if err != nil {
		_ = issh.WriteSyncFrame(w, domain.SyncWireFrame{Type: "error", Error: err.Error()})
		return err
	}
	if err := issh.WriteSyncFrame(w, ack); err != nil {
		return err
	}
	if hello.ProbeOnly {
		return nil
	}

	for i := 0; i < len(pullEntries); i += syncBatchSize {
		end := min(i+syncBatchSize, len(pullEntries))
		if err := issh.WriteSyncFrame(w, domain.SyncWireFrame{
			Type:    "entries",
			Entries: pullEntries[i:end],
		}); err != nil {
			return err
		}
	}
	if err := issh.WriteSyncFrame(w, domain.SyncWireFrame{Type: "batch_done"}); err != nil {
		return err
	}

	var pushEntries []domain.MemoryTxEntry
	for {
		f, err := issh.ReadSyncFrame(sc)
		if err != nil {
			return err
		}
		if f.Type == "entries" {
			pushEntries = append(pushEntries, f.Entries...)
			continue
		}
		if f.Type == "batch_done" {
			break
		}
		if f.Type == "error" {
			return errors.New(f.Error)
		}
		return fmt.Errorf("unexpected client frame %q", f.Type)
	}

	stats, err := backend.ApplySyncDeltas(ctx, hello.DeviceID, pushEntries)
	if err != nil {
		_ = issh.WriteSyncFrame(w, domain.SyncWireFrame{Type: "error", Error: err.Error()})
		return err
	}
	return issh.WriteSyncFrame(w, domain.SyncWireFrame{
		Type:  "push_ack",
		Stats: &stats,
	})
}

// RunSyncStream executes the initiator side of a bidirectional Pull + Push sync over rw.
func (s *Service) RunSyncStream(ctx context.Context, peer domain.SyncPeer, rw io.ReadWriter) (domain.SyncRunReport, error) {
	start := time.Now()
	report := domain.SyncRunReport{
		PeerID:    peer.PeerID,
		SSHTarget: peer.SSHTarget,
	}
	if !domain.IsMemorySyncEnabled() {
		return report, domain.ErrMemorySyncDisabled
	}
	selfDev, selfLabel := s.SelfDeviceID(ctx)
	report.LocalDeviceID = selfDev

	mode, scopes := domain.NormalizeSyncScopes(peer.ScopeMode, peer.Scopes)
	localWM, err := s.loadEffectiveWatermarks(ctx, peer.RemoteDeviceID, scopes)
	if err != nil {
		return report, err
	}

	if err := issh.WriteSyncFrame(rw, domain.SyncWireFrame{
		Type:            "hello",
		ProtocolVersion: domain.SyncProtocolVer,
		DeviceID:        selfDev,
		DeviceLabel:     selfLabel,
		ScopeMode:       mode,
		Scopes:          scopes,
		Watermarks:      localWM,
	}); err != nil {
		return report, err
	}

	sc := issh.NewFrameScanner(rw)
	ack, err := issh.ReadSyncFrame(sc)
	if err != nil {
		return report, err
	}
	if ack.Type == "error" {
		return report, errors.New(ack.Error)
	}
	if ack.Type != "hello_ack" {
		return report, fmt.Errorf("unexpected handshake response %q", ack.Type)
	}
	report.RemoteDeviceID = ack.DeviceID
	report.MatchedScopes = ack.MatchedScopes

	var pullEntries []domain.MemoryTxEntry
	for {
		f, err := issh.ReadSyncFrame(sc)
		if err != nil {
			return report, err
		}
		if f.Type == "entries" {
			pullEntries = append(pullEntries, f.Entries...)
			continue
		}
		if f.Type == "batch_done" {
			break
		}
		if f.Type == "error" {
			return report, errors.New(f.Error)
		}
		return report, fmt.Errorf("unexpected pull frame %q", f.Type)
	}

	pullStats, err := s.ApplySyncDeltas(ctx, ack.DeviceID, pullEntries)
	if err != nil {
		_ = issh.WriteSyncFrame(rw, domain.SyncWireFrame{Type: "error", Error: err.Error()})
		return report, err
	}
	report.PulledApplied = pullStats.Applied
	report.PulledDeduped = pullStats.Deduped
	report.PulledSkipped = pullStats.Skipped
	report.PullConflicts = pullStats.Conflicts

	pushEntries, err := s.ExportSyncDeltas(ctx, mode, scopes, ack.Watermarks)
	if err != nil {
		_ = issh.WriteSyncFrame(rw, domain.SyncWireFrame{Type: "error", Error: err.Error()})
		return report, err
	}
	report.PushedSent = len(pushEntries)
	for i := 0; i < len(pushEntries); i += syncBatchSize {
		end := min(i+syncBatchSize, len(pushEntries))
		if err := issh.WriteSyncFrame(rw, domain.SyncWireFrame{
			Type:    "entries",
			Entries: pushEntries[i:end],
		}); err != nil {
			return report, err
		}
	}
	if err := issh.WriteSyncFrame(rw, domain.SyncWireFrame{Type: "batch_done"}); err != nil {
		return report, err
	}

	pushAck, err := issh.ReadSyncFrame(sc)
	if err != nil {
		return report, err
	}
	if pushAck.Type == "error" {
		return report, errors.New(pushAck.Error)
	}
	if pushAck.Type != "push_ack" || pushAck.Stats == nil {
		return report, fmt.Errorf("unexpected push_ack frame %q", pushAck.Type)
	}
	report.PushedApplied = pushAck.Stats.Applied
	report.PushedConflicts = pushAck.Stats.Conflicts
	report.ElapsedMs = time.Since(start).Milliseconds()

	if peer.PeerID != "" {
		summary := fmt.Sprintf("pulled %d (dedup %d, conflicts %d) · pushed %d (%d applied)",
			report.PulledApplied, report.PulledDeduped, report.PullConflicts, report.PushedSent, report.PushedApplied)
		s.recordSyncPeerOutcome(ctx, peer.PeerID, ack.DeviceID, ack.DeviceLabel, summary, "")
	}
	return report, nil
}

// TestSyncTarget probes a remote SSH host by performing a non-destructive hello handshake.
func (s *Service) TestSyncTarget(ctx context.Context, rawTarget, remoteBin string) domain.SyncPeerProbe {
	start := time.Now()
	probe := domain.SyncPeerProbe{SSHTarget: strings.TrimSpace(rawTarget)}
	if !domain.IsMemorySyncEnabled() {
		probe.Error = domain.ErrMemorySyncDisabled.Error()
		return probe
	}
	tCtx, cancel := context.WithTimeout(ctx, 18*time.Second)
	defer cancel()

	conn, err := issh.OpenSyncPipe(tCtx, rawTarget, remoteBin)
	if err != nil {
		probe.ElapsedMs = time.Since(start).Milliseconds()
		probe.Error = err.Error()
		return probe
	}
	selfDev, selfLabel := s.SelfDeviceID(ctx)
	if err := issh.WriteSyncFrame(conn, domain.SyncWireFrame{
		Type:            "hello",
		ProtocolVersion: domain.SyncProtocolVer,
		DeviceID:        selfDev,
		DeviceLabel:     selfLabel,
		ProbeOnly:       true,
	}); err != nil {
		_ = conn.Close()
		probe.ElapsedMs = time.Since(start).Milliseconds()
		probe.Error = issh.FormatSSHFailure(err, conn.StderrText()).Error()
		return probe
	}

	sc := issh.NewFrameScanner(conn)
	ack, err := issh.ReadSyncFrame(sc)
	closeErr := conn.Close()
	probe.ElapsedMs = time.Since(start).Milliseconds()
	if err != nil {
		probe.Error = issh.FormatSSHFailure(err, conn.StderrText()).Error()
		return probe
	}
	if ack.Type == "error" {
		probe.Error = ack.Error
		return probe
	}
	if closeErr != nil && ack.DeviceID == "" {
		probe.Error = closeErr.Error()
		return probe
	}
	probe.OK = true
	probe.RemoteDeviceID = ack.DeviceID
	probe.RemoteLabel = ack.DeviceLabel
	probe.ProtocolVersion = ack.ProtocolVersion
	return probe
}

// SyncWithPeer runs bidirectional SSH delta synchronization with a saved peer_id (or raw SSH target).
func (s *Service) SyncWithPeer(ctx context.Context, peerIDOrTarget string) (domain.SyncRunReport, error) {
	if !domain.IsMemorySyncEnabled() {
		return domain.SyncRunReport{SSHTarget: strings.TrimSpace(peerIDOrTarget)}, domain.ErrMemorySyncDisabled
	}
	peer, err := s.GetSyncPeer(ctx, peerIDOrTarget)
	if err != nil {
		spec, perr := domain.ParseSyncTarget(peerIDOrTarget)
		if perr != nil {
			return domain.SyncRunReport{}, err
		}
		saved, serr := s.UpsertSyncPeer(ctx, domain.SyncPeer{
			PeerID:    spec.HostSpec,
			SSHTarget: strings.TrimSpace(peerIDOrTarget),
			ScopeMode: domain.SyncScopeModeAll,
		})
		if serr != nil {
			return domain.SyncRunReport{}, serr
		}
		peer = &saved
	}
	return s.SyncWithPeerSpec(ctx, *peer)
}

// SyncWithPeerSpec runs bidirectional SSH delta synchronization against peer.
func (s *Service) SyncWithPeerSpec(ctx context.Context, peer domain.SyncPeer) (domain.SyncRunReport, error) {
	if !domain.IsMemorySyncEnabled() {
		return domain.SyncRunReport{PeerID: peer.PeerID, SSHTarget: peer.SSHTarget}, domain.ErrMemorySyncDisabled
	}
	sCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	conn, err := issh.OpenSyncPipe(sCtx, peer.SSHTarget, peer.RemoteBin)
	if err != nil {
		s.recordSyncPeerOutcome(ctx, peer.PeerID, "", "", "error", err.Error())
		return domain.SyncRunReport{PeerID: peer.PeerID, SSHTarget: peer.SSHTarget}, err
	}

	report, runErr := s.RunSyncStream(sCtx, peer, conn)
	closeErr := conn.Close()
	if runErr != nil {
		enriched := issh.FormatSSHFailure(runErr, conn.StderrText())
		s.recordSyncPeerOutcome(ctx, peer.PeerID, report.RemoteDeviceID, "", "error", enriched.Error())
		return report, enriched
	}
	if closeErr != nil {
		s.recordSyncPeerOutcome(ctx, peer.PeerID, report.RemoteDeviceID, "", "error", closeErr.Error())
		return report, closeErr
	}
	return report, nil
}
