package sitewarm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sitewarm/sdk-go/models"
)

const testKey = "swk_secret_key"

func newTest(t *testing.T, h http.HandlerFunc, opts ...Option) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(testKey, srv.URL+"/v1", append([]Option{WithRetryDelays(0, 0)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func writeJSON(w http.ResponseWriter, status int, body string, hdr ...string) {
	for i := 0; i+1 < len(hdr); i += 2 {
		w.Header().Set(hdr[i], hdr[i+1])
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func TestRequestShapeAndAuth(t *testing.T) {
	var got *http.Request
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		writeJSON(w, 200, `{"id":"s1","hostname":"example.com"}`)
	})
	s, err := c.Sites.Get(context.Background(), "a/b")
	if err != nil || s.Id != "s1" {
		t.Fatalf("%v %+v", err, s)
	}
	if got.URL.EscapedPath() != "/v1/sites/a%2Fb" || got.Header.Get("Authorization") != "Bearer "+testKey {
		t.Fatalf("path=%s auth=%s", got.URL.EscapedPath(), got.Header.Get("Authorization"))
	}
}

func TestProblemToTypedError(t *testing.T) {
	cases := map[int]error{401: ErrAuthentication, 403: ErrPermission, 404: ErrNotFound, 409: ErrConflict, 422: ErrValidation, 429: ErrRateLimited, 500: ErrServer}
	for status, sentinel := range cases {
		c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, status, `{"type":"https://x.test/problems/some-slug","title":"T","detail":"D","status":1}`,
				"Retry-After", "0", "RateLimit-Remaining", "0", "Content-Type", "application/problem+json")
		}, WithMaxRetries(0))
		_, err := c.Sites.Get(context.Background(), "s1")
		var ae *APIError
		if !errors.As(err, &ae) || !errors.Is(err, sentinel) {
			t.Fatalf("%d: %v", status, err)
		}
		if ae.Status != status || ae.Code != "some-slug" || ae.RateLimit.Remaining != 0 || ae.RateLimit.Limit != -1 {
			t.Fatalf("%+v", ae)
		}
		if strings.Contains(err.Error(), testKey) {
			t.Fatal("key leaked")
		}
	}
}

func TestRetries429ThenSucceeds(t *testing.T) {
	var n atomic.Int32
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			writeJSON(w, 429, `{}`, "Retry-After", "0")
			return
		}
		writeJSON(w, 200, `{"id":"s1"}`)
	})
	if _, err := c.Sites.Get(context.Background(), "s1"); err != nil || n.Load() != 3 {
		t.Fatalf("%v calls=%d", err, n.Load())
	}
}

func TestRetryAfterBeyondCapIsSurfaced(t *testing.T) {
	var n atomic.Int32
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		writeJSON(w, 429, `{}`, "Retry-After", "120")
	}, WithRetryDelays(0, 5*time.Second))
	_, err := c.Sites.Get(context.Background(), "s1")
	var ae *APIError
	if !errors.As(err, &ae) || n.Load() != 1 || ae.RetryAfter != 120*time.Second {
		t.Fatalf("%v calls=%d", err, n.Load())
	}
}

func TestPostWithoutKeyNotRetriedKeyedPostIs(t *testing.T) {
	operations["fakePost"] = operation{method: "POST", path: "/fake", idempotency: IdempotencyNone}
	defer delete(operations, "fakePost")
	var n atomic.Int32
	var keys []string
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		writeJSON(w, 503, `{}`)
	}, WithMaxRetries(2))
	_, _, err := Call[map[string]any](context.Background(), c, "fakePost", Request{})
	if !errors.Is(err, ErrServer) || n.Load() != 1 || keys[0] != "" {
		t.Fatalf("%v calls=%d keys=%v", err, n.Load(), keys)
	}

	n.Store(0)
	keys = nil
	_, err = c.WarmRuns.Create(context.Background(), "s1", models.WarmRunCreate{})
	if !errors.Is(err, ErrServer) || n.Load() != 3 {
		t.Fatalf("%v calls=%d", err, n.Load())
	}
	if keys[0] == "" || keys[0] != keys[1] || keys[1] != keys[2] {
		t.Fatalf("one key per call expected: %v", keys)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(keys[0]) {
		t.Fatalf("not a uuid v4: %s", keys[0])
	}
}

func TestGivenIdempotencyKeyIsSent(t *testing.T) {
	var key string
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		key = r.Header.Get("Idempotency-Key")
		writeJSON(w, 202, `{"id":"run1"}`)
	})
	if _, err := c.WarmRuns.Create(context.Background(), "s1", models.WarmRunCreate{}, WithIdempotencyKey("mine")); err != nil || key != "mine" {
		t.Fatalf("%v %s", err, key)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var other atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { other.Add(1) }))
	defer target.Close()
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}, WithMaxRetries(0))
	_, err := c.Sites.Get(context.Background(), "s1")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 302 || other.Load() != 0 {
		t.Fatalf("%v other=%d", err, other.Load())
	}
}

