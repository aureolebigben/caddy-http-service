package httpservice

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
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

// ServeHTTP handles the incoming request by proxying it to the configured
// external service.
func (h *HttpService) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	// Pass through to the next handler for now. The full proxy logic will
	// replace this stub.

	// Demonstrate reusable buffer pool for reading/writing request
	// bodies. A sync.Pool avoids allocating new buffers on every request.
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	_ = buf // placeholder — will be used for body reads

	return next.ServeHTTP(w, r)
}

// Interface guards.
var (
	_ caddy.Provisioner           = (*HttpService)(nil)
	_ caddyhttp.MiddlewareHandler = (*HttpService)(nil)
)
