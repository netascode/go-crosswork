package crosswork

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/tidwall/gjson"
)

// Do makes a request.
// Requests for Do are built outside of the client, e.g.
//
//	req := client.NewReq("GET", "/crosswork/inventory/v1/nodes/count", nil)
//	res, _ := client.Do(req)
//
// On a failed call Res still holds the parsed response body, next to the *APIError.
func (client *Client) Do(req Req) (Res, error) {
	if req.buildErr != nil {
		return Res{}, req.buildErr
	}
	var body []byte
	if req.HttpReq.Body != nil {
		body, _ = io.ReadAll(req.HttpReq.Body)
	}
	applyDefaultHeaders(&req)

	if req.UseMutex && req.HttpReq.Method != http.MethodGet {
		client.s.writeMu.Lock()
		defer client.s.writeMu.Unlock()
	}
	if req.MutexKey != "" && req.HttpReq.Method != http.MethodGet {
		unlock, err := client.s.lockKey(req.HttpReq.Context(), req.MutexKey)
		if err != nil {
			return Res{}, err
		}
		defer unlock()
	}
	ctx := req.HttpReq.Context()
	defer func() {
		log.Printf("[DEBUG] Exit from Do method: %s, %s", req.HttpReq.Method, req.HttpReq.URL)
	}()

	attempts := 0
	for {
		if err := client.Authenticate(); err != nil {
			return Res{}, err
		}
		token, _ := client.s.current()
		status, hdr, raw, err := client.send(req, body, token)
		if err != nil {
			if ctx.Err() != nil {
				return Res{}, ctx.Err()
			}
			// Nothing was sent (dial or TLS failure): safe to retry for every method.
			// Otherwise the outcome is unknown: only reads and repeatable writes are retried.
			if (isPreSend(err) || canRetryAmbiguous(req)) && client.backoff(ctx, attempts) {
				log.Printf("[ERROR] HTTP Connection failed: %s, retries: %v", err, attempts)
				attempts++
				continue
			}
			log.Printf("[ERROR] HTTP Connection error occurred: %+v", err)
			return Res{}, err
		}
		res := gjson.ParseBytes(raw)
		if req.LogPayload {
			log.Printf("[DEBUG] HTTP Response: %s", truncate(redact(oneLine(string(raw))), 2000))
		}

		if status >= 200 && status <= 299 {
			res, err = client.finish(req, status, res, raw)
			if err == nil && req.PlanPath != "" && !req.NoWait && req.HttpReq.Method != http.MethodGet {
				err = client.waitForPlan(req)
			}
			if err == nil && req.AfterWrite != nil && req.HttpReq.Method != http.MethodGet {
				if werr := req.AfterWrite(req.HttpReq.Context(), res); werr != nil {
					err = &WaitError{Err: werr}
				}
			}
			return res, err
		}

		// Token no longer accepted: log in again once per request and retry.
		// A 403 on a token minted moments ago is a real permission denial, but only if that
		// token is still the current one: if another request already replaced it, this
		// response belongs to the old token and the request just needs the new one.
		current, tokenAge := client.s.snapshot()
		replaced := current != "" && current != token
		if isAuthFailure(status, hdr) && !client.s.static && !req.reAuthAttempted &&
			!(status == http.StatusForbidden && isAccessDenied(raw)) &&
			(replaced || !(status == http.StatusForbidden && tokenAge < reauthGuard)) {
			log.Printf("[WARNING] Received %d. Attempting to re-authenticate.", status)
			req.reAuthAttempted = true
			if err := client.reauth(token); err != nil {
				return res, err
			}
			continue
		}

		// Rate limited: the request was not processed, so it is safe for every method.
		if status == http.StatusTooManyRequests && attempts < client.MaxRetries {
			d := client.backoffDelay(attempts)
			if ra := retryAfter(hdr); ra > d {
				d = ra
			}
			log.Printf("[WARNING] HTTP Request rate limited, waiting %v seconds, Retries: %v", d.Seconds(), attempts)
			if !sleep(ctx, d) {
				return res, ctx.Err()
			}
			attempts++
			continue
		}

		// NSO is busy (lock-denied or in-use): the write was not applied, so any method may be retried.
		if attempts < client.MaxRetries && client.nsoBusy(req, status, raw) {
			log.Printf("[WARNING] NSO is busy (StatusCode %v), waiting for the datastore lock, Retries: %v", status, attempts)
			client.waitForNSOLock(ctx, req)
			if client.backoff(ctx, attempts) {
				attempts++
				continue
			}
		}

		if client.retryStatus(status) && canRetryAmbiguous(req) && client.backoff(ctx, attempts) {
			log.Printf("[ERROR] HTTP Request failed: StatusCode %v, Retries: %v", status, attempts)
			attempts++
			continue
		}
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		err = client.apiError(req, status, string(raw), "")
		log.Printf("[ERROR] %v", err)
		return res, err
	}
}

