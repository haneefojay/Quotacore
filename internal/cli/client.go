package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"

	contract "github.com/quotacore/quotacore/api"
)

// client makes a command's calls to one service with one credential.
type client struct {
	base  *url.URL
	key   string
	http  *http.Client
	out   io.Writer
	errw  io.Writer
	agent string
	sleep func(time.Duration)
}

func (o Options) client(urlFlag string) (*client, error) {
	base, err := baseURL(urlFlag, o.Getenv)
	if err != nil {
		return nil, err
	}
	key := adminKey(o.Getenv)
	if key == "" {
		return nil, usagef("no credential: set QUOTACORE_ADMIN_KEY to an admin key")
	}
	h := o.HTTP
	if h == nil {
		h = &http.Client{Timeout: time.Minute}
	}
	version := o.Version
	if version == "" {
		version = "dev"
	}
	c := &client{base: base, key: key, http: h, out: o.Stdout, errw: o.Stderr, agent: "quotacore/" + version, sleep: o.Sleep}
	// The admin key crosses the network in the clear on plain HTTP to
	// anything but loopback; a compose network is not hostile, so this warns
	// rather than refuses (IP-14 S8).
	if base.Scheme == "http" && !loopbackHost(base.Hostname()) {
		fmt.Fprintf(o.Stderr, "warning: %s is plain HTTP and not loopback, so the admin key crosses the network unencrypted\n", base.Redacted())
	}
	return c, nil
}

func loopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// result is one call's answer, or the reason there is none.
type result struct {
	status    int
	body      []byte
	requestID string
	// retryAfter is the Retry-After header, which a bare 429 carries alone.
	retryAfter string
	err        error // the call never got an answer
}

// call makes one request with token. The request id is minted here, so it is
// the one to quote even when the service never answered (NFR-S10).
func (c *client) call(op, path string, q url.Values, b body, token string) result {
	o, ok := operation(op)
	if !ok {
		return result{err: fmt.Errorf("operation %s is not in the contract", op)}
	}
	u := *c.base
	u.RawPath = c.base.EscapedPath() + path
	u.Path, _ = url.PathUnescape(u.RawPath)
	if enc := q.Encode(); enc != "" {
		u.RawQuery = enc
	}
	var payload io.Reader
	if b != nil {
		raw, err := b.MarshalJSON()
		if err != nil {
			return result{err: err}
		}
		payload = bytes.NewReader(raw)
	}
	ctx, cancel := callContext()
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, o.Method, u.String(), payload)
	if err != nil {
		return result{err: err}
	}
	id := uuid.NewString()
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Request-Id", id)
	req.Header.Set("User-Agent", c.agent)
	req.Header.Set("Accept", "application/json")
	if b != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return result{requestID: id, err: err}
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return result{requestID: id, err: err}
	}
	if rid := res.Header.Get("X-Request-Id"); rid != "" {
		id = rid
	}
	return result{status: res.StatusCode, body: raw, requestID: id, retryAfter: res.Header.Get("Retry-After")}
}

// finish prints a result and returns the exit code (IP-14 D6).
func (c *client) finish(cmd Command, r result) int {
	if r.err != nil {
		return c.failed(r)
	}
	if r.status >= 200 && r.status < 300 {
		if err := writeJSON(c.out, r.body); err != nil {
			fmt.Fprintf(c.errw, "error: the service answered %d with a body that is not JSON (request_id %s)\n", r.status, r.requestID)
			return ExitInternal
		}
		if cmd.Op == "createKey" {
			fmt.Fprintln(c.errw, shownOnce)
		}
		return ExitOK
	}
	return c.refused(r)
}

// failed reports a call that got no answer. The error is net/http's, which
// names the URL without its credentials and never the header.
func (c *client) failed(r result) int {
	fmt.Fprintf(c.errw, "error: no answer from %s: %v (request_id %s)\n", c.base.Redacted(), r.err, r.requestID)
	return ExitUnavailable
}

