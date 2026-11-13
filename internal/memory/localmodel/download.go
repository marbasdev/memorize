package localmodel

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

func (m *Manager) CancelDownload() {
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *Manager) setProgress(fn func(*DownloadJob)) {
	m.mu.Lock()
	fn(&m.job)
	m.mu.Unlock()
}

func (m *Manager) finishJob(err error) {
	m.setProgress(func(j *DownloadJob) {
		j.Active = false
		if err != nil {
			j.Phase = "error"
			j.Error = err.Error()
		} else {
			j.Phase = "done"
			j.Percent = 100
			j.Error = ""
		}
	})
}

func validateDownloadURL(rawURL string) (string, string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", "", fmt.Errorf("invalid URL")
	}
	host := strings.ToLower(u.Hostname())
	isLocal := host == "127.0.0.1" || host == "localhost"
	if u.Scheme != "https" && !(u.Scheme == "http" && isLocal) {
		return "", "", fmt.Errorf("only https:// model URLs are permitted")
	}
	fname := path.Base(u.Path)
	if !safeFileRe.MatchString(fname) {
		return "", "", fmt.Errorf("URL path must end with a valid .gguf filename (got %q)", fname)
	}
	return u.String(), fname, nil
}

func (m *Manager) StartModelDownload(presetID, customURL string) (string, error) {
	if !IsSupportedPlatform() {
		return "", unsupportedPlatformErr()
	}
	dlURL := strings.TrimSpace(customURL)
	var fname string
	if presetID != "" {
		for _, p := range Presets() {
			if p.ID == presetID {
				dlURL = p.URL
				fname = p.FileName
				break
			}
		}
		if dlURL == "" {
			return "", fmt.Errorf("unknown preset %q", presetID)
		}
	} else {
		var err error
		dlURL, fname, err = validateDownloadURL(dlURL)
		if err != nil {
			return "", err
		}
	}

	m.mu.Lock()
	if m.job.Active {
		m.mu.Unlock()
		return "", fmt.Errorf("another download is already in progress (%s)", m.job.Target)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.job = DownloadJob{Active: true, Kind: "model", Target: fname, Phase: "connecting"}
	m.mu.Unlock()

	go func() {
		defer cancel()
		m.finishJob(m.downloadFileWithProgress(ctx, dlURL, filepath.Join(m.dir, fname)))
	}()
	return fname, nil
}

func (m *Manager) StartRuntimeInstall() error {
	if !IsSupportedPlatform() {
		return unsupportedPlatformErr()
	}
	m.mu.Lock()
	if m.job.Active {
		m.mu.Unlock()
		return fmt.Errorf("another download is already in progress (%s)", m.job.Target)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.job = DownloadJob{Active: true, Kind: "runtime", Target: "llama-server", Phase: "fetching release metadata"}
	m.mu.Unlock()

	go func() {
		defer cancel()
		m.finishJob(m.InstallRuntimeSync(ctx))
	}()
	return nil
}

func platformAssetSuffix() (string, error) {
	if !IsSupportedPlatform() {
		return "", unsupportedPlatformErr()
	}
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return "bin-macos-arm64.tar.gz", nil
	case "darwin/amd64":
		return "bin-macos-x64.tar.gz", nil
	case "linux/arm64":
		return "bin-ubuntu-arm64.tar.gz", nil
	case "linux/amd64":
		return "bin-ubuntu-x64.tar.gz", nil
	default:
		return "", fmt.Errorf("unsupported auto-install platform %s/%s (install llama-server manually)", runtime.GOOS, runtime.GOARCH)
	}
}

func (m *Manager) InstallRuntimeSync(ctx context.Context) error {
	suffix, err := platformAssetSuffix()
	if err != nil {
		return err
	}
	apiURL := strings.TrimSpace(os.Getenv("LLAMA_RELEASE_API_URL"))
	if apiURL == "" {
		apiURL = "https://api.github.com/repos/ggml-org/llama.cpp/releases?per_page=5"
	} else {
		u, uerr := url.Parse(apiURL)
		if uerr != nil || !isAllowedDownloadScheme(u) {
			return fmt.Errorf("invalid LLAMA_RELEASE_API_URL scheme")
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "memorize-localmodel")
	resp, err := apiClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("github release lookup status %d", resp.StatusCode)
	}
	var releases []struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&releases); err != nil {
		return err
	}
	var assetURL string
	for _, rel := range releases {
		for _, a := range rel.Assets {
			if strings.HasSuffix(a.Name, suffix) && !strings.HasPrefix(a.Name, "cudart-") {
				if au, aerr := url.Parse(strings.TrimSpace(a.URL)); aerr == nil && isAllowedDownloadScheme(au) {
					assetURL = au.String()
					break
				}
			}
		}
		if assetURL != "" {
			break
		}
	}
	if assetURL == "" {
		return fmt.Errorf("no release asset matching %s found", suffix)
	}

	tmpArchive := filepath.Join(m.RuntimeDir(), "runtime.tar.gz")
	defer os.Remove(tmpArchive)
	if err := m.downloadFileWithProgress(ctx, assetURL, tmpArchive); err != nil {
		return err
	}
	m.setProgress(func(j *DownloadJob) { j.Phase = "extracting runtime" })
	return extractTarGzFlat(tmpArchive, m.RuntimeDir())
}

