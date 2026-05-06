package httpservice

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/certmagic"
	"go.uber.org/zap"
)

// mockStorage implements certmagic.Storage with an in-memory map.
type mockStorage struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newMockStorage() *mockStorage {
	return &mockStorage{data: make(map[string][]byte)}
}

func (m *mockStorage) Lock(_ context.Context, _ string) error  { return nil }
func (m *mockStorage) Unlock(_ context.Context, _ string) error { return nil }

func (m *mockStorage) Store(_ context.Context, key string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
	return nil
}

func (m *mockStorage) Load(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[key]
	if !ok {
		return nil, errors.New("key not found")
	}
	return v, nil
}

func (m *mockStorage) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

func (m *mockStorage) Exists(_ context.Context, key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.data[key]
	return ok
}

func (m *mockStorage) List(_ context.Context, _ string, _ bool) ([]string, error) {
	return nil, nil
}

func (m *mockStorage) Stat(_ context.Context, _ string) (certmagic.KeyInfo, error) {
	return certmagic.KeyInfo{}, nil
}

// testHandler is a caddyhttp.Handler that records whether it was called.
type testHandler struct {
	called bool
	fn     func(w http.ResponseWriter, r *http.Request) error
}

func (th *testHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) error {
	th.called = true
	if th.fn != nil {
		return th.fn(w, r)
	}
	return nil
}

// newTestRequest creates an httptest request with a caddy.Replacer in the
// context, as expected by ServeHTTP.
func newTestRequest(method, url string) *http.Request {
	r := httptest.NewRequest(method, url, nil)
	repl := caddy.NewReplacer()
	ctx := context.WithValue(r.Context(), caddy.ReplacerCtxKey, repl)
	return r.WithContext(ctx)
}

// replacerFromRequest extracts the replacer from the request context.
func replacerFromRequest(r *http.Request) *caddy.Replacer {
	return r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
}

// ---------------------------------------------------------------------------
// flattenJSON tests
// ---------------------------------------------------------------------------

func TestFlattenJSON_String(t *testing.T) {
	repl := caddy.NewReplacer()
	data := map[string]interface{}{"name": "hello"}
	flattenJSON("http_service", data, repl)

	val, ok := repl.Get("http_service.name")
	if !ok {
		t.Fatal("expected key http_service.name to be set")
	}
	if val != "hello" {
		t.Fatalf("expected 'hello', got %q", val)
	}
}

func TestFlattenJSON_Integer(t *testing.T) {
	repl := caddy.NewReplacer()
	data := map[string]interface{}{"count": float64(42)}
	flattenJSON("http_service", data, repl)

	val, ok := repl.Get("http_service.count")
	if !ok {
		t.Fatal("expected key http_service.count to be set")
	}
	if val != "42" {
		t.Fatalf("expected '42', got %q", val)
	}
}

func TestFlattenJSON_Float(t *testing.T) {
	repl := caddy.NewReplacer()
	data := map[string]interface{}{"ratio": 3.14}
	flattenJSON("http_service", data, repl)

	val, ok := repl.Get("http_service.ratio")
	if !ok {
		t.Fatal("expected key http_service.ratio to be set")
	}
	if val != "3.14" {
		t.Fatalf("expected '3.14', got %q", val)
	}
}

func TestFlattenJSON_Bool(t *testing.T) {
	repl := caddy.NewReplacer()
	data := map[string]interface{}{"active": true}
	flattenJSON("http_service", data, repl)

	val, ok := repl.Get("http_service.active")
	if !ok {
		t.Fatal("expected key http_service.active to be set")
	}
	if val != "true" {
		t.Fatalf("expected 'true', got %q", val)
	}
}

func TestFlattenJSON_Null(t *testing.T) {
	repl := caddy.NewReplacer()
	data := map[string]interface{}{"nothing": nil}
	flattenJSON("http_service", data, repl)

	val, ok := repl.Get("http_service.nothing")
	if !ok {
		t.Fatal("expected key http_service.nothing to be set")
	}
	if val != "" {
		t.Fatalf("expected empty string for nil, got %q", val)
	}
}

