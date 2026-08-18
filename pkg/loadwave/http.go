// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package loadwave

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// HTTPOptions configures how a run talks HTTP. Defaults are tuned for load
// generation rather than for a typical application client.
type HTTPOptions struct {
	// BaseURL is prefixed to relative request paths.
	BaseURL string

	// Timeout bounds a whole request, including body transfer. Zero applies
	// DefaultHTTPTimeout; a load test with no timeout will eventually wedge
	// every VU behind one unresponsive endpoint.
	Timeout time.Duration

	// Headers are sent with every request. Per-request headers win on
	// conflict.
	Headers http.Header

	// UserAgent overrides the default User-Agent header.
	UserAgent string

	// InsecureSkipTLSVerify disables certificate validation. Load
	// environments frequently use self-signed certificates; production ones
	// should not.
	InsecureSkipTLSVerify bool

	// MaxIdleConnsPerHost caps pooled idle connections per host. Go's default
	// is 2, which throttles a load test to a trickle of connection churn and
	// makes it measure the client rather than the server. Zero applies
	// DefaultMaxIdleConnsPerHost.
	MaxIdleConnsPerHost int

	// DisableKeepAlives forces a fresh connection per request, which measures
	// connection setup cost as well as request cost.
	DisableKeepAlives bool

	// DisableCompression stops the transport requesting gzip.
	DisableCompression bool

	// FollowRedirects makes the client follow 3xx responses. Off by default:
	// a load test usually wants to measure the redirect itself.
	FollowRedirects bool

	// MaxRedirects caps redirect depth when FollowRedirects is set. Zero
	// applies DefaultMaxRedirects.
	MaxRedirects int

	// IsolatePerVU gives every virtual user its own connection pool, so each
	// behaves like a distinct client. More faithful, but costs a file
	// descriptor per VU per host and will hit ulimits at high VU counts.
	// Off by default: VUs share one pool.
	IsolatePerVU bool

	// DiscardBody streams response bodies to nowhere instead of buffering
	// them. Bytes are still counted. Use it when scenarios never inspect
	// bodies and responses are large.
	DiscardBody bool

	// MaxBodyBytes caps how much of a response body is buffered. Zero applies
	// DefaultMaxBodyBytes. Bytes beyond the cap are read and counted but
	// discarded, so the server still does the full work.
	MaxBodyBytes int64

	// Trace collects connection-level timings — time to first byte, connect
	// and TLS handshake duration — via httptrace. Costs a few hundred
	// nanoseconds per request. On by default.
	Trace *bool

	// Proxy is an optional proxy URL. Empty uses the environment's proxy
	// settings.
	Proxy string

	// IsSuccess decides whether a response counts toward the failure rate.
	// The default treats a 2xx or 3xx status with no transport error as
	// success.
	IsSuccess func(*Response) bool

	// BetweenRequests pauses after every request, whatever its outcome.
	//
	// This is the run's pacing floor. Without it a scenario with no explicit
	// think time loops as fast as the network allows, and one whose request
	// fails instantly loops as fast as the CPU allows — which is how a load
	// test becomes an accidental denial of service against a service that has
	// already fallen over.
	//
	// The zero value applies DefaultBetweenRequests. Use NoBetweenRequests to
	// mean genuinely none, which is what a pure throughput test wants.
	BetweenRequests Pause

	// NoBetweenRequests disables pacing entirely, overriding BetweenRequests.
	//
	// A separate flag rather than a zero duration because zero has to keep
	// meaning "not configured": a run that never mentions pacing should get
	// the safe default, not flat out.
	NoBetweenRequests bool
}

// Defaults applied when the corresponding HTTPOptions field is zero.
const (
	DefaultHTTPTimeout         = 30 * time.Second
	DefaultMaxIdleConnsPerHost = 512
	DefaultMaxRedirects        = 10
	DefaultMaxBodyBytes        = 4 << 20 // 4 MiB
)

