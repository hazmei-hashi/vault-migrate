package client

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"vault-migrate/config"
)

// fakeClientVault is an httptest.Server-backed fake Vault sys/health +
// auth/token/lookup-self backend, mirroring the fakeVault pattern in
// kvv2/kvv2_mock_test.go.
type fakeClientVault struct {
	mu sync.Mutex

	healthStatus int
	healthBody   map[string]any
	healthDelay  time.Duration
	healthCount  int

	lookupStatus    int
	lookupBody      map[string]any
	lookupNamespace string
	lookupCount     int

	server *httptest.Server
}

func newFakeClientVault(t *testing.T) *fakeClientVault {
	t.Helper()

	f := &fakeClientVault{
		healthStatus: http.StatusOK,
		healthBody: map[string]any{
			"initialized": true,
			"sealed":      false,
			"cluster_id":  "test-cluster",
		},
		lookupStatus: http.StatusOK,
		lookupBody: map[string]any{
			"data": map[string]any{"ttl": 3600},
		},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeClientVault) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/v1/sys/health"):
		f.mu.Lock()
		f.healthCount++
		status := f.healthStatus
		body := f.healthBody
		delay := f.healthDelay
		f.mu.Unlock()

		if delay > 0 {
			time.Sleep(delay)
		}
		writeJSONBody(w, status, body)
	case strings.HasSuffix(r.URL.Path, "/v1/auth/token/lookup-self"):
		f.mu.Lock()
		f.lookupCount++
		f.lookupNamespace = r.Header.Get("X-Vault-Namespace")
		status := f.lookupStatus
		body := f.lookupBody
		f.mu.Unlock()

		writeJSONBody(w, status, body)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func writeJSONBody(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeClientVault) setHealth(status int, initialized, sealed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.healthStatus = status
	f.healthBody = map[string]any{
		"initialized": initialized,
		"sealed":      sealed,
		"cluster_id":  "test-cluster",
	}
}

func (f *fakeClientVault) setHealthDelay(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.healthDelay = d
}

func (f *fakeClientVault) setLookup(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookupStatus = status
}

func (f *fakeClientVault) getHealthCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.healthCount
}

func (f *fakeClientVault) getLookupNamespace() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookupNamespace
}

// closedPortAddr allocates then immediately closes a TCP listener, returning
// an address guaranteed to be unreachable (connection refused).
func closedPortAddr(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate a port: %v", err)
	}
	addr := "http://" + ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("failed to close listener: %v", err)
	}
	return addr
}

func TestGetClient_HealthAndLookupOutcomes(t *testing.T) {
	tests := []struct {
		name         string
		healthStatus int
		initialized  bool
		sealed       bool
		lookupStatus int
		wantErr      bool
		errContains  string
	}{
		{name: "healthy initialized unsealed", healthStatus: http.StatusOK, initialized: true, sealed: false, lookupStatus: http.StatusOK, wantErr: false},
		{name: "health 500", healthStatus: http.StatusInternalServerError, initialized: true, sealed: false, lookupStatus: http.StatusOK, wantErr: true, errContains: "health check failed"},
		{name: "health 503", healthStatus: http.StatusServiceUnavailable, initialized: true, sealed: false, lookupStatus: http.StatusOK, wantErr: true, errContains: "health check failed"},
		{name: "not initialized", healthStatus: http.StatusOK, initialized: false, sealed: false, lookupStatus: http.StatusOK, wantErr: true, errContains: "not initialized"},
		{name: "sealed", healthStatus: http.StatusOK, initialized: true, sealed: true, lookupStatus: http.StatusOK, wantErr: true, errContains: "is sealed"},
		{name: "lookup 403", healthStatus: http.StatusOK, initialized: true, sealed: false, lookupStatus: http.StatusForbidden, wantErr: true, errContains: "token lookup failed"},
		{name: "lookup 500", healthStatus: http.StatusOK, initialized: true, sealed: false, lookupStatus: http.StatusInternalServerError, wantErr: true, errContains: "token lookup failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeClientVault(t)
			f.setHealth(tt.healthStatus, tt.initialized, tt.sealed)
			f.setLookup(tt.lookupStatus)

			// maxRetries=0: 500/503 responses are retried by the underlying
			// retryablehttp policy, and we want deterministic single-attempt
			// behavior in these outcome checks (retry behavior itself is
			// covered by TestGetClient_AppliesMaxRetries).
			c, err := getClient(f.server.URL, "test-token", "", false, 5*time.Second, 0)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("getClient() error = nil, want error containing %q", tt.errContains)
				}
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Fatalf("getClient() error = %q, want contains %q", err.Error(), tt.errContains)
				}
				if strings.Contains(err.Error(), "test-token") {
					t.Fatalf("error message leaked token value: %q", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("getClient() unexpected error: %v", err)
			}
			if c == nil {
				t.Fatalf("getClient() returned nil client with no error")
			}
		})
	}
}

