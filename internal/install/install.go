// Package install obtains and verifies official rclone release binaries.
package install

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/neurekadev/rclone-manager/internal/config"
)

const (
	defaultAPIBase  = "https://api.github.com"
	maxReleaseJSON  = 4 << 20
	maxArchiveSize  = 256 << 20
	maxBinarySize   = 256 << 20
	versionTimeout  = 5 * time.Second
	manifestVersion = 1
)

// Versioner reports the version of an rclone executable.
type Versioner interface {
	Version(ctx context.Context, path string) (string, error)
}

// Result describes the selected runtime binary.
type Result struct {
	Version     string
	Downloaded  bool
	UsedCached  bool
	FallbackErr error
}

// Installer resolves and atomically installs rclone releases.
type Installer struct {
	BinaryPath       string
	ManifestPath     string
	APIBase          string
	Architecture     string
	Client           *http.Client
	Versioner        Versioner
	TrustedAssetHost map[string]bool
}

type release struct {
	TagName    string  `json:"tag_name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []asset `json:"assets"`
}

type asset struct {
	Name               string `json:"name"`
	State              string `json:"state"`
	Size               int64  `json:"size"`
	Digest             string `json:"digest"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type manifest struct {
	Format       int    `json:"format"`
	Version      string `json:"version"`
	BinarySHA256 string `json:"binary_sha256"`
}

// PermanentError identifies invalid selectors or unusable release metadata that
// retrying without an operator change will not fix.
type PermanentError struct {
	Err error
}

// Error implements error.
func (e *PermanentError) Error() string { return e.Err.Error() }

// Unwrap exposes the underlying failure.
func (e *PermanentError) Unwrap() error { return e.Err }

// IsPermanent reports whether retrying the operation is inappropriate.
func IsPermanent(err error) bool {
	var permanent *PermanentError
	return errors.As(err, &permanent)
}

// Ensure returns an existing matching binary or obtains the selected release.
func (i Installer) Ensure(ctx context.Context, selector string) (Result, error) {
	if i.BinaryPath == "" || i.ManifestPath == "" {
		return Result{}, &PermanentError{Err: errors.New("installer paths are required")}
	}
	arch, err := i.architecture()
	if err != nil {
		return Result{}, err
	}

	current, currentErr := i.currentVersion(ctx, i.BinaryPath)
	if selector != config.DefaultVersion && currentErr == nil && current == selector {
		return Result{Version: current}, nil
	}
	validated := currentErr == nil && i.validatedCache(current)

	rel, err := i.fetchRelease(ctx, selector)
	if err != nil {
		if selector == config.DefaultVersion && validated {
			return Result{Version: current, UsedCached: true, FallbackErr: err}, nil
		}
		return Result{}, err
	}

	version, err := releaseVersion(rel.TagName)
	if err != nil {
		return Result{}, &PermanentError{Err: err}
	}
	if selector != config.DefaultVersion && version != selector {
		return Result{}, &PermanentError{Err: fmt.Errorf("release tag %q does not match requested version %q", rel.TagName, selector)}
	}
	if currentErr == nil && current == version {
		return Result{Version: current}, nil
	}

	selected, err := selectAsset(rel, version, arch)
	if err != nil {
		if selector == config.DefaultVersion && validated {
			return Result{Version: current, UsedCached: true, FallbackErr: err}, nil
		}
		return Result{}, err
	}
	if err := i.installAsset(ctx, selected, version, arch); err != nil {
		if selector == config.DefaultVersion && validated {
			return Result{Version: current, UsedCached: true, FallbackErr: err}, nil
		}
		return Result{}, err
	}
	return Result{Version: version, Downloaded: true}, nil
}

// ExecVersioner invokes the candidate with a clean rclone environment.
type ExecVersioner struct {
	Environ []string
}

// Version returns the stable version printed by `rclone version`.
func (v ExecVersioner) Version(ctx context.Context, path string) (string, error) {
	cmd := exec.CommandContext(ctx, path, "version")
	environ := v.Environ
	if environ == nil {
		environ = os.Environ()
	}
	cmd.Env = config.SanitizedEnvironment(environ)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("run rclone version: %w", err)
	}
	line, _, _ := strings.Cut(string(output), "\n")
	const prefix = "rclone v"
	if !strings.HasPrefix(line, prefix) {
		return "", fmt.Errorf("parse rclone version output %q", line)
	}
	return releaseVersion(strings.TrimPrefix(line, "rclone "))
}

func (i Installer) currentVersion(ctx context.Context, path string) (string, error) {
	versioner := i.Versioner
	if versioner == nil {
		versioner = ExecVersioner{}
	}
	probeCtx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	return versioner.Version(probeCtx, path)
}