// DefaultIsSuccess treats a response as successful when the transport
// succeeded and the status is below 400.
func DefaultIsSuccess(r *Response) bool {
	return r.Err == nil && r.StatusCode >= 100 && r.StatusCode < 400
}

func (o *HTTPOptions) applyDefaults() {
	if o.Timeout <= 0 {
		o.Timeout = DefaultHTTPTimeout
	}
	if o.MaxIdleConnsPerHost <= 0 {
		o.MaxIdleConnsPerHost = DefaultMaxIdleConnsPerHost
	}
	if o.MaxRedirects <= 0 {
		o.MaxRedirects = DefaultMaxRedirects
	}
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if o.Trace == nil {
		on := true
		o.Trace = &on
	}
	if o.IsSuccess == nil {
		o.IsSuccess = DefaultIsSuccess
	}
	switch {
	case o.NoBetweenRequests:
		o.BetweenRequests = Pause{}
	case o.BetweenRequests.IsZero():
		o.BetweenRequests = NewPause(DefaultBetweenRequests)
	}
}

// HTTPClientFactory builds one HTTPClient per virtual user.
//
// It exists so that the expensive, shareable part — the transport and its
// connection pool — is built once per worker process, while the cheap
// per-user part is built ten thousand times.
type HTTPClientFactory struct {
	opts      HTTPOptions
	base      *url.URL
	shared    *http.Transport
	transport func() *http.Transport
}

// NewHTTPClientFactory validates the options and prepares the shared state.
func NewHTTPClientFactory(opts HTTPOptions) (*HTTPClientFactory, error) {
	opts.applyDefaults()

	var base *url.URL
	if opts.BaseURL != "" {
		parsed, err := url.Parse(opts.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("parse base URL %q: %w", opts.BaseURL, err)
		}
		if parsed.Scheme == "" || parsed.Host == "" {
			return nil, fmt.Errorf("base URL %q must be absolute, with a scheme and host", opts.BaseURL)
		}
		base = parsed
	}

	var proxy = http.ProxyFromEnvironment
	if opts.Proxy != "" {
		proxyURL, err := url.Parse(opts.Proxy)
		if err != nil {
			return nil, fmt.Errorf("parse proxy URL %q: %w", opts.Proxy, err)
		}
		proxy = http.ProxyURL(proxyURL)
	}

	newTransport := func() *http.Transport {
		return &http.Transport{
			Proxy: proxy,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          0, // unlimited; the per-host cap governs
			MaxIdleConnsPerHost:   opts.MaxIdleConnsPerHost,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			DisableKeepAlives:     opts.DisableKeepAlives,
			DisableCompression:    opts.DisableCompression,
			//nolint:gosec // Opt-in, and load environments routinely use self-signed certificates.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: opts.InsecureSkipTLSVerify},
		}
	}

	f := &HTTPClientFactory{opts: opts, base: base, transport: newTransport}
	if !opts.IsolatePerVU {
		f.shared = newTransport()
	}
	return f, nil
}

// New returns a client for one virtual user.
func (f *HTTPClientFactory) New() *HTTPClient {
	transport := f.shared
	owned := false
	if transport == nil {
		transport = f.transport()
		owned = true
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   f.opts.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if !f.opts.FollowRedirects {
				return http.ErrUseLastResponse
			}
			if len(via) >= f.opts.MaxRedirects {
				return fmt.Errorf("stopped after %d redirects", f.opts.MaxRedirects)
			}
			return nil
		},
	}

	return &HTTPClient{opts: &f.opts, base: f.base, client: client, transport: transport, owned: owned}
}

// Close releases the shared transport's pooled connections.
func (f *HTTPClientFactory) Close() {
	if f.shared != nil {
		f.shared.CloseIdleConnections()
	}
}

