package sha256convert

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gitsync "entire.io/entire/git-sync"
)

// TestBackSync_SHA256ToReadOnlySHA1 describes the end state of a SHA1 to
// SHA256 transition where the original SHA1 repository stays around as a
// read-only reference. The SHA256 repository becomes canonical and git-sync
// carries main and every tag back to the SHA1 repository.
//
// The expected SHA1 hashes come from an oracle: a plain SHA1 clone of the
// original repository where the same changes are replayed with pinned
// author and committer dates. Back-sync is correct when the SHA1 repository
// ends up byte-identical to that oracle, i.e. as if every change had been
// made on the SHA1 side directly.
func TestBackSync_SHA256ToReadOnlySHA1(t *testing.T) {
	if os.Getenv(gitHTTPBackendEnv) == "" {
		t.Skipf("set %s=1 to run the SHA256 to SHA1 back-sync end-to-end test", gitHTTPBackendEnv)
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git binary not available: %v", err)
	}
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skipf("ssh-keygen not available: %v", err)
	}

	const syncToken = "back-sync-token"

	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	canonical := filepath.Join(root, "canonical.git")
	seed := filepath.Join(root, "seed")
	dev := filepath.Join(root, "dev")
	oracle := filepath.Join(root, "oracle")

	gitAt(t, root, "", "init", "--bare", "--object-format=sha1", "--initial-branch=main", origin)
	gitAt(t, origin, "", "config", "http.receivepack", "true")
	// Pin upload-pack's defaults, so a global config cannot let the target
	// check pass by wanting commits no ref points at.
	gitAt(t, origin, "", "config", "uploadpack.allowReachableSHA1InWant", "false")
	gitAt(t, origin, "", "config", "uploadpack.allowAnySHA1InWant", "false")
	gitAt(t, root, "", "init", "-b", "main", seed)
	configIdentity(t, seed)
	mustWrite(t, filepath.Join(seed, "README"), "hello\n")
	gitAt(t, seed, "1700000000 +0000", "add", "README")
	gitAt(t, seed, "1700000000 +0000", "commit", "-m", "initial")
	mustWrite(t, filepath.Join(seed, "lib.txt"), "lib\n")
	gitAt(t, seed, "1700000100 +0000", "add", "lib.txt")
	gitAt(t, seed, "1700000100 +0000", "commit", "-m", "add lib")
	gitAt(t, seed, "1700000200 +0000", "tag", "-a", "v1.0", "-m", "release 1.0")
	gitAt(t, seed, "", "branch", "legacy")
	gitAt(t, seed, "", "push", origin, "main", "legacy", "v1.0")

	preCutoverMain := revParse(t, origin, "refs/heads/main")
	preCutoverTag := revParse(t, origin, "refs/tags/v1.0")
	preCutoverLegacy := revParse(t, origin, "refs/heads/legacy")
	preCutoverFirst := revParse(t, origin, "refs/heads/main~1")

	srv := newReadOnlyOriginServer(t, gitBin, root, "origin.git", syncToken)
	defer srv.Close()
	originURL := srv.URL + "/origin.git"
	canonicalURL := srv.URL + "/canonical.git"

	if _, err := Run(context.Background(), Request{
		SourceURL: originURL,
		TargetDir: canonical,
		Check:     true,
		Out:       io.Discard,
	}); err != nil {
		t.Fatalf("convert-sha256: %v", err)
	}
	gitAt(t, canonical, "", "config", "http.receivepack", "true")

	if got := strings.TrimSpace(mustGitOutput(t, canonical, "rev-parse", "--show-object-format")); got != "sha256" {
		t.Fatalf("canonical object format: got %q, want sha256", got)
	}

	assertHumanPushRejected(t, root, originURL)

	gitAt(t, root, "", "clone", canonical, dev)
	gitAt(t, root, "", "clone", origin, oracle)
	for _, dir := range []string{dev, oracle} {
		configIdentity(t, dir)
		replayPostCutoverChanges(t, dir)
	}
	gitAt(t, dev, "", "push", "origin", "main", "v1.1", "v2.0", "v2.0-lw")

	gitAt(t, dev, "1700002000 +0000", "checkout", "-b", "experimental")
	mustWrite(t, filepath.Join(dev, "wip.txt"), "unfinished\n")
	gitAt(t, dev, "1700002000 +0000", "add", "wip.txt")
	gitAt(t, dev, "1700002000 +0000", "commit", "-m", "wip")
	gitAt(t, dev, "", "push", "origin", "experimental")
	gitAt(t, dev, "", "checkout", "main")

	client := gitsync.New(gitsync.Options{
		HTTPClient: srv.Client(),
		Auth: gitsync.StaticAuthProvider{
			Target: gitsync.EndpointAuth{Username: "git-sync", Token: syncToken},
		},
	})
	req := gitsync.SyncRequest{
		Source: gitsync.Endpoint{URL: canonicalURL},
		Target: gitsync.Endpoint{URL: originURL},
		Scope:  gitsync.RefScope{Branches: []string{"main"}},
		Policy: gitsync.SyncPolicy{IncludeTags: true, Protocol: gitsync.ProtocolAuto},
	}

	assertOtherSHA1RepoRefused(t, client, req, root, srv.URL)

	// Protocol v1 checks the target holds v1.1's pre-cutover commit, which no
	// ref points at, through a have rather than a want.
	first := req
	first.Policy.Protocol = gitsync.ProtocolV1
	res, err := client.Sync(context.Background(), first)
	if err != nil {
		t.Fatalf("first back-sync: %v", err)
	}
	if res.Counts.Blocked != 0 {
		t.Fatalf("first back-sync blocked refs: %+v", res.Refs)
	}
	assertMatchesOracle(t, origin, oracle,
		"refs/heads/main", "refs/tags/v1.1", "refs/tags/v2.0", "refs/tags/v2.0-lw")

	if got := revParse(t, origin, "refs/tags/v1.0"); got != preCutoverTag {
		t.Errorf("pre-cutover tag v1.0 changed: got %s, want %s", got, preCutoverTag)
	}
	if got := revParse(t, origin, "refs/heads/legacy"); got != preCutoverLegacy {
		t.Errorf("out-of-scope branch legacy changed: got %s, want %s", got, preCutoverLegacy)
	}
	gitAt(t, origin, "", "merge-base", "--is-ancestor", preCutoverMain, "refs/heads/main")
	if got := revParse(t, origin, "refs/tags/v1.1^{commit}"); got != preCutoverFirst {
		t.Errorf("tag created after cutover on a pre-cutover commit must peel to the original SHA1 commit: got %s, want %s", got, preCutoverFirst)
	}
	assertRefAbsent(t, origin, "refs/heads/experimental")
	assertRefAbsent(t, origin, "refs/notes/sha1-origin")
	assertSHA1AndClean(t, origin)
	assertOriginNote(t, origin, canonical)

	assertIncrementalBackSync(t, client, req, origin, canonical, dev, oracle)

	assertNotesRefusalKeepsMain(t, client, req, origin, canonical, dev)
	assertTagRetryRepairsNotes(t, client, req, origin, canonical, dev)

	signingKey := filepath.Join(root, "signing-key")
	keygen := exec.CommandContext(t.Context(), "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", signingKey)
	if out, err := keygen.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	gitAt(t, dev, "", "config", "gpg.format", "ssh")
	gitAt(t, dev, "", "config", "user.signingkey", signingKey)
	gitAt(t, dev, "1700003500 +0000", "commit", "-S", "--allow-empty", "-m", "signed on the SHA256 side")
	gitAt(t, dev, "", "push", "origin", "main")
	res, err = client.Sync(context.Background(), req)
	if err != nil {
		t.Fatalf("signed back-sync: %v", err)
	}
	if res.Counts.Applied != 1 {
		t.Fatalf("signed back-sync: want main applied, got %+v", res.Refs)
	}
	assertSignatureKept(t, dev, origin, "refs/heads/main")
	assertOriginNote(t, origin, canonical)

	bypass := filepath.Join(root, "bypass")
	gitAt(t, root, "", "clone", origin, bypass)
	configIdentity(t, bypass)
	mustWrite(t, filepath.Join(bypass, "README"), "written behind git-sync's back\n")
	gitAt(t, bypass, "1700004000 +0000", "commit", "-am", "direct write")
	gitAt(t, bypass, "", "push", "origin", "main")
	divergedMain := revParse(t, origin, "refs/heads/main")

	mustWrite(t, filepath.Join(dev, "lib.txt"), "lib\nfollow-up\nmore\n")
	gitAt(t, dev, "1700005000 +0000", "commit", "-am", "more")
	gitAt(t, dev, "", "push", "origin", "main")

	res, err = client.Sync(context.Background(), req)
	if err == nil {
		t.Fatalf("back-sync over a directly written SHA1 main must fail, got %+v", res.Refs)
	}
	if res.Counts.Blocked != 1 {
		t.Errorf("back-sync over a directly written SHA1 main: want main blocked, got %+v", res.Refs)
	}
	if got := revParse(t, origin, "refs/heads/main"); got != divergedMain {
		t.Errorf("blocked back-sync moved main: got %s, want %s", got, divergedMain)
	}
}

