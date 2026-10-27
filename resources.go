package sitewarm

import (
	"context"
	"errors"
	"iter"
	"net/url"
	"strconv"
	"time"

	"github.com/sitewarm/sdk-go/models"
)

type listPage[T any] struct {
	Data []T             `json:"data"`
	Page models.PageInfo `json:"page"`
}

// Pages yields the pages of a list operation, following page.next_cursor. Cursors are opaque.
func Pages[T any](ctx context.Context, c *Client, operationID string, req Request) iter.Seq2[[]T, error] {
	return func(yield func([]T, error) bool) {
		cursor := req.Query.Get("cursor")
		for n := 0; n < 10_000; n++ {
			q := url.Values{}
			for k, v := range req.Query {
				q[k] = v
			}
			if cursor != "" {
				q.Set("cursor", cursor)
			}
			r := req
			r.Query = q
			page, _, err := Call[listPage[T]](ctx, c, operationID, r)
			if err != nil {
				yield(nil, err)
				return
			}
			if !yield(page.Data, nil) {
				return
			}
			next := page.Page.NextCursor
			if !page.Page.HasMore || next == nil || *next == "" {
				return
			}
			if *next == cursor {
				yield(nil, errors.New("sitewarm: pagination cursor did not advance"))
				return
			}
			cursor = *next
		}
	}
}

// Items yields every item of a list operation across pages.
func Items[T any](ctx context.Context, c *Client, operationID string, req Request) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		for page, err := range Pages[T](ctx, c, operationID, req) {
			if err != nil {
				var zero T
				yield(zero, err)
				return
			}
			for _, it := range page {
				if !yield(it, nil) {
					return
				}
			}
		}
	}
}

// ListOptions are the paging query parameters shared by list operations.
type ListOptions struct {
	Cursor string
	Limit  int
}

func (o ListOptions) query() url.Values {
	q := url.Values{}
	if o.Cursor != "" {
		q.Set("cursor", o.Cursor)
	}
	if o.Limit > 0 {
		q.Set("limit", strconv.Itoa(o.Limit))
	}
	return q
}

// IdempotencyKey is an optional argument of create calls; a random key is used when omitted.
type CallOption func(*Request)

// WithIdempotencyKey sets the Idempotency-Key of a POST. Retries of the call reuse it.
func WithIdempotencyKey(k string) CallOption { return func(r *Request) { r.IdempotencyKey = k } }

func build(r Request, opts []CallOption) Request {
	for _, o := range opts {
		o(&r)
	}
	return r
}

// SitesService: operations on sites.
type SitesService struct{ c *Client }

func (s *SitesService) List(ctx context.Context, o ListOptions) (models.SiteList, error) {
	v, _, err := Call[models.SiteList](ctx, s.c, "listSites", Request{Query: o.query()})
	return v, err
}
func (s *SitesService) All(ctx context.Context) iter.Seq2[models.Site, error] {
	return Items[models.Site](ctx, s.c, "listSites", Request{})
}
func (s *SitesService) Get(ctx context.Context, siteID string) (models.Site, error) {
	v, _, err := Call[models.Site](ctx, s.c, "getSite", Request{Path: map[string]string{"siteId": siteID}})
	return v, err
}
func (s *SitesService) Create(ctx context.Context, body models.SiteCreate, opts ...CallOption) (models.Site, error) {
	v, _, err := Call[models.Site](ctx, s.c, "createSite", build(Request{Body: body}, opts))
	return v, err
}

// SitemapsService: operations on sitemaps.
type SitemapsService struct{ c *Client }

func (s *SitemapsService) Create(ctx context.Context, siteID string, body models.SitemapCreate, opts ...CallOption) (models.Sitemap, error) {
	v, _, err := Call[models.Sitemap](ctx, s.c, "createSitemap", build(Request{Path: map[string]string{"siteId": siteID}, Body: body}, opts))
	return v, err
}

// WarmRunsService: operations on warm runs.
type WarmRunsService struct{ c *Client }