// HTTPClient issues requests on behalf of a single virtual user and records
// the standard HTTP metrics for each one.
type HTTPClient struct {
	opts      *HTTPOptions
	base      *url.URL
	client    *http.Client
	transport *http.Transport
	owned     bool
	vu        *VU
}

// attach binds the client to the VU whose metrics and tags it should use.
func (c *HTTPClient) attach(vu *VU) { c.vu = vu }

// close releases the client's own connection pool, if it has one.
func (c *HTTPClient) close() error {
	if c.owned && c.transport != nil {
		c.transport.CloseIdleConnections()
	}
	return nil
}

// Request describes one HTTP call.
type Request struct {
	// Method defaults to GET.
	Method string

	// URL is absolute, or a path resolved against the run's base URL.
	URL string

	// Name is the metric label for this call site. When empty, it is derived
	// from the method and path with variable segments collapsed to `*`, so
	// /users/1 and /users/2 share one series. Set it explicitly whenever the
	// derived name would still be high-cardinality.
	Name string

	// Header is merged over the run-wide headers.
	Header http.Header

	// Query parameters appended to the URL.
	Query url.Values

	// Body is the raw request body. Mutually exclusive with JSON and Form.
	Body []byte

	// JSON is marshalled as the body, with a JSON content type.
	JSON any

	// Form is encoded as the body, with a form content type.
	Form url.Values

	// Timeout overrides the run-wide timeout for this request.
	Timeout time.Duration

	// Tags are added to this request's metrics.
	Tags Labels

	// ExpectStatus lists the acceptable status codes. When set, it replaces
	// the run's success predicate for this request.
	ExpectStatus []int

	// BetweenRequests overrides the run's pacing for this one request. A
	// pointer to the zero Pause means no pause at all; nil means use the
	// run's default.
	BetweenRequests *Pause
}

// Response is the outcome of a Request. It is always non-nil, including when
// the transport failed, so scenarios can branch on Err or OK without a nil
// check first.
type Response struct {
	// StatusCode is the HTTP status, or 0 when no response was received.
	StatusCode int
	Status     string
	Proto      string
	Header     http.Header

	// Body holds the response body, empty when HTTPOptions.DiscardBody is set
	// or the body exceeded MaxBodyBytes.
	Body []byte

	// Truncated reports that the body was longer than MaxBodyBytes.
	Truncated bool

	// Duration is the whole request, from first byte written to last byte read.
	Duration time.Duration
	// TTFB is the wait for the first response byte.
	TTFB time.Duration
	// Connecting is TCP setup time, zero when the connection was reused.
	Connecting time.Duration
	// TLSHandshake is handshake time, zero for plaintext or a reused connection.
	TLSHandshake time.Duration
	// ConnReused reports whether a pooled connection served this request.
	ConnReused bool

	BytesIn  int64
	BytesOut int64

	// Err is the transport error, nil when a response was received. An HTTP
	// 500 is not an error here; it is a successful exchange with a bad status.
	Err error
}

// OK reports whether the request completed with a status below 400.
func (r *Response) OK() bool { return r.Err == nil && r.StatusCode >= 100 && r.StatusCode < 400 }

// JSON unmarshals the response body into v.
func (r *Response) JSON(v any) error {
	if r.Err != nil {
		return fmt.Errorf("request failed: %w", r.Err)
	}
	if len(r.Body) == 0 {
		return errors.New("response body is empty")
	}
	if err := json.Unmarshal(r.Body, v); err != nil {
		return fmt.Errorf("decode JSON response: %w", err)
	}
	return nil
}

// Text returns the body as a string.
func (r *Response) Text() string { return string(r.Body) }

// String renders a compact summary, for logs and check messages.
func (r *Response) String() string {
	if r.Err != nil {
		return fmt.Sprintf("error: %v (after %s)", r.Err, r.Duration.Round(time.Millisecond))
	}
	return fmt.Sprintf("%d %s in %s, %d bytes",
		r.StatusCode, r.Status, r.Duration.Round(time.Millisecond), r.BytesIn)
}

