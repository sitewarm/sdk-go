package sitewarm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNonPositiveTimeoutKeepsDefault(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		c, err := New(testKey, "https://x.test/v1", WithTimeout(d))
		if err != nil || c.timeout != 30*time.Second {
			t.Fatalf("%v timeout=%v", err, c.timeout)
		}
	}
	if c, _ := New(testKey, "https://x.test/v1", WithTimeout(time.Second)); c.timeout != time.Second {
		t.Fatalf("timeout=%v", c.timeout)
	}
	if c, _ := New(testKey, "https://x.test/v1", WithMaxRetries(-1)); c.maxRetries != 3 {
		t.Fatalf("maxRetries=%d", c.maxRetries)
	}
}

func TestBodyReadErrorIsRetried(t *testing.T) {
	var n atomic.Int32
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"id"`))
			panic(http.ErrAbortHandler) // closes the connection mid-body
		}
		writeJSON(w, 200, `{"id":"s1"}`)
	})
	s, err := c.Sites.Get(context.Background(), "s1")
	if err != nil || s.Id != "s1" || n.Load() != 2 {
		t.Fatalf("%v calls=%d", err, n.Load())
	}
}

func TestBodyReadErrorOnUnkeyedPostIsNotRetried(t *testing.T) {
	operations["fakePost"] = operation{method: "POST", path: "/fake", idempotency: IdempotencyNone}
	defer delete(operations, "fakePost")
	var n atomic.Int32
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id"`))
		panic(http.ErrAbortHandler)
	})
	_, _, err := Call[map[string]any](context.Background(), c, "fakePost", Request{})
	var ce *ConnectionError
	if !errors.As(err, &ce) || n.Load() != 1 {
		t.Fatalf("%v calls=%d", err, n.Load())
	}
}

func TestOversizedResponseIsTypedAndNotRetried(t *testing.T) {
	var n atomic.Int32
	chunk := make([]byte, 1<<20)
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(200)
		for i := 0; i < 33; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	_, err := c.Sites.Get(context.Background(), "s1")
	var tl *ResponseTooLargeError
	if !errors.As(err, &tl) || n.Load() != 1 {
		t.Fatalf("%v calls=%d", err, n.Load())
	}
}

func TestOnlyRepeatableMethodsRetryAmbiguousFailures(t *testing.T) {
	for _, m := range []string{"GET", "PUT", "DELETE", "PATCH"} {
		operations["fakeOp"] = operation{method: m, path: "/fake", idempotency: IdempotencyNone}
		var n atomic.Int32
		c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			writeJSON(w, 503, `{}`)
		}, WithMaxRetries(2))
		_, _, err := Call[map[string]any](context.Background(), c, "fakeOp", Request{})
		want := int32(1)
		if m == "GET" || m == "PUT" {
			want = 3
		}
		if !errors.Is(err, ErrServer) || n.Load() != want {
			t.Errorf("%s: %v calls=%d want=%d", m, err, n.Load(), want)
		}
	}
	delete(operations, "fakeOp")

	// 429 means "not processed": every method is retried.
	operations["fakeOp"] = operation{method: "DELETE", path: "/fake", idempotency: IdempotencyNone}
	defer delete(operations, "fakeOp")
	var n atomic.Int32
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			writeJSON(w, 429, `{}`, "Retry-After", "0")
			return
		}
		writeJSON(w, 200, `{}`)
	})
	if _, _, err := Call[map[string]any](context.Background(), c, "fakeOp", Request{}); err != nil || n.Load() != 3 {
		t.Fatalf("%v calls=%d", err, n.Load())
	}
}

func TestPaginationCursorCycleAndCap(t *testing.T) {
	// A -> B -> A: the repeated cursor is an error even though it is not the immediately previous one.
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		next := "b"
		if r.URL.Query().Get("cursor") == "b" {
			next = "a"
		}
		writeJSON(w, 200, fmt.Sprintf(`{"data":[{"id":"x"}],"page":{"has_more":true,"next_cursor":%q}}`, next))
	})
	var last error
	for _, err := range c.Sites.All(context.Background(), ListOptions{}) {
		last = err
	}
	if last == nil || !strings.Contains(last.Error(), "did not advance") {
		t.Fatalf("cycle: %v", last)
	}

	// An endless but ever-changing cursor stops at the cap with an error instead of ending silently.
	old := maxPages
	maxPages = 5
	defer func() { maxPages = old }()
	var n atomic.Int32
	endless := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, fmt.Sprintf(`{"data":[],"page":{"has_more":true,"next_cursor":"c%d"}}`, n.Add(1)))
	})
	last = nil
	for _, err := range endless.Sites.All(context.Background(), ListOptions{}) {
		last = err
	}
	if last == nil || !strings.Contains(last.Error(), "stopped after 5 pages") || n.Load() != 5 {
		t.Fatalf("cap: %v calls=%d", last, n.Load())
	}
}

func TestListLimitIsValidated(t *testing.T) {
	var n atomic.Int32
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		writeJSON(w, 200, `{"data":[],"page":{"has_more":false}}`)
	})
	for _, l := range []int{-1, 101} {
		if _, err := c.Sites.List(context.Background(), ListOptions{Limit: l}); err == nil {
			t.Errorf("limit %d accepted", l)
		}
	}
	if n.Load() != 0 {
		t.Fatalf("invalid limits reached the server: %d", n.Load())
	}
	for _, l := range []int{0, 1, 100} {
		if _, err := c.Sites.List(context.Background(), ListOptions{Limit: l}); err != nil {
			t.Errorf("limit %d: %v", l, err)
		}
	}
}

func TestKeyNeverPrintedByFmt(t *testing.T) {
	c, _ := New(testKey, "https://x.test/v1")
	for _, s := range []string{
		fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c),
		fmt.Sprintf("%v", *c), fmt.Sprintf("%+v", *c), fmt.Sprintf("%#v", *c), fmt.Sprintf("%s", c),
	} {
		if strings.Contains(s, testKey) {
			t.Fatalf("key leaked: %s", s)
		}
	}
}

func TestSuppliedHTTPClientNeverFollowsRedirects(t *testing.T) {
	var other atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { other.Add(1) }))
	defer target.Close()
	mine := &http.Client{} // follows redirects by default
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}, WithHTTPClient(mine), WithMaxRetries(0))
	_, err := c.Sites.Get(context.Background(), "s1")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 302 || other.Load() != 0 {
		t.Fatalf("%v other=%d", err, other.Load())
	}
	if mine.CheckRedirect != nil {
		t.Fatal("the caller's client was modified")
	}
}