func (i Installer) fetchRelease(ctx context.Context, selector string) (release, error) {
	base := strings.TrimRight(i.APIBase, "/")
	if base == "" {
		base = defaultAPIBase
	}
	endpoint := base + "/repos/rclone/rclone/releases/latest"
	if selector != config.DefaultVersion {
		endpoint = base + "/repos/rclone/rclone/releases/tags/" + url.PathEscape("v"+selector)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return release{}, fmt.Errorf("create GitHub release request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "rclone-manager")

	response, err := i.client().Do(request)
	if err != nil {
		return release{}, fmt.Errorf("fetch GitHub release: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		err = fmt.Errorf("fetch GitHub release: unexpected HTTP status %s", response.Status)
		if response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusTooManyRequests {
			return release{}, &PermanentError{Err: err}
		}
		return release{}, err
	}

	var rel release
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxReleaseJSON))
	if err := decoder.Decode(&rel); err != nil {
		return release{}, &PermanentError{Err: fmt.Errorf("decode GitHub release: %w", err)}
	}
	if rel.Draft || rel.Prerelease {
		return release{}, &PermanentError{Err: fmt.Errorf("release %q is not a stable published release", rel.TagName)}
	}
	return rel, nil
}

func selectAsset(rel release, version, arch string) (asset, error) {
	expected := fmt.Sprintf("rclone-v%s-linux-%s.zip", version, arch)
	for _, candidate := range rel.Assets {
		if candidate.Name != expected {
			continue
		}
		if candidate.State != "uploaded" || candidate.Size <= 0 || candidate.Size > maxArchiveSize {
			return asset{}, &PermanentError{Err: fmt.Errorf("release asset %s has invalid state or size", expected)}
		}
		if _, err := digest(candidate.Digest); err != nil {
			return asset{}, &PermanentError{Err: fmt.Errorf("release asset %s: %w", expected, err)}
		}
		return candidate, nil
	}
	return asset{}, &PermanentError{Err: fmt.Errorf("release does not contain %s", expected)}
}

func (i Installer) installAsset(ctx context.Context, selected asset, version, arch string) error {
	downloadURL, err := url.Parse(selected.BrowserDownloadURL)
	if err != nil || downloadURL.Scheme != "https" || !i.trustedHost(downloadURL.Hostname()) {
		return &PermanentError{Err: fmt.Errorf("release asset has untrusted download URL %q", selected.BrowserDownloadURL)}
	}
	expectedDigest, _ := digest(selected.Digest)

	directory := filepath.Dir(i.BinaryPath)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create rclone binary directory: %w", err)
	}
	archive, err := os.CreateTemp(directory, ".rclone-*.zip")
	if err != nil {
		return fmt.Errorf("create rclone archive: %w", err)
	}
	archivePath := archive.Name()
	defer func() { _ = os.Remove(archivePath) }()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, selected.BrowserDownloadURL, nil)
	if err != nil {
		_ = archive.Close()
		return fmt.Errorf("create rclone download request: %w", err)
	}
	request.Header.Set("User-Agent", "rclone-manager")
	response, err := i.client().Do(request)
	if err != nil {
		_ = archive.Close()
		return fmt.Errorf("download rclone release: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		_ = archive.Close()
		return fmt.Errorf("download rclone release: unexpected HTTP status %s", response.Status)
	}

	archiveHash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(archive, archiveHash), io.LimitReader(response.Body, maxArchiveSize+1))
	if copyErr != nil {
		_ = archive.Close()
		return fmt.Errorf("download rclone release: %w", copyErr)
	}
	if written > maxArchiveSize {
		_ = archive.Close()
		return &PermanentError{Err: errors.New("rclone release archive exceeds size limit")}
	}
	if err := archive.Sync(); err != nil {
		_ = archive.Close()
		return fmt.Errorf("sync rclone release archive: %w", err)
	}
	if err := archive.Close(); err != nil {
		return fmt.Errorf("close rclone release archive: %w", err)
	}
	if !equalHash(archiveHash, expectedDigest) {
		return &PermanentError{Err: errors.New("rclone release archive SHA-256 does not match GitHub digest")}
	}

	binaryHash, stagedPath, err := extractBinary(archivePath, directory, version, arch)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(stagedPath) }()
	stagedVersion, err := i.currentVersion(ctx, stagedPath)
	if err != nil {
		return &PermanentError{Err: fmt.Errorf("validate downloaded rclone: %w", err)}
	}
	if stagedVersion != version {
		return &PermanentError{Err: fmt.Errorf("downloaded rclone reports version %q, expected %q", stagedVersion, version)}
	}

	manifestPath, err := stageManifest(i.ManifestPath, manifest{
		Format:       manifestVersion,
		Version:      version,
		BinarySHA256: binaryHash,
	})
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(manifestPath) }()
	if err := os.Rename(stagedPath, i.BinaryPath); err != nil {
		return fmt.Errorf("install rclone binary: %w", err)
	}
	if err := os.Rename(manifestPath, i.ManifestPath); err != nil {
		return fmt.Errorf("install rclone manifest: %w", err)
	}
	return nil
}

