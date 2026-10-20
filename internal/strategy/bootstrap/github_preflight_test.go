package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/transport"
	transporthttp "github.com/go-git/go-git/v6/plumbing/transport/http"

	"entire.io/entire/git-sync/internal/gitproto"
	"entire.io/entire/git-sync/internal/planner"
)

const (
	preflightToken    = "ghs_preflight-secret"
	preflightEndpoint = "https://github.com/acme/private.git"
	largeRepoKB       = 7 * 1024 * 1024
)

var errBatchedPathReached = errors.New("batched path reached")

func installationAuth() gitproto.AuthMethod {
	return &transporthttp.BasicAuth{Username: "x-access-token", Password: preflightToken}
}

// repoAPI serves GET /repos/acme/private, answering through respond, and
// returns a client that reaches it as https://api.github.com. The real API
// URL matters: credentials go only to a destination on github.com's site.
func repoAPI(t *testing.T, respond func(http.ResponseWriter, *http.Request)) *http.Client {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/private" {
			t.Errorf("unexpected preflight request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		respond(w, r)
	}))
	t.Cleanup(server.Close)
	return apiClient(t, server, "api.github.com:443")
}

// apiClient returns a client trusting server's certificate that dials each of
// hosts (host:port) to server instead. Every other address dials as usual.
func apiClient(t *testing.T, server *httptest.Server, hosts ...string) *http.Client {
	t.Helper()
	client := *server.Client()
	transport := httpTransport(t, &client).Clone()
	// The test certificate names example.com, not the hosts dialed to it.
	transport.TLSClientConfig.ServerName = "example.com"
	addr := server.Listener.Addr().String()
	var dialer net.Dialer
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if slices.Contains(hosts, address) {
			address = addr
		}
		return dialer.DialContext(ctx, network, address)
	}
	client.Transport = transport
	return &client
}

func httpTransport(t *testing.T, client *http.Client) *http.Transport {
	t.Helper()
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport is %T, want *http.Transport", client.Transport)
	}
	return transport
}

// privateRepoAPI answers like GitHub does for a private repository: 404 to an
// anonymous caller, the metadata to one presenting the token, as Basic
// (installation token) or Bearer credentials.
func privateRepoAPI(t *testing.T, sizeKB int64) *http.Client {
	t.Helper()
	return repoAPI(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		basic := ok && user == "x-access-token" && pass == preflightToken
		if !basic && r.Header.Get("Authorization") != "Bearer "+preflightToken {
			writeAPIError(t, w, http.StatusNotFound, "Not Found")
			return
		}
		writeRepoSize(t, w, sizeKB)
	})
}

func writeRepoSize(t *testing.T, w http.ResponseWriter, sizeKB int64) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(map[string]any{"full_name": "acme/private", "size": sizeKB}); err != nil {
		t.Errorf("write repository metadata: %v", err)
	}
}

func writeAPIError(t *testing.T, w http.ResponseWriter, status int, message string) {
	t.Helper()
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]string{"message": message, "documentation_url": "https://docs.github.com/rest"}); err != nil {
		t.Errorf("write api error: %v", err)
	}
}

func useGitHubAPI(t *testing.T, base string) {
	t.Helper()
	prev := GitHubRepoAPIBaseURL
	GitHubRepoAPIBaseURL = base
	t.Cleanup(func() { GitHubRepoAPIBaseURL = prev })
}

func preflightParams(t *testing.T, client *http.Client, auth gitproto.AuthMethod, out *bytes.Buffer) Params {
	t.Helper()
	return preflightParamsFor(t, preflightEndpoint, client, auth, out)
}

func preflightParamsFor(t *testing.T, endpoint string, client *http.Client, auth gitproto.AuthMethod, out *bytes.Buffer) Params {
	t.Helper()
	ep, err := transport.ParseURL(endpoint)
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	return Params{
		SourceConn:    gitproto.NewHTTPConnWithClient(ep, "source", auth, client),
		SourceService: fakeBootstrapSource{},
		PushLogger:    slog.New(slog.NewJSONHandler(out, nil)),
	}
}

