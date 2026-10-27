# sdk-go

Go client for the sitewarm API. Types in [`models`](models) are generated from the OpenAPI 3.1 spec in [`openapi/openapi.yaml`](openapi/openapi.yaml) with [oapi-codegen](https://github.com/oapi-codegen/oapi-codegen); the client in the root package is hand-written on `net/http`. Go 1.24+.

**Status: pre-release (no tagged version).** The API and this client can still change. The spec describes the whole planned surface; the service may not offer every operation yet.

```go
c, err := sitewarm.New(os.Getenv("SITEWARM_API_KEY"), "https://<api host>/v1")
if err != nil { log.Fatal(err) }

site, err := c.Sites.Create(ctx, models.SiteCreate{Hostname: "example.com"})
run, err := c.WarmRuns.Create(ctx, site.Id, models.WarmRunCreate{ /* source */ })
done, err := c.WarmRuns.Wait(ctx, run.Id, sitewarm.WaitOptions{})
for r, err := range c.WarmRuns.Results(ctx, done.Id, models.Miss) {
    if err != nil { log.Fatal(err) }
    fmt.Println(r.Url)
}
```

A site must be verified before a run is accepted; verification happens outside this API.

## Surface

- Services: `Sites`, `Sitemaps`, `WarmRuns` (`Create`, `Get`, `Cancel`, `Results`, `Wait`), `CacheReport`.
- Every operation of the spec is reachable with `sitewarm.Call[T](ctx, c, "<operationId>", sitewarm.Request{...})`; `Items` and `Pages` iterate list operations (`iter.Seq2`, cursors are opaque). The operation table is generated and a test fails if it drifts.
- Webhook callbacks are server-to-customer and not covered.

## Behavior

- **Auth**: `Authorization: Bearer <key>`. The key is not in `String()`/`%#v`, errors or logs (the SDK never logs). `baseURL` must be https (http only for localhost) with no credentials; redirects are never followed (a client passed with `WithHTTPClient` is copied and its `CheckRedirect` replaced), so the key never goes to another origin. The key is held in a redacting type, so `%v`/`%+v`/`%#v` of the client never print it.
- **No default `baseURL`**: pass the API host you were given.
- **Retries**: up to 3 on `429`, `5xx` (not `501`) and network errors, exponential backoff with jitter, capped at 30 s. `Retry-After` is honored; if it exceeds the cap the error is returned instead of sleeping. `429` is always retried; `5xx`, network errors and a response body that breaks off only when repeating is safe (`GET`, `HEAD`, `PUT`, `OPTIONS`, or a POST with an `Idempotency-Key`; never `DELETE`/`PATCH`).
- **Idempotency**: every POST that accepts `Idempotency-Key` sends one (random UUID unless `WithIdempotencyKey`), generated once per call and reused by its retries, so a retried `WarmRuns.Create` never starts two runs.
- **Limits**: response bodies over 32 MiB return `*ResponseTooLargeError` (not retried); list iteration returns an error on a repeated cursor or after 10,000 pages; `ListOptions.Limit` outside 1..100 is rejected before the request. `WithTimeout(<=0)` keeps the default.
- **Errors**: `*APIError` (`Status`, `Problem` as RFC 9457, `Code`, `RetryAfter`, `RateLimit`), matchable with `errors.Is(err, sitewarm.ErrNotFound)` and the other sentinels; `*ConnectionError`; `*WaitTimeoutError` from `Wait`, which does not cancel the run.
- The only dependency is the small runtime of the type generator (`github.com/oapi-codegen/runtime`).

## Development

```
go generate ./...        # regenerate models/ and ops_gen.go from openapi/openapi.yaml
go vet ./... && go test ./... -race
```

To update the spec, replace `openapi/openapi.yaml` and run `go generate`; CI fails if generated files are out of date.

## License

Apache-2.0, see [LICENSE](LICENSE).