func TestFlattenJSON_Nested(t *testing.T) {
	repl := caddy.NewReplacer()
	data := map[string]interface{}{
		"user": map[string]interface{}{
			"name": "alice",
			"addr": map[string]interface{}{
				"city": "Paris",
			},
		},
	}
	flattenJSON("http_service", data, repl)

	tests := []struct{ key, want string }{
		{"http_service.user.name", "alice"},
		{"http_service.user.addr.city", "Paris"},
	}
	for _, tc := range tests {
		val, ok := repl.Get(tc.key)
		if !ok {
			t.Errorf("expected key %s to be set", tc.key)
			continue
		}
		if val != tc.want {
			t.Errorf("key %s: expected %q, got %q", tc.key, tc.want, val)
		}
	}
}

func TestFlattenJSON_EmptyMap(t *testing.T) {
	repl := caddy.NewReplacer()
	data := map[string]interface{}{}
	flattenJSON("http_service", data, repl)
	// Should not panic and should set nothing.
}

// ---------------------------------------------------------------------------
// Provision validation tests
// ---------------------------------------------------------------------------

func TestProvision_EmptyURLError(t *testing.T) {
	hs := &HttpService{}
	hs.logger = zap.NewNop()
	err := hs.Provision(caddy.Context{Context: context.Background()})
	if err == nil {
		t.Fatal("expected error for empty URL")
	}
	if err.Error() != "url is required" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProvision_DefaultsBehavior(t *testing.T) {
	// Simulate a fully-provisioned HttpService with default values
	// to verify the defaults work correctly end-to-end.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"key": "val"})
	}))
	defer server.Close()

	hs := &HttpService{
		// URL is set but Method is left empty (Provision would default to GET).
		URL:              server.URL,
		Method:           http.MethodGet, // Provision default
		CacheKeyTemplate: "{method}:{url}", // Provision default
		client:           &http.Client{},
		logger:           zap.NewNop(),
	}

	r := newTestRequest("GET", "/")
	w := httptest.NewRecorder()
	next := &testHandler{}

	if err := hs.ServeHTTP(w, r, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	if !next.called {
		t.Fatal("next handler was not called")
	}
}

func TestProvision_CustomMethodBehavior(t *testing.T) {
	var receivedMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	}))
	defer server.Close()

	hs := &HttpService{
		URL:              server.URL,
		Method:           "POST",
		CacheKeyTemplate: "{method}:{url}",
		client:           &http.Client{},
		logger:           zap.NewNop(),
	}

	r := newTestRequest("GET", "/")
	w := httptest.NewRecorder()
	next := &testHandler{}

	if err := hs.ServeHTTP(w, r, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	if receivedMethod != "POST" {
		t.Errorf("expected POST method to external service, got %s", receivedMethod)
	}
}

// ---------------------------------------------------------------------------
// ServeHTTP - basic JSON injection tests (mock HTTP server)
// ---------------------------------------------------------------------------

func TestServeHTTP_JSONPlaceholderInjection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"tenant":  "acme",
			"version": float64(2),
		})
	}))
	defer server.Close()

	hs := &HttpService{
		URL:    server.URL + "/tenant/{host}",
		Method: "GET",
		client: &http.Client{},
		logger: zap.NewNop(),
	}

	r := newTestRequest("GET", "/")
	w := httptest.NewRecorder()
	next := &testHandler{}

	if err := hs.ServeHTTP(w, r, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	if !next.called {
		t.Fatal("next handler was not called")
	}

	repl := replacerFromRequest(r)
	val, ok := repl.Get("http_service.tenant")
	if !ok || val != "acme" {
		t.Errorf("expected http_service.tenant='acme', got %q (ok=%v)", val, ok)
	}
	val, ok = repl.Get("http_service.version")
	if !ok || val != "2" {
		t.Errorf("expected http_service.version='2', got %q (ok=%v)", val, ok)
	}
}

