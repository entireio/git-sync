package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"entire.io/entire/git-sync/internal/gitproto"
	"entire.io/entire/git-sync/internal/sanitize"
	"entire.io/entire/git-sync/internal/useragent"
)

// githubLargeRepoThresholdKB is the reported size at which a GitHub source
// bootstraps in batches.
const githubLargeRepoThresholdKB = 1536 * 1024

// GitHubRepoAPIBaseURL is the base for GitHub API calls (replaceable in tests).
var GitHubRepoAPIBaseURL = "https://api.github.com"

// githubBatchLimit asks GitHub how large the source repository is and returns
// the batch limit to bootstrap with when it is at or over
// githubLargeRepoThresholdKB. It reports false, keeping the one-shot path,
// whenever the size is unknown.
//
// Every lookup it makes is logged through PushLogger, which callers wire
// independently of verbose mode, and a failed one is also surfaced as a notice:
// it sends a repository of any size as one pack, which an operator has to be
// able to see without a code read.
func githubBatchLimit(ctx context.Context, p Params) (int64, bool) {
	if p.TargetMaxPack > 0 || p.SourceConn == nil || p.SourceConn.Endpoint() == nil {
		return 0, false
	}
	if p.SourceService == nil || !p.SourceService.SupportsBootstrapBatch() {
		return 0, false
	}
	lookup, ok := lookupGitHubRepoSize(ctx, p.SourceConn)
	if !ok || ctx.Err() != nil {
		// A cancelled run is not a failed lookup, and the bootstrap it would
		// report on is not going to start.
		return 0, false
	}
	var limit int64
	if lookup.err == nil && lookup.sizeKB >= githubLargeRepoThresholdKB {
		limit = defaultTargetMaxPackBytes
		if p.MaxPackBytes > 0 && p.MaxPackBytes < limit {
			limit = p.MaxPackBytes
		}
	}
	p.logGitHubPreflight(ctx, lookup, limit)
	return limit, limit > 0
}

// githubPreflightTimeout bounds each size-lookup request. The lookup only
// chooses between two working strategies, so a stalled API must not hold the
// bootstrap. Each attempt gets its own, so a slow refusal does not starve the
// anonymous retry.
const githubPreflightTimeout = 30 * time.Second

// maxGitHubRepoResponseBytes bounds how much of a repository metadata response
// is read. The real document is a few KiB.
const maxGitHubRepoResponseBytes = 1 << 20

// maxGitHubErrorMessageBytes bounds the server-authored message carried into
// logs and notices.
const maxGitHubErrorMessageBytes = 200

// githubPreflightMsg names the size-lookup record in both the verbose log and
// PushLogger, so one query finds either.
const githubPreflightMsg = "Bootstrap GitHub size preflight"

// githubAPIAnswer is the outcome of one request for the repository metadata.
type githubAPIAnswer struct {
	// authenticated reports whether the request that produced the answer
	// carried credentials; a redirect that stripped them makes it false.
	authenticated bool
	status        int   // HTTP status; 0 when no response arrived
	sizeKB        int64 // valid only when err is nil
	err           error
}

// githubSizeLookup is the outcome of one GitHub repository-size lookup.
type githubSizeLookup struct {
	// githubAPIAnswer is the answer the batching decision rests on.
	githubAPIAnswer

	owner, repo string
	// refused is the authenticated attempt the API refused (401/403) before
	// the anonymous retry; nil when there was none. Its err, always set,
	// names the real cause (SSO enforcement, an exhausted rate limit) that
	// the retry's own answer, a 404 for a private repository, would hide.
	refused *githubAPIAnswer
}