// wireError is the error envelope (error-catalog.md section 1).
type wireError struct {
	Error struct {
		Code         contract.ErrorCode `json:"code"`
		Message      string             `json:"message"`
		RequestID    string             `json:"request_id"`
		RetryAfterMs *int64             `json:"retry_after_ms"`
		Details      map[string]any     `json:"details"`
	} `json:"error"`
}

func parseError(raw []byte) (wireError, bool) {
	var e wireError
	if err := json.Unmarshal(raw, &e); err != nil || e.Error.Code == "" {
		return e, false
	}
	return e, true
}

// refused prints an error answer: one line to quote, then the envelope.
func (c *client) refused(r result) int {
	e, ok := parseError(r.body)
	if !ok {
		// GET /v1/admin/status answers a caller it refuses with a bare 401,
		// 403 or 429, by contract, so it can never fail with an envelope.
		switch r.status {
		case http.StatusUnauthorized, http.StatusForbidden:
			fmt.Fprintf(c.errw, "error: the service refused the credential (%d) (request_id %s)\n", r.status, r.requestID)
			return ExitCredential
		case http.StatusTooManyRequests:
			fmt.Fprintf(c.errw, "error: rate limited; retry after %s seconds (request_id %s)\n", r.retryAfter, r.requestID)
			return ExitRateLimited
		}
		fmt.Fprintf(c.errw, "error: the service answered %d with no error envelope (request_id %s)\n", r.status, r.requestID)
		return ExitInternal
	}
	id := e.Error.RequestID
	if id == "" {
		id = r.requestID
	}
	fmt.Fprintf(c.errw, "error: %s: %s (request_id %s)\n", e.Error.Code, e.Error.Message, id)
	if e.Error.Code == contract.ErrorCodeConfirmationRequired {
		fmt.Fprintf(c.errw, "nothing was applied; read affected_tenants and earliest_cycle_end below, then re-run with --confirmation-token %v (DR-046)\n",
			e.Error.Details["confirmation_token"])
	}
	_ = writeJSON(c.errw, r.body)
	return ExitFor(e.Error.Code)
}

// all follows next_cursor and prints every page's items as one page. A read
// is safe to repeat, so one rate-limit answer per page is waited out (S4).
func (c *client) all(path string, q url.Values, op string) int {
	var field string
	var items []json.RawMessage
	for {
		r := c.call(op, path, q, nil, c.key)
		if r.err == nil && r.status == http.StatusTooManyRequests {
			if e, ok := parseError(r.body); ok && e.Error.Code == contract.ErrorCodeRateLimited {
				c.sleep(retryAfter(e))
				r = c.call(op, path, q, nil, c.key)
			}
		}
		if r.err != nil {
			return c.failed(r)
		}
		if r.status != http.StatusOK {
			return c.refused(r)
		}
		var page map[string]json.RawMessage
		if err := json.Unmarshal(r.body, &page); err != nil {
			fmt.Fprintf(c.errw, "error: a page is not a JSON object (request_id %s)\n", r.requestID)
			return ExitInternal
		}
		for k, v := range page {
			if k != "next_cursor" && len(v) > 0 && v[0] == '[' {
				field = k
				var part []json.RawMessage
				if err := json.Unmarshal(v, &part); err != nil {
					return ExitInternal
				}
				items = append(items, part...)
			}
		}
		var next *string
		_ = json.Unmarshal(page["next_cursor"], &next)
		if next == nil || *next == "" {
			break
		}
		q.Set("cursor", *next)
	}
	if items == nil {
		items = []json.RawMessage{}
	}
	out, _ := json.Marshal(map[string]any{field: items, "next_cursor": nil})
	if err := writeJSON(c.out, out); err != nil {
		return ExitInternal
	}
	return ExitOK
}

func retryAfter(e wireError) time.Duration {
	d := time.Second
	if e.Error.RetryAfterMs != nil {
		d = time.Duration(*e.Error.RetryAfterMs) * time.Millisecond
	}
	return min(max(d, 100*time.Millisecond), time.Minute)
}

// writeJSON indents a JSON body onto w. An empty body, a 204, prints nothing.
func writeJSON(w io.Writer, raw []byte) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return err
	}
	buf.WriteByte('\n')
	_, err := w.Write(buf.Bytes())
	return err
}