func TestServeHTTP_PlaceholderExpansion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"path": r.URL.Path,
		})
	}))
	defer server.Close()

	hs := &HttpService{
		URL:    server.URL + "/api",
		Method: "GET",
		client: &http.Client{},
		logger: zap.NewNop(),
	}

	r := newTestRequest("GET", "/")
	w := httptest.NewRecorder()
	next := &testHandler{}

	if err := hs.ServeHTTP(w, r, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	if !next.called {
		t.Fatal("next handler was not called")
	}

	repl := replacerFromRequest(r)
	val, ok := repl.Get("http_service.path")
	if !ok || val != "/api" {
		t.Errorf("expected http_service.path='/api', got %q (ok=%v)", val, ok)
	}
}

func TestServeHTTP_CustomHeaders(t *testing.T) {
	var receivedHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeader = r.Header.Get("X-API-Key")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	}))
	defer server.Close()

	hs := &HttpService{
		URL: server.URL,
		Headers: map[string]string{
			"X-API-Key": "secret-token",
		},
		Method: "GET",
		client: &http.Client{},
		logger: zap.NewNop(),
	}

	r := newTestRequest("GET", "/")
	w := httptest.NewRecorder()
	next := &testHandler{}

	if err := hs.ServeHTTP(w, r, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	if receivedHeader != "secret-token" {
		t.Errorf("expected header 'secret-token', got %q", receivedHeader)
	}
}

// ---------------------------------------------------------------------------
// ServeHTTP - error handling tests
// ---------------------------------------------------------------------------

func TestServeHTTP_NonJSONResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("not json"))
	}))
	defer server.Close()

	hs := &HttpService{
		URL:    server.URL,
		Method: "GET",
		client: &http.Client{},
		logger: zap.NewNop(),
	}

	r := newTestRequest("GET", "/")
	w := httptest.NewRecorder()
	next := &testHandler{}

	if err := hs.ServeHTTP(w, r, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	if !next.called {
		t.Fatal("next handler should still be called on non-JSON response")
	}

	repl := replacerFromRequest(r)
	_, ok := repl.Get("http_service.anything")
	if ok {
		t.Error("no http_service placeholders should be set for non-JSON response")
	}
}

func TestServeHTTP_ServiceDown_NoCache(t *testing.T) {
	// Point to a server that is not running.
	hs := &HttpService{
		URL:    "http://127.0.0.1:1/no-such-service",
		Method: "GET",
		client: &http.Client{Timeout: 50 * time.Millisecond},
		logger: zap.NewNop(),
	}

	r := newTestRequest("GET", "/")
	w := httptest.NewRecorder()
	next := &testHandler{}

	// Should not return an error; it passes through to next handler.
	if err := hs.ServeHTTP(w, r, next); err != nil {
		t.Fatalf("ServeHTTP should not return error on upstream failure: %v", err)
	}
	if !next.called {
		t.Fatal("next handler should be called even when service is down")
	}
}

func TestServeHTTP_Non2xxResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer server.Close()

	hs := &HttpService{
		URL:    server.URL,
		Method: "GET",
		client: &http.Client{},
		logger: zap.NewNop(),
	}

	r := newTestRequest("GET", "/")
	w := httptest.NewRecorder()
	next := &testHandler{}

	if err := hs.ServeHTTP(w, r, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	if !next.called {
		t.Fatal("next handler should be called on non-2xx")
	}

	repl := replacerFromRequest(r)
	_, ok := repl.Get("http_service.error")
	if ok {
		t.Error("no http_service placeholders should be set for non-2xx")
	}
}

// ---------------------------------------------------------------------------
// Cache tests
// ---------------------------------------------------------------------------