// Get issues a GET request.
func (c *HTTPClient) Get(ctx context.Context, rawURL string) (*Response, error) {
	return c.Do(ctx, Request{Method: http.MethodGet, URL: rawURL})
}

// PostJSON issues a POST request with a JSON body.
func (c *HTTPClient) PostJSON(ctx context.Context, rawURL string, body any) (*Response, error) {
	return c.Do(ctx, Request{Method: http.MethodPost, URL: rawURL, JSON: body})
}

// PutJSON issues a PUT request with a JSON body.
func (c *HTTPClient) PutJSON(ctx context.Context, rawURL string, body any) (*Response, error) {
	return c.Do(ctx, Request{Method: http.MethodPut, URL: rawURL, JSON: body})
}

// Delete issues a DELETE request.
func (c *HTTPClient) Delete(ctx context.Context, rawURL string) (*Response, error) {
	return c.Do(ctx, Request{Method: http.MethodDelete, URL: rawURL})
}

// Do issues the request and records its metrics.
//
// The returned Response is never nil. The returned error is non-nil only for
// transport-level failures; a 4xx or 5xx response returns a nil error with the
// status set, because at load-test altitude a 500 is a measurement, not an
// exception. Scenarios that treat bad statuses as failures should say so with
// a check or by returning their own error.
func (c *HTTPClient) Do(ctx context.Context, req Request) (*Response, error) {
	if req.Method == "" {
		req.Method = http.MethodGet
	}

	target, err := c.resolve(req.URL, req.Query)
	if err != nil {
		// A malformed request never reaches the network, but a scenario that
		// keeps producing them still spins, so it is paced like any other.
		c.pace(ctx, &req)
		return &Response{Err: err}, err
	}

	body, contentType, err := encodeBody(&req)
	if err != nil {
		c.pace(ctx, &req)
		return &Response{Err: err}, err
	}

	name := req.Name
	if name == "" {
		name = DeriveRequestName(req.Method, target.Path)
	}

	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	resp := c.execute(ctx, &req, target, body, contentType)
	c.record(name, &req, resp)
	c.pace(ctx, &req)
	return resp, resp.Err
}

// pace waits out the configured gap before the caller gets its response back.
//
// Applied on every path, including a transport failure, because the runaway
// case this exists to prevent is precisely a request that fails in a
// microsecond inside a loop with nothing to slow it down.
//
// The wait goes through VU.Think, which makes it interruptible when the run is
// stopping and excludes it from iteration_duration — pacing is not work, and
// counting it would make every iteration look a second slower than it is.
func (c *HTTPClient) pace(ctx context.Context, req *Request) {
	if c.vu == nil {
		return
	}

	pause := c.opts.BetweenRequests
	if req.BetweenRequests != nil {
		pause = *req.BetweenRequests
	}
	if pause.IsZero() {
		return
	}

	c.vu.Think(ctx, pause.Duration(c.vu.Rand()))
}

