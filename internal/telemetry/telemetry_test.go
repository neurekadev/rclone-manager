package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
)

func TestTelemetryGateIsExact(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		enabled bool
	}{
		{name: "unset", enabled: true},
		{name: "false", value: "false", enabled: false},
		{name: "uppercase", value: "FALSE", enabled: true},
		{name: "other", value: "0", enabled: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dataDir := filepath.Join(t.TempDir(), "data")
			transport := &recordingTransport{}
			reporter, err := newReporter(testOptions(dataDir), testDependencies(transport, test.value))
			if err != nil {
				t.Fatalf("newReporter() error = %v", err)
			}
			t.Cleanup(func() { reporter.Close(false) })
			if reporter.Enabled() != test.enabled {
				t.Fatalf("Enabled() = %t, want %t", reporter.Enabled(), test.enabled)
			}
			_, statErr := os.Stat(filepath.Join(dataDir, installIDFilename))
			if test.enabled && statErr != nil {
				t.Fatalf("install ID stat error = %v", statErr)
			}
			if !test.enabled && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("disabled install ID stat error = %v, want not exist", statErr)
			}
			if !test.enabled && transport.configured {
				t.Fatal("disabled reporter initialized Sentry")
			}
		})
	}
}

func TestInstallIDPersists(t *testing.T) {
	dataDir := t.TempDir()
	first, err := newReporter(testOptions(dataDir), testDependencies(&recordingTransport{}, "true"))
	if err != nil {
		t.Fatalf("first newReporter() error = %v", err)
	}
	firstID := first.installID
	first.Close(false)

	second, err := newReporter(testOptions(dataDir), testDependencies(&recordingTransport{}, "true"))
	if err != nil {
		t.Fatalf("second newReporter() error = %v", err)
	}
	t.Cleanup(func() { second.Close(false) })
	if second.installID != firstID {
		t.Fatalf("second install ID = %q, want %q", second.installID, firstID)
	}
	if !validInstallID(firstID) {
		t.Fatalf("install ID %q is not a random UUID", firstID)
	}
	info, err := os.Stat(filepath.Join(dataDir, installIDFilename))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("install ID permissions = %#o, want 0600", info.Mode().Perm())
	}
}

func TestErrorReportingConfigurationAndCapture(t *testing.T) {
	transport := &recordingTransport{}
	reporter, err := newReporter(testOptions(t.TempDir()), testDependencies(transport, "true"))
	if err != nil {
		t.Fatalf("newReporter() error = %v", err)
	}
	t.Cleanup(func() { reporter.Close(false) })

	if !transport.options.DisableClientReports || !transport.options.DisableTelemetryBuffer {
		t.Fatalf("unsupported Sentry features remain enabled: %+v", transport.options)
	}
	if transport.options.EnableTracing || transport.options.TracesSampleRate != 0 {
		t.Fatalf("tracing enabled: %+v", transport.options)
	}
	dataCollection := transport.options.DataCollection
	if dataCollection == nil || dataCollection.UserInfo.Value ||
		dataCollection.Cookies.Mode != sentry.CollectionOff ||
		dataCollection.HTTPHeaders.Request.Mode != sentry.CollectionOff ||
		dataCollection.HTTPHeaders.Response.Mode != sentry.CollectionOff ||
		dataCollection.QueryParams.Mode != sentry.CollectionOff ||
		len(dataCollection.HTTPBodies) != 0 {
		t.Fatalf("automatic Sentry data collection enabled: %+v", dataCollection)
	}
	if transport.options.BeforeSendTransaction(nil, nil) != nil ||
		transport.options.BeforeSendLog(nil) != nil ||
		transport.options.BeforeSendMetric(nil) != nil {
		t.Fatal("unsupported Sentry event filters do not drop their events")
	}
	if transport.options.Release != "1.2.3" || transport.options.Environment != "production" {
		t.Fatalf("release/environment = %q/%q", transport.options.Release, transport.options.Environment)
	}
	if transport.options.Dsn != "https://5cfb545465a6dfcc24aad52e44803280@beacon.neureka.dev/api/v1/sentry/64846939127031436" {
		t.Fatalf("Sentry DSN = %q", transport.options.Dsn)
	}

	if !reporter.CaptureException(errors.New("handled verification error")) {
		t.Fatal("CaptureException() = false")
	}
	if !reporter.Recover(errors.New("unhandled verification panic")) {
		t.Fatal("Recover() = false")
	}
	if !reporter.FlushErrors() {
		t.Fatal("FlushErrors() = false")
	}
	events := transport.Events()
	if len(events) != 2 {
		t.Fatalf("captured events = %d, want 2", len(events))
	}
	for _, event := range events {
		if event.User.ID != reporter.installID {
			t.Fatalf("event user ID = %q, want %q", event.User.ID, reporter.installID)
		}
		if event.Release != "1.2.3" || event.Environment != "production" {
			t.Fatalf("event release/environment = %q/%q", event.Release, event.Environment)
		}
		if event.Type != "" {
			t.Fatalf("event type = %q, want error event", event.Type)
		}
	}
}

