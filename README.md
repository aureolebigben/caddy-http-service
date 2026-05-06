# http_service — Caddy HTTP Handler Module

A [Caddy v2](https://github.com/caddyserver/caddy) HTTP handler module that proxies requests to an external HTTP service, parses the JSON response, and injects each key as a `{http_service.<key>}` placeholder available to subsequent handlers in the middleware chain.

Supports response caching using Caddy's configurable storage backend (file system, Redis, Consul, etc.) with a stale-if-error fallback pattern: when the upstream service is unavailable, a stale (expired) cache entry is served instead of returning an error.

## Configuration Reference

All fields are optional unless noted otherwise.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `url` | `string` | **required** | External service URL. Supports Caddy placeholders (`{host}`, `{path}`, etc.). |
| `method` | `string` | `GET` | HTTP method for the external request. |
| `headers` | `map[string]string` | — | Custom headers sent to the external service. Keys and values support placeholders. |
| `body` | `string` | — | Request body template. Supports placeholders. Typically used with POST/PUT. |
| `timeout` | `duration` | — | Maximum duration for the external request. Zero means no timeout. |
| `tls_skip_verify` | `bool` | `false` | Skip TLS certificate verification. Only use in trusted environments. |
| `cache_enabled` | `bool` | `false` | Enable response caching via the configured Caddy storage backend. |
| `cache_ttl` | `duration` | — | Time-to-live for cache entries. A zero value means entries never expire. |
| `cache_stale_enabled` | `bool` | `false` | Serve stale (expired) cache entries when the upstream request fails. |
| `cache_key_template` | `string` | `{method}:{url}` | Template for building cache keys. Supports Caddy placeholders. |

## Placeholder Usage

After a successful external service call, every top-level JSON key is available as `{http_service.<key>}`. Nested objects are flattened with dot-separated keys.

**Example JSON response:**
```json
{
    "tenant": "acme",
    "version": 2,
    "user": {
        "name": "alice",
        "role": "admin"
    }
}
```

**Available placeholders:**
```
{http_service.tenant}         → "acme"
{http_service.version}         → "2"
{http_service.user.name}      → "alice"
{http_service.user.role}      → "admin"
```

JSON types are converted to strings:
- `string` → as-is
- `float64` → integer formatting when whole, decimal otherwise
- `bool` → `"true"` or `"false"`
- `null` → `""` (empty string)

## Cache Behavior

When `cache_enabled` is true and a Caddy storage backend is configured:

1. **Cache Hit** — A cached entry exists and hasn't expired. The stored JSON data is injected as placeholders without making an outbound HTTP call.
2. **Cache Miss** — No cached entry exists. The outbound HTTP call is made, the JSON response is injected, and the result is stored in the cache.
3. **Expired Entry** — A cached entry exists but its TTL has passed. A fresh HTTP call is made and the cache is updated.

With `cache_stale_enabled` enabled:

4. **Stale Fallback** — If the outbound HTTP call fails (connection error, timeout, 5xx) and a cache entry exists (even if expired), the stale data is served instead of passing through to the next handler with no placeholders set.

Cache keys are prefixed with `http_service/` in storage to avoid collisions with other modules.

## Caddyfile Syntax

### Caddyfile Directives

| Directive | Arguments | Description |
|-----------|-----------|-------------|
| `url` | `<string>` | External service URL (required). |
| `method` | `<string>` | HTTP method (default: `GET`). |
| `header` | `<key> <value>` | Add a custom header (repeatable). |
| `body` | `<string>` | Request body template. |
| `timeout` | `<duration>` | Request timeout (e.g., `5s`, `1m`). |
| `tls_skip_verify` | — | Skip TLS certificate verification. |
| `cache_enabled` | — | Enable response caching. |
| `cache_disabled` | — | Explicitly disable caching. |
| `cache_ttl` | `<duration>` | Cache TTL (e.g., `1h`, `30m`). |
| `cache_stale_enabled` | — | Enable stale-if-error fallback. |
| `cache_key_template` | `<string>` | Cache key template. |

### Examples

**Basic — Tenant resolution:**
```
example.com {
    http_service {
        url http://internal-api/tenants/{host}
    }
    root * /srv/{http_service.tenant}
    file_server
}
```

**With caching and Redis storage:**
```
{
    storage redis {
        host localhost
        port 6379
    }
}

geo.example.com {
    http_service {
        url https://geo-api.internal/lookup/{http.request.remote.host}
        cache_enabled
        cache_ttl 1h
        cache_stale_enabled
        cache_key_template geo:{http.request.remote.host}
    }
    header X-Geo-Country {http_service.country}
}
```

**Fallback pattern — static paths bypass the API:**
```
app.example.com {
    handle /static/* {
        root * /var/www/static
        file_server
    }
    handle {
        http_service {
            url http://api.internal/info
            cache_enabled
            cache_ttl 30m
        }
        root * /data/{http_service.tenant_id}
        file_server
    }
}
```

See the [Caddyfile](./Caddyfile) for more complete examples.

## Build & Install

Build with [xcaddy](https://github.com/caddyserver/xcaddy):

```bash
xcaddy build \
    --with github.com/aureolebigben/caddy-http-service
```

Replace the module path with your own if you've forked or renamed it.

## License

MIT — see [LICENSE](./LICENSE).