// Create starts a run (202). An Idempotency-Key is always sent (random unless WithIdempotencyKey).
func (s *WarmRunsService) Create(ctx context.Context, siteID string, body models.WarmRunCreate, opts ...CallOption) (models.WarmRun, error) {
	v, _, err := Call[models.WarmRun](ctx, s.c, "createWarmRun", build(Request{Path: map[string]string{"siteId": siteID}, Body: body}, opts))
	return v, err
}
func (s *WarmRunsService) Get(ctx context.Context, runID string) (models.WarmRun, error) {
	v, _, err := Call[models.WarmRun](ctx, s.c, "getWarmRun", Request{Path: map[string]string{"runId": runID}})
	return v, err
}
func (s *WarmRunsService) Cancel(ctx context.Context, runID string, opts ...CallOption) (models.WarmRun, error) {
	v, _, err := Call[models.WarmRun](ctx, s.c, "cancelWarmRun", build(Request{Path: map[string]string{"runId": runID}}, opts))
	return v, err
}

// Results iterates all results of a run; finalState ("" for all) filters by final cache state.
func (s *WarmRunsService) Results(ctx context.Context, runID string, finalState models.CacheState) iter.Seq2[models.Result, error] {
	q := url.Values{}
	if finalState != "" {
		q.Set("final_state", string(finalState))
	}
	return Items[models.Result](ctx, s.c, "listWarmRunResults", Request{Path: map[string]string{"runId": runID}, Query: q})
}

// WaitOptions tune WarmRuns.Wait. Zero values mean the defaults.
type WaitOptions struct {
	Timeout      time.Duration // default 15m
	PollInterval time.Duration // default 5s, used when the server sends no Retry-After
	MinPoll      time.Duration // default 1s
	MaxPoll      time.Duration // default 60s
}

func terminal(s models.RunStatus) bool {
	return s == models.Completed || s == models.Canceled || s == models.Failed
}

// Wait polls the run until it is completed, canceled or failed, honoring Retry-After. On timeout it returns
// *WaitTimeoutError and does NOT cancel the run.
func (s *WarmRunsService) Wait(ctx context.Context, runID string, w WaitOptions) (models.WarmRun, error) {
	if w.Timeout == 0 {
		w.Timeout = 15 * time.Minute
	}
	if w.PollInterval == 0 {
		w.PollInterval = 5 * time.Second
	}
	if w.MinPoll == 0 {
		w.MinPoll = time.Second
	}
	if w.MaxPoll == 0 {
		w.MaxPoll = time.Minute
	}
	deadline := time.Now().Add(w.Timeout)
	for {
		run, resp, err := Call[models.WarmRun](ctx, s.c, "getWarmRun", Request{Path: map[string]string{"runId": runID}})
		if err != nil {
			return run, err
		}
		if terminal(run.Status) {
			return run, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return run, &WaitTimeoutError{RunID: runID, LastStatus: run.Status}
		}
		delay := w.PollInterval
		if ra, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
			delay = ra
		}
		delay = min(w.MaxPoll, max(w.MinPoll, delay))
		if delay >= remaining {
			if err := s.c.sleep(ctx, remaining); err != nil {
				return run, err
			}
			final, _, err := Call[models.WarmRun](ctx, s.c, "getWarmRun", Request{Path: map[string]string{"runId": runID}})
			if err != nil {
				return final, err
			}
			if terminal(final.Status) {
				return final, nil
			}
			return final, &WaitTimeoutError{RunID: runID, LastStatus: final.Status}
		}
		if err := s.c.sleep(ctx, delay); err != nil {
			return run, err
		}
	}
}

// CacheStatusReportsService: cache status reports.
type CacheStatusReportsService struct{ c *Client }

func (s *CacheStatusReportsService) Get(ctx context.Context, reportID string) (models.CacheStatusReport, error) {
	v, _, err := Call[models.CacheStatusReport](ctx, s.c, "getCacheStatusReport", Request{Path: map[string]string{"reportId": reportID}})
	return v, err
}
