// Package sitewarm is a client for the sitewarm API (pre-release).
//
// Types are generated from the OpenAPI spec into the models package; this package is the hand-written
// HTTP client. Every operation is reachable through [Call] by its operationId.
package sitewarm

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sitewarm/sdk-go/models"
)

// IdempotencyMode says whether an operation takes an Idempotency-Key.
type IdempotencyMode int

const (
	IdempotencyNone IdempotencyMode = iota
	IdempotencyOptional
	IdempotencyRequired
)

// Version is the SDK version, sent in the User-Agent.
const Version = "0.1.0-preview"

const defaultUserAgent = "sitewarm-go/" + Version + " (+https://github.com/sitewarm/sdk-go)"

type operation struct {
	method, path string
	idempotency  IdempotencyMode
}

// Client talks to the API. It is safe for concurrent use.
type Client struct {
	apiKey      *secret // pointer: fmt cannot reach into unexported fields, so a value would print raw
	baseURL     string
	http        *http.Client
	maxRetries  int
	retryBase   time.Duration
	retryMax    time.Duration
	timeout     time.Duration
	userAgent   string
	sleep       func(context.Context, time.Duration) error
	Sites       *SitesService
	Sitemaps    *SitemapsService
	WarmRuns    *WarmRunsService
	CacheReport *CacheStatusReportsService
}

// Option configures New.
type Option func(*Client)

// WithHTTPClient sets the HTTP client (default: a fresh client). The client is copied and its CheckRedirect
// replaced: redirects are never followed, whatever policy h had, so the key never goes to another origin.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			cp := *h
			c.http = &cp
		}
	}
}

// WithMaxRetries sets the retries after the first attempt (default 3).
func WithMaxRetries(n int) Option {
	return func(c *Client) {
		if n >= 0 {
			c.maxRetries = n
		}
	}
}

// WithRetryDelays sets the backoff base (default 500ms) and the cap for one wait (default 30s).
func WithRetryDelays(base, max time.Duration) Option {
	return func(c *Client) { c.retryBase, c.retryMax = base, max }
}

// WithTimeout sets the per-attempt timeout (default 30s). A value <= 0 keeps the default.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// WithUserAgent overrides the User-Agent. An empty string keeps the default.
func WithUserAgent(s string) Option {
	return func(c *Client) {
		if s != "" {
			c.userAgent = s
		}
	}
}

var keyRE = regexp.MustCompile(`^[\x21-\x7e]+$`)

// New validates the arguments (errors never echo the key). baseURL has no default: pass the API host you
// were given, including the version prefix, for example "https://<host>/v1". https only (http for loopback).
func New(apiKey, baseURL string, opts ...Option) (*Client, error) {
	if !keyRE.MatchString(apiKey) {
		return nil, errors.New("sitewarm: apiKey must be non-empty printable ASCII without whitespace")
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("sitewarm: baseURL must be an absolute URL such as https://<host>/v1")
	}
	loop := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && loop) {
		return nil, errors.New("sitewarm: baseURL must use https (http only for localhost)")
	}
	if u.User != nil {
		return nil, errors.New("sitewarm: baseURL must not contain credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("sitewarm: baseURL must not contain a query or fragment")
	}
	c := &Client{
		apiKey: newSecret(apiKey), baseURL: u.Scheme + "://" + u.Host + strings.TrimRight(u.EscapedPath(), "/"),
		maxRetries: 3, retryBase: 500 * time.Millisecond, retryMax: 30 * time.Second, timeout: 30 * time.Second,
		userAgent: defaultUserAgent, sleep: sleepCtx,
	}
	for _, o := range opts {
		o(c)
	}
	if c.http == nil {
		c.http = &http.Client{}
	}
	c.http.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.Sites, c.Sitemaps, c.WarmRuns, c.CacheReport = &SitesService{c}, &SitemapsService{c}, &WarmRunsService{c}, &CacheStatusReportsService{c}
	return c, nil
}

// String never contains the key.
func (c *Client) String() string { return "sitewarm.Client{baseURL: " + c.baseURL + "}" }

// GoString keeps %#v from printing the key.
func (c *Client) GoString() string { return c.String() }

// Request carries the arguments of one operation.
type Request struct {
	Path  map[string]string
	Query url.Values
	Body  any
	// IdempotencyKey overrides the random key sent for operations that accept one.
	IdempotencyKey string
}

