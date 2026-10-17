package core

import (
	"compress/gzip"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func NewHTTPClientWithProxy(proxyURLStr string) (*http.Client, error) {
	// Create the base transport with default values
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig.InsecureSkipVerify = false

	// Configure connection pooling for concurrent uploads
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 10
	transport.MaxConnsPerHost = 10
	transport.IdleConnTimeout = 90 * time.Second

	// Configure proxy if provided
	if proxyURLStr != "" {
		proxyURL, err := url.Parse(proxyURLStr)
		if err != nil {
			return nil, err
		}
		transport.Proxy = http.ProxyURL(proxyURL)
		transport.TLSClientConfig.InsecureSkipVerify = true
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   0, // No timeout for large uploads - context handles cancellation
	}

	return client, nil
}

// RetryConfig holds configuration for retry behavior
type RetryConfig struct {
	MaxRetries   int
	InitialDelay time.Duration
	MaxDelay     time.Duration
}

// DefaultRetryConfig returns sensible defaults for retries
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:   3,
		InitialDelay: 1 * time.Second,
		MaxDelay:     30 * time.Second,
	}
}

// ShouldRetry determines if an HTTP response warrants a retry
func ShouldRetry(resp *http.Response, err error) bool {
	if err != nil {
		return true // Network errors should be retried
	}
	if resp == nil {
		return true
	}
	// Retry on 5xx server errors and 429 (rate limit)
	return resp.StatusCode >= 500 || resp.StatusCode == 429
}

// CalculateBackoff returns the delay for a given attempt (exponential backoff
// with up to 10% jitter). Doubling stops at MaxDelay, so the large attempt
// counts that --max-retries allows cannot overflow, and a zero delay cannot
// panic in rand.
func CalculateBackoff(attempt int, config RetryConfig) time.Duration {
	delay := min(config.InitialDelay, config.MaxDelay)
	for i := 0; i < attempt && delay < config.MaxDelay; i++ {
		delay = min(delay*2, config.MaxDelay)
	}
	if delay <= 0 {
		return 0
	}
	return delay + time.Duration(rand.Int63n(max(int64(1), int64(delay/10))))
}

// CheckResponse validates an HTTP response and returns an error if not successful (2xx)
func CheckResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := ReadResponseBody(resp)
	return fmt.Errorf("request failed with status %d: %s", resp.StatusCode, string(body))
}

func ReadResponseBody(resp *http.Response) ([]byte, error) {
	var reader io.Reader = resp.Body
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Encoding")), "gzip") {
		gzipReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to create gzip reader: %w", err)
		}
		defer func() { _ = gzipReader.Close() }()
		reader = gzipReader
	}
	return io.ReadAll(reader)
}
