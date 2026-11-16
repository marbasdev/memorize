package ssh

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"memorize/internal/domain"
)

const defaultRemoteServeCmd = `if command -v memorize >/dev/null 2>&1; then exec memorize --sync-serve; elif [ -x "$HOME/.memorize/bin/memorize" ]; then exec "$HOME/.memorize/bin/memorize" --sync-serve; elif [ -x /opt/homebrew/bin/memorize ]; then exec /opt/homebrew/bin/memorize --sync-serve; elif [ -x /usr/local/bin/memorize ]; then exec /usr/local/bin/memorize --sync-serve; else memorize --sync-serve; fi`

// BuildSSHArgs validates the peer SSH target and optional remote binary path,
// returning hardened non-interactive SSH arguments ending with "-- <host> <remoteCmd>".
func BuildSSHArgs(target, remoteBin string) ([]string, error) {
	spec, err := domain.ParseSyncTarget(target)
	if err != nil {
		return nil, err
	}
	cleanBin, err := domain.ValidateRemoteBin(remoteBin)
	if err != nil {
		return nil, err
	}
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=3",
		"-T",
	}
	if spec.Port > 0 {
		args = append(args, "-p", strconv.Itoa(spec.Port))
	}
	remoteCmd := defaultRemoteServeCmd
	if cleanBin != "" {
		remoteCmd = cleanBin + " --sync-serve"
	}
	args = append(args, "--", spec.HostSpec, remoteCmd)
	return args, nil
}

// PipeConn wraps a running non-interactive SSH subprocess stdin/stdout pipe pair.
type PipeConn struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	stderr    bytes.Buffer
	stderrMu  sync.Mutex
	closeOnce sync.Once
	waitErr   error
}

// OpenSyncPipe starts an SSH session invoking `memorize --sync-serve` on target.
func OpenSyncPipe(ctx context.Context, target, remoteBin string) (*PipeConn, error) {
	args, err := BuildSSHArgs(target, remoteBin)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "ssh", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	pc := &PipeConn{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
	}
	cmd.Stderr = &lockedBuffer{mu: &pc.stderrMu, buf: &pc.stderr}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("ssh start failed: %w", err)
	}
	return pc, nil
}

type lockedBuffer struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buf.Len() >= 8192 {
		return len(p), nil
	}
	remain := 8192 - l.buf.Len()
	if len(p) > remain {
		p = p[:remain]
	}
	return l.buf.Write(p)
}

func (p *PipeConn) Read(b []byte) (int, error)  { return p.stdout.Read(b) }
func (p *PipeConn) Write(b []byte) (int, error) { return p.stdin.Write(b) }

// StderrText returns captured trimmed stderr diagnostics from the SSH process.
func (p *PipeConn) StderrText() string {
	p.stderrMu.Lock()
	defer p.stderrMu.Unlock()
	return strings.TrimSpace(p.stderr.String())
}

// Close closes stdin to signal EOF to the remote sync-serve process and waits for SSH exit.
func (p *PipeConn) Close() error {
	p.closeOnce.Do(func() {
		if p.stdin != nil {
			_ = p.stdin.Close()
		}
		if p.cmd != nil {
			p.waitErr = p.cmd.Wait()
		}
		if p.stdout != nil {
			_ = p.stdout.Close()
		}
	})
	if p.waitErr != nil {
		if msg := p.StderrText(); msg != "" {
			return fmt.Errorf("ssh sync failed: %s (%w)", msg, p.waitErr)
		}
		return p.waitErr
	}
	return nil
}

// NewFrameScanner creates a buffered line scanner capable of reading large batched TX frames.
func NewFrameScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	return sc
}

// WriteSyncFrame encodes f with the SyncFramePrefix line marker onto w.
func WriteSyncFrame(w io.Writer, f domain.SyncWireFrame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s%s\n", domain.SyncFramePrefix, b)
	return err
}

// ReadSyncFrame reads from sc until a line with SyncFramePrefix is found and decodes it,
// safely ignoring stray shell/MOTD banner lines.
func ReadSyncFrame(sc *bufio.Scanner) (domain.SyncWireFrame, error) {
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, domain.SyncFramePrefix) {
			continue
		}
		raw := strings.TrimPrefix(line, domain.SyncFramePrefix)
		var f domain.SyncWireFrame
		if err := json.Unmarshal([]byte(raw), &f); err != nil {
			return domain.SyncWireFrame{}, fmt.Errorf("decode sync frame: %w", err)
		}
		return f, nil
	}
	if err := sc.Err(); err != nil {
		return domain.SyncWireFrame{}, err
	}
	return domain.SyncWireFrame{}, io.EOF
}

// FormatSSHFailure enriches stream EOF errors with captured stderr diagnostics when available.
func FormatSSHFailure(err error, stderr string) error {
	msg := strings.TrimSpace(stderr)
	if err == nil {
		return nil
	}
	if msg == "" {
		return err
	}
	if errors.Is(err, io.EOF) {
		return fmt.Errorf("ssh sync aborted: %s", msg)
	}
	return fmt.Errorf("%w: %s", err, msg)
}
