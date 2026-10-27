package sitewarm

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sitewarm/sdk-go/models"
)

func siteCreateBody() models.SiteCreate { return models.SiteCreate{Hostname: "example.com"} }

func TestConflictOnKeyedPostIsRetriedButKeyReusedIsNot(t *testing.T) {
	for _, tc := range []struct {
		code string
		want int32
	}{{"conflict", 3}, {"idempotency-key-reused", 1}} {
		var n atomic.Int32
		c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			writeJSON(w, 409, `{"type":"https://x.test/problems/`+tc.code+`","title":"c","status":409}`)
		}, WithMaxRetries(2))
		_, err := c.Sites.Create(context.Background(), siteCreateBody())
		var ae *APIError
		if !errors.As(err, &ae) || n.Load() != tc.want {
			t.Errorf("%s: %v calls=%d want=%d", tc.code, err, n.Load(), tc.want)
		}
		if tc.code == "idempotency-key-reused" && (!errors.Is(err, ErrIdempotencyKeyReused) || !errors.Is(err, ErrConflict)) {
			t.Errorf("sentinels: %v", err)
		}
	}
}

func TestQuotaExceededSentinel(t *testing.T) {
	err := &APIError{Status: 422, Code: "quota-exceeded"}
	if !errors.Is(err, ErrQuotaExceeded) || !errors.Is(err, ErrValidation) || errors.Is(err, ErrConflict) {
		t.Fatal("quota-exceeded sentinels")
	}
}

func TestNonTransientNetworkErrorIsNotRetried(t *testing.T) {
	var n atomic.Int32
	c, _ := New(testKey, "https://nonexistent.invalid/v1", WithRetryDelays(0, 0), WithHTTPClient(&http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			n.Add(1)
			return nil, errors.New("net/http: invalid header field value")
		}),
	}))
	_, err := c.Sites.Get(context.Background(), "s1")
	var ce *ConnectionError
	if !errors.As(err, &ce) || n.Load() != 1 {
		t.Fatalf("%v calls=%d", err, n.Load())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRetryAfterZeroStillWaitsTheBase(t *testing.T) {
	var slept []time.Duration
	var n atomic.Int32
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			writeJSON(w, 429, `{}`, "Retry-After", "0")
			return
		}
		writeJSON(w, 200, `{"id":"s1"}`)
	}, WithRetryDelays(200*time.Millisecond, 5*time.Second))
	c.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	if _, err := c.Sites.Get(context.Background(), "s1"); err != nil || len(slept) != 1 || slept[0] < 200*time.Millisecond {
		t.Fatalf("%v slept=%v", err, slept)
	}
}

func TestIdempotencyKeyIsValidated(t *testing.T) {
	var n atomic.Int32
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) { n.Add(1); writeJSON(w, 200, `{"id":"s1"}`) })
	for _, k := range []string{"has space", "bad\nkey", strings.Repeat("a", 256), "ñ"} {
		if _, err := c.Sites.Create(context.Background(), siteCreateBody(), WithIdempotencyKey(k)); err == nil {
			t.Errorf("key %q accepted", k)
		}
	}
	// A key on an operation that takes none is an error, not silently dropped.
	if _, err := c.Sites.Get(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Call[map[string]any](context.Background(), c, "getSite", Request{Path: map[string]string{"siteId": "s1"}, IdempotencyKey: "k"}); err == nil {
		t.Error("key on GET accepted")
	}
	if n.Load() != 1 {
		t.Fatalf("invalid calls reached the server: %d", n.Load())
	}
}

func TestNonJSONSuccessIsDecodeError(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("<html>proxy</html>"))
	})
	_, err := c.Sites.Get(context.Background(), "s1")
	var de *DecodeError
	if !errors.As(err, &de) || de.Status != 200 {
		t.Fatalf("%v", err)
	}
}

func TestUserAgentAndEscapedBasePath(t *testing.T) {
	c, _ := New(testKey, "https://x.test/a%2Fb/v1", WithUserAgent(""))
	if !strings.HasPrefix(c.userAgent, "sitewarm-go/"+Version) || c.baseURL != "https://x.test/a%2Fb/v1" {
		t.Fatalf("%q %q", c.userAgent, c.baseURL)
	}
}

func TestDotSegmentIDsAreRejected(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) { t.Error("request sent") })
	for _, id := range []string{".", ".."} {
		if _, err := c.Sites.Get(context.Background(), id); err == nil {
			t.Errorf("id %q accepted", id)
		}
	}
}

func TestSitesAllValidatesLimit(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) { t.Error("request sent") })
	var got error
	for _, err := range c.Sites.All(context.Background(), ListOptions{Limit: 500}) {
		got = err
	}
	if got == nil {
		t.Fatal("limit accepted")
	}
}