// withVerboseLog wires p.Logger, the -v log, to a buffer it returns.
func withVerboseLog(p *Params) *bytes.Buffer {
	var out bytes.Buffer
	p.Logger = slog.New(slog.NewJSONHandler(&out, nil))
	return &out
}

// captureNotices collects p's notices into the slice it returns.
func captureNotices(p *Params) *[]string {
	var notices []string
	p.OnNotice = func(msg string) { notices = append(notices, msg) }
	return &notices
}

// redirectingAPI returns a client whose API redirects every request to
// another site, which answers through respond, and the count of requests that
// site received.
func redirectingAPI(t *testing.T, respond http.HandlerFunc) (*http.Client, *int) {
	t.Helper()
	hits := new(int)
	elsewhere := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		respond(w, r)
	}))
	t.Cleanup(elsewhere.Close)
	client := repoAPI(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	})
	return client, hits
}

func preflightEvent(t *testing.T, out *bytes.Buffer) map[string]any {
	t.Helper()
	var event map[string]any
	if err := json.Unmarshal(out.Bytes(), &event); err != nil {
		t.Fatalf("expected exactly one preflight log record: %v; output=%s", err, out)
	}
	if event["msg"] != githubPreflightMsg {
		t.Fatalf("msg = %v, want the preflight record; output=%s", event["msg"], out)
	}
	if strings.Contains(out.String(), preflightToken) {
		t.Fatalf("preflight log leaked the token: %s", out)
	}
	return event
}

func eventError(t *testing.T, event map[string]any) string {
	t.Helper()
	msg, ok := event["error"].(string)
	if !ok || msg == "" {
		t.Fatalf("error = %v, want a non-empty string", event["error"])
	}
	return msg
}

func assertFields(t *testing.T, event map[string]any, want map[string]any) {
	t.Helper()
	for key, w := range want {
		if event[key] != w {
			t.Errorf("%s = %v (%T), want %v (%T)", key, event[key], event[key], w, w)
		}
	}
}

func TestGitHubPreflightAuthenticatesPrivateRepoLookup(t *testing.T) {
	client := privateRepoAPI(t, largeRepoKB)
	var out bytes.Buffer
	p := preflightParams(t, client, installationAuth(), &out)
	verboseOut := withVerboseLog(&p)

	limit, ok := githubBatchLimit(context.Background(), p)
	if !ok || limit != defaultTargetMaxPackBytes {
		t.Fatalf("githubBatchLimit = (%d, %v), want (%d, true): a private repo over the threshold must bootstrap batched",
			limit, ok, int64(defaultTargetMaxPackBytes))
	}
	event := preflightEvent(t, &out)
	assertFields(t, event, map[string]any{
		"level":                 "INFO",
		"repository":            "acme/private",
		"authenticated":         true,
		"http_status":           float64(http.StatusOK),
		"size_kb":               float64(largeRepoKB),
		"threshold_kb":          float64(githubLargeRepoThresholdKB),
		"mode":                  "batched",
		"target_max_pack_bytes": float64(defaultTargetMaxPackBytes),
	})
	if _, ok := event["refused_auth_status"]; ok {
		t.Fatalf("an accepted token logged a refusal: %v", event["refused_auth_status"])
	}
	if verbose := preflightEvent(t, verboseOut); verbose["mode"] != "batched" {
		t.Fatalf("verbose record mode = %v, want batched", verbose["mode"])
	}
}

// Credentials written into the source URL authenticate git through
// req.URL.User and never become HTTPConn.Auth, so the lookup has to take them
// from the endpoint itself.
func TestGitHubPreflightUsesURLCredentials(t *testing.T) {
	client := privateRepoAPI(t, largeRepoKB)
	var out bytes.Buffer
	p := preflightParamsFor(t, "https://x-access-token:"+preflightToken+"@github.com/acme/private.git", client, nil, &out)

	if _, ok := githubBatchLimit(context.Background(), p); !ok {
		t.Fatalf("credentials from the URL were not sent; log=%s", &out)
	}
	assertFields(t, preflightEvent(t, &out), map[string]any{"authenticated": true, "mode": "batched"})
}