// Response is the transport data of a successful call.
type Response struct {
	Status int
	Header http.Header
}

// Call performs one operation by operationId and decodes the JSON body into T.
func Call[T any](ctx context.Context, c *Client, operationID string, req Request) (T, *Response, error) {
	var out T
	resp, err := c.do(ctx, operationID, req, &out)
	return out, resp, err
}

func (c *Client) do(ctx context.Context, operationID string, req Request, out any) (*Response, error) {
	op, ok := operations[operationID]
	if !ok {
		return nil, fmt.Errorf("sitewarm: unknown operation %q", operationID)
	}
	path, err := expandPath(op.path, req.Path)
	if err != nil {
		return nil, err
	}
	u := c.baseURL + path
	if q := req.Query.Encode(); q != "" {
		u += "?" + q
	}
	var body []byte
	if req.Body != nil {
		if body, err = json.Marshal(req.Body); err != nil {
			return nil, fmt.Errorf("sitewarm: encoding body: %w", err)
		}
	}
	sendsKey := op.idempotency != IdempotencyNone && op.method == http.MethodPost
	key := req.IdempotencyKey
	if key != "" {
		if !sendsKey {
			return nil, fmt.Errorf("sitewarm: operation %q does not accept an Idempotency-Key", operationID)
		}
		if len(key) > 255 || !keyRE.MatchString(key) {
			return nil, errors.New("sitewarm: IdempotencyKey must be 1-255 printable ASCII characters without whitespace")
		}
	}
	if sendsKey && key == "" {
		key = newUUID() // one key per call, reused by every retry
	}
	// 429 = not processed. 5xx and network errors may have been, so retry them only when repeating is safe:
	// a method that is safe to repeat, or a POST that carries an Idempotency-Key. DELETE and PATCH are not retried.
	retryAmbiguous := repeatable(op.method) || sendsKey

	for attempt := 0; ; attempt++ {
		last := attempt >= c.maxRetries
		resp, err := c.attempt(ctx, op.method, u, body, key, sendsKey)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if !last && retryAmbiguous && transient(err) {
				if serr := c.sleep(ctx, c.backoff(attempt)); serr != nil {
					return nil, serr
				}
				continue
			}
			return nil, connectionError(err)
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		resp.Body.Close()
		if rerr != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// A body that breaks off mid-read is as ambiguous as a network error, except after a 429.
			if !last && (retryAmbiguous || resp.StatusCode == 429) {
				if serr := c.sleep(ctx, c.backoff(attempt)); serr != nil {
					return nil, serr
				}
				continue
			}
			return nil, connectionError(rerr)
		}
		if len(data) > maxBody {
			return nil, &ResponseTooLargeError{Limit: maxBody}
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if out != nil && len(bytes.TrimSpace(data)) > 0 && resp.StatusCode != http.StatusNoContent {
				if jerr := json.Unmarshal(data, out); jerr != nil {
					return nil, &DecodeError{Status: resp.StatusCode, Err: jerr}
				}
			}
			return &Response{Status: resp.StatusCode, Header: resp.Header}, nil
		}
		retryAfter, hasRA := parseRetryAfter(resp.Header.Get("Retry-After"))
		aerr := apiError(resp.StatusCode, data, retryAfter, resp.Header)
		retryable := resp.StatusCode == 429 || (resp.StatusCode >= 500 && resp.StatusCode != 501 && retryAmbiguous)
		// A 409 "conflict" on a keyed POST is the original request still running (a timeout retry racing it).
		// "idempotency-key-reused" is a caller bug and never retried.
		if resp.StatusCode == 409 && sendsKey && aerr.Code == "conflict" {
			retryable = true
		}
		if retryable && !last {
			wait := c.backoff(attempt)
			if hasRA {
				// Never a hot retry on "Retry-After: 0" or a date in the past; jitter spreads clients out.
				wait = max(retryAfter, c.retryBase)
				if wait <= c.retryMax { // a longer server-provided wait is surfaced, not ignored
					wait += time.Duration(mrand.Float64() * float64(wait) / 4)
				}
			}
			if wait <= c.retryMax || (hasRA && max(retryAfter, c.retryBase) <= c.retryMax) {
				if serr := c.sleep(ctx, wait); serr != nil {
					return nil, serr
				}
				continue
			}
		}
		return nil, aerr
	}
}