// assertOtherSHA1RepoRefused syncs to an empty SHA1 repository, which lacks
// the pre-cutover commits the source's notes name.
func assertOtherSHA1RepoRefused(t *testing.T, client *gitsync.Client, req gitsync.SyncRequest, root, serverURL string) {
	t.Helper()
	elsewhere := filepath.Join(root, "elsewhere.git")
	gitAt(t, root, "", "init", "--bare", "--object-format=sha1", "--initial-branch=main", elsewhere)
	gitAt(t, elsewhere, "", "config", "http.receivepack", "true")
	req.Target = gitsync.Endpoint{URL: serverURL + "/elsewhere.git"}
	for _, protocol := range []gitsync.ProtocolMode{gitsync.ProtocolV1, gitsync.ProtocolV2} {
		req.Policy.Protocol = protocol
		if _, err := client.Sync(context.Background(), req); err == nil || !strings.Contains(err.Error(), "does not have") {
			t.Fatalf("%s back-sync to a SHA1 repository that is not the original: got %v, want an error naming the missing commits", protocol, err)
		}
	}
}

// assertNotesRefusalKeepsMain has the target refuse the notes ref, and
// checks that an atomic sync does not move main without them.
func assertNotesRefusalKeepsMain(t *testing.T, client *gitsync.Client, req gitsync.SyncRequest, origin, canonical, dev string) {
	t.Helper()
	req.Policy.Atomic = true
	hook := filepath.Join(origin, "hooks", "update")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntest \"$1\" != refs/notes/sha256-origin\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	beforeRefusal := revParse(t, origin, "refs/heads/main")
	gitAt(t, dev, "1700003200 +0000", "commit", "--allow-empty", "-m", "notes refused")
	gitAt(t, dev, "", "push", "origin", "main")
	if res, err := client.Sync(context.Background(), req); err == nil {
		t.Fatalf("back-sync with the notes ref refused must fail, got %+v", res.Refs)
	}
	if got := revParse(t, origin, "refs/heads/main"); got != beforeRefusal {
		t.Fatalf("main moved although its notes were refused: got %s, want %s", got, beforeRefusal)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if res, err := client.Sync(context.Background(), req); err != nil || res.Counts.Applied != 1 {
		t.Fatalf("back-sync after the refusal: err %v, refs %+v", err, res.Refs)
	}
	assertOriginNote(t, origin, canonical)
}

// assertIncrementalBackSync syncs one new commit on main, then syncs again
// with nothing new.
func assertIncrementalBackSync(t *testing.T, client *gitsync.Client, req gitsync.SyncRequest, origin, canonical, dev, oracle string) {
	t.Helper()
	for _, dir := range []string{dev, oracle} {
		mustWrite(t, filepath.Join(dir, "lib.txt"), "lib\nfollow-up\n")
		gitAt(t, dir, "1700003000 +0000", "commit", "-am", "follow-up")
	}
	gitAt(t, dev, "", "push", "origin", "main")

	res, err := client.Sync(context.Background(), req)
	if err != nil {
		t.Fatalf("incremental back-sync: %v", err)
	}
	if res.Counts.Applied != 1 || res.Counts.Blocked != 0 {
		t.Fatalf("incremental back-sync: want exactly main applied, got %+v", res.Refs)
	}
	assertMatchesOracle(t, origin, oracle, "refs/heads/main")
	assertSHA1AndClean(t, origin)
	assertOriginNote(t, origin, canonical)

	res, err = client.Sync(context.Background(), req)
	if err != nil {
		t.Fatalf("no-op back-sync: %v", err)
	}
	if res.Counts.Applied != 0 || res.Counts.Blocked != 0 {
		t.Fatalf("no-op back-sync must apply nothing, got %+v", res.Refs)
	}
}

// assertTagRetryRepairsNotes has the target take a new tag on a commit no
// branch reaches while refusing its notes. A plain retry leaves the tag
// alone and writes nothing. One with RetranslateTags writes the missing note
// and still leaves the tag alone.
func assertTagRetryRepairsNotes(t *testing.T, client *gitsync.Client, req gitsync.SyncRequest, origin, canonical, dev string) {
	t.Helper()
	hook := filepath.Join(origin, "hooks", "update")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntest \"$1\" != refs/notes/sha256-origin\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitAt(t, dev, "", "checkout", "-q", "--detach")
	gitAt(t, dev, "1700003300 +0000", "commit", "--allow-empty", "-m", "tagged only")
	gitAt(t, dev, "", "tag", "v3.0")
	gitAt(t, dev, "", "push", "origin", "v3.0")
	gitAt(t, dev, "", "checkout", "-q", "main")
	if res, err := client.Sync(context.Background(), req); err == nil {
		t.Fatalf("back-sync with the notes ref refused must fail, got %+v", res.Refs)
	}
	tagged := revParse(t, origin, "refs/tags/v3.0")
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}

	if _, err := client.Sync(context.Background(), req); err != nil {
		t.Fatalf("plain retry: %v", err)
	}
	if _, ok := originNote(t, origin, tagged); ok {
		t.Fatalf("a plain retry must leave tags alone, but wrote a note for %s", tagged)
	}

	req.Policy.RetranslateTags = true
	res, err := client.Sync(context.Background(), req)
	if err != nil || res.Counts.Applied != 0 {
		t.Fatalf("retry with RetranslateTags: err %v, refs %+v", err, res.Refs)
	}
	note, ok := originNote(t, origin, tagged)
	if want := revParse(t, canonical, "refs/tags/v3.0"); !ok || note != want {
		t.Errorf("sha256-origin note for v3.0: got %q, want %s", note, want)
	}
	if got := revParse(t, origin, "refs/tags/v3.0"); got != tagged {
		t.Errorf("RetranslateTags moved v3.0: got %s, want %s", got, tagged)
	}
}

