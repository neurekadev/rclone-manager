// Package telemetry reports anonymous application errors and lifecycle events.
package telemetry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/getsentry/sentry-go"
)

const (
	analyticsEndpoint = "https://beacon.neureka.dev/api/v1/analytics/events"
	analyticsAPIKey   = "bcn_810265d9_703358389f07be13f00b75042b1f4c13"
	installIDFilename = "install-id"
	heartbeatInterval = 15 * time.Minute
	deliveryTimeout   = 3 * time.Second
)

// Options identifies this application build and deployment to Beacon.
type Options struct {
	DataDir      string
	Release      string
	Environment  string
	Platform     string
	Logger       *slog.Logger
	Verification bool
}

// Reporter owns Beacon error reporting and lifecycle delivery.
type Reporter struct {
	enabled           bool
	installID         string
	release           string
	platform          string
	logger            *slog.Logger
	verification      bool
	analyticsEndpoint string
	analyticsAPIKey   string
	analyticsClient   *http.Client
	analyticsEvents   chan string
	analyticsDone     chan struct{}
	now               func() time.Time
	heartbeatInterval time.Duration
	deliveryTimeout   time.Duration
	sentryClient      *sentry.Client
	sentryHub         *sentry.Hub

	mu              sync.Mutex
	started         bool
	closed          bool
	heartbeatCancel context.CancelFunc
	heartbeatDone   chan struct{}
	closeOnce       sync.Once
}

type dependencies struct {
	getenv            func(string) string
	analyticsEndpoint string
	analyticsAPIKey   string
	analyticsClient   *http.Client
	sentryTransport   sentry.Transport
	now               func() time.Time
	heartbeatInterval time.Duration
	deliveryTimeout   time.Duration
}

type analyticsEnvelope struct {
	Events []analyticsEvent `json:"events"`
}

type analyticsEvent struct {
	Event     string `json:"event"`
	InstallID string `json:"installId"`
	Timestamp string `json:"timestamp,omitempty"`
	Release   string `json:"release,omitempty"`
	Platform  string `json:"platform,omitempty"`
}

// New initializes Beacon telemetry unless TELEMETRY is exactly "false".
func New(options Options) (*Reporter, error) {
	return newReporter(options, dependencies{
		getenv:            os.Getenv,
		analyticsEndpoint: analyticsEndpoint,
		analyticsAPIKey:   analyticsAPIKey,
		now:               time.Now,
		heartbeatInterval: heartbeatInterval,
		deliveryTimeout:   deliveryTimeout,
	})
}