func TestCache_Hit(t *testing.T) {
	store := newMockStorage()

	const cacheKey = "http_service/test-cache-key"
	entry := cacheEntry{
		Data:      map[string]interface{}{"cached": "yes"},
		ExpiresAt: time.Now().Add(time.Hour),
	}
	raw, _ := json.Marshal(entry)
	store.Store(context.Background(), cacheKey, raw)

	hs := &HttpService{
		URL:              "http://example.com/api",
		Method:           "GET",
		CacheEnabled:     true,
		CacheKeyTemplate: "test-cache-key",
		client:           &http.Client{},
		logger:           zap.NewNop(),
		storage:          store,
	}

	r := newTestRequest("GET", "/")
	w := httptest.NewRecorder()
	next := &testHandler{}

	if err := hs.ServeHTTP(w, r, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	if !next.called {
		t.Fatal("next handler was not called")
	}

	repl := replacerFromRequest(r)
	val, ok := repl.Get("http_service.cached")
	if !ok || val != "yes" {
		t.Fatalf("expected cached value 'yes', got %q (ok=%v)", val, ok)
	}
}

func TestCache_Miss(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"fresh": "data"})
	}))
	defer server.Close()

	store := newMockStorage()

	hs := &HttpService{
		URL:              server.URL,
		Method:           "GET",
		CacheEnabled:     true,
		CacheKeyTemplate: "miss-test-key",
		CacheTTL:         caddy.Duration(time.Minute),
		client:           &http.Client{},
		logger:           zap.NewNop(),
		storage:          store,
	}

	r := newTestRequest("GET", "/")
	w := httptest.NewRecorder()
	next := &testHandler{}

	if err := hs.ServeHTTP(w, r, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}

	repl := replacerFromRequest(r)
	val, ok := repl.Get("http_service.fresh")
	if !ok || val != "data" {
		t.Fatalf("expected 'data', got %q (ok=%v)", val, ok)
	}

	// The response should have been cached.
	if !store.Exists(context.Background(), "http_service/miss-test-key") {
		t.Fatal("cache entry was not stored after miss")
	}
}

func TestCache_StaleFallback(t *testing.T) {
	store := newMockStorage()

	// Pre-populate an expired cache entry.
	entry := cacheEntry{
		Data:      map[string]interface{}{"stale": "fallback"},
		ExpiresAt: time.Now().Add(-time.Hour), // expired
	}
	raw, _ := json.Marshal(entry)

	const cacheKey = "http_service/stale-test-key"
	store.Store(context.Background(), cacheKey, raw)

	// Point to a non-running service.
	hs := &HttpService{
		URL:               "http://127.0.0.1:1/broken",
		Method:            "GET",
		CacheEnabled:      true,
		CacheKeyTemplate:  "stale-test-key",
		CacheStaleEnabled: true,
		CacheTTL:          caddy.Duration(time.Minute),
		client:            &http.Client{Timeout: 50 * time.Millisecond},
		logger:            zap.NewNop(),
		storage:           store,
	}

	r := newTestRequest("GET", "/")
	w := httptest.NewRecorder()
	next := &testHandler{}

	if err := hs.ServeHTTP(w, r, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}

	repl := replacerFromRequest(r)
	val, ok := repl.Get("http_service.stale")
	if !ok || val != "fallback" {
		t.Fatalf("expected stale fallback 'fallback', got %q (ok=%v)", val, ok)
	}
}

