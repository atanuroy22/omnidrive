package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// sensitiveParams are query parameters that must never reach an error message,
// a log line or the UI. Several provider APIs (pCloud most notably) take
// credentials in the query string, so any error carrying a raw URL would leak
// the user's password verbatim.
var sensitiveParams = map[string]bool{
	"password":         true,
	"auth":             true,
	"access_token":     true,
	"refresh_token":    true,
	"code":             true,
	"client_secret":    true,
	"code_verifier":    true,
	"signature":        true,
	"x-amz-signature":  true,
	"x-amz-credential": true,
}

// redactURL replaces the values of sensitive query parameters with a marker.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<url>"
	}
	q := u.Query()
	dirty := false
	for k := range q {
		if sensitiveParams[strings.ToLower(k)] {
			q.Set(k, "REDACTED")
			dirty = true
		}
	}
	if !dirty {
		return raw
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// redactErr scrubs credentials out of transport errors, which embed the full
// request URL.
func redactErr(err error) error {
	if err == nil {
		return nil
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s %s: %w", ue.Op, redactURL(ue.URL), ue.Err)
	}
	return err
}

// apiError carries the provider's own error text, which is far more useful
// when debugging on a phone than a bare status code.
type apiError struct {
	Status int
	Body   string
	URL    string
}

func (e *apiError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 400 {
		body = body[:400] + "…"
	}
	if body == "" {
		body = http.StatusText(e.Status)
	}
	return fmt.Sprintf("%s: HTTP %d: %s", e.URL, e.Status, body)
}

func (e *apiError) NotFound() bool { return e.Status == http.StatusNotFound }

// checkResponse converts a non-2xx response into an apiError and closes body.
func checkResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	resp.Body.Close()
	err := &apiError{Status: resp.StatusCode, Body: string(body), URL: redactURL(resp.Request.URL.String())}
	if err.NotFound() {
		return fmt.Errorf("%w: %s", ErrNotFound, err.Error())
	}
	return err
}

// doJSON performs req and decodes a JSON response into out (which may be nil).
func doJSON(hc *http.Client, req *http.Request, out any) error {
	resp, err := hc.Do(req)
	if err != nil {
		return redactErr(err)
	}
	if err := checkResponse(resp); err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// readAll performs req and returns the whole body, used where a response has
// to be inspected twice (pCloud embeds its error code in the success payload).
func readAll(hc *http.Client, req *http.Request) ([]byte, error) {
	resp, err := hc.Do(req)
	if err != nil {
		return nil, redactErr(err)
	}
	if err := checkResponse(resp); err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

func jsonUnmarshal(b []byte, out any) error { return json.Unmarshal(b, out) }

func jsonDecode(r io.Reader, out any) error {
	if out == nil {
		_, err := io.Copy(io.Discard, r)
		return err
	}
	return json.NewDecoder(r).Decode(out)
}

func jsonRequest(ctx context.Context, method, url string, payload any) (*http.Request, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// retryable reports whether a failed call is worth repeating. Mobile networks
// drop connections constantly, so a couple of retries removes most spurious
// failures the user would otherwise see as a red toast.
func retryable(resp *http.Response, err error) bool {
	if err != nil {
		return true
	}
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// backoff sleeps between attempts, honouring ctx cancellation.
func backoff(ctx context.Context, attempt int) error {
	d := time.Duration(1<<attempt) * 400 * time.Millisecond
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