func newReporter(options Options, deps dependencies) (*Reporter, error) {
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	reporter := &Reporter{logger: logger}
	if deps.getenv == nil {
		deps.getenv = os.Getenv
	}
	if deps.getenv("TELEMETRY") == "false" {
		return reporter, nil
	}
	if strings.TrimSpace(options.DataDir) == "" {
		return reporter, errors.New("telemetry data directory is required")
	}
	if strings.TrimSpace(options.Release) == "" {
		return reporter, errors.New("telemetry release is required")
	}
	if strings.TrimSpace(options.Environment) == "" {
		return reporter, errors.New("telemetry environment is required")
	}
	if strings.TrimSpace(options.Platform) == "" {
		return reporter, errors.New("telemetry platform is required")
	}

	installID, err := loadOrCreateInstallID(options.DataDir)
	if err != nil {
		return reporter, fmt.Errorf("prepare telemetry install ID: %w", err)
	}
	if deps.analyticsEndpoint == "" {
		deps.analyticsEndpoint = analyticsEndpoint
	}
	if deps.analyticsAPIKey == "" {
		deps.analyticsAPIKey = analyticsAPIKey
	}
	if deps.now == nil {
		deps.now = time.Now
	}
	if deps.heartbeatInterval <= 0 {
		deps.heartbeatInterval = heartbeatInterval
	}
	if deps.deliveryTimeout <= 0 {
		deps.deliveryTimeout = deliveryTimeout
	}
	if deps.analyticsClient == nil {
		deps.analyticsClient = &http.Client{Timeout: deps.deliveryTimeout}
	}

	sentryOptions := sentry.ClientOptions{
		Dsn:                    "https://5cfb545465a6dfcc24aad52e44803280@beacon.neureka.dev/api/v1/sentry/64846939127031436",
		Release:                options.Release,
		Environment:            options.Environment,
		EnableTracing:          false,
		TracesSampleRate:       0,
		DisableClientReports:   true,
		DisableTelemetryBuffer: true,
		DataCollection: &sentry.DataCollection{
			UserInfo:   sentry.Set(false),
			Cookies:    &sentry.KeyValueCollectionBehavior{Mode: sentry.CollectionOff},
			HTTPBodies: []sentry.BodyType{},
			HTTPHeaders: &sentry.HeaderCollectionConfig{
				Request:  &sentry.KeyValueCollectionBehavior{Mode: sentry.CollectionOff},
				Response: &sentry.KeyValueCollectionBehavior{Mode: sentry.CollectionOff},
			},
			QueryParams: &sentry.KeyValueCollectionBehavior{Mode: sentry.CollectionOff},
		},
		BeforeSendTransaction: func(*sentry.Event, *sentry.EventHint) *sentry.Event {
			return nil
		},
		BeforeSendLog: func(*sentry.Log) *sentry.Log {
			return nil
		},
		BeforeSendMetric: func(*sentry.Metric) *sentry.Metric {
			return nil
		},
	}
	if deps.sentryTransport != nil {
		sentryOptions.Transport = deps.sentryTransport
	} else {
		sentryOptions.HTTPClient = &http.Client{
			Timeout: deps.deliveryTimeout,
			Transport: statusRoundTripper{
				base:         http.DefaultTransport,
				logger:       logger,
				verification: options.Verification,
				kind:         "error event",
			},
		}
	}
	sentryClient, err := sentry.NewClient(sentryOptions)
	if err != nil {
		return reporter, fmt.Errorf("initialize Beacon error reporting: %w", err)
	}
	sentryHub := sentry.NewHub(sentryClient, sentry.NewScope())
	sentryHub.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetUser(sentry.User{ID: installID})
	})

	reporter.enabled = true
	reporter.installID = installID
	reporter.release = options.Release
	reporter.platform = options.Platform
	reporter.verification = options.Verification
	reporter.analyticsEndpoint = deps.analyticsEndpoint
	reporter.analyticsAPIKey = deps.analyticsAPIKey
	reporter.analyticsClient = deps.analyticsClient
	reporter.analyticsEvents = make(chan string, 4)
	reporter.analyticsDone = make(chan struct{})
	reporter.now = deps.now
	reporter.heartbeatInterval = deps.heartbeatInterval
	reporter.deliveryTimeout = deps.deliveryTimeout
	reporter.sentryClient = sentryClient
	reporter.sentryHub = sentryHub
	go reporter.runDelivery()
	return reporter, nil
}

// Enabled reports whether telemetry initialized successfully and was not opted out.
func (r *Reporter) Enabled() bool {
	return r != nil && r.enabled
}

// Start reports a successful application launch and begins periodic heartbeats.
func (r *Reporter) Start() {
	if !r.Enabled() {
		return
	}
	r.mu.Lock()
	if r.started || r.closed {
		r.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r.started = true
	r.heartbeatCancel = cancel
	r.heartbeatDone = done
	r.enqueue("app_started")
	go r.runHeartbeat(ctx, done)
	r.mu.Unlock()
}

// CaptureException submits a handled error using Sentry's exception API.
func (r *Reporter) CaptureException(err error) bool {
	if !r.Enabled() || err == nil {
		return false
	}
	return r.sentryHub.CaptureException(err) != nil
}

// Recover submits an unhandled panic using Sentry's recovery API.
func (r *Reporter) Recover(recovered any) bool {
	if !r.Enabled() || recovered == nil {
		return false
	}
	return r.sentryHub.Recover(recovered) != nil
}

// FlushErrors waits a bounded time for queued error events to be delivered.
func (r *Reporter) FlushErrors() bool {
	if !r.Enabled() {
		return false
	}
	return r.sentryHub.Flush(r.deliveryTimeout)
}

// Close stops heartbeat delivery, reports a clean exit when appropriate, and
// performs a bounded drain of queued telemetry.
func (r *Reporter) Close(clean bool) {
	if !r.Enabled() {
		return
	}
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		started := r.started
		cancel := r.heartbeatCancel
		done := r.heartbeatDone
		r.mu.Unlock()

		if cancel != nil {
			cancel()
			<-done
		}
		if clean && started {
			r.enqueue("app_exited")
		}
		close(r.analyticsEvents)
		r.waitForDeliveries()
		if !r.FlushErrors() {
			r.logger.Warn("Beacon error event flush timed out")
		}
		r.sentryClient.Close()
	})
}