func TestCache_StaleDisabled(t *testing.T) {
	store := newMockStorage()

	entry := cacheEntry{
		Data:      map[string]interface{}{"stale": "fallback"},
		ExpiresAt: time.Now().Add(-time.Hour),
	}
	raw, _ := json.Marshal(entry)
	store.Store(context.Background(), "http_service/stale-disabled-key", raw)

	hs := &HttpService{
		URL:               "http://127.0.0.1:1/broken2",
		Method:            "GET",
		CacheEnabled:      true,
		CacheKeyTemplate:  "stale-disabled-key",
		CacheStaleEnabled: false, // stale disabled
		client:            &http.Client{Timeout: 50 * time.Millisecond},
		logger:            zap.NewNop(),
		storage:           store,
	}

	r := newTestRequest("GET", "/")
	w := httptest.NewRecorder()
	next := &testHandler{}

	if err := hs.ServeHTTP(w, r, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}

	repl := replacerFromRequest(r)
	_, ok := repl.Get("http_service.stale")
	if ok {
		t.Error("stale data should NOT be served when CacheStaleEnabled is false")
	}
}

func TestCache_Disabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"item": "direct"})
	}))
	defer server.Close()

	store := newMockStorage()

	hs := &HttpService{
		URL:          server.URL,
		Method:       "GET",
		CacheEnabled: false,
		client:       &http.Client{},
		logger:       zap.NewNop(),
		storage:      store,
	}

	r := newTestRequest("GET", "/")
	w := httptest.NewRecorder()
	next := &testHandler{}

	if err := hs.ServeHTTP(w, r, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}

	repl := replacerFromRequest(r)
	val, _ := repl.Get("http_service.item")
	if val != "direct" {
		t.Errorf("expected 'direct', got %q", val)
	}

	// Nothing should be stored in the cache.
	cacheKey := "http_service/" + "GET:" + server.URL
	if store.Exists(context.Background(), cacheKey) {
		t.Error("cache should not have been written when disabled")
	}
}

func TestCache_ExpiredEntrySkipped_NoStale(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"fresh": callCount})
	}))
	defer server.Close()

	store := newMockStorage()

	// Pre-populate expired cache entry.
	entry := cacheEntry{
		Data:      map[string]interface{}{"stale": "old"},
		ExpiresAt: time.Now().Add(-time.Hour),
	}
	raw, _ := json.Marshal(entry)
	cacheKey := "http_service/expired-test-key"
	store.Store(context.Background(), cacheKey, raw)

	hs := &HttpService{
		URL:               server.URL,
		Method:            "GET",
		CacheEnabled:      true,
		CacheKeyTemplate:  "expired-test-key",
		CacheStaleEnabled: false,
		CacheTTL:          caddy.Duration(time.Minute),
		client:            &http.Client{},
		logger:            zap.NewNop(),
		storage:           store,
	}

	r := newTestRequest("GET", "/")
	w := httptest.NewRecorder()
	next := &testHandler{}

	if err := hs.ServeHTTP(w, r, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}

	repl := replacerFromRequest(r)
	val, _ := repl.Get("http_service.fresh")
	if val != "1" {
		t.Errorf("expected fresh call (count=1), got %q", val)
	}
}

// TestServeHTTP_CacheHitSkipsHTTPCall verifies that on a valid cache hit the
// external service is NOT called.
func TestServeHTTP_CacheHitSkipsHTTPCall(t *testing.T) {
	// Create a mock server that would panic if called, ensuring the cache
	// path does not attempt to dial out.
	store := newMockStorage()
	entry := cacheEntry{
		Data:      map[string]interface{}{"from": "cache"},
		ExpiresAt: time.Now().Add(time.Hour),
	}
	raw, _ := json.Marshal(entry)
	store.Store(context.Background(), "http_service/skip-call-key", raw)

	hs := &HttpService{
		URL:              "http://should-not-call.example/",
		Method:           "GET",
		CacheEnabled:     true,
		CacheKeyTemplate: "skip-call-key",
		client:           &http.Client{},
		logger:           zap.NewNop(),
		storage:          store,
	}

	r := newTestRequest("GET", "/")
	w := httptest.NewRecorder()
	next := &testHandler{}

	if err := hs.ServeHTTP(w, r, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}

	repl := replacerFromRequest(r)
	val, ok := repl.Get("http_service.from")
	if !ok || val != "cache" {
		t.Fatalf("expected cached value 'cache', got %q (ok=%v)", val, ok)
	}
}