// execute performs the exchange and fills in the timings.
func (c *HTTPClient) execute(
	ctx context.Context, req *Request, target *url.URL, body []byte, contentType string,
) *Response {
	resp := &Response{}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, target.String(), reader)
	if err != nil {
		resp.Err = fmt.Errorf("build request: %w", err)
		return resp
	}
	if body != nil {
		// Setting ContentLength explicitly lets the transport avoid chunked
		// encoding, and lets a retry re-read the body.
		httpReq.ContentLength = int64(len(body))
	}

	c.applyHeaders(httpReq, req, contentType)

	var tr *requestTrace
	if *c.opts.Trace {
		tr = &requestTrace{}
		// Derived from the request's own context, so cancellation and the
		// deadline both still apply; httptrace only attaches callbacks.
		//nolint:contextcheck // WithClientTrace wraps httpReq.Context(), it does not replace it.
		httpReq = httpReq.WithContext(httptrace.WithClientTrace(httpReq.Context(), tr.clientTrace()))
	}

	resp.BytesOut = int64(len(body)) + estimateHeaderBytes(httpReq)

	start := time.Now()
	httpResp, err := c.client.Do(httpReq)
	if err != nil {
		resp.Duration = time.Since(start)
		resp.Err = err
		if tr != nil {
			tr.fill(resp, start)
		}
		return resp
	}
	defer func() { _ = httpResp.Body.Close() }()

	resp.StatusCode = httpResp.StatusCode
	resp.Status = http.StatusText(httpResp.StatusCode)
	resp.Proto = httpResp.Proto
	resp.Header = httpResp.Header

	read, truncated, err := c.readBody(httpResp.Body)
	resp.Duration = time.Since(start)
	if tr != nil {
		tr.fill(resp, start)
	}

	resp.BytesIn = read.consumed
	resp.Body = read.buffered
	resp.Truncated = truncated
	if err != nil {
		// A body that fails partway through is a real failure of the
		// exchange, even though the status line arrived intact.
		resp.Err = fmt.Errorf("read response body: %w", err)
	}
	return resp
}

// bodyRead reports what came back from readBody.
type bodyRead struct {
	// consumed is every byte pulled off the wire, whether kept or not, so
	// throughput accounting stays honest under DiscardBody and truncation.
	consumed int64
	buffered []byte
}

func (c *HTTPClient) readBody(r io.Reader) (bodyRead, bool, error) {
	if c.opts.DiscardBody {
		n, err := io.Copy(io.Discard, r)
		return bodyRead{consumed: n}, false, err
	}

	limit := c.opts.MaxBodyBytes
	var buf bytes.Buffer
	kept, err := io.Copy(&buf, io.LimitReader(r, limit))
	if err != nil {
		return bodyRead{consumed: kept, buffered: buf.Bytes()}, false, err
	}

	if kept < limit {
		return bodyRead{consumed: kept, buffered: buf.Bytes()}, false, nil
	}

	// Hit the cap. Drain the rest so the server completes its write and the
	// connection stays reusable, but only count the bytes.
	extra, err := io.Copy(io.Discard, r)
	return bodyRead{consumed: kept + extra, buffered: buf.Bytes()}, extra > 0, err
}

// applyHeaders merges run-wide headers, per-request headers and the derived
// content type, in increasing order of precedence.
func (c *HTTPClient) applyHeaders(httpReq *http.Request, req *Request, contentType string) {
	for key, values := range c.opts.Headers {
		for _, v := range values {
			httpReq.Header.Add(key, v)
		}
	}
	for key, values := range req.Header {
		httpReq.Header.Del(key)
		for _, v := range values {
			httpReq.Header.Add(key, v)
		}
	}
	if contentType != "" && httpReq.Header.Get("Content-Type") == "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	if c.opts.UserAgent != "" {
		httpReq.Header.Set("User-Agent", c.opts.UserAgent)
	}
}