func TestGetClient_UnreachableAddress(t *testing.T) {
	addr := closedPortAddr(t)

	_, err := getClient(addr, "test-token", "", false, 2*time.Second, 0)
	if err == nil {
		t.Fatalf("expected error for unreachable address, got nil")
	}
	if !strings.Contains(err.Error(), "health check failed") {
		t.Fatalf("getClient() error = %q, want contains %q", err.Error(), "health check failed")
	}
}

// TestGetClient_NamespaceSetBeforeLookup is a regression lock for B5:
// SetNamespace must happen before LookupSelf, otherwise the lookup runs
// against the wrong (empty) namespace.
func TestGetClient_NamespaceSetBeforeLookup(t *testing.T) {
	f := newFakeClientVault(t)

	const wantNamespace = "admin/team-a"
	c, err := getClient(f.server.URL, "test-token", wantNamespace, false, 5*time.Second, 0)
	if err != nil {
		t.Fatalf("getClient() unexpected error: %v", err)
	}
	if c == nil {
		t.Fatalf("getClient() returned nil client with no error")
	}

	if got := f.getLookupNamespace(); got != wantNamespace {
		t.Fatalf("lookup-self namespace header = %q, want %q", got, wantNamespace)
	}
}

// TestGetClient_AppliesMaxRetries is a regression lock for B14:
// client.SetMaxRetries(maxRetries) must actually be honored by the
// underlying retry transport.
func TestGetClient_AppliesMaxRetries(t *testing.T) {
	f := newFakeClientVault(t)
	f.setHealth(http.StatusInternalServerError, true, false)

	const maxRetries = 1
	_, err := getClient(f.server.URL, "test-token", "", false, 5*time.Second, maxRetries)
	if err == nil {
		t.Fatalf("expected error from persistent 500 health response, got nil")
	}

	wantAttempts := maxRetries + 1
	if got := f.getHealthCount(); got != wantAttempts {
		t.Fatalf("health endpoint hit %d times, want %d (maxRetries=%d honored)", got, wantAttempts, maxRetries)
	}
}

// TestGetClient_AppliesTimeout is a regression lock for B8:
// client.SetClientTimeout(timeout) must actually bound each request.
func TestGetClient_AppliesTimeout(t *testing.T) {
	f := newFakeClientVault(t)
	f.setHealthDelay(300 * time.Millisecond)

	const timeout = 50 * time.Millisecond
	start := time.Now()
	_, err := getClient(f.server.URL, "test-token", "", false, timeout, 0)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "health check failed") {
		t.Fatalf("getClient() error = %q, want contains %q", err.Error(), "health check failed")
	}
	if elapsed >= 300*time.Millisecond {
		t.Fatalf("getClient took %v, want well under the 300ms handler delay (timeout not honored)", elapsed)
	}
}

// TestBuildClients_PropagatesGetClientError proves BuildClients returns
// getClient's error (via fmt.Errorf %w chain) instead of terminating the
// process, and that no partial clients leak out on failure.
func TestBuildClients_PropagatesGetClientError(t *testing.T) {
	badAddr := closedPortAddr(t)
	otherAddr := closedPortAddr(t)

	setFlags := config.SetFlags{
		"srcAddr":       true,
		"srcToken":      true,
		"srcNamespace":  true,
		"dstAddr":       true,
		"dstToken":      true,
		"dstNamespace":  true,
		"tlsSkipVerify": true,
	}

	cfg := config.VaultClientConfig{
		SrcAddr:       badAddr,
		SrcToken:      "test-token",
		DstAddr:       otherAddr,
		DstToken:      "test-token",
		LogLevel:      "error",
		MaxRetries:    0,
		ClientTimeout: 2 * time.Second,
	}

	src, dst, err := BuildClients(cfg, setFlags)
	if err == nil {
		t.Fatalf("expected BuildClients to propagate getClient error, got nil")
	}
	if src != nil || dst != nil {
		t.Fatalf("BuildClients() = (%v, %v), want (nil, nil) on error", src, dst)
	}
	if strings.Contains(err.Error(), "test-token") {
		t.Fatalf("error message leaked token value: %q", err.Error())
	}
}
