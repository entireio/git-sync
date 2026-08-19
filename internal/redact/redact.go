// Package redact strips credentials out of values on their way to somewhere
// a human or a log aggregator will read them: status output, JSON results,
// and error messages.
//
// Remote URLs are the reason this exists. Callers routinely embed a secret in
// the URL itself — `https://user:token@host/repo.git` in CI, or the
// token-as-username form `https://ghp_.../repo.git` that GitHub Apps and PATs
// use — and every one of those strings is a credential that must not be
// echoed back. Redact at the point of display rather than at the point of
// input: the original URL is what authenticates, so it has to stay intact on
// the request path.
package redact

import (
	"errors"
	"net/url"
)

// placeholder stands in for a URL that could not be parsed. Emitting the raw
// string instead would echo whatever credentials it carried, and a URL we
// failed to parse is exactly the case where we cannot locate them.
const placeholder = "<url redacted>"

// URL removes any credentials embedded in a URL string.
//
// The entire userinfo component is dropped, not just the password: token auth
// commonly carries the secret in the username position
// (https://<token>@host/...), which url.URL.Redacted would leave intact
// because there is no password component to mask.
//
// Unparseable input yields placeholder rather than the original string.
func URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return placeholder
	}
	return Endpoint(u)
}

// Endpoint is URL for an already-parsed *url.URL. The input is not modified.
// A nil URL yields placeholder.
func Endpoint(u *url.URL) string {
	if u == nil {
		return placeholder
	}
	clean := *u
	clean.User = nil
	return clean.String()
}

// URLError unwraps a *url.Error to its underlying cause, dropping the raw URL
// string that url.Parse embeds in its message (`parse "<url>": ...`). Use it
// when reporting a parse failure: the caller already knows which endpoint it
// passed, and the message would otherwise carry the credentials verbatim.
//
// Errors that are not *url.Error are returned unchanged, so this is safe to
// apply to any parse path.
func URLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}