func originNote(t *testing.T, repo, commit string) (string, bool) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "git", "-C", repo, "notes", "--ref=sha256-origin", "show", commit).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// assertSignatureKept checks that the commit ref points at on the SHA1
// repository carries the SHA256 signature of the commit it translates, byte
// for byte, as git's object conversion keeps it.
func assertSignatureKept(t *testing.T, sha256Repo, sha1Repo, ref string) {
	t.Helper()
	signature := func(repo string) string {
		raw := mustGitOutput(t, repo, "cat-file", "commit", ref)
		header, _, _ := strings.Cut(raw, "\n\n")
		start := strings.Index(header, "\ngpgsig-sha256 ")
		if start < 0 {
			t.Fatalf("%s in %s carries no gpgsig-sha256 header:\n%s", ref, repo, raw)
		}
		return header[start:]
	}
	if got, want := signature(sha1Repo), signature(sha256Repo); got != want {
		t.Errorf("gpgsig-sha256 on the SHA1 repository:\n%s\nwant:\n%s", got, want)
	}
}

// assertOriginNote checks that the SHA1 repository records, for the commit
// ref points at, the SHA256 commit it was translated from.
func assertOriginNote(t *testing.T, sha1Repo, sha256Repo string) {
	const ref = "refs/heads/main"
	t.Helper()
	tip := revParse(t, sha1Repo, ref)
	note := strings.TrimSpace(mustGitOutput(t, sha1Repo, "notes", "--ref=sha256-origin", "show", tip))
	if want := revParse(t, sha256Repo, ref); note != want {
		t.Errorf("sha256-origin note for %s: got %q, want %s", ref, note, want)
	}
	if n := strings.TrimSpace(mustGitOutput(t, sha1Repo, "rev-list", "--count", "refs/notes/sha256-origin")); n != "1" {
		t.Errorf("refs/notes/sha256-origin holds %s commits, want a single snapshot", n)
	}
}

