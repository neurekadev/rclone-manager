package install

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neurekadev/rclone-manager/internal/config"
)

func TestEnsureExactMatchDoesNotUseNetwork(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	binaryPath := filepath.Join(directory, "rclone")
	if err := os.WriteFile(binaryPath, []byte("current"), 0o755); err != nil {
		t.Fatal(err)
	}
	requests := 0
	installer := Installer{
		BinaryPath:   binaryPath,
		ManifestPath: filepath.Join(directory, "rclone.json"),
		Architecture: "amd64",
		Client: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			requests++
			return nil, errors.New("network must not be used")
		})},
		Versioner: versionerFunc(func(context.Context, string) (string, error) { return "1.2.3", nil }),
	}
	result, err := installer.Ensure(context.Background(), "1.2.3")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if result.Version != "1.2.3" || result.Downloaded || requests != 0 {
		t.Fatalf("Ensure() = %+v, requests = %d", result, requests)
	}
}

func TestEnsureDownloadsAndValidatesRelease(t *testing.T) {
	t.Parallel()
	const version = "1.2.3"
	archive := releaseArchive(t, version, "amd64", []byte("new"))
	digest := sha256.Sum256(archive)
	server := releaseServer(t, version, archive, "sha256:"+hex.EncodeToString(digest[:]))
	directory := t.TempDir()
	installer := Installer{
		BinaryPath:       filepath.Join(directory, "bin", "rclone"),
		ManifestPath:     filepath.Join(directory, "rclone.json"),
		APIBase:          server.URL,
		Architecture:     "amd64",
		Client:           server.Client(),
		TrustedAssetHost: trustedServerHost(server),
		Versioner: versionerFunc(func(_ context.Context, path string) (string, error) {
			data, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			if string(data) == "new" {
				return version, nil
			}
			return "", errors.New("unexpected binary")
		}),
	}
	result, err := installer.Ensure(context.Background(), version)
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if !result.Downloaded || result.Version != version {
		t.Fatalf("Ensure() = %+v", result)
	}
	data, err := os.ReadFile(installer.BinaryPath)
	if err != nil || string(data) != "new" {
		t.Fatalf("installed binary = %q, error = %v", data, err)
	}
	if !installer.validatedCache(version) {
		t.Fatal("validatedCache() = false")
	}
}

func TestEnsureRejectsBadDigestWithoutReplacingBinary(t *testing.T) {
	t.Parallel()
	const version = "1.2.3"
	archive := releaseArchive(t, version, "amd64", []byte("new"))
	server := releaseServer(t, version, archive, "sha256:"+strings.Repeat("0", 64))
	directory := t.TempDir()
	binaryPath := filepath.Join(directory, "rclone")
	if err := os.WriteFile(binaryPath, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	installer := Installer{
		BinaryPath:       binaryPath,
		ManifestPath:     filepath.Join(directory, "rclone.json"),
		APIBase:          server.URL,
		Architecture:     "amd64",
		Client:           server.Client(),
		TrustedAssetHost: trustedServerHost(server),
		Versioner: versionerFunc(func(_ context.Context, path string) (string, error) {
			data, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			if string(data) == "old" {
				return "1.0.0", nil
			}
			return version, nil
		}),
	}
	_, err := installer.Ensure(context.Background(), version)
	if err == nil || !IsPermanent(err) {
		t.Fatalf("Ensure() error = %v, want permanent error", err)
	}
	data, readErr := os.ReadFile(binaryPath)
	if readErr != nil || string(data) != "old" {
		t.Fatalf("existing binary = %q, error = %v", data, readErr)
	}
}

func TestEnsureLatestUsesValidatedCacheWhenOffline(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	binaryPath := filepath.Join(directory, "rclone")
	data := []byte("cached")
	if err := os.WriteFile(binaryPath, data, 0o755); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	manifestPath := filepath.Join(directory, "rclone.json")
	staged, err := stageManifest(manifestPath, manifest{Format: manifestVersion, Version: "1.2.3", BinarySHA256: hex.EncodeToString(digest[:])})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staged, manifestPath); err != nil {
		t.Fatal(err)
	}
	installer := Installer{
		BinaryPath:   binaryPath,
		ManifestPath: manifestPath,
		Architecture: "amd64",
		Client: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("offline")
		})},
		Versioner: versionerFunc(func(context.Context, string) (string, error) { return "1.2.3", nil }),
	}
	result, err := installer.Ensure(context.Background(), config.DefaultVersion)
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if !result.UsedCached || result.FallbackErr == nil || result.Version != "1.2.3" {
		t.Fatalf("Ensure() = %+v", result)
	}
}

func TestReleaseVersion(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"v1.2.3", "1.2.3"} {
		if got, err := releaseVersion(value); err != nil || got != "1.2.3" {
			t.Fatalf("releaseVersion(%q) = %q, %v", value, got, err)
		}
	}
	for _, value := range []string{"latest", "v1.2", "v1.2.3-beta"} {
		if _, err := releaseVersion(value); err == nil {
			t.Fatalf("releaseVersion(%q) error = nil", value)
		}
	}
}

type versionerFunc func(context.Context, string) (string, error)

func (f versionerFunc) Version(ctx context.Context, path string) (string, error) {
	return f(ctx, path)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func releaseArchive(t *testing.T, version, arch string, binary []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, err := writer.Create("rclone-v" + version + "-linux-" + arch + "/rclone")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func releaseServer(t *testing.T, version string, archive []byte, digest string) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/asset":
			response.WriteHeader(http.StatusOK)
			_, _ = response.Write(archive)
		default:
			payload := release{
				TagName: "v" + version,
				Assets: []asset{{
					Name:               "rclone-v" + version + "-linux-amd64.zip",
					State:              "uploaded",
					Size:               int64(len(archive)),
					Digest:             digest,
					BrowserDownloadURL: server.URL + "/asset",
				}},
			}
			response.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(response).Encode(payload); err != nil {
				t.Errorf("encode release: %v", err)
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func trustedServerHost(server *httptest.Server) map[string]bool {
	parsed, _ := url.Parse(server.URL)
	return map[string]bool{parsed.Hostname(): true}
}