func (m *Manager) downloadFileWithProgress(ctx context.Context, dlURL, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "memorize-localmodel")
	resp, err := downloadClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("download status %d", resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return err
	}
	total := resp.ContentLength
	partPath := destPath + ".part"
	_ = os.Remove(partPath)
	out, err := os.Create(partPath)
	if err != nil {
		return err
	}

	m.setProgress(func(j *DownloadJob) {
		j.Phase = "downloading"
		j.TotalBytes = total
		j.DownloadedBytes = 0
		j.Percent = 0
	})

	buf := make([]byte, 256*1024)
	var written int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				_ = out.Close()
				_ = os.Remove(partPath)
				return werr
			}
			written += int64(n)
			m.setProgress(func(j *DownloadJob) {
				j.DownloadedBytes = written
				if total > 0 {
					j.Percent = float64(written) * 100.0 / float64(total)
				}
			})
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_ = out.Close()
			_ = os.Remove(partPath)
			return rerr
		}
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(partPath)
		return err
	}
	return os.Rename(partPath, destPath)
}

func extractTarGzFlat(tarPath, destDir string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	const (
		maxTarEntryBytes = 512 << 20
		maxTarTotalBytes = 2 << 30
	)
	var totalWritten int64
	cleanDest := filepath.Clean(destDir) + string(os.PathSeparator)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		rel := strings.TrimLeft(filepath.ToSlash(hdr.Name), "/")
		if idx := strings.IndexByte(rel, '/'); idx >= 0 {
			rel = rel[idx+1:]
		}
		if rel == "" || strings.Contains(rel, "..") {
			continue
		}
		target := filepath.Join(destDir, filepath.FromSlash(rel))
		if !strings.HasPrefix(filepath.Clean(target)+string(os.PathSeparator), cleanDest) && filepath.Clean(target) != filepath.Clean(destDir) {
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			_ = os.MkdirAll(target, 0o755)
		case tar.TypeReg:
			if hdr.Size < 0 || hdr.Size > maxTarEntryBytes || totalWritten+hdr.Size > maxTarTotalBytes {
				return fmt.Errorf("tar entry %q exceeds safe size limit", rel)
			}
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			_ = os.Remove(target)
			mode := os.FileMode(hdr.Mode) & 0o777
			if mode == 0 || strings.HasSuffix(target, "llama-server") || strings.Contains(target, ".dylib") || strings.Contains(target, ".so") {
				mode = 0o755
			}
			wf, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			n, cerr := io.Copy(wf, io.LimitReader(tr, maxTarEntryBytes+1))
			_ = wf.Close()
			if cerr != nil {
				_ = os.Remove(target)
				return cerr
			}
			if n > maxTarEntryBytes || totalWritten+n > maxTarTotalBytes {
				_ = os.Remove(target)
				return fmt.Errorf("tar archive exceeds safe extracted size limit")
			}
			totalWritten += n
		case tar.TypeSymlink:
			link := strings.TrimSpace(hdr.Linkname)
			if link == "" || filepath.IsAbs(link) || strings.HasPrefix(link, "/") || strings.Contains(link, "..") {
				continue
			}
			resolvedLink := filepath.Clean(filepath.Join(filepath.Dir(target), filepath.FromSlash(link)))
			if !strings.HasPrefix(resolvedLink+string(os.PathSeparator), cleanDest) {
				continue
			}
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			_ = os.Remove(target)
			if err := os.Symlink(link, target); err != nil {
				return err
			}
		}
	}
	return nil
}
