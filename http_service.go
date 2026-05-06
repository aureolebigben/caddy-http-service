package httpservice

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(HttpService{})
}

// bufferPool provides a shared pool of bytes.Buffer instances for
// efficient reuse when reading and writing request/response bodies.
var bufferPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
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

	// client is the HTTP client configured during provisioning.
	client *http.Client

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

// Provision validates the configuration and prepares the HTTP client.
func (h *HttpService) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger()

	if h.URL == "" {
		return fmt.Errorf("url is required")
	}

	if h.Method == "" {
		h.Method = http.MethodGet
	}

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
func (h *HttpService) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)

	// Expand placeholders in URL, method, and body.
	urlStr := repl.ReplaceAll(h.URL, "")
	method := repl.ReplaceAll(h.Method, "")

	var bodyReader io.Reader
	if h.Body != "" {
		bodyStr := repl.ReplaceAll(h.Body, "")
		bodyReader = bytes.NewReader([]byte(bodyStr))
	}

	// Build the outgoing HTTP request. Use context.WithoutCancel so the
	// request can complete even if the client disconnects.
	ctx := context.WithoutCancel(r.Context())
	req, err := http.NewRequestWithContext(ctx, method, urlStr, bodyReader)
	if err != nil {
		h.logger.Warn("failed to build request to external service",
			zap.String("url", urlStr),
			zap.String("method", method),
			zap.Error(err),
		)
		return next.ServeHTTP(w, r)
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
		return next.ServeHTTP(w, r)
	}
	defer resp.Body.Close()

	// Read the response body into a reusable buffer.
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	if _, err := buf.ReadFrom(resp.Body); err != nil {
		h.logger.Warn("failed to read response body from external service",
			zap.String("url", urlStr),
			zap.Error(err),
		)
		return next.ServeHTTP(w, r)
	}

	bodyBytes := buf.Bytes()

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

	flattenJSON("http_service", jsonData, repl)

	return next.ServeHTTP(w, r)
}

// flattenJSON recursively flattens a JSON object into dot-separated keys
// and sets each one on the replacer as a string value. For example:
//
//	{"data": {"root": "abc", "count": 3}}
//
// becomes:
//
//	{http_service.data.root}  -> "abc"
//	{http_service.data.count} -> "3"
func flattenJSON(prefix string, data map[string]interface{}, repl *caddy.Replacer) {
	for key, val := range data {
		fullKey := prefix + "." + key
		switch v := val.(type) {
		case map[string]interface{}:
			flattenJSON(fullKey, v, repl)
		case string:
			repl.Set(fullKey, v)
		case float64:
			// JSON numbers decode as float64. Print without trailing .0 for
			// integer-like values.
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
)
