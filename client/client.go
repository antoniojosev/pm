// Package client is the official Go client of the pm daemon's HTTP API v1.
//
// It imports only the standard library: no third-party package is compiled
// into your binary. The API it speaks is documented in docs/api/openapi.yaml.
//
//	c := client.New("http://127.0.0.1:9797")
//	lease, err := c.Leases.Create(ctx, client.LeaseRequest{Owner: "wt-qa", Count: 2})
//
// Before its first request a Client asks the daemon which API generation it
// speaks and fails with a *VersionError (errors.Is(err, ErrIncompatible)) when
// it is not APIVersion, instead of sending requests the daemon would
// misunderstand.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// APIVersion is the API generation this client speaks: the `api` field of
// GET /api/v1/version. pm only changes it with a breaking change, so the
// daemon must report exactly this number.
const APIVersion = 1

// maxBody bounds how much of an answer the client reads.
const maxBody = 32 << 20

// Client talks to one pm daemon. It is safe for concurrent use.
type Client struct {
	// Leases reserves ports without starting anything.
	Leases *LeasesService
	// Owners acts on everything that belongs to an owner at once.
	Owners *OwnersService

	base      string
	http      *http.Client
	skipCheck bool
	mu        sync.Mutex
	versionOK bool
}

// Option customises New.
type Option func(*Client)

// WithHTTPClient uses hc for every request, e.g. to set a transport. The
// default is http.DefaultClient: bound requests with their context.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.http = hc }
}

// WithoutVersionCheck turns off the automatic API check before the first
// request. Version still reports the mismatch when you call it.
func WithoutVersionCheck() Option {
	return func(c *Client) { c.skipCheck = true }
}

