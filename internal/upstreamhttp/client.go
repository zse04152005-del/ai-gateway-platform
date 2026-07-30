// Package upstreamhttp owns the process-wide provider HTTP connection pool.
package upstreamhttp

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"
)

var (
	// ErrInvalidRequest means an adapter produced an unsafe or incomplete request.
	ErrInvalidRequest = errors.New("upstream HTTP request is invalid")
	// ErrTransport means the provider exchange failed before a usable response existed.
	ErrTransport = errors.New("upstream HTTP transport failed")
	// ErrTimeout means a configured transport deadline expired.
	ErrTimeout = errors.New("upstream HTTP request timed out")
)

// Options contains bounded connection, TLS, header, and pooling controls.
type Options struct {
	ConnectTimeout         time.Duration
	KeepAlive              time.Duration
	TLSHandshakeTimeout    time.Duration
	ResponseHeaderTimeout  time.Duration
	TotalTimeout           time.Duration
	IdleConnTimeout        time.Duration
	ExpectContinueTimeout  time.Duration
	MaxIdleConns           int
	MaxIdleConnsPerHost    int
	MaxConnsPerHost        int
	MaxResponseHeaderBytes int64
}

// Client is safe for concurrent use and must be shared for the process lifetime.
type Client struct {
	httpClient   *http.Client
	streamClient *http.Client
	transport    *http.Transport
}

// NewClient creates one hardened provider client and reusable connection pool.
func NewClient(options Options) (*Client, error) {
	if err := options.validate(); err != nil {
		return nil, err
	}
	dialer := &net.Dialer{
		Timeout:   options.ConnectTimeout,
		KeepAlive: options.KeepAlive,
	}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            dialer.DialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           options.MaxIdleConns,
		MaxIdleConnsPerHost:    options.MaxIdleConnsPerHost,
		MaxConnsPerHost:        options.MaxConnsPerHost,
		IdleConnTimeout:        options.IdleConnTimeout,
		TLSHandshakeTimeout:    options.TLSHandshakeTimeout,
		ExpectContinueTimeout:  options.ExpectContinueTimeout,
		ResponseHeaderTimeout:  options.ResponseHeaderTimeout,
		DisableCompression:     true,
		MaxResponseHeaderBytes: options.MaxResponseHeaderBytes,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   options.TotalTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	streamClient := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Client{httpClient: client, streamClient: streamClient, transport: transport}, nil
}

// Do sends an adapter-owned request after removing browser/proxy/forwarding
// metadata that must never cross the gateway-to-provider trust boundary.
func (client *Client) Do(request *http.Request) (*http.Response, error) {
	if client == nil {
		return nil, ErrTransport
	}
	return client.do(client.httpClient, request)
}

// DoStream sends a streaming request without applying the ordinary whole-body
// timeout. Dial, TLS, and response-header deadlines still come from the shared
// Transport; the caller must use a bounded streaming Context for total,
// first-token, and no-progress deadlines.
func (client *Client) DoStream(request *http.Request) (*http.Response, error) {
	if client == nil {
		return nil, ErrTransport
	}
	return client.do(client.streamClient, request)
}

func (client *Client) do(httpClient *http.Client, request *http.Request) (*http.Response, error) {
	if httpClient == nil || client.transport == nil {
		return nil, ErrTransport
	}
	if request == nil || request.URL == nil || request.URL.Host == "" {
		return nil, ErrInvalidRequest
	}
	if request.URL.Scheme != "http" && request.URL.Scheme != "https" {
		return nil, ErrInvalidRequest
	}
	if request.URL.User != nil {
		return nil, ErrInvalidRequest
	}
	if err := request.Context().Err(); err != nil {
		return nil, errors.Join(ErrTransport, err)
	}

	outbound := request.Clone(request.Context())
	outbound.Host = ""
	outbound.Close = false
	outbound.TransferEncoding = nil
	outbound.Trailer = nil
	sanitizeHeaders(outbound.Header)
	response, err := httpClient.Do(outbound)
	if err != nil {
		if contextErr := request.Context().Err(); contextErr != nil {
			return nil, errors.Join(ErrTransport, contextErr)
		}
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			return nil, errors.Join(ErrTransport, ErrTimeout)
		}
		return nil, ErrTransport
	}
	if response == nil {
		return nil, ErrTransport
	}
	return response, nil
}

// CloseIdleConnections releases pooled idle sockets during graceful shutdown.
func (client *Client) CloseIdleConnections() {
	if client != nil && client.httpClient != nil {
		client.httpClient.CloseIdleConnections()
	}
}

func (options Options) validate() error {
	if options.ConnectTimeout <= 0 || options.ConnectTimeout > 30*time.Second {
		return errors.New("upstream connect timeout must be between 0s and 30s")
	}
	if options.KeepAlive <= 0 || options.KeepAlive > 5*time.Minute {
		return errors.New("upstream keepalive must be between 0s and 5m")
	}
	if options.TLSHandshakeTimeout <= 0 || options.TLSHandshakeTimeout > 30*time.Second {
		return errors.New("upstream TLS handshake timeout must be between 0s and 30s")
	}
	if options.ResponseHeaderTimeout <= 0 || options.ResponseHeaderTimeout > 10*time.Minute {
		return errors.New("upstream response header timeout must be between 0s and 10m")
	}
	if options.TotalTimeout <= 0 || options.TotalTimeout > 30*time.Minute {
		return errors.New("upstream total timeout must be between 0s and 30m")
	}
	if options.IdleConnTimeout <= 0 || options.IdleConnTimeout > 10*time.Minute {
		return errors.New("upstream idle connection timeout must be between 0s and 10m")
	}
	if options.ExpectContinueTimeout < 0 || options.ExpectContinueTimeout > 30*time.Second {
		return errors.New("upstream expect-continue timeout must be between 0s and 30s")
	}
	if options.MaxIdleConns < 1 || options.MaxIdleConns > 100_000 {
		return errors.New("upstream maximum idle connections must be between 1 and 100000")
	}
	if options.MaxIdleConnsPerHost < 1 || options.MaxIdleConnsPerHost > options.MaxIdleConns {
		return errors.New("upstream maximum idle connections per host must be positive and not exceed the global limit")
	}
	if options.MaxConnsPerHost < options.MaxIdleConnsPerHost || options.MaxConnsPerHost > 100_000 {
		return errors.New("upstream maximum connections per host must cover the idle per-host limit and not exceed 100000")
	}
	if options.MaxResponseHeaderBytes < 1024 || options.MaxResponseHeaderBytes > 1<<20 {
		return errors.New("upstream maximum response header bytes must be between 1024 and 1048576")
	}
	return nil
}

func sanitizeHeaders(header http.Header) {
	for _, token := range strings.Split(header.Get("Connection"), ",") {
		if name := strings.TrimSpace(token); name != "" {
			header.Del(name)
		}
	}
	for _, name := range []string{
		"Connection", "Proxy-Connection", "Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade",
		"Proxy-Authorization", "Cookie", "Cookie2", "Forwarded", "Via",
		"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Port",
		"X-Real-IP", "X-Original-Forwarded-For", "X-Original-URL", "X-Rewrite-URL",
		"CF-Connecting-IP", "True-Client-IP",
	} {
		header.Del(name)
	}
}