// record emits the standard metric set for one request.
func (c *HTTPClient) record(name string, req *Request, resp *Response) {
	if c.vu == nil {
		return
	}
	rec := c.vu.Metrics()

	labels := c.vu.Labels().
		With(LabelName, name, LabelMethod, req.Method, LabelStatus, strconv.Itoa(resp.StatusCode))
	if req.Tags.Len() > 0 {
		req.Tags.All(func(k, v string) bool {
			labels = labels.With(k, v)
			return true
		})
	}
	if resp.Err != nil {
		labels = labels.With(LabelError, classifyError(resp.Err))
	}

	failed := !c.succeeded(req, resp)

	rec.Count(MetricHTTPReqs, labels, 1)
	rec.Trend(MetricHTTPReqDuration, labels, msOf(resp.Duration))
	rec.Rate(MetricHTTPReqFailed, labels, failed)
	rec.Count(MetricHTTPReqBytesIn, labels, float64(resp.BytesIn))
	rec.Count(MetricHTTPReqBytesOut, labels, float64(resp.BytesOut))

	if resp.TTFB > 0 {
		rec.Trend(MetricHTTPReqWaiting, labels, msOf(resp.TTFB))
	}
	// Connection metrics are only meaningful on a fresh connection; recording
	// the zeros from reused connections would drag every percentile to zero.
	if resp.Connecting > 0 {
		rec.Trend(MetricHTTPReqConnecting, labels, msOf(resp.Connecting))
	}
	if resp.TLSHandshake > 0 {
		rec.Trend(MetricHTTPReqTLS, labels, msOf(resp.TLSHandshake))
	}

	// Only on the failure path, so a healthy run never pays for this.
	if failed {
		c.reportFailure(name, req, resp)
	}
}

// succeeded applies the request's expected statuses, falling back to the
// run-wide predicate.
func (c *HTTPClient) succeeded(req *Request, resp *Response) bool {
	if resp.Err != nil {
		return false
	}
	if len(req.ExpectStatus) > 0 {
		for _, want := range req.ExpectStatus {
			if resp.StatusCode == want {
				return true
			}
		}
		return false
	}
	return c.opts.IsSuccess(resp)
}

// resolve turns a possibly relative URL into an absolute one.
func (c *HTTPClient) resolve(raw string, query url.Values) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("request URL is empty")
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse URL %q: %w", raw, err)
	}

	if !parsed.IsAbs() {
		if c.base == nil {
			return nil, fmt.Errorf("relative URL %q requires a base URL", raw)
		}
		parsed = c.base.ResolveReference(parsed)
	}

	if len(query) > 0 {
		merged := parsed.Query()
		for key, values := range query {
			for _, v := range values {
				merged.Add(key, v)
			}
		}
		parsed.RawQuery = merged.Encode()
	}
	return parsed, nil
}

// encodeBody renders whichever body form the request used.
func encodeBody(req *Request) ([]byte, string, error) {
	set := 0
	if len(req.Body) > 0 {
		set++
	}
	if req.JSON != nil {
		set++
	}
	if len(req.Form) > 0 {
		set++
	}
	if set > 1 {
		return nil, "", errors.New("request sets more than one of Body, JSON and Form")
	}

	switch {
	case req.JSON != nil:
		encoded, err := json.Marshal(req.JSON)
		if err != nil {
			return nil, "", fmt.Errorf("encode JSON body: %w", err)
		}
		return encoded, "application/json", nil
	case len(req.Form) > 0:
		return []byte(req.Form.Encode()), "application/x-www-form-urlencoded", nil
	default:
		return req.Body, "", nil
	}
}

// requestTrace collects connection timings through httptrace.
//
// The callbacks fire on the transport's goroutines, but never concurrently for
// a single request, and every field is read only after client.Do has returned,
// so no synchronisation is needed.
type requestTrace struct {
	connectStart time.Time
	connectDone  time.Time
	tlsStart     time.Time
	tlsDone      time.Time
	firstByte    time.Time
	reused       bool
}

func (t *requestTrace) clientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		ConnectStart:         func(_, _ string) { t.connectStart = time.Now() },
		ConnectDone:          func(_, _ string, _ error) { t.connectDone = time.Now() },
		TLSHandshakeStart:    func() { t.tlsStart = time.Now() },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { t.tlsDone = time.Now() },
		GotFirstResponseByte: func() { t.firstByte = time.Now() },
		GotConn:              func(info httptrace.GotConnInfo) { t.reused = info.Reused },
	}
}