func TestAnalyticsLifecycleContract(t *testing.T) {
	requests := make(chan receivedAnalytics, 3)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var payload map[string]json.RawMessage
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decode envelope: %v", err)
		}
		var envelope analyticsEnvelope
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Errorf("decode analytics envelope: %v", err)
		}
		requests <- receivedAnalytics{
			authorization: request.Header.Get("Authorization"),
			contentType:   request.Header.Get("Content-Type"),
			fields:        payload,
			event:         envelope.Events[0],
		}
		response.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	deps := testDependencies(&recordingTransport{}, "true")
	deps.analyticsEndpoint = server.URL
	deps.analyticsAPIKey = "test-public-key"
	deps.analyticsClient = server.Client()
	deps.heartbeatInterval = time.Hour
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	deps.now = func() time.Time { return now }
	reporter, err := newReporter(testOptions(t.TempDir()), deps)
	if err != nil {
		t.Fatalf("newReporter() error = %v", err)
	}
	reporter.Start()
	started := awaitAnalytics(t, requests)
	reporter.Close(true)
	exited := awaitAnalytics(t, requests)

	for _, received := range []receivedAnalytics{started, exited} {
		if received.authorization != "Bearer test-public-key" {
			t.Fatalf("Authorization = %q", received.authorization)
		}
		if received.contentType != "application/json" {
			t.Fatalf("Content-Type = %q", received.contentType)
		}
		if len(received.fields) != 1 || received.fields["events"] == nil {
			t.Fatalf("top-level fields = %v, want only events", received.fields)
		}
		if received.event.InstallID != reporter.installID || received.event.Timestamp != "2026-01-01T00:00:00Z" {
			t.Fatalf("event identity/time = %+v", received.event)
		}
		if received.event.Release != "1.2.3" || received.event.Platform != "linux-amd64" {
			t.Fatalf("event release/platform = %+v", received.event)
		}
	}
	if started.event.Event != "app_started" || exited.event.Event != "app_exited" {
		t.Fatalf("lifecycle events = %q, %q", started.event.Event, exited.event.Event)
	}
}

func TestAnalyticsHeartbeat(t *testing.T) {
	requests := make(chan receivedAnalytics, 3)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var envelope analyticsEnvelope
		if err := json.NewDecoder(request.Body).Decode(&envelope); err != nil {
			t.Errorf("decode envelope: %v", err)
		}
		requests <- receivedAnalytics{event: envelope.Events[0]}
		response.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	deps := testDependencies(&recordingTransport{}, "true")
	deps.analyticsEndpoint = server.URL
	deps.analyticsClient = server.Client()
	deps.heartbeatInterval = time.Millisecond
	reporter, err := newReporter(testOptions(t.TempDir()), deps)
	if err != nil {
		t.Fatalf("newReporter() error = %v", err)
	}
	reporter.Start()
	if event := awaitAnalytics(t, requests).event.Event; event != "app_started" {
		t.Fatalf("first event = %q, want app_started", event)
	}
	for {
		if event := awaitAnalytics(t, requests).event.Event; event == "heartbeat" {
			break
		}
	}
	reporter.Close(false)
}

func TestAnalyticsFailureIsBestEffort(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	deps := testDependencies(&recordingTransport{}, "true")
	deps.analyticsEndpoint = server.URL
	deps.analyticsClient = server.Client()
	reporter, err := newReporter(testOptions(t.TempDir()), deps)
	if err != nil {
		t.Fatalf("newReporter() error = %v", err)
	}
	reporter.Start()
	reporter.Close(true)
}

func testOptions(dataDir string) Options {
	return Options{
		DataDir:     dataDir,
		Release:     "1.2.3",
		Environment: "production",
		Platform:    "linux-amd64",
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func testDependencies(transport sentry.Transport, telemetryValue string) dependencies {
	return dependencies{
		getenv:            func(string) string { return telemetryValue },
		sentryTransport:   transport,
		now:               time.Now,
		heartbeatInterval: time.Hour,
		deliveryTimeout:   time.Second,
	}
}

func awaitAnalytics(t *testing.T, events <-chan receivedAnalytics) receivedAnalytics {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for analytics event")
		return receivedAnalytics{}
	}
}

type receivedAnalytics struct {
	authorization string
	contentType   string
	fields        map[string]json.RawMessage
	event         analyticsEvent
}

type recordingTransport struct {
	mu         sync.Mutex
	configured bool
	options    sentry.ClientOptions
	events     []*sentry.Event
}

func (t *recordingTransport) Configure(options sentry.ClientOptions) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.configured = true
	t.options = options
}

func (t *recordingTransport) SendEvent(event *sentry.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, event)
}

func (t *recordingTransport) Flush(time.Duration) bool { return true }

func (t *recordingTransport) FlushWithContext(context.Context) bool { return true }

func (t *recordingTransport) Close() {}

func (t *recordingTransport) Events() []*sentry.Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]*sentry.Event(nil), t.events...)
}