func extractBinary(archivePath, directory, version, arch string) (string, string, error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", "", &PermanentError{Err: fmt.Errorf("open rclone release archive: %w", err)}
	}
	defer func() { _ = reader.Close() }()

	expected := fmt.Sprintf("rclone-v%s-linux-%s/rclone", version, arch)
	var entry *zip.File
	for _, candidate := range reader.File {
		if candidate.Name == expected {
			entry = candidate
			break
		}
	}
	if entry == nil || entry.FileInfo().IsDir() || entry.UncompressedSize64 > maxBinarySize {
		return "", "", &PermanentError{Err: fmt.Errorf("archive does not contain a valid %s", expected)}
	}

	source, err := entry.Open()
	if err != nil {
		return "", "", &PermanentError{Err: fmt.Errorf("open rclone binary in archive: %w", err)}
	}
	defer func() { _ = source.Close() }()
	target, err := os.CreateTemp(directory, ".rclone-*")
	if err != nil {
		return "", "", fmt.Errorf("create staged rclone binary: %w", err)
	}
	targetPath := target.Name()
	cleanup := func() {
		_ = target.Close()
		_ = os.Remove(targetPath)
	}

	binaryHash := sha256.New()
	written, err := io.Copy(io.MultiWriter(target, binaryHash), io.LimitReader(source, maxBinarySize+1))
	if err != nil || written > maxBinarySize {
		cleanup()
		if err != nil {
			return "", "", &PermanentError{Err: fmt.Errorf("extract rclone binary: %w", err)}
		}
		return "", "", &PermanentError{Err: errors.New("rclone binary exceeds size limit")}
	}
	if err := target.Chmod(0o755); err != nil {
		cleanup()
		return "", "", fmt.Errorf("make rclone executable: %w", err)
	}
	if err := target.Sync(); err != nil {
		cleanup()
		return "", "", fmt.Errorf("sync rclone binary: %w", err)
	}
	if err := target.Close(); err != nil {
		_ = os.Remove(targetPath)
		return "", "", fmt.Errorf("close rclone binary: %w", err)
	}
	return hex.EncodeToString(binaryHash.Sum(nil)), targetPath, nil
}

func stageManifest(path string, value manifest) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create manifest directory: %w", err)
	}
	target, err := os.CreateTemp(filepath.Dir(path), ".rclone-manifest-*")
	if err != nil {
		return "", fmt.Errorf("create rclone manifest: %w", err)
	}
	targetPath := target.Name()
	encoder := json.NewEncoder(target)
	if err := encoder.Encode(value); err != nil {
		_ = target.Close()
		_ = os.Remove(targetPath)
		return "", fmt.Errorf("encode rclone manifest: %w", err)
	}
	if err := target.Chmod(0o644); err != nil {
		_ = target.Close()
		_ = os.Remove(targetPath)
		return "", fmt.Errorf("set rclone manifest permissions: %w", err)
	}
	if err := target.Sync(); err != nil {
		_ = target.Close()
		_ = os.Remove(targetPath)
		return "", fmt.Errorf("sync rclone manifest: %w", err)
	}
	if err := target.Close(); err != nil {
		_ = os.Remove(targetPath)
		return "", fmt.Errorf("close rclone manifest: %w", err)
	}
	return targetPath, nil
}

func (i Installer) validatedCache(version string) bool {
	data, err := os.ReadFile(i.ManifestPath)
	if err != nil || len(data) > 64*1024 {
		return false
	}
	var saved manifest
	if json.Unmarshal(data, &saved) != nil || saved.Format != manifestVersion || saved.Version != version {
		return false
	}
	expected, err := hex.DecodeString(saved.BinarySHA256)
	if err != nil || len(expected) != sha256.Size {
		return false
	}
	file, err := os.Open(i.BinaryPath)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	actual := sha256.New()
	if _, err := io.Copy(actual, file); err != nil {
		return false
	}
	return equalHash(actual, expected)
}

func (i Installer) architecture() (string, error) {
	arch := i.Architecture
	if arch == "" {
		arch = runtime.GOARCH
	}
	if arch != "amd64" && arch != "arm64" {
		return "", &PermanentError{Err: fmt.Errorf("unsupported Linux architecture %q", arch)}
	}
	return arch, nil
}

func (i Installer) client() *http.Client {
	if i.Client != nil {
		return i.Client
	}
	return &http.Client{Timeout: 2 * time.Minute}
}

func (i Installer) trustedHost(host string) bool {
	if i.TrustedAssetHost != nil {
		return i.TrustedAssetHost[host]
	}
	return host == "github.com"
}

func releaseVersion(tag string) (string, error) {
	version := strings.TrimPrefix(strings.TrimSpace(tag), "v")
	normalized, err := config.NormalizeVersion(version)
	if err != nil || normalized == config.DefaultVersion {
		return "", fmt.Errorf("invalid stable rclone release tag %q", tag)
	}
	return normalized, nil
}

func digest(value string) ([]byte, error) {
	algorithm, encoded, ok := strings.Cut(value, ":")
	if !ok || algorithm != "sha256" {
		return nil, errors.New("GitHub asset digest is not SHA-256")
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != sha256.Size {
		return nil, errors.New("GitHub asset digest is malformed")
	}
	return decoded, nil
}

func equalHash(actual hash.Hash, expected []byte) bool {
	return subtle.ConstantTimeCompare(actual.Sum(nil), expected) == 1
}