// replayPostCutoverChanges makes the same post-cutover changes in dir with
// pinned dates, so a SHA256 clone and a SHA1 clone produce objects whose
// content differs only in embedded hashes. It covers the shapes back-sync
// must translate: plain commits, exec and symlink tree entries, a merge
// commit, annotated and lightweight tags, and a new tag on a pre-cutover
// commit.
func replayPostCutoverChanges(t *testing.T, dir string) {
	t.Helper()

	mustWrite(t, filepath.Join(dir, "feature.txt"), "feature\n")
	mustWrite(t, filepath.Join(dir, "run.sh"), "#!/bin/sh\necho hi\n")
	if err := os.Chmod(filepath.Join(dir, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("feature.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	gitAt(t, dir, "1700001000 +0000", "add", "feature.txt", "run.sh", "link")
	gitAt(t, dir, "1700001000 +0000", "commit", "-m", "add feature")

	gitAt(t, dir, "1700001100 +0000", "checkout", "-b", "topic")
	mustWrite(t, filepath.Join(dir, "topic.txt"), "topic\n")
	gitAt(t, dir, "1700001100 +0000", "add", "topic.txt")
	gitAt(t, dir, "1700001100 +0000", "commit", "-m", "topic work")
	gitAt(t, dir, "1700001200 +0000", "checkout", "main")
	gitAt(t, dir, "1700001200 +0000", "merge", "--no-ff", "-m", "merge topic", "topic")

	gitAt(t, dir, "1700001300 +0000", "tag", "-a", "v2.0", "-m", "release 2.0")
	gitAt(t, dir, "", "tag", "v2.0-lw")
	gitAt(t, dir, "1700001400 +0000", "tag", "-a", "v1.1", "-m", "re-tag of the first commit", "v1.0^{commit}~1")
}

// newReadOnlyOriginServer serves every repo under root through
// git-http-backend, and refuses pushes to readOnlyRepo unless they carry
// the back-sync token. That stands in for a forge's branch protection that
// lets only the git-sync identity write to the reference repository.
func newReadOnlyOriginServer(t *testing.T, gitBin, root, readOnlyRepo, token string) *httptest.Server {
	t.Helper()
	backend := &cgi.Handler{
		Path: gitBin,
		Args: []string{"http-backend"},
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isPush := strings.HasSuffix(r.URL.Path, "/git-receive-pack") ||
			r.URL.Query().Get("service") == "git-receive-pack"
		if isPush && strings.HasPrefix(r.URL.Path, "/"+readOnlyRepo+"/") {
			if _, pass, ok := r.BasicAuth(); !ok || pass != token {
				w.Header().Set("WWW-Authenticate", `Basic realm="read-only"`)
				http.Error(w, "repository is read-only", http.StatusUnauthorized)
				return
			}
		}
		// net/http/cgi refuses chunked request bodies, and git-sync streams
		// its pushes. Buffer the body so the CGI gets a Content-Length.
		if len(r.TransferEncoding) > 0 {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			r.TransferEncoding = nil
		}
		backend.ServeHTTP(w, r)
	}))
}

func assertHumanPushRejected(t *testing.T, root, originURL string) {
	t.Helper()
	human := filepath.Join(root, "human")
	gitAt(t, root, "", "clone", originURL, human)
	configIdentity(t, human)
	mustWrite(t, filepath.Join(human, "README"), "sneaky\n")
	gitAt(t, human, "1700000500 +0000", "commit", "-am", "direct push")
	cmd := exec.CommandContext(t.Context(), "git", "push", "origin", "main")
	cmd.Dir = human
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("push to the read-only SHA1 repository succeeded without the sync identity:\n%s", out)
	}
}