// A username without a password is as likely an account name
// ("alice@github.com") as a token, and the API would only refuse an account
// name with a 401, so it is not sent.
func TestGitHubPreflightDoesNotSendURLUsernameWithoutPassword(t *testing.T) {
	client := repoAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("a bare URL username was sent as %q", auth)
		}
		writeRepoSize(t, w, largeRepoKB)
	})
	var out bytes.Buffer
	p := preflightParamsFor(t, "https://alice@github.com/acme/private.git", client, nil, &out)

	if _, ok := githubBatchLimit(context.Background(), p); !ok {
		t.Fatalf("the anonymous lookup of a public repo failed; log=%s", &out)
	}
	assertFields(t, preflightEvent(t, &out), map[string]any{"authenticated": false, "mode": "batched"})
}

// With TLS verification off a MITM cannot be told from api.github.com, so the
// source credentials stay home and the lookup goes out anonymously.
func TestGitHubPreflightWithholdsCredentialsWithoutTLSVerification(t *testing.T) {
	client := privateRepoAPI(t, largeRepoKB)
	httpTransport(t, client).TLSClientConfig.InsecureSkipVerify = true
	var out bytes.Buffer
	p := preflightParams(t, client, installationAuth(), &out)

	if _, ok := githubBatchLimit(context.Background(), p); ok {
		t.Fatal("credentials reached the API with TLS verification off")
	}
	assertFields(t, preflightEvent(t, &out), map[string]any{"authenticated": false, "http_status": float64(http.StatusNotFound)})
}

// The destination decides, not the configured base: an API base that is not
// on the source's site gets no credentials.
func TestGitHubPreflightWithholdsCredentialsFromAnotherSite(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("credentials reached %s: %q", r.Host, auth)
		}
		writeAPIError(t, w, http.StatusNotFound, "Not Found")
	}))
	t.Cleanup(server.Close)
	useGitHubAPI(t, "https://api.elsewhere.example")
	var out bytes.Buffer
	p := preflightParams(t, apiClient(t, server, "api.elsewhere.example:443"), installationAuth(), &out)

	githubBatchLimit(context.Background(), p)
	assertFields(t, preflightEvent(t, &out), map[string]any{"authenticated": false, "http_status": float64(http.StatusNotFound)})
}

func TestGitHubPreflightSendsBearerToken(t *testing.T) {
	var out bytes.Buffer
	p := preflightParams(t, privateRepoAPI(t, largeRepoKB), &transporthttp.TokenAuth{Token: preflightToken}, &out)

	if _, ok := githubBatchLimit(context.Background(), p); !ok {
		t.Fatalf("bearer credentials were not sent; log=%s", &out)
	}
}

func TestGitHubPreflightLookupFailureLogsWarningAndKeepsOneShot(t *testing.T) {
	client := privateRepoAPI(t, largeRepoKB)
	var out bytes.Buffer
	p := preflightParams(t, client, nil, &out)
	notices := captureNotices(&p)
	verboseOut := withVerboseLog(&p)

	if limit, ok := githubBatchLimit(context.Background(), p); ok {
		t.Fatalf("githubBatchLimit = (%d, true), want one-shot when the size is unknown", limit)
	}
	// The notice already reports it; a verbose record too would print it twice.
	if verboseOut.Len() != 0 {
		t.Fatalf("a failed lookup was also logged verbosely: %s", verboseOut)
	}
	event := preflightEvent(t, &out)
	assertFields(t, event, map[string]any{
		"level":         "WARN",
		"repository":    "acme/private",
		"authenticated": false,
		"http_status":   float64(http.StatusNotFound),
		"mode":          "one_shot",
	})
	if msg := eventError(t, event); !strings.Contains(msg, "404") || !strings.Contains(msg, "Not Found") {
		t.Fatalf("error = %q, want the status and GitHub's message", msg)
	}
	// Without a structured logger (the CLI), the notice is the only signal.
	if len(*notices) != 1 || !strings.Contains((*notices)[0], "404") || !strings.Contains((*notices)[0], "single pack") {
		t.Fatalf("notices = %q, want one notice naming the failure and the one-shot fallback", *notices)
	}
}

