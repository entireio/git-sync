package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/transport"
	transporthttp "github.com/go-git/go-git/v6/plumbing/transport/http"

	"entire.io/entire/git-sync/internal/gitproto"
	"entire.io/entire/git-sync/internal/planner"
)

const preflightToken = "ghs_preflight-secret"

var errBatchedPathReached = errors.New("batched path reached")

// privateRepoAPI serves GET /repos/acme/private like GitHub does for a private
// repository: 404 to an anonymous caller, the metadata to one presenting the
// installation token.
func privateRepoAPI(t *testing.T, sizeKB int64) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/private" {
			t.Errorf("unexpected preflight request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "x-access-token" || pass != preflightToken {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"full_name": "acme/private", "size": sizeKB})
	}))
	t.Cleanup(server.Close)
	useGitHubAPI(t, server.URL)
	return server
}

func useGitHubAPI(t *testing.T, base string) {
	t.Helper()
	prev := GitHubRepoAPIBaseURL
	GitHubRepoAPIBaseURL = base
	t.Cleanup(func() { GitHubRepoAPIBaseURL = prev })
}

func preflightParams(t *testing.T, client *http.Client, auth gitproto.AuthMethod, out *bytes.Buffer) Params {
	t.Helper()
	ep, err := transport.ParseURL("https://github.com/acme/private.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	return Params{
		SourceConn:    gitproto.NewHTTPConnWithClient(ep, "source", auth, client),
		SourceService: fakeBootstrapSource{},
		PushLogger:    slog.New(slog.NewJSONHandler(out, nil)),
	}
}

func preflightEvent(t *testing.T, out *bytes.Buffer) map[string]any {
	t.Helper()
	var event map[string]any
	if err := json.Unmarshal(out.Bytes(), &event); err != nil {
		t.Fatalf("expected exactly one preflight log record: %v; output=%s", err, out)
	}
	if event["msg"] != "Bootstrap GitHub size preflight" {
		t.Fatalf("msg = %v, want the preflight record; output=%s", event["msg"], out)
	}
	if strings.Contains(out.String(), preflightToken) {
		t.Fatalf("preflight log leaked the token: %s", out)
	}
	return event
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
	server := privateRepoAPI(t, 7*1024*1024)
	var out bytes.Buffer
	auth := &transporthttp.BasicAuth{Username: "x-access-token", Password: preflightToken}
	p := preflightParams(t, server.Client(), auth, &out)

	limit, ok := githubBatchLimit(context.Background(), p)
	if !ok || limit != defaultTargetMaxPackBytes {
		t.Fatalf("githubBatchLimit = (%d, %v), want (%d, true): a private repo over the threshold must bootstrap batched",
			limit, ok, int64(defaultTargetMaxPackBytes))
	}
	assertFields(t, preflightEvent(t, &out), map[string]any{
		"level":                 "INFO",
		"repository":            "acme/private",
		"authenticated":         true,
		"http_status":           float64(http.StatusOK),
		"size_kb":               float64(7 * 1024 * 1024),
		"threshold_kb":          float64(githubLargeRepoThresholdKB),
		"mode":                  "batched",
		"target_max_pack_bytes": float64(defaultTargetMaxPackBytes),
	})
}

func TestGitHubPreflightLookupFailureLogsWarningAndKeepsOneShot(t *testing.T) {
	server := privateRepoAPI(t, 7*1024*1024)
	var out bytes.Buffer
	p := preflightParams(t, server.Client(), nil, &out)

	if limit, ok := githubBatchLimit(context.Background(), p); ok {
		t.Fatalf("githubBatchLimit = (%d, true), want one-shot when the size is unknown", limit)
	}
	event := preflightEvent(t, &out)
	assertFields(t, event, map[string]any{
		"level":         "WARN",
		"repository":    "acme/private",
		"authenticated": false,
		"http_status":   float64(http.StatusNotFound),
		"mode":          "one_shot",
	})
	if msg, _ := event["error"].(string); !strings.Contains(msg, "404") || !strings.Contains(msg, "Not Found") {
		t.Fatalf("error = %q, want the status and GitHub's message", msg)
	}
}

func TestGitHubPreflightTransportErrorLogsWarning(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	useGitHubAPI(t, server.URL)
	client := server.Client()
	server.Close()
	var out bytes.Buffer
	p := preflightParams(t, client, nil, &out)

	if _, ok := githubBatchLimit(context.Background(), p); ok {
		t.Fatal("githubBatchLimit selected batched mode without a size")
	}
	event := preflightEvent(t, &out)
	assertFields(t, event, map[string]any{"level": "WARN", "http_status": float64(0), "mode": "one_shot"})
	if msg, _ := event["error"].(string); msg == "" {
		t.Fatal("transport failure logged without an error")
	}
}

func TestGitHubPreflightUnreadableBodyLogsWarning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>not json</html>`))
	}))
	t.Cleanup(server.Close)
	useGitHubAPI(t, server.URL)
	var out bytes.Buffer
	p := preflightParams(t, server.Client(), nil, &out)

	if _, ok := githubBatchLimit(context.Background(), p); ok {
		t.Fatal("githubBatchLimit selected batched mode without a size")
	}
	assertFields(t, preflightEvent(t, &out), map[string]any{"level": "WARN", "http_status": float64(http.StatusOK), "mode": "one_shot"})
}

func TestGitHubPreflightSmallRepoLogsOneShotDecision(t *testing.T) {
	server := privateRepoAPI(t, 1024)
	var out bytes.Buffer
	auth := &transporthttp.BasicAuth{Username: "x-access-token", Password: preflightToken}
	p := preflightParams(t, server.Client(), auth, &out)

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
}

func TestGitHubPreflightSkippedLogsNothing(t *testing.T) {
	server := privateRepoAPI(t, 7*1024*1024)
	var out bytes.Buffer
	p := preflightParams(t, server.Client(), nil, &out)
	p.TargetMaxPack = 1 << 20 // the caller already chose batched mode

	if limit, ok := githubBatchLimit(context.Background(), p); ok {
		t.Fatalf("githubBatchLimit = (%d, true), want the caller's choice to stand", limit)
	}
	if out.Len() != 0 {
		t.Fatalf("a skipped preflight logged: %s", &out)
	}
}

// The selection must reach Execute: a private repo over the threshold enters
// batched mode instead of streaming the whole repository as one pack.
func TestExecuteBatchesPrivateGitHubRepoFromAuthenticatedPreflight(t *testing.T) {
	server := privateRepoAPI(t, 7*1024*1024)
	var out bytes.Buffer
	auth := &transporthttp.BasicAuth{Username: "x-access-token", Password: preflightToken}
	p := preflightParams(t, server.Client(), auth, &out)
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