func assertMatchesOracle(t *testing.T, repo, oracle string, refs ...string) {
	t.Helper()
	for _, ref := range refs {
		want := revParse(t, oracle, ref)
		got, ok := tryRevParse(t, repo, ref)
		if !ok {
			t.Errorf("%s missing on the SHA1 repository, want %s", ref, want)
			continue
		}
		if got != want {
			t.Errorf("%s: got %s, want %s (the SHA1 history a direct commit would have produced)", ref, got, want)
		}
	}
}

func assertRefAbsent(t *testing.T, repo, ref string) {
	t.Helper()
	if got, ok := tryRevParse(t, repo, ref); ok {
		t.Errorf("%s must not be synced to the SHA1 repository, found %s", ref, got)
	}
}

func assertSHA1AndClean(t *testing.T, repo string) {
	t.Helper()
	if got := strings.TrimSpace(mustGitOutput(t, repo, "rev-parse", "--show-object-format")); got != "sha1" {
		t.Errorf("object format of the reference repository: got %q, want sha1", got)
	}
	out, err := exec.CommandContext(t.Context(), "git", "-C", repo, "fsck", "--full", "--strict").CombinedOutput()
	if err != nil {
		t.Errorf("git fsck on the reference repository: %v\n%s", err, out)
	}
}

func configIdentity(t *testing.T, dir string) {
	t.Helper()
	gitAt(t, dir, "", "config", "user.name", "back-sync test")
	gitAt(t, dir, "", "config", "user.email", "back-sync@example.com")
	gitAt(t, dir, "", "config", "commit.gpgsign", "false")
	gitAt(t, dir, "", "config", "tag.gpgsign", "false")
}

// gitAt runs git in dir. A non-empty date pins both the author and
// committer dates, which is what makes the oracle replay deterministic.
func gitAt(t *testing.T, dir, date string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if date != "" {
		cmd.Env = append(cmd.Env, "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func revParse(t *testing.T, repo, rev string) string {
	t.Helper()
	return strings.TrimSpace(mustGitOutput(t, repo, "rev-parse", "--verify", "--quiet", rev))
}

func tryRevParse(t *testing.T, repo, rev string) (string, bool) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "git", "-C", repo, "rev-parse", "--verify", "--quiet", rev).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}
