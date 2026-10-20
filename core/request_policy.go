package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Defaults for NewRequestPolicy. Requests are not paced by default: the run
// goes as fast as its workers allow, and only a 429 slows it down, by pausing
// every request until the server's Retry-After. An Api without a policy gets
// DefaultRetryConfig retries, 429 retried, no pacing and no shared pause.
const (
	DefaultMaxRetries        = 3
	DefaultRequestsPerMinute = 0
)

// RequestPolicy paces Photos RPCs and shares 429 cooldowns between every Api
// built with it. Create one with NewRequestPolicy and share the pointer.
type RequestPolicy struct {
	maxRetries int
	retry429   bool
	interval   time.Duration // between Photos RPCs; 0 disables pacing

	mu       sync.Mutex
	next     time.Time
	cooldown time.Time
	// adaptive paces Photos RPCs after a 429 even when interval is 0: it is
	// set to half the number of RPCs sent in the last minute and relaxed step by step
	// while requests succeed (see observe). The slower of interval and
	// adaptive applies.
	adaptive time.Duration
	okSince  int         // successful RPCs since adaptive last changed
	recent   []time.Time // when recent Photos RPCs were sent, last minute

	// logger receives request, wait, 429 and retry events; see SetLogger.
	logger *slog.Logger
	// onPause, if set, is told whenever a 429 starts or extends the pause.
	onPause func(until time.Time)
}

// OnPause registers fn to be called, outside any lock, whenever an HTTP 429
// starts or extends the shared pause; until is when requests resume. Call
// it before the run starts.
func (p *RequestPolicy) OnPause(fn func(until time.Time)) { p.onPause = fn }

// SetLogger sends the policy's events, and those of every Api built with it,
// to logger: each request (debug), waits before a request (debug), 429
// pauses (warn) and retries (info). Call it before the run starts.
func (p *RequestPolicy) SetLogger(logger *slog.Logger) { p.logger = logger }

var discardLogger = slog.New(slog.DiscardHandler)

func (p *RequestPolicy) log() *slog.Logger {
	if p == nil || p.logger == nil {
		return discardLogger
	}
	return p.logger
}

// NewRequestPolicy validates and builds a policy. maxRetries excludes the
// initial attempt; requestsPerMinute 0 disables proactive pacing but not
// server-requested cooldowns.
func NewRequestPolicy(maxRetries int, retry429 bool, requestsPerMinute int) (*RequestPolicy, error) {
	if maxRetries < 0 || maxRetries > 100 {
		return nil, fmt.Errorf("max-retries must be between 0 and 100, got %d", maxRetries)
	}
	if requestsPerMinute < 0 || requestsPerMinute > 1000000 {
		return nil, fmt.Errorf("requests-per-minute must be between 0 and 1000000, got %d", requestsPerMinute)
	}
	p := &RequestPolicy{maxRetries: maxRetries, retry429: retry429}
	if requestsPerMinute > 0 {
		p.interval = time.Minute / time.Duration(requestsPerMinute)
	}
	return p, nil
}

// cooldownJitter spreads requests that waited out a 429 pause.
const cooldownJitter = 2 * time.Second

// wait blocks until a request may be sent: after any 429 cooldown and, for
// Photos RPCs, after the pacing interval.
func (p *RequestPolicy) wait(ctx context.Context, rpc bool) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.mu.Lock()
		now := time.Now()
		interval := max(p.interval, p.adaptive)
		ready := p.cooldown
		if rpc && interval > 0 && p.next.After(ready) {
			ready = p.next
		}
		delay := ready.Sub(now)
		if delay > 0 && ready.Equal(p.cooldown) {
			// Without pacing, every waiting worker would retry the moment the
			// pause ends and trip the quota again. Spread them out a little.
			delay += time.Duration(rand.Int63n(int64(cooldownJitter)))
		}
		if delay <= 0 {
			if rpc {
				if interval > 0 {
					p.next = now.Add(interval)
				}
				p.recordRPC(now)
			}
			p.mu.Unlock()
			return nil
		}
		reason := "pacing"
		if ready.Equal(p.cooldown) {
			reason = "429 pause"
		}
		p.mu.Unlock()
		p.log().Debug("waiting before request", "reason", reason, "wait_ms", delay.Milliseconds())
		if err := waitRequest(ctx, delay); err != nil {
			return err
		}
	}
}

// Adaptive pacing after a 429: start at half the RPCs sent in the last minute, and
// speed up by a fifth after every adaptiveRelaxAfter successful RPCs until
// pacing is off again.
const (
	adaptiveRelaxAfter = 50
	adaptiveMinimum    = 20 * time.Millisecond
	// adaptiveSample is the fewest RPCs in the last minute that say our own
	// rate tripped the quota; with fewer (say, a quota already used up by
	// another run) the 429 pause alone applies.
	adaptiveSample = 20
	// adaptiveMaximum caps the slowest adaptive pace at 30 RPCs a minute.
	adaptiveMaximum = 2 * time.Second
)

// recordRPC notes that a Photos RPC is sent at now. Callers hold p.mu.
func (p *RequestPolicy) recordRPC(now time.Time) {
	cutoff := now.Add(-time.Minute)
	i := 0
	for i < len(p.recent) && p.recent[i].Before(cutoff) {
		i++
	}
	p.recent = append(p.recent[i:], now)
}

// slowDown halves the RPC rate seen in the last minute (or the current
// adaptive rate, if that is already slower). Callers hold p.mu.
func (p *RequestPolicy) slowDown() {
	p.okSince = 0
	perMinute := len(p.recent)
	if perMinute < adaptiveSample {
		return
	}
	next := min(max(time.Minute/time.Duration(perMinute/2), 2*p.adaptive), adaptiveMaximum)
	if next != p.adaptive {
		p.adaptive = next
		p.log().Info("slowing Photos requests after a 429", "requests_per_minute", int(time.Minute/next))
	}
}

