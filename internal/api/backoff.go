package api

import (
	"errors"
	"math/rand/v2"
	"net/http"
	"time"
)

const (
	backoffMin = 2 * time.Second
	backoffMax = 30 * time.Second
)

// Transient reports whether a reader that keeps waiting on the API should
// wait out err: the network, the server and a rate limit (the key's
// long-poll slots being taken included) get better by waiting. Anything
// else ends the wait, a key or a cursor the server refuses and a used-up
// quota among them.
func Transient(err error) bool {
	var te *TransportError
	var ae *Error
	switch {
	case errors.As(err, &te):
		return true
	case errors.As(err, &ae):
		return ae.Status >= 500 || (ae.Status == http.StatusTooManyRequests && ae.Code != CodeQuotaExceeded)
	}
	return false
}

// Backoff is the wait after the n-th Transient failure in a row, counting
// from 0: 2s doubling to 30s, up to a quarter less for jitter, so readers
// that lost the server together do not come back together. A longer
// retry_after_ms from the server wins, up to the same 30s.
func Backoff(n int, err error) time.Duration {
	base := min(backoffMin<<min(n, 5), backoffMax)
	d := base - rand.N(base/4) // #nosec G404 -- retry jitter, not security-sensitive
	var ae *Error
	if errors.As(err, &ae) && ae.RetryAfterMs > 0 {
		d = max(d, min(time.Duration(ae.RetryAfterMs)*time.Millisecond, backoffMax))
	}
	return d
}