// New returns a client for the daemon at baseURL, e.g. "http://127.0.0.1:9797".
// Nothing is sent until the first call.
func New(baseURL string, opts ...Option) *Client {
	c := &Client{base: strings.TrimRight(baseURL, "/"), http: http.DefaultClient}
	c.Leases = &LeasesService{c}
	c.Owners = &OwnersService{c}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Version asks the daemon who it is and what the host offers. When the daemon
// speaks another API generation than APIVersion it returns the info it got
// together with a *VersionError. A daemon too old to have the endpoint gives
// the same error with Got 0, whether it answers 404 or, like pm v0.1.0 whose
// dashboard answers any unknown path, 200 with something that is not the
// version JSON (index.html) or JSON without an `api` field.
func (c *Client) Version(ctx context.Context) (VersionInfo, error) {
	var v VersionInfo
	err := c.call(ctx, http.MethodGet, "/version", nil, nil, &v)
	var ae *Error
	var de *decodeError
	if (errors.As(err, &ae) && ae.Status == http.StatusNotFound) || errors.As(err, &de) {
		return v, &VersionError{Want: APIVersion}
	}
	if err != nil {
		return v, err
	}
	if v.API != APIVersion {
		return v, &VersionError{Want: APIVersion, Got: v.API, PM: v.Version}
	}
	c.mu.Lock()
	c.versionOK = true
	c.mu.Unlock()
	return v, nil
}

// Run starts the process spec describes, without registering it. The unit is
// pm-<spec.Name>.service; Stop with the same name brings it down.
func (c *Client) Run(ctx context.Context, spec RunSpec) (RunResult, error) {
	var res RunResult
	err := c.checked(ctx, http.MethodPost, "/run", nil, spec, &res)
	return res, err
}

// Stop stops a project or a run by name. A run is forgotten once stopped.
func (c *Client) Stop(ctx context.Context, name string) error {
	return c.checked(ctx, http.MethodPost, "/projects/"+url.PathEscape(name)+"/stop", nil, nil, nil)
}

// Logs returns the last n lines of the output of a project or run; n <= 0 lets
// the daemon choose (200). A process that has written nothing yet gives "".
func (c *Client) Logs(ctx context.Context, name string, n int) (string, error) {
	q := url.Values{}
	if n > 0 {
		q.Set("n", strconv.Itoa(n))
	}
	var out struct {
		Logs string `json:"logs"`
	}
	err := c.checked(ctx, http.MethodGet, "/projects/"+url.PathEscape(name)+"/logs", q, nil, &out)
	return out.Logs, err
}

// LeasesService is the lease part of the API: ports reserved for an owner.
type LeasesService struct{ c *Client }

// Create reserves ports. Two clients asking at once never get the same port.
func (s *LeasesService) Create(ctx context.Context, req LeaseRequest) (Lease, error) {
	var l Lease
	err := s.c.checked(ctx, http.MethodPost, "/leases", nil, req, &l)
	return l, err
}

// Delete releases a lease by id.
func (s *LeasesService) Delete(ctx context.Context, id string) error {
	return s.c.checked(ctx, http.MethodDelete, "/leases/"+url.PathEscape(id), nil, nil, nil)
}

// List returns the leases, oldest first; owner "" lists all of them.
func (s *LeasesService) List(ctx context.Context, owner string) ([]Lease, error) {
	q := url.Values{}
	if owner != "" {
		q.Set("owner", owner)
	}
	leases := []Lease{}
	err := s.c.checked(ctx, http.MethodGet, "/leases", q, nil, &leases)
	return leases, err
}

// OwnersService acts on everything that belongs to an owner.
type OwnersService struct{ c *Client }

// Stop stops every process of owner and keeps its leases, so the owner can
// start again on the same ports. An owner with nothing running is not an error.
// When some process cannot be stopped the others still are: the error says
// which, and the result is returned along with it.
func (s *OwnersService) Stop(ctx context.Context, owner string) (OwnerResult, error) {
	return s.owner(ctx, http.MethodPost, "/owners/"+url.PathEscape(owner)+"/stop")
}

// Delete stops every process of owner and then releases its leases. If
// something cannot be stopped the leases are kept and the error says so; the
// call can be repeated.
func (s *OwnersService) Delete(ctx context.Context, owner string) (OwnerResult, error) {
	return s.owner(ctx, http.MethodDelete, "/owners/"+url.PathEscape(owner))
}

func (s *OwnersService) owner(ctx context.Context, method, path string) (OwnerResult, error) {
	var res OwnerResult
	err := s.c.checked(ctx, method, path, nil, nil, &res)
	var ae *Error
	if errors.As(err, &ae) {
		// A failure still says what was done before it: keep it when it parses.
		_ = json.Unmarshal(ae.body, &res)
	}
	return res, err
}

// checked runs call after making sure, once, that the daemon speaks APIVersion.
func (c *Client) checked(ctx context.Context, method, path string, q url.Values, in, out any) error {
	if !c.skipCheck {
		c.mu.Lock()
		ok := c.versionOK
		c.mu.Unlock()
		if !ok {
			if _, err := c.Version(ctx); err != nil {
				return err
			}
		}
	}
	return c.call(ctx, method, path, q, in, out)
}

// call sends one request to /api/v1<path>. in is the JSON body (nil for none);
// a 2xx answer is decoded into out (nil to ignore it); anything else is an *Error.
func (c *Client) call(ctx context.Context, method, path string, q url.Values, in, out any) error {
	full := "/api/v1" + path
	u := c.base + full
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("pm: encode %s %s: %w", method, full, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return fmt.Errorf("pm: %s %s: %w", method, full, err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("pm: %s %s: %w", method, full, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err != nil {
		return fmt.Errorf("pm: %s %s: read answer: %w", method, full, err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return &Error{Method: method, Path: full, Status: res.StatusCode, Message: errorMessage(raw), body: raw}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &decodeError{err: fmt.Errorf("pm: %s %s: unexpected answer: %w", method, full, err)}
	}
	return nil
}

// errorMessage extracts {"error": "..."} from an answer, or falls back to the
// trimmed raw text (a proxy's HTML page, say).
func errorMessage(raw []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error != "" {
		return e.Error
	}
	msg := strings.TrimSpace(string(raw))
	if len(msg) > 200 {
		msg = msg[:200] + "..."
	}
	if msg == "" {
		msg = "no message"
	}
	return msg
}