// fill copies the collected timings onto the response.
func (t *requestTrace) fill(resp *Response, start time.Time) {
	resp.ConnReused = t.reused
	if !t.connectStart.IsZero() && t.connectDone.After(t.connectStart) {
		resp.Connecting = t.connectDone.Sub(t.connectStart)
	}
	if !t.tlsStart.IsZero() && t.tlsDone.After(t.tlsStart) {
		resp.TLSHandshake = t.tlsDone.Sub(t.tlsStart)
	}
	if !t.firstByte.IsZero() {
		resp.TTFB = t.firstByte.Sub(start)
	}
}

// classifyError reduces a transport error to one of a small, fixed set of
// labels.
//
// The raw error text is unbounded — it embeds addresses, ports and hostnames —
// and using it as a metric label would create a new time series per failure.
// The bounded vocabulary here keeps the failure breakdown useful and the
// coordinator's memory finite.
func classifyError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "eof"
	case errors.Is(err, syscallConnRefused):
		return "connection_refused"
	case errors.Is(err, syscallConnReset):
		return "connection_reset"
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}

	var tlsErr *tls.CertificateVerificationError
	if errors.As(err, &tlsErr) {
		return "tls"
	}

	// Fall back to substring matching for errors the standard library only
	// reports as text.
	text := err.Error()
	switch {
	case strings.Contains(text, "connection refused"):
		return "connection_refused"
	case strings.Contains(text, "connection reset"):
		return "connection_reset"
	case strings.Contains(text, "no such host"):
		return "dns"
	case strings.Contains(text, "tls:"), strings.Contains(text, "x509:"):
		return "tls"
	case strings.Contains(text, "stopped after"):
		return "too_many_redirects"
	default:
		return "unknown"
	}
}

// DeriveRequestName builds a low-cardinality metric label from a method and
// path by collapsing segments that look like identifiers.
//
// Without this, a run against /orders/{id} produces one time series per order
// and the dashboard becomes unreadable long before the coordinator runs out of
// memory. The heuristic is deliberately conservative; scenarios that need
// precision should set Request.Name.
func DeriveRequestName(method, path string) string {
	if path == "" || path == "/" {
		return method + " /"
	}

	segments := strings.Split(path, "/")
	for i, seg := range segments {
		if looksLikeIdentifier(seg) {
			segments[i] = "*"
		}
	}
	return method + " " + strings.Join(segments, "/")
}

// looksLikeIdentifier reports whether a path segment is probably a variable.
func looksLikeIdentifier(seg string) bool {
	if seg == "" {
		return false
	}

	digits, hexDigits, dashes := 0, 0, 0
	for _, r := range seg {
		switch {
		case r >= '0' && r <= '9':
			digits++
			hexDigits++
		case (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F'):
			hexDigits++
		case r == '-':
			dashes++
		}
	}
	length := len(seg)

	switch {
	case digits == length:
		return true // 42
	case hexDigits == length && length >= 12:
		return true // a3f9c1e07b42
	case dashes == 4 && length == 36 && hexDigits+dashes == length:
		return true // a UUID
	case digits > 0 && length >= 16:
		return true // long opaque token with digits in it
	default:
		return false
	}
}

// estimateHeaderBytes approximates the on-the-wire size of the request head.
//
// Go's transport does not report how many bytes it wrote, and instrumenting
// the connection to find out would cost more than the number is worth. This
// approximation keeps the throughput figure in the right order of magnitude,
// which is all it is used for.
func estimateHeaderBytes(req *http.Request) int64 {
	// Request line: "METHOD /path HTTP/1.1\r\n".
	total := len(req.Method) + len(req.URL.RequestURI()) + len("  HTTP/1.1\r\n")
	total += len("Host: ") + len(req.Host) + len("\r\n")
	for key, values := range req.Header {
		for _, v := range values {
			total += len(key) + len(": ") + len(v) + len("\r\n")
		}
	}
	return int64(total + len("\r\n"))
}

// msOf converts a duration to fractional milliseconds, the unit every latency
// metric in LoadWave is reported in.
func msOf(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1e6 }