// A token git accepted can still be refused by the REST API (an organization
// enforcing SSO the token is not authorized for). A public repository must not
// lose the batching an anonymous lookup would have given it.
func TestGitHubPreflightRetriesAnonymouslyWhenTokenRefused(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client := repoAPI(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					writeAPIError(t, w, status, "Resource protected by organization SAML enforcement.")
					return
				}
				writeRepoSize(t, w, largeRepoKB)
			})
			var out bytes.Buffer
			p := preflightParams(t, client, installationAuth(), &out)

			if _, ok := githubBatchLimit(context.Background(), p); !ok {
				t.Fatalf("a refused token cost a public repo its batching; log=%s", &out)
			}
			assertFields(t, preflightEvent(t, &out), map[string]any{
				"level":               "INFO",
				"authenticated":       false,
				"refused_auth_status": float64(status),
				"http_status":         float64(http.StatusOK),
				"mode":                "batched",
			})
		})
	}
}

// When the anonymous retry fails too (a private repository answers 404), the
// refusal is the real cause and must not be lost behind the retry's answer.
func TestGitHubPreflightReportsRefusalWhenAnonymousRetryFails(t *testing.T) {
	const refusal = "Resource protected by organization SAML enforcement."
	client := repoAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			writeAPIError(t, w, http.StatusForbidden, refusal)
			return
		}
		writeAPIError(t, w, http.StatusNotFound, "Not Found")
	})
	var out bytes.Buffer
	p := preflightParams(t, client, installationAuth(), &out)
	notices := captureNotices(&p)

	if _, ok := githubBatchLimit(context.Background(), p); ok {
		t.Fatal("githubBatchLimit selected batched mode without a size")
	}
	event := preflightEvent(t, &out)
	assertFields(t, event, map[string]any{
		"level":               "WARN",
		"authenticated":       false,
		"http_status":         float64(http.StatusNotFound),
		"refused_auth_status": float64(http.StatusForbidden),
	})
	if got, ok := event["refused_auth_error"].(string); !ok || !strings.Contains(got, refusal) {
		t.Fatalf("refused_auth_error = %q, want GitHub's refusal message", got)
	}
	if len(*notices) != 1 || !strings.Contains((*notices)[0], refusal) || !strings.Contains((*notices)[0], "404") {
		t.Fatalf("notices = %q, want one notice naming both the refusal and the retry's failure", *notices)
	}
}

func TestGitHubPreflightDoesNotRetryOtherFailures(t *testing.T) {
	requests := 0
	client := repoAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		requests++
		writeAPIError(t, w, http.StatusNotFound, "Not Found")
	})
	var out bytes.Buffer
	p := preflightParams(t, client, installationAuth(), &out)

	if _, ok := githubBatchLimit(context.Background(), p); ok {
		t.Fatal("githubBatchLimit selected batched mode without a size")
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1: an anonymous retry cannot see more than the token", requests)
	}
	assertFields(t, preflightEvent(t, &out), map[string]any{"level": "WARN", "authenticated": true, "http_status": float64(http.StatusNotFound)})
}

func TestGitHubPreflightTransportErrorLogsWarning(t *testing.T) {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	client := apiClient(t, server, "api.github.com:443")
	server.Close()
	var out bytes.Buffer
	p := preflightParams(t, client, nil, &out)

	if _, ok := githubBatchLimit(context.Background(), p); ok {
		t.Fatal("githubBatchLimit selected batched mode without a size")
	}
	event := preflightEvent(t, &out)
	assertFields(t, event, map[string]any{"level": "WARN", "http_status": float64(0), "mode": "one_shot"})
	eventError(t, event)
}

