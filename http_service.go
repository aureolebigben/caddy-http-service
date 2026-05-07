package httpservice

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/certmagic"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(HttpService{})
	httpcaddyfile.RegisterHandlerDirective("http_service", parseCaddyfile)
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	hs := new(HttpService)
	if err := hs.UnmarshalCaddyfile(h.Dispenser); err != nil {
		return nil, err
	}
	return hs, nil
}

// UnmarshalCaddyfile parses the http_service Caddyfile directive.
func (h *HttpService) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		for nesting := d.Nesting(); d.NextBlock(nesting); {
			switch d.Val() {
			case "url":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.URL = d.Val()
			case "method":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.Method = d.Val()
			case "header":
				if !d.NextArg() {
					return d.ArgErr()
				}
				key := d.Val()
				if !d.NextArg() {
					return d.ArgErr()
				}
				val := d.Val()
				if h.Headers == nil {
					h.Headers = make(map[string]string)
				}
				h.Headers[key] = val
			case "param":
				if !d.NextArg() {
					return d.ArgErr()
				}
				key := d.Val()
				if !d.NextArg() {
					return d.ArgErr()
				}
				val := d.Val()
				if h.Params == nil {
					h.Params = make(map[string]string)
				}
				h.Params[key] = val
			case "body":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.Body = d.Val()
			case "timeout":
				if !d.NextArg() {
					return d.ArgErr()
				}
				dur, err := caddy.ParseDuration(d.Val())
				if err != nil {
					return d.WrapErr(err)
				}
				h.Timeout = caddy.Duration(dur)
			case "tls_skip_verify":
				h.TLSSkipVerify = true
			case "cache_enabled":
				h.CacheEnabled = true
			case "cache_disabled":
				h.CacheEnabled = false
			case "cache_ttl":
				if !d.NextArg() {
					return d.ArgErr()
				}
				dur, err := caddy.ParseDuration(d.Val())
				if err != nil {
					return d.WrapErr(err)
				}
				h.CacheTTL = caddy.Duration(dur)
			case "cache_stale_enabled":
				h.CacheStaleEnabled = true
			case "cache_key_template":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.CacheKeyTemplate = d.Val()
			default:
				return d.Errf("unknown subdirective: %s", d.Val())
			}
		}
	}
	return nil
}

// storageNamespace is the prefix used for all cache keys stored in the
// Caddy storage backend to avoid collisions with other modules.
const storageNamespace = "http_service/"

// cacheEntry is the serialized representation of a cached HTTP service
// response. It contains the parsed JSON data and the absolute time at
// which this entry expires.
type cacheEntry struct {
	Data      map[string]interface{} `json:"data"`
	ExpiresAt time.Time              `json:"expires_at"`
}

// HttpService implements an HTTP handler module that proxies requests to an
// external HTTP service. It is registered as http.handlers.http_service.
type HttpService struct {
	// URL is the external service URL. It may contain Caddy placeholders
	// such as {host}, {uri}, {path}, {method}, etc.
	URL string `json:"url,omitempty"`

	// Method is the HTTP method used when calling the external service.
	// Defaults to GET if empty.
	Method string `json:"method,omitempty"`

	// Headers are custom headers to include in the request to the external
	// service.
	Headers map[string]string `json:"headers,omitempty"`

	// Body, when non-empty, provides a request body template. Typically
	// used for POST and PUT requests.
	Body string `json:"body,omitempty"`

	// Timeout is the maximum duration for the request to the external
	// service. Zero means no timeout.
	Timeout caddy.Duration `json:"timeout,omitempty"`

	// TLSSkipVerify controls whether the TLS certificate presented by the
	// external service is verified. It should only be used in trusted
	// development or internal environments.
	TLSSkipVerify bool `json:"tls_skip_verify,omitempty"`

	// CacheEnabled controls whether responses from the external service
	// are cached. When true, responses are stored using the configured
	// Caddy storage backend and served from cache on subsequent matching
	// requests until the TTL expires. Default: true.
	CacheEnabled bool `json:"cache_enabled,omitempty"`

	// CacheTTL is the time-to-live for cache entries. After this duration,
	// the cached entry is considered expired and a fresh request is made
	// to the external service. A zero value means entries do not expire.
	CacheTTL caddy.Duration `json:"cache_ttl,omitempty"`

	// CacheStaleEnabled controls whether a stale (expired) cache entry is
	// served when the external service request fails. When true, if a
	// fresh HTTP call fails and a cache entry exists (even if expired),
	// the stale data is served instead of returning an error. Default: true.
	CacheStaleEnabled bool `json:"cache_stale_enabled,omitempty"`

	// CacheKeyTemplate defines the template used to build the cache key.
	// It supports Caddy placeholders. Default: "{method}:{url}".
	CacheKeyTemplate string `json:"cache_key_template,omitempty"`

	// Params are query parameters to append to the URL. Each key is the
	// parameter name; each value is a Caddy placeholder template. Values
	// are expanded and URL-encoded before being appended as query strings.
	Params map[string]string `json:"params,omitempty"`

	// client is the HTTP client configured during provisioning.
	client *http.Client

	// storage is the Caddy storage backend used for the cache. It may
	// be any registered storage module (file, redis, consul, etc.).
	storage certmagic.Storage

	// logger is used for logging warnings and errors.
	logger *zap.Logger
}