func (c *Client) attempt(ctx context.Context, method, u string, body []byte, key string, sendsKey bool) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	r, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, &permanentError{err}
	}
	r.Header.Set("Authorization", "Bearer "+string(*c.apiKey))
	r.Header.Set("Accept", "application/json, application/problem+json")
	r.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if sendsKey {
		r.Header.Set("Idempotency-Key", key)
	}
	resp, err := c.http.Do(r)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelBody{resp.Body, cancel}
	return resp, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error { defer b.cancel(); return b.ReadCloser.Close() }

// maxBody is the largest response body that is read (32 MiB).
const maxBody = 32 << 20

// secret keeps the API key out of fmt output (%v, %+v, %#v), JSON and text encodings.
type secret string

const redacted = "[redacted]"

func (secret) String() string               { return redacted }
func (secret) GoString() string             { return redacted }
func (secret) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, redacted) }
func (secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }
func (secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

func newSecret(s string) *secret { v := secret(s); return &v }

// repeatable reports whether sending the method twice has the same effect as once.
func repeatable(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodOptions:
		return true
	}
	return false
}

// permanentError marks a failure that repeating cannot fix.
type permanentError struct{ error }

func (e *permanentError) Unwrap() error { return e.error }

// transient reports whether a transport error may go away on a retry. Certificate failures, unknown hosts and
// malformed requests do not.
func transient(err error) bool {
	var perm *permanentError
	var unknownAuth x509.UnknownAuthorityError
	var certInvalid x509.CertificateInvalidError
	var hostErr x509.HostnameError
	var verify *tls.CertificateVerificationError
	var dns *net.DNSError
	var addr *net.AddrError
	switch {
	case errors.As(err, &perm), errors.As(err, &unknownAuth), errors.As(err, &certInvalid), errors.As(err, &hostErr),
		errors.As(err, &verify), errors.As(err, &addr):
		return false
	case errors.As(err, &dns):
		return !dns.IsNotFound
	}
	return !strings.Contains(err.Error(), "invalid header field")
}

func connectionError(err error) *ConnectionError {
	ce := &ConnectionError{Err: scrub(err)}
	var ne interface{ Timeout() bool }
	ce.Timeout = errors.As(err, &ne) && ne.Timeout()
	return ce
}

// scrub drops the request URL/headers from transport errors so nothing sensitive is carried along.
func scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func (c *Client) backoff(attempt int) time.Duration {
	ceil := math.Min(float64(c.retryMax), float64(c.retryBase)*math.Pow(2, float64(attempt)))
	return time.Duration(ceil/2 + mrand.Float64()*ceil/2)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func parseRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if n, err := strconv.ParseUint(v, 10, 32); err == nil {
		return time.Duration(n) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return d.Round(time.Second), true
	}
	return 0, false
}

func apiError(status int, body []byte, retryAfter time.Duration, h http.Header) *APIError {
	p := models.Problem{Type: "about:blank", Title: http.StatusText(status), Status: status}
	var parsed models.Problem
	if json.Unmarshal(body, &parsed) == nil && (parsed.Title != "" || parsed.Type != "") {
		p = parsed
		if p.Type == "" {
			p.Type = "about:blank"
		}
		if p.Status == 0 {
			p.Status = status
		}
	}
	num := func(name string) int {
		if n, err := strconv.Atoi(strings.TrimSpace(h.Get(name))); err == nil && n >= 0 {
			return n
		}
		return -1
	}
	return &APIError{
		Status: status, Problem: p, Code: problemCode(p), RetryAfter: retryAfter,
		RateLimit: RateLimit{Limit: num("RateLimit-Limit"), Remaining: num("RateLimit-Remaining"), Reset: num("RateLimit-Reset")},
	}
}

var pathParamRE = regexp.MustCompile(`\{(\w+)\}`)

func expandPath(tmpl string, values map[string]string) (string, error) {
	var missing, bad string
	out := pathParamRE.ReplaceAllStringFunc(tmpl, func(m string) string {
		name := m[1 : len(m)-1]
		v := values[name]
		if v == "" {
			missing = name
		} else if v == "." || v == ".." {
			bad = name
		}
		return url.PathEscape(v)
	})
	if missing != "" {
		return "", fmt.Errorf("sitewarm: missing path parameter %q", missing)
	}
	if bad != "" {
		return "", fmt.Errorf("sitewarm: invalid path parameter %q", bad)
	}
	return out, nil
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