// lookupGitHubRepoSize asks the GitHub REST API for the size of the source
// repository. It reports false when the source is not a GitHub repository
// reached over HTTP, in which case no request was made.
//
// The source credentials go along when HTTPConn.CredentialsFor allows it:
// GitHub answers 404 to an anonymous caller for a private repository. A token
// the API refuses although git accepted it (one not authorized for an
// organization's SSO, say) is retried once anonymously, so a public repository
// is never worse off for having credentials.
func lookupGitHubRepoSize(ctx context.Context, conn gitproto.Conn) (githubSizeLookup, bool) {
	httpConn, ok := conn.(*gitproto.HTTPConn)
	if !ok || httpConn.HTTP == nil {
		return githubSizeLookup{}, false
	}
	owner, repo, ok := GitHubOwnerRepo(conn)
	if !ok {
		return githubSizeLookup{}, false
	}
	lookup := githubSizeLookup{owner: owner, repo: repo}
	base, err := url.Parse(GitHubRepoAPIBaseURL)
	if err != nil {
		lookup.err = fmt.Errorf("parse github api base url: %w", err)
		return lookup, true
	}
	apiURL := base.JoinPath("repos", owner, repo)
	if creds := httpConn.CredentialsFor(apiURL); creds != nil {
		answer := requestGitHubRepoSize(ctx, httpConn.HTTP, apiURL, creds)
		// Only an answer to a request that still carried the credentials
		// refuses them; one a redirect stripped them from would get the
		// same answer again anonymously.
		refused := answer.status == http.StatusUnauthorized || answer.status == http.StatusForbidden
		if !refused || !answer.authenticated {
			lookup.githubAPIAnswer = answer
			return lookup, true
		}
		lookup.refused = &answer
	}
	lookup.githubAPIAnswer = requestGitHubRepoSize(ctx, httpConn.HTTP, apiURL, nil)
	return lookup, true
}

func requestGitHubRepoSize(ctx context.Context, client *http.Client, apiURL *url.URL, auth gitproto.AuthMethod) githubAPIAnswer {
	ctx, cancel := context.WithTimeout(ctx, githubPreflightTimeout)
	defer cancel()
	var answer githubAPIAnswer
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL.String(), nil)
	if err != nil {
		answer.err = fmt.Errorf("build request: %w", err)
		return answer
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-Github-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", useragent.Plain())
	req.Header.Set(gitproto.StatsPhaseHeader, "github repo metadata")
	gitproto.ApplyAuth(req, auth)
	answer.authenticated = req.Header.Get("Authorization") != "" // kept on a transport error
	resp, err := client.Do(req)
	if err != nil {
		answer.err = err
		return answer
	}
	defer resp.Body.Close()
	// The redirect guard drops Authorization on a hop that leaves the API's
	// site, so whether the answer was authenticated is read off the request
	// that produced it, not the one sent. A caller-supplied transport may
	// leave Response.Request unset; then the provenance is unknown and
	// authentication is not claimed.
	answer.authenticated = resp.Request != nil && resp.Request.Header.Get("Authorization") != ""
	answer.status = resp.StatusCode
	body := io.LimitReader(resp.Body, maxGitHubRepoResponseBytes)
	if resp.StatusCode != http.StatusOK {
		answer.err = githubAPIError(resp.StatusCode, body)
		return answer
	}
	var payload struct {
		Size *int64 `json:"size"`
	}
	if err := json.NewDecoder(body).Decode(&payload); err != nil {
		answer.err = fmt.Errorf("decode repository metadata: %w", err)
		return answer
	}
	if payload.Size == nil || *payload.Size < 0 {
		answer.err = errors.New("repository metadata reports no size")
		return answer
	}
	// GitHub reports 0 for an empty repository and for one whose size it has
	// not computed yet (a fresh push or import). Bootstrap only runs with
	// source refs, so here it is the latter: a size as unknown as no size.
	if *payload.Size == 0 {
		answer.err = errors.New("repository metadata reports size 0; GitHub may not have sized it yet")
		return answer
	}
	answer.sizeKB = *payload.Size
	return answer
}