func (r *Reporter) runHeartbeat(ctx context.Context, done chan<- struct{}) {
	ticker := time.NewTicker(r.heartbeatInterval)
	defer ticker.Stop()
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.enqueue("heartbeat")
		}
	}
}

func (r *Reporter) enqueue(eventName string) {
	select {
	case r.analyticsEvents <- eventName:
	default:
		r.logger.Warn("Beacon analytics queue is full", "event", eventName)
	}
}

func (r *Reporter) runDelivery() {
	defer close(r.analyticsDone)
	for eventName := range r.analyticsEvents {
		if err := r.deliver(eventName); err != nil {
			r.logger.Warn("Beacon analytics delivery failed", "event", eventName, "error", err)
		}
	}
}

func (r *Reporter) deliver(eventName string) error {
	payload := analyticsEnvelope{Events: []analyticsEvent{{
		Event:     eventName,
		InstallID: r.installID,
		Timestamp: r.now().UTC().Format(time.RFC3339),
		Release:   r.release,
		Platform:  r.platform,
	}}}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.deliveryTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.analyticsEndpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+r.analyticsAPIKey)
	response, err := r.analyticsClient.Do(request)
	if err != nil {
		return fmt.Errorf("send event: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("beacon returned HTTP %d", response.StatusCode)
	}
	if r.verification {
		r.logger.Info("Beacon accepted analytics event", "event", eventName, "status", response.StatusCode)
	} else {
		r.logger.Debug("Beacon accepted analytics event", "event", eventName, "status", response.StatusCode)
	}
	return nil
}

func (r *Reporter) waitForDeliveries() {
	timer := time.NewTimer(2 * r.deliveryTimeout)
	defer timer.Stop()
	select {
	case <-r.analyticsDone:
	case <-timer.C:
		r.logger.Warn("Beacon analytics delivery drain timed out")
	}
}

type statusRoundTripper struct {
	base         http.RoundTripper
	logger       *slog.Logger
	verification bool
	kind         string
}

func (t statusRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		if t.verification {
			t.logger.Info("Beacon accepted "+t.kind, "status", response.StatusCode)
		} else {
			t.logger.Debug("Beacon accepted "+t.kind, "status", response.StatusCode)
		}
	} else {
		t.logger.Warn("Beacon rejected "+t.kind, "status", response.StatusCode)
	}
	return response, nil
}

func loadOrCreateInstallID(dataDir string) (string, error) {
	path := filepath.Join(dataDir, installIDFilename)
	installID, err := readInstallID(path)
	if err == nil {
		return installID, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return "", fmt.Errorf("create data directory: %w", err)
	}
	installID, err = randomInstallID()
	if err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(dataDir, ".install-id-*")
	if err != nil {
		return "", fmt.Errorf("create temporary install ID: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("protect temporary install ID: %w", err)
	}
	if _, err := io.WriteString(temporary, installID+"\n"); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("write temporary install ID: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("sync temporary install ID: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close temporary install ID: %w", err)
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return readInstallID(path)
		}
		return "", fmt.Errorf("persist install ID: %w", err)
	}
	if directory, err := os.Open(dataDir); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return installID, nil
}

func readInstallID(path string) (string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	installID := strings.TrimSpace(string(contents))
	if !validInstallID(installID) {
		return "", errors.New("stored install ID is not a valid random UUID")
	}
	return installID, nil
}

func randomInstallID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate install ID: %w", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}

func validInstallID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	compact := strings.ReplaceAll(value, "-", "")
	decoded, err := hex.DecodeString(compact)
	if err != nil || len(decoded) != 16 {
		return false
	}
	return decoded[6]>>4 == 4 && decoded[8]&0xc0 == 0x80
}