// CaddyModule returns the Caddy module information.
func (HttpService) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.http_service",
		New: func() caddy.Module { return new(HttpService) },
	}
}

// Provision validates the configuration and prepares the HTTP client and
// cache storage backend.
func (h *HttpService) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger()

	if h.URL == "" {
		return fmt.Errorf("url is required")
	}

	if h.Method == "" {
		h.Method = http.MethodGet
	}

	// Default cache key template.
	if h.CacheEnabled && h.CacheKeyTemplate == "" {
		return fmt.Errorf("cache_key_template is required when caching is enabled")
	}

	// Capture the Caddy storage backend. This may be any registered
	// implementation (file system, Redis, Consul, etc.). If storage is
	// not available, caching is effectively disabled.
	h.storage = ctx.Storage()

	h.client = &http.Client{
		Timeout: time.Duration(h.Timeout),
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: h.TLSSkipVerify,
			},
		},
	}

	return nil
}

// ServeHTTP calls the configured external HTTP service, injects JSON response
// keys as {http_service.<key>} placeholders, and passes control to the next
// handler in the middleware chain.
//
// When caching is enabled, the handler first checks the configured Caddy
// storage backend for a matching cache entry. On a cache hit, the stored
// JSON data is injected without making an outbound HTTP call. On a miss,
// the HTTP call is made and the result is cached.
//
// If CacheStaleEnabled is true and the outbound call fails, a stale
// (expired) cache entry is served as a fallback.
func (h HttpService) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)

	// Expand placeholders in URL, method, and body.
	urlStr := repl.ReplaceAll(h.URL, "")
	if len(h.Params) > 0 {
		urlStr = buildURLWithParams(urlStr, h.Params, repl, h.logger)
	}
	method := repl.ReplaceAll(h.Method, "")

	var bodyReader io.Reader
	if h.Body != "" {
		bodyStr := repl.ReplaceAll(h.Body, "")
		bodyReader = bytes.NewReader([]byte(bodyStr))
	}

	// ---- Cache lookup (before making the HTTP call) ----
	var cacheKey string
	if h.CacheEnabled && h.storage != nil {
		cacheKey = h.buildCacheKey(repl)
		if entry, ok := h.getCache(cacheKey); ok {
			if time.Now().Before(entry.ExpiresAt) || entry.ExpiresAt.IsZero() {
				// Cache hit and not expired (or no TTL configured).
				h.logger.Debug("cache hit",
					zap.String("cache_key", cacheKey),
				)
				flattenJSON("http_service", entry.Data, repl)
				return next.ServeHTTP(w, r)
			} else {
				h.logger.Debug("cache expired, fetching fresh data",
					zap.String("cache_key", cacheKey),
				)
			}
		} else {
			h.logger.Debug("cache miss",
				zap.String("cache_key", cacheKey),
			)
		}
	}

	// Build the outgoing HTTP request.
	ctx := context.WithoutCancel(r.Context())
	req, err := http.NewRequestWithContext(ctx, method, urlStr, bodyReader)
	if err != nil {
		h.logger.Warn("failed to build request to external service",
			zap.String("url", urlStr),
			zap.String("method", method),
			zap.Error(err),
		)
		return h.serveStaleOrNext(cacheKey, repl, w, r, next)
	}

	// Set headers from configuration (expand keys and values).
	for key, val := range h.Headers {
		expandedKey := repl.ReplaceAll(key, "")
		expandedVal := repl.ReplaceAll(val, "")
		req.Header.Set(expandedKey, expandedVal)
	}

	// Execute the request to the external service.
	resp, err := h.client.Do(req)
	if err != nil {
		h.logger.Warn("failed to call external service",
			zap.String("url", urlStr),
			zap.String("method", method),
			zap.Error(err),
		)
		return h.serveStaleOrNext(cacheKey, repl, w, r, next)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		h.logger.Warn("failed to read response body from external service",
			zap.String("url", urlStr),
			zap.Error(err),
		)
		return h.serveStaleOrNext(cacheKey, repl, w, r, next)
	}

	// Warn on non-2xx responses and pass through.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		h.logger.Warn("external service returned non-2xx status",
			zap.String("url", urlStr),
			zap.Int("status", resp.StatusCode),
			zap.String("body", string(bodyBytes)),
		)
		return next.ServeHTTP(w, r)
	}

	// Parse JSON and inject each key as {http_service.<key>}.
	var jsonData map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &jsonData); err != nil {
		h.logger.Warn("external service response is not valid JSON",
			zap.String("url", urlStr),
			zap.String("content_type", resp.Header.Get("Content-Type")),
			zap.Error(err),
		)
		return next.ServeHTTP(w, r)
	}

	// ---- Store in cache after a successful call ----
	if cacheKey != "" && jsonData != nil {
		h.setCache(cacheKey, jsonData, time.Duration(h.CacheTTL))
	}

	flattenJSON("http_service", jsonData, repl)

	return next.ServeHTTP(w, r)
}