// speedUp relaxes adaptive pacing after enough successful RPCs. Callers
// hold p.mu.
func (p *RequestPolicy) speedUp() {
	if p.adaptive == 0 {
		return
	}
	if p.okSince++; p.okSince < adaptiveRelaxAfter {
		return
	}
	p.okSince = 0
	p.adaptive = p.adaptive * 4 / 5
	if p.adaptive < adaptiveMinimum {
		p.adaptive = 0
		p.log().Info("Photos requests back to full speed")
		return
	}
	p.log().Debug("speeding up Photos requests", "requests_per_minute", int(time.Minute/p.adaptive))
}

// observe records the outcome of a request; rpc says whether it was a
// Photos RPC (the paced kind).
func (p *RequestPolicy) observe(resp *http.Response, rpc bool) {
	if resp.StatusCode != http.StatusTooManyRequests {
		if rpc && resp.StatusCode < 400 {
			p.mu.Lock()
			p.speedUp()
			p.mu.Unlock()
		}
		return
	}
	now := time.Now()
	until := now.Add(retryAfter(resp.Header.Get("Retry-After"), now))
	p.mu.Lock()
	// Several workers often get a 429 from the same burst; only the one that
	// starts a pause slows the rate, the others just extend the pause.
	newPause := !now.Before(p.cooldown)
	extended := until.After(p.cooldown)
	if newPause {
		p.slowDown()
	}
	if extended {
		p.cooldown = until
		host := ""
		if resp.Request != nil {
			host = resp.Request.URL.Hostname()
		}
		p.log().Warn("rate limited (HTTP 429): pausing all requests",
			"host", host, "retry_after", resp.Header.Get("Retry-After"),
			"pause_ms", until.Sub(now).Milliseconds(), "until", until.Format(time.RFC3339))
	}
	p.mu.Unlock()
	if extended && p.onPause != nil {
		p.onPause(until)
	}
}

// waitRequest sleeps for delay unless ctx ends first. An already-canceled
// ctx always wins, even over a zero delay.
func waitRequest(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// retryAfter parses a Retry-After header, waiting about a minute when absent.
func retryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(value, 10, 32); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil {
		return max(time.Duration(0), date.Sub(now))
	}
	return time.Minute + time.Duration(rand.Int63n(int64(5*time.Second)))
}

// httpStatusError is a non-2xx response from any Google endpoint. Retry
// decisions use its status code rather than matching error text.
type httpStatusError struct {
	statusCode int
	body       string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("request failed with status %d: %s", e.statusCode, e.body)
}

// shouldRetry reports whether a failed request may be retried: network
// errors, 5xx, 429 (unless the policy disables it), and the upload
// checksum mismatch that a fresh upload session resolves.
func (a *Api) shouldRetry(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// A truncated or garbled upload response committed nothing, and every
	// upload retry opens a new session, so retrying is safe.
	if errors.Is(err, errBadUploadResponse) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var httpErr *httpStatusError
	if errors.As(err, &httpErr) {
		switch {
		case httpErr.statusCode == http.StatusTooManyRequests:
			return a.policy == nil || a.policy.retry429
		case httpErr.statusCode >= 500:
			return true
		default:
			// A stale or interrupted resumable session can make the server compare the
			// declared SHA-1 with a different request body. A new upload session and a
			// fresh file read are safe for this specific 400 response.
			return httpErr.statusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(httpErr.body), "checksum from header")
		}
	}
	var networkErr net.Error
	return errors.As(err, &networkErr)
}

func (a *Api) retryConfig() RetryConfig {
	c := DefaultRetryConfig()
	if a.policy != nil {
		c.MaxRetries = a.policy.maxRetries
	}
	return c
}

// withRetry runs op until it succeeds, fails with an error shouldRetry
// rejects, or the retry budget or ctx runs out. attempt is 1-based.
func (a *Api) withRetry(ctx context.Context, config RetryConfig, op func(attempt int) error) error {
	var lastErr error
	for attempt := 1; attempt <= config.MaxRetries+1; attempt++ {
		if attempt > 1 {
			if err := waitRequest(ctx, CalculateBackoff(attempt-2, config)); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		err := op(attempt)
		if err == nil {
			return nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !a.shouldRetry(err) {
			return fmt.Errorf("failed after %d attempt(s): %w", attempt, err)
		}
		if attempt <= config.MaxRetries {
			a.policy.log().Info("retrying request", "attempt", attempt+1, "of", config.MaxRetries+1, "error", err.Error())
		}
	}
	return fmt.Errorf("failed after %d attempts: %w", config.MaxRetries+1, lastErr)
}

// do sends req under the policy, using req's own context for cancellation.
func (a *Api) do(req *http.Request) (*http.Response, error) {
	if a.policy != nil {
		rpc := req.URL.Hostname() == "photosdata-pa.googleapis.com"
		if err := a.policy.wait(req.Context(), rpc); err != nil {
			return nil, err
		}
	}
	start := time.Now()
	resp, err := a.client.Do(req)
	log := a.policy.log()
	if err != nil {
		log.Debug("request", "method", req.Method, "host", req.URL.Hostname(), "path", req.URL.Path,
			"duration_ms", time.Since(start).Milliseconds(), "error", err.Error())
		return resp, err
	}
	log.Debug("request", "method", req.Method, "host", req.URL.Hostname(), "path", req.URL.Path,
		"status", resp.StatusCode, "duration_ms", time.Since(start).Milliseconds())
	if a.policy != nil {
		a.policy.observe(resp, req.URL.Hostname() == "photosdata-pa.googleapis.com")
	}
	return resp, err
}