// An answer without a usable size is an unknown size. GitHub reports 0 for an
// empty repository and for one it has not sized yet; bootstrap only runs with
// source refs, so here 0 means not sized yet, and warns like the rest.
func TestGitHubPreflightUnusableBodyLogsWarning(t *testing.T) {
	for name, tc := range map[string]struct{ body, wantErr string }{
		"not json":      {body: `<html>not json</html>`, wantErr: "decode"},
		"no size":       {body: `{"full_name":"acme/private"}`, wantErr: "no size"},
		"negative size": {body: `{"full_name":"acme/private","size":-1}`, wantErr: "no size"},
		"zero size":     {body: `{"full_name":"acme/private","size":0}`, wantErr: "size 0"},
	} {
		t.Run(name, func(t *testing.T) {
			client := repoAPI(t, func(w http.ResponseWriter, _ *http.Request) {
				if _, err := io.WriteString(w, tc.body); err != nil {
					t.Errorf("write body: %v", err)
				}
			})
			var out bytes.Buffer
			p := preflightParams(t, client, nil, &out)
			notices := captureNotices(&p)

			if _, ok := githubBatchLimit(context.Background(), p); ok {
				t.Fatal("githubBatchLimit selected batched mode without a size")
			}
			event := preflightEvent(t, &out)
			assertFields(t, event, map[string]any{"level": "WARN", "http_status": float64(http.StatusOK), "mode": "one_shot"})
			if _, ok := event["size_kb"]; ok {
				t.Fatalf("an unknown size was logged as size_kb=%v", event["size_kb"])
			}
			if msg := eventError(t, event); !strings.Contains(msg, tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", msg, tc.wantErr)
			}
			if len(*notices) != 1 || !strings.Contains((*notices)[0], "single pack") {
				t.Fatalf("notices = %q, want one notice for the unknown size", *notices)
			}
		})
	}
}

func TestGitHubPreflightSanitizesServerMessage(t *testing.T) {
	client := repoAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeAPIError(t, w, http.StatusForbidden, "API rate limit exceeded\x1b[31m\n\u009b2K\u202e"+strings.Repeat("x", 1000))
	})
	var out bytes.Buffer
	p := preflightParams(t, client, nil, &out)

	githubBatchLimit(context.Background(), p)
	msg := eventError(t, preflightEvent(t, &out))
	if strings.ContainsAny(msg, "\x1b\n\u009b\u202e") {
		t.Fatalf("error carries control characters: %q", msg)
	}
	if !strings.HasPrefix(msg, "github api returned 403: API rate limit exceeded") || len(msg) > 300 {
		t.Fatalf("error = %q, want GitHub's message, bounded", msg)
	}
}

func TestGitHubPreflightSmallRepoLogsOneShotDecision(t *testing.T) {
	client := privateRepoAPI(t, 1024)
	var out bytes.Buffer
	p := preflightParams(t, client, installationAuth(), &out)
	notices := captureNotices(&p)

	if _, ok := githubBatchLimit(context.Background(), p); ok {
		t.Fatal("githubBatchLimit selected batched mode for a repo under the threshold")
	}
	event := preflightEvent(t, &out)
	assertFields(t, event, map[string]any{
		"level":       "INFO",
		"http_status": float64(http.StatusOK),
		"size_kb":     float64(1024),
		"mode":        "one_shot",
	})
	if _, ok := event["error"]; ok {
		t.Fatalf("a successful lookup logged an error: %v", event["error"])
	}
	if len(*notices) != 0 {
		t.Fatalf("a successful lookup raised notices: %q", *notices)
	}
}

// A cancelled run is not a failed lookup: it must not tell the operator the
// bootstrap is starting with a single pack.
func TestGitHubPreflightCancelledRunLogsNothing(t *testing.T) {
	client := repoAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeRepoSize(t, w, largeRepoKB)
	})
	var out bytes.Buffer
	p := preflightParams(t, client, installationAuth(), &out)
	notices := captureNotices(&p)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, ok := githubBatchLimit(ctx, p); ok {
		t.Fatal("githubBatchLimit selected batched mode for a cancelled run")
	}
	if out.Len() != 0 || len(*notices) != 0 {
		t.Fatalf("a cancelled run reported a failed lookup: log=%s notices=%q", &out, *notices)
	}
}

