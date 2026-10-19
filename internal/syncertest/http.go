package syncertest

import "net/http"

// RoundTripperFunc adapts a function to http.RoundTripper for transport tests.
type RoundTripperFunc func(*http.Request) (*http.Response, error)

func (f RoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
