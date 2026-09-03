package llm

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/NachiketKandari/pr-review-agent/xlog"
)

const defaultTimeout = 7200 * time.Second

type Options struct {
	APIBase            string
	APIKey             string
	Headers            map[string]string
	InsecureSkipVerify bool
	CABundlePath       string
	Proxy              string
	Timeout            time.Duration
}

type Client struct {
	baseURL *url.URL
	apiKey  string
	headers map[string]string
	http    *http.Client
}

func New(opts Options) (*Client, error) {
	if opts.APIBase == "" {
		return nil, fmt.Errorf("apiBase is required")
	}

	base, err := url.Parse(opts.APIBase)
	if err != nil {
		return nil, fmt.Errorf("parse apiBase: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("apiBase must use http or https, got %q", base.Scheme)
	}
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
	}

	tlsCfg := &tls.Config{InsecureSkipVerify: opts.InsecureSkipVerify}
	if opts.CABundlePath != "" {
		pem, err := os.ReadFile(opts.CABundlePath)
		if err != nil {
			return nil, fmt.Errorf("read CA bundle: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates found in %s", opts.CABundlePath)
		}
		tlsCfg.RootCAs = pool
	}

	proxy := http.ProxyFromEnvironment
	if opts.Proxy != "" {
		proxyURL, err := url.Parse(opts.Proxy)
		if err != nil {
			return nil, fmt.Errorf("parse proxy: %w", err)
		}
		proxy = http.ProxyURL(proxyURL)
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	transport := &http.Transport{
		Proxy:           proxy,
		TLSClientConfig: tlsCfg,
	}

	return &Client{
		baseURL: base,
		apiKey:  opts.APIKey,
		headers: opts.Headers,
		http: &http.Client{
			Timeout:   timeout,
			Transport: logTransport{base: transport},
		},
	}, nil
}

func (c *Client) endpoint(path string) string {
	return c.baseURL.ResolveReference(&url.URL{Path: path}).String()
}

func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path), reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("api-key", c.apiKey)
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request to %s: %w", c.endpoint(path), err)
	}
	return resp, nil
}

func checkStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	var apiErr struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(string(body))
	if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Error.Message != "" {
		msg = apiErr.Error.Message
	}

	return fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, xlog.SafeURL(resp.Request.URL.String()), msg)
}

// logTransport emits Debug-level records for every HTTP round trip with the
// URL sanitized (query strings, fragments, and userinfo stripped) so that
// credentials embedded in URLs or headers never reach the log. Response
// bodies and headers are never logged.
type logTransport struct {
	base http.RoundTripper
}

func (t logTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	attrs := []any{
		"method", req.Method,
		"url", xlog.SafeURL(req.URL.String()),
		"duration_ms", time.Since(start).Milliseconds(),
	}
	if err != nil {
		xlog.Debug("llm request failed", append(attrs, "error", err)...)
		return nil, err
	}
	if resp != nil {
		attrs = append(attrs, "status", resp.StatusCode)
	}
	xlog.Debug("llm request complete", attrs...)
	return resp, err
}