// Transport errors carry server- or proxy-authored text (a certificate's
// names, a CONNECT status line), so they are cleaned like GitHub's message.
func TestGitHubPreflightSanitizesTransportError(t *testing.T) {
	var out bytes.Buffer
	p := Params{PushLogger: slog.New(slog.NewJSONHandler(&out, nil))}
	notices := captureNotices(&p)
	lookup := githubSizeLookup{owner: "acme", repo: "private"}
	lookup.err = errors.New("x509: certificate is valid for evil\x1b[2K\u009b2K\u202e, not api.github.com")
	lookup.refused = &githubAPIAnswer{status: http.StatusForbidden, err: errors.New("refused\x1b[31m")}

	p.logGitHubPreflight(context.Background(), lookup, 0)
	event := preflightEvent(t, &out)
	refusedText, ok := event["refused_auth_error"].(string)
	if !ok {
		t.Fatalf("refused_auth_error = %v, want a string", event["refused_auth_error"])
	}
	for _, got := range append([]string{eventError(t, event), refusedText}, *notices...) {
		if strings.ContainsAny(got, "\x1b\u009b\u202e") {
			t.Fatalf("control characters reached the operator: %q", got)
		}
	}
}

// A 403 to a request that carried no credentials (a redirect stripped them)
// is not a refused token: there is nothing to retry and nothing to blame.
func TestGitHubPreflightUnauthenticatedRefusalIsNotRetried(t *testing.T) {
	client, hits := redirectingAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeAPIError(t, w, http.StatusForbidden, "Forbidden")
	})
	var out bytes.Buffer
	p := preflightParams(t, client, installationAuth(), &out)

	githubBatchLimit(context.Background(), p)
	if *hits != 1 {
		t.Fatalf("redirect target hits = %d, want 1: an anonymous retry repeats the same request", *hits)
	}
	event := preflightEvent(t, &out)
	assertFields(t, event, map[string]any{"authenticated": false, "http_status": float64(http.StatusForbidden)})
	if _, ok := event["refused_auth_status"]; ok {
		t.Fatalf("a request without credentials was logged as a refused token: %v", event["refused_auth_status"])
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// Each attempt gets the whole timeout: a slow refusal must not starve the
// anonymous retry that would have batched a public repository.
func TestGitHubPreflightRetryGetsItsOwnTimeout(t *testing.T) {
	client := repoAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			writeAPIError(t, w, http.StatusForbidden, "Resource protected by organization SAML enforcement.")
			return
		}
		writeRepoSize(t, w, largeRepoKB)
	})
	var deadlines []time.Time
	inner := client.Transport
	client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		deadline, ok := req.Context().Deadline()
		if !ok {
			t.Error("a lookup request has no deadline")
		}
		deadlines = append(deadlines, deadline)
		return inner.RoundTrip(req)
	})
	var out bytes.Buffer
	p := preflightParams(t, client, installationAuth(), &out)

	if _, ok := githubBatchLimit(context.Background(), p); !ok {
		t.Fatalf("the anonymous retry failed; log=%s", &out)
	}
	if len(deadlines) != 2 || !deadlines[1].After(deadlines[0]) {
		t.Fatalf("deadlines = %v, want the retry's own, later than the first attempt's", deadlines)
	}
}

func TestGitHubPreflightSkippedLogsNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		endpoint      string
		targetMaxPack int64
	}{
		"caller chose batching": {endpoint: preflightEndpoint, targetMaxPack: 1 << 20},
		// The same host on another port is a different service: GitHub's API
		// does not describe it and must not receive its credentials.
		"non-default port": {endpoint: "https://github.com:8443/acme/private.git"},
		"http on 443":      {endpoint: "http://github.com:443/acme/private.git"},
		"not github":       {endpoint: "https://example.com/acme/private.git"},
		// A dot segment would steer the credentialed request to another API
		// endpoint once the path is joined (/repos/../user is /user).
		"dot-dot owner":         {endpoint: "https://github.com/../user"},
		"encoded dot-dot owner": {endpoint: "https://github.com/%2e%2e/user"},
		"dot repo":              {endpoint: "https://github.com/acme/."},
		"name outside charset":  {endpoint: "https://github.com/acme/pri%3Fvate"},
		// A wiki has no repository of its own for the API to size.
		"wiki": {endpoint: "https://github.com/acme/private.wiki.git"},
	} {
		t.Run(name, func(t *testing.T) {
			client := repoAPI(t, func(w http.ResponseWriter, _ *http.Request) {
				t.Error("unexpected preflight request")
				writeRepoSize(t, w, largeRepoKB)
			})
			var out bytes.Buffer
			p := preflightParamsFor(t, tc.endpoint, client, installationAuth(), &out)
			p.TargetMaxPack = tc.targetMaxPack

			if limit, ok := githubBatchLimit(context.Background(), p); ok {
				t.Fatalf("githubBatchLimit = (%d, true), want no preflight", limit)
			}
			if out.Len() != 0 {
				t.Fatalf("a skipped preflight logged: %s", &out)
			}
		})
	}
}

// The credentials go to the API host only: a redirect elsewhere must arrive
// without them.
func TestGitHubPreflightRedirectDropsCredentials(t *testing.T) {
	var leaked string
	client, hits := redirectingAPI(t, func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization")
		writeRepoSize(t, w, largeRepoKB)
	})
	var out bytes.Buffer
	p := preflightParams(t, client, installationAuth(), &out)

	githubBatchLimit(context.Background(), p)
	if *hits != 1 {
		t.Fatalf("redirect target hits = %d, want 1: the test proves nothing unless the redirect is followed", *hits)
	}
	if leaked != "" {
		t.Fatalf("the redirect target received credentials: %q", leaked)
	}
	// The answer came from a request without credentials, and the record says so.
	assertFields(t, preflightEvent(t, &out), map[string]any{"authenticated": false, "mode": "batched"})
}

// The selection must reach Execute: a private repo over the threshold enters
// batched mode instead of streaming the whole repository as one pack.
func TestExecuteBatchesPrivateGitHubRepoFromAuthenticatedPreflight(t *testing.T) {
	client := privateRepoAPI(t, largeRepoKB)
	var out bytes.Buffer
	p := preflightParams(t, client, installationAuth(), &out)
	p.TargetPusher = fakeBootstrapPusher{}
	mainRef := plumbing.NewBranchReferenceName("main")
	p.DesiredRefs = map[plumbing.ReferenceName]planner.DesiredRef{
		mainRef: {
			SourceRef:  mainRef,
			TargetRef:  mainRef,
			SourceHash: plumbing.NewHash("cccccccccccccccccccccccccccccccccccccccc"),
			Kind:       planner.RefKindBranch,
		},
	}
	p.TargetRefs = map[plumbing.ReferenceName]plumbing.Hash{}
	p.SourceService = fakeBootstrapSource{
		fetchPack: func(context.Context, gitproto.Conn, map[plumbing.ReferenceName]gitproto.DesiredRef, map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
			t.Fatal("a private repo over the threshold fetched a one-shot pack")
			return nil, nil //nolint:nilnil // unreachable after t.Fatal
		},
		fetchCommitParents: func(context.Context, gitproto.Conn, gitproto.DesiredRef, []plumbing.Hash) (map[plumbing.Hash][]plumbing.Hash, error) {
			return nil, errBatchedPathReached
		},
	}

	_, err := Execute(context.Background(), p, "empty target")
	if !errors.Is(err, errBatchedPathReached) {
		t.Fatalf("Execute() error = %v, want the batched path", err)
	}
}