// githubAPIError describes a non-200 answer, carrying GitHub's own message
// ("Not Found", "API rate limit exceeded ...") when the body has one, made
// safe and bounded before it reaches a log or a terminal.
func githubAPIError(status int, body io.Reader) error {
	var payload struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(body).Decode(&payload) //nolint:errcheck // a body without a usable message leaves Message empty
	msg := sanitize.Line(payload.Message)
	if msg == "" {
		return fmt.Errorf("github api returned %d", status)
	}
	if len(msg) > maxGitHubErrorMessageBytes {
		msg = strings.ToValidUTF8(msg[:maxGitHubErrorMessageBytes], "") + "..."
	}
	return fmt.Errorf("github api returned %d: %s", status, msg)
}

func (p Params) logGitHubPreflight(ctx context.Context, lookup githubSizeLookup, limit int64) {
	mode := "one_shot"
	if limit > 0 {
		mode = "batched"
	}
	attrs := []any{
		slog.String("repository", lookup.owner+"/"+lookup.repo),
		slog.Bool("authenticated", lookup.authenticated),
		slog.Int("http_status", lookup.status),
		slog.Int64("threshold_kb", githubLargeRepoThresholdKB),
		slog.String("mode", mode),
	}
	// Transport errors carry server- or proxy-authored text too (a
	// certificate's names, a CONNECT status line), so every error is cleaned,
	// not only GitHub's message.
	var refusedText string
	if lookup.refused != nil {
		refusedText = sanitize.Line(lookup.refused.err.Error())
		attrs = append(attrs,
			slog.Int("refused_auth_status", lookup.refused.status),
			slog.String("refused_auth_error", refusedText))
	}
	level := slog.LevelInfo
	if lookup.err == nil {
		attrs = append(attrs, slog.Int64("size_kb", lookup.sizeKB))
		if limit > 0 {
			attrs = append(attrs, slog.Int64("target_max_pack_bytes", limit))
		}
		p.log(githubPreflightMsg, attrs...)
	} else {
		level = slog.LevelWarn
		errText := sanitize.Line(lookup.err.Error())
		attrs = append(attrs, slog.String("error", errText))
		// The notice reaches the operator verbose or not, so the verbose log
		// stays out of it rather than print the failure twice under -v.
		reason := errText
		if lookup.refused != nil {
			reason = fmt.Sprintf("credentials refused: %s; anonymous retry: %s", refusedText, errText)
		}
		// "Starting": a target that rejects the pack as too large still
		// switches the bootstrap to batches later.
		p.notice(fmt.Sprintf("could not read the GitHub repository size (%s) — starting with a single pack; set a target max pack size to batch from the start",
			reason))
	}
	if p.PushLogger != nil {
		p.PushLogger.Log(ctx, level, githubPreflightMsg, attrs...)
	}
}

// GitHubOwnerRepo extracts the owner/repo from a GitHub endpoint. Only
// github.com on the default port of its scheme qualifies: the same host on
// another port is a different service, which the GitHub API does not describe.
// A wiki ("owner/repo.wiki") does not qualify either: the API has no
// repository of that name to size.
func GitHubOwnerRepo(conn gitproto.Conn) (string, string, bool) {
	if conn == nil || conn.Endpoint() == nil {
		return "", "", false
	}
	ep := conn.Endpoint()
	if (ep.Scheme != "https" && ep.Scheme != "http") || !gitproto.UsesDefaultPort(ep) {
		return "", "", false
	}
	if !strings.EqualFold(ep.Hostname(), "github.com") {
		return "", "", false
	}
	path := strings.TrimSuffix(strings.Trim(ep.Path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || !isGitHubName(parts[0]) || !isGitHubName(parts[1]) || strings.HasSuffix(parts[1], ".wiki") {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// githubNamePattern is the character set GitHub allows in owner and
// repository names.
var githubNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// isGitHubName reports whether s can be a GitHub owner or repository name.
// The names become segments of a credentialed API path, so a dot segment
// ("." or "..", possibly percent-encoded in the source URL) must not get
// through: path joining would resolve it and send the token to a different
// endpoint.
func isGitHubName(s string) bool {
	return s != "." && s != ".." && githubNamePattern.MatchString(s)
}
