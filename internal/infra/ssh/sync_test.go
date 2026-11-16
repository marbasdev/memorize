package ssh

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"memorize/internal/domain"
)

func TestBuildSSHArgsHardening(t *testing.T) {
	args, err := BuildSSHArgs("ssh://alice@100.64.2.5:2222", "")
	if err != nil {
		t.Fatalf("BuildSSHArgs valid failed: %v", err)
	}
	if !slices.Contains(args, "BatchMode=yes") || !slices.Contains(args, "2222") || !slices.Contains(args, "--") {
		t.Fatalf("expected hardened ssh flags and delimiter, got %v", args)
	}
	idxDash := slices.Index(args, "--")
	if idxDash < 0 || idxDash+2 >= len(args) {
		t.Fatalf("unexpected args structure: %v", args)
	}
	if args[idxDash+1] != "alice@100.64.2.5" {
		t.Fatalf("unexpected host arg: %q", args[idxDash+1])
	}
	if !strings.Contains(args[idxDash+2], "--sync-serve") {
		t.Fatalf("unexpected remote cmd: %q", args[idxDash+2])
	}

	argsCustom, err := BuildSSHArgs("home-mac", "/opt/memorize/bin/memorize")
	if err != nil {
		t.Fatalf("BuildSSHArgs custom bin failed: %v", err)
	}
	if gotCmd := argsCustom[len(argsCustom)-1]; gotCmd != "/opt/memorize/bin/memorize --sync-serve" {
		t.Fatalf("unexpected custom remote command: %q", gotCmd)
	}

	if _, err := BuildSSHArgs("-oProxyCommand=evil", ""); err == nil {
		t.Fatalf("expected option injection rejection")
	}
	if _, err := BuildSSHArgs("home-mac", "memorize;evil"); err == nil {
		t.Fatalf("expected remote bin injection rejection")
	}
}

func TestSyncFrameSkipsMOTDBanners(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("Last login: Sat Nov 14 23:00:00 from 100.64.0.1\n")
	buf.WriteString("Welcome to home-mac!\n")
	if err := WriteSyncFrame(&buf, domain.SyncWireFrame{
		Type:            "hello_ack",
		ProtocolVersion: domain.SyncProtocolVer,
		DeviceID:        "home-mac-1234",
		DeviceLabel:     "home-mac",
	}); err != nil {
		t.Fatalf("WriteSyncFrame: %v", err)
	}

	sc := NewFrameScanner(&buf)
	frame, err := ReadSyncFrame(sc)
	if err != nil {
		t.Fatalf("ReadSyncFrame: %v", err)
	}
	if frame.Type != "hello_ack" || frame.DeviceID != "home-mac-1234" {
		t.Fatalf("unexpected decoded frame: %+v", frame)
	}
}