// isAccessDenied reports whether a body carries the RESTCONF error-tag access-denied (RFC 8040),
// which NSO's access control returns for a user without rights. It is a structured tag, not
// message text, and it means the token is fine: logging in again would not help.
func isAccessDenied(raw []byte) bool {
	_, _, rc := parseError(string(raw))
	for _, e := range rc {
		if e.Tag == "access-denied" {
			return true
		}
	}
	return false
}

// retryStatus reports whether a status is worth retrying when the call may be retried.
// The gateway's 500 "NATS request failed" is never retried: it means the request could
// not be routed or parsed, which a retry cannot fix.
func (client *Client) retryStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return contains(intStrings(client.RetryOnErrorCodes), strconv.Itoa(status))
}

func intStrings(v []int) []string {
	out := make([]string, len(v))
	for i, n := range v {
		out[i] = strconv.Itoa(n)
	}
	return out
}

// send makes one attempt and returns the status, headers and body.
func (client *Client) send(req Req, body []byte, token string) (int, http.Header, []byte, error) {
	ctx := req.HttpReq.Context()
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout(req)
	}
	httpClient := client.HttpClient
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
		// The context deadline replaces the client timeout, so a request may take longer than it.
		hc := *httpClient
		hc.Timeout = 0
		httpClient = &hc
	}
	r := req.HttpReq.Clone(ctx)
	if len(body) > 0 {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	} else {
		r.Body, r.ContentLength = nil, 0
	}
	r.Header.Set("Authorization", "Bearer "+token)
	if req.LogPayload && len(body) > 0 {
		log.Printf("[DEBUG] HTTP Request: %s, %s, %s", r.Method, r.URL, truncate(redact(string(body)), 2000))
	} else {
		log.Printf("[DEBUG] HTTP Request: %s, %s", r.Method, r.URL)
	}
	res, err := httpClient.Do(r)
	if err != nil {
		return 0, nil, nil, err
	}
	raw, err := readAll(res)
	if err != nil {
		return 0, nil, nil, err
	}
	return res.StatusCode, res.Header, raw, nil
}

// finish handles a 2xx response: it turns a failure reported inside the body into an error.
func (client *Client) finish(req Req, status int, res Res, raw []byte) (Res, error) {
	if req.noResultCheck {
		return res, nil
	}
	if req.resultCheck != nil {
		if err := req.resultCheck(res); err != nil {
			if asAPIError(err) != nil {
				return res, err
			}
			return res, client.apiError(req, status, string(raw), err.Error())
		}
		return res, nil
	}
	if msg, ok := resultFailure(res); ok {
		err := client.apiError(req, status, string(raw), msg)
		if err.Code == "" {
			err.Code = statusErrorCode(res)
		}
		err.EntryStatus = entryStatuses(res)
		log.Printf("[ERROR] %v", err)
		return res, err
	}
	logUnmatched(res)
	return res, nil
}

// apiError builds the error for a response. msg overrides the message extracted from the body.
func (client *Client) apiError(req Req, status int, body, msg string) *APIError {
	m, code, rc := parseError(body)
	if msg != "" {
		m, code = msg, codeFromMessage(msg)
	}
	return &APIError{Method: req.HttpReq.Method, URL: req.HttpReq.URL.String(), StatusCode: status,
		Message: m, Code: code, Body: body, RestconfErrors: rc}
}

// retryAfter reads a Retry-After header: seconds, or an HTTP date. "0" waits one second.
func retryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n <= 0 {
			return time.Second
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return time.Until(t)
	}
	return 0
}

// isPreSend reports whether a transport error happened before the request was sent
// (dial, DNS or TLS certificate failures), so nothing can have reached the server.
func isPreSend(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	var dns *net.DNSError
	var unknown x509.UnknownAuthorityError
	var host x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verify *tls.CertificateVerificationError
	var header tls.RecordHeaderError
	return errors.As(err, &dns) || errors.As(err, &unknown) || errors.As(err, &host) ||
		errors.As(err, &invalid) || errors.As(err, &verify) || errors.As(err, &header)
}