// serveStaleOrNext attempts to serve a stale cache entry when the external
// service request fails. If no stale entry is available (or stale fallback
// is disabled), it passes control to the next handler.
func (h HttpService) serveStaleOrNext(cacheKey string, repl *caddy.Replacer, w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if cacheKey != "" && h.CacheStaleEnabled {
		if entry, ok := h.getCache(cacheKey); ok {
			h.logger.Warn("serving stale cache entry due to upstream failure",
				zap.String("cache_key", cacheKey),
			)
			flattenJSON("http_service", entry.Data, repl)
			return next.ServeHTTP(w, r)
		}
	}
	return next.ServeHTTP(w, r)
}

// buildCacheKey returns the storage key for the current request by expanding
// the configured CacheKeyTemplate with the request's replacer and prefixing
// the result with the storage namespace.
func (h HttpService) buildCacheKey(repl *caddy.Replacer) string {
	rawKey := repl.ReplaceAll(h.CacheKeyTemplate, "")
	return storageNamespace + rawKey
}

// getCache retrieves and deserializes a cache entry from the storage backend.
// The second return value indicates whether a valid entry was found.
// Storage errors are logged and treated as a cache miss.
func (h HttpService) getCache(key string) (cacheEntry, bool) {
	raw, err := h.storage.Load(context.Background(), key)
	if err != nil {
		if !isNotFoundErr(err) {
			h.logger.Warn("cache load error", zap.String("key", key), zap.Error(err))
		}
		return cacheEntry{}, false
	}

	var entry cacheEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		h.logger.Warn("failed to deserialize cache entry",
			zap.String("key", key),
			zap.Error(err),
		)
		return cacheEntry{}, false
	}

	return entry, true
}

// setCache serializes a cache entry and stores it in the storage backend.
// Storage errors are logged and otherwise ignored (the response was already
// served successfully).
func (h HttpService) setCache(key string, data map[string]interface{}, ttl time.Duration) {
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl)
	}

	entry := cacheEntry{
		Data:      data,
		ExpiresAt: expiresAt,
	}

	raw, err := json.Marshal(entry)
	if err != nil {
		h.logger.Warn("failed to serialize cache entry",
			zap.String("key", key),
			zap.Error(err),
		)
		return
	}

	if err := h.storage.Store(context.Background(), key, raw); err != nil {
		h.logger.Warn("cache store error",
			zap.String("key", key),
			zap.Error(err),
		)
	}
}

// buildURLWithParams appends URL-encoded query parameters to baseURL.
// Each parameter value is expanded via the replacer and then encoded.
func buildURLWithParams(baseURL string, params map[string]string, repl *caddy.Replacer, logger *zap.Logger) string {
	if len(params) == 0 {
		return baseURL
	}

	u, err := url.Parse(baseURL)
	if err != nil {
		logger.Warn("failed to parse URL for query params, using as-is",
			zap.String("url", baseURL),
			zap.Error(err),
		)
		return baseURL
	}

	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	q := u.Query()
	for _, key := range keys {
		rawVal := params[key]
		expanded := repl.ReplaceAll(rawVal, "")
		if expanded == "" {
			continue
		}
		q.Add(key, expanded)
	}
	u.RawQuery = q.Encode()

	return u.String()
}

// isNotFoundErr returns true if err indicates the key was not found in
// storage. It checks the fs.ErrNotExist sentinel first, then falls back to
// substring matching for backends that return custom error messages.
func isNotFoundErr(err error) bool {
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "no such file") ||
		strings.Contains(msg, "does not exist")
}

// flattenJSON recursively flattens a JSON object into dot-separated keys
// and sets each one on the replacer as a string value.
func flattenJSON(prefix string, data map[string]interface{}, repl *caddy.Replacer) {
	for key, val := range data {
		fullKey := prefix + "." + key
		switch v := val.(type) {
		case map[string]interface{}:
			flattenJSON(fullKey, v, repl)
		case string:
			repl.Set(fullKey, v)
		case float64:
			if v == float64(int64(v)) {
				repl.Set(fullKey, fmt.Sprintf("%d", int64(v)))
			} else {
				repl.Set(fullKey, fmt.Sprintf("%v", v))
			}
		case bool:
			repl.Set(fullKey, fmt.Sprintf("%v", v))
		case nil:
			repl.Set(fullKey, "")
		default:
			repl.Set(fullKey, fmt.Sprintf("%v", v))
		}
	}
}

// Interface guards.
var (
	_ caddy.Provisioner           = (*HttpService)(nil)
	_ caddyhttp.MiddlewareHandler = (*HttpService)(nil)
	_ caddyfile.Unmarshaler       = (*HttpService)(nil)
)