func TestConnectionErrorHasNoKey(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c, _ := New(testKey, url+"/v1", WithMaxRetries(0))
	_, err := c.Sites.Get(context.Background(), "s1")
	var ce *ConnectionError
	if !errors.As(err, &ce) || strings.Contains(err.Error(), testKey) || strings.Contains(ce.Err.Error(), testKey) {
		t.Fatalf("%v", err)
	}
}

func TestItemsFollowsCursorAndDetectsStall(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			writeJSON(w, 200, `{"data":[{"id":"a"},{"id":"b"}],"page":{"has_more":true,"next_cursor":"c1"}}`)
			return
		}
		writeJSON(w, 200, `{"data":[{"id":"c"}],"page":{"has_more":false,"next_cursor":null}}`)
	})
	var ids []string
	for s, err := range c.Sites.All(context.Background()) {
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.Id)
	}
	if strings.Join(ids, ",") != "a,b,c" {
		t.Fatalf("%v", ids)
	}

	stuck := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, `{"data":[{"id":"a"}],"page":{"has_more":true,"next_cursor":"c1"}}`)
	})
	var last error
	for _, err := range stuck.Sites.All(context.Background()) {
		last = err
	}
	if last == nil || !strings.Contains(last.Error(), "did not advance") {
		t.Fatalf("%v", last)
	}
}

func TestWaitPollsUntilTerminalAndTimesOutWithoutCancel(t *testing.T) {
	statuses := []string{"queued", "running", "completed"}
	var i atomic.Int32
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		s := statuses[min(int(i.Add(1))-1, 2)]
		writeJSON(w, 200, `{"id":"r1","status":"`+s+`"}`, "Retry-After", "0")
	})
	run, err := c.WarmRuns.Wait(context.Background(), "r1", WaitOptions{MinPoll: time.Nanosecond})
	if err != nil || run.Status != models.Completed {
		t.Fatalf("%v %v", err, run.Status)
	}

	var methods atomic.Value
	methods.Store("")
	never := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			methods.Store(r.Method)
		}
		writeJSON(w, 200, `{"id":"r1","status":"running"}`)
	})
	_, err = never.WarmRuns.Wait(context.Background(), "r1", WaitOptions{Timeout: 20 * time.Millisecond, PollInterval: time.Millisecond, MinPoll: time.Millisecond})
	var wt *WaitTimeoutError
	if !errors.As(err, &wt) || methods.Load() != "" {
		t.Fatalf("%v method=%v", err, methods.Load())
	}
}

func TestNewValidation(t *testing.T) {
	for _, u := range []string{"http://api.example.test/v1", "https://u:p@api.example.test", "https://x.test/v1?a=1", "api.example.test", ""} {
		if _, err := New(testKey, u); err == nil {
			t.Errorf("accepted %q", u)
		}
	}
	if _, err := New(testKey, "http://localhost:8080/v1"); err != nil {
		t.Error(err)
	}
	for _, k := range []string{"", "has space", "tab\t"} {
		if _, err := New(k, "https://x.test/v1"); err == nil || strings.Contains(err.Error(), "has space") {
			t.Errorf("key %q: %v", k, err)
		}
	}
}

func TestClientStringsHideKey(t *testing.T) {
	c, _ := New(testKey, "https://x.test/v1")
	if strings.Contains(c.String()+c.GoString(), testKey) {
		t.Fatal("key leaked")
	}
}

func TestUnknownOperationAndMissingPathParam(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, `{}`) })
	if _, _, err := Call[any](context.Background(), c, "nope", Request{}); err == nil || !strings.Contains(err.Error(), "unknown operation") {
		t.Fatal(err)
	}
	if _, _, err := Call[any](context.Background(), c, "getSite", Request{}); err == nil || !strings.Contains(err.Error(), "missing path parameter") {
		t.Fatal(err)
	}
}

func TestOperationTableMatchesSpec(t *testing.T) {
	raw, err := os.ReadFile("openapi/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(operations); got != 36 {
		t.Fatalf("operations = %d", got)
	}
	if strings.Contains(string(raw), "x-open-question") {
		t.Fatal("spec carries x-open-question")
	}
	if operations["createWarmRun"].idempotency != IdempotencyRequired || operations["getWarmRun"].idempotency != IdempotencyNone {
		t.Fatal("idempotency table wrong")
	}
	for id, op := range operations {
		if op.method == "" || !strings.HasPrefix(op.path, "/") {
			t.Fatalf("%s: %+v", id, op)
		}
	}
}