// ---------------------------------------------------------------------------
// Module info test
// ---------------------------------------------------------------------------

func TestCaddyModule(t *testing.T) {
	hs := &HttpService{}
	info := hs.CaddyModule()
	if info.ID != "http.handlers.http_service" {
		t.Errorf("unexpected module ID: %s", info.ID)
	}
	if info.New == nil {
		t.Error("module New function is nil")
	}
}

// ---------------------------------------------------------------------------
// Interface guards verification
// ---------------------------------------------------------------------------

func TestInterfaceImplementation(t *testing.T) {
	var hs any = (*HttpService)(nil)

	if _, ok := hs.(caddy.Provisioner); !ok {
		t.Error("HttpService does not implement caddy.Provisioner")
	}
	if _, ok := hs.(caddyhttp.MiddlewareHandler); !ok {
		t.Error("HttpService does not implement caddyhttp.MiddlewareHandler")
	}
}

// ---------------------------------------------------------------------------
// buildURLWithParams tests
// ---------------------------------------------------------------------------

func TestBuildURLWithParams(t *testing.T) {
	repl := caddy.NewReplacer()
	repl.Set("host", "example.com")

	tests := []struct {
		name     string
		baseURL  string
		params   map[string]string
		wantURL  string
	}{
		{
			name:    "single param",
			baseURL: "http://example.com/api",
			params:  map[string]string{"domain": "{host}"},
			wantURL: "http://example.com/api?domain=example.com",
		},
		{
			name:    "multiple params",
			baseURL: "http://example.com/api",
			params:  map[string]string{"page": "1", "domain": "{host}"},
			wantURL: "http://example.com/api?domain=example.com&page=1",
		},
		{
			name:    "special chars are URL-encoded",
			baseURL: "http://example.com/api",
			params:  map[string]string{"name": "john & sons"},
			wantURL: "http://example.com/api?name=john+%26+sons",
		},
		{
			name:    "preserves existing query string",
			baseURL: "http://example.com/api?existing=1",
			params:  map[string]string{"domain": "{host}"},
			wantURL: "http://example.com/api?domain=example.com&existing=1",
		},
		{
			name:    "empty params returns base URL",
			baseURL: "http://example.com/api",
			params:  map[string]string{},
			wantURL: "http://example.com/api",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildURLWithParams(tc.baseURL, tc.params, repl)
			if got != tc.wantURL {
				t.Errorf("buildURLWithParams() = %q, want %q", got, tc.wantURL)
			}
		})
	}
}

func TestBuildURLWithParams_PlaceholderExpansion(t *testing.T) {
	repl := caddy.NewReplacer()
	repl.Set("host", "myhost")
	repl.Set("path", "/users")

	got := buildURLWithParams("http://example.com/api", map[string]string{
		"host": "{host}",
		"path": "{path}",
	}, repl)

	if got != "http://example.com/api?host=myhost&path=%2Fusers" {
		t.Errorf("unexpected URL: %s", got)
	}
}

// ---------------------------------------------------------------------------
// UnmarshalCaddyfile param directive tests
// ---------------------------------------------------------------------------

func TestUnmarshalCaddyfile_ParamDirective(t *testing.T) {
	input := `http_service {
		url http://example.com/api
		param domain {host}
		param page 1
	}`

	d := caddyfile.NewTestDispenser(input)
	hs := new(HttpService)

	if err := hs.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("UnmarshalCaddyfile failed: %v", err)
	}

	if hs.Params == nil {
		t.Fatal("Params should not be nil")
	}
	if hs.Params["domain"] != "{host}" {
		t.Errorf("Params[domain] = %q, want %q", hs.Params["domain"], "{host}")
	}
	if hs.Params["page"] != "1" {
		t.Errorf("Params[page] = %q, want %q", hs.Params["page"], "1")
	}
}
