package translate

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/storage/memory"
)

// TestTranslate_MatchesGitCompatObjectFormat checks the translator against
// git's own conversion (object-file-convert.c). It builds the objects
// t/t1016-compatObjectFormat.sh builds, in a repository with
// extensions.compatObjectFormat set, so git records the other format's hash
// of every object it writes. Translating each object must produce exactly
// that hash, in both directions.
func TestTranslate_MatchesGitCompatObjectFormat(t *testing.T) {
	t.Parallel()
	requireCompatGit(t)

	for _, tc := range []struct{ from, to formatcfg.ObjectFormat }{
		{formatcfg.SHA256, formatcfg.SHA1},
		{formatcfg.SHA1, formatcfg.SHA256},
	} {
		t.Run(tc.from.String()+"-to-"+tc.to.String(), func(t *testing.T) {
			t.Parallel()
			repo := buildCompatRepo(t, tc.from, tc.to)
			objects := catAllObjects(t, repo)
			requireCoverage(t, objects)

			src := memory.NewStorage(memory.WithObjectFormat(tc.from))
			for _, o := range objects {
				if got := writeRaw(t, src, o.typ, string(o.content)); got != o.hash {
					t.Fatalf("stored %s as %s", o.hash, got)
				}
			}
			want := compatHashes(t, repo, tc.to, objects)

			tr := New(src, memory.NewStorage(memory.WithObjectFormat(tc.to)), tc.from, tc.to, nil)
			for _, o := range objects {
				got, err := tr.Translate(o.hash)
				if err != nil {
					t.Errorf("translate %s %s: %v", o.typ, o.hash, err)
					continue
				}
				if got != want[o.hash] {
					t.Errorf("%s %s: got %s, git computes %s\n%s", o.typ, o.hash, got, want[o.hash], o.content)
				}
			}
		})
	}
}

// requireCoverage fails when the repository lacks an object shape the test
// exists to check, such as when signing silently did nothing.
func requireCoverage(t *testing.T, objects []rawObject) {
	t.Helper()
	shapes := map[string]bool{
		"commit with both signatures":    false,
		"commit with only gpgsig":        false,
		"commit with only gpgsig-sha256": false,
		"commit with mergetag":           false,
		"commit with encoding":           false,
		"tag with header signature":      false,
		"tag with body signature":        false,
		"tag with NUL in signature":      false,
		"tag with NUL in header":         false,
	}
	for _, o := range objects {
		header, _, _ := bytes.Cut(o.content, []byte("\n\n"))
		sha1Sig := bytes.Contains(header, []byte("\ngpgsig "))
		sha256Sig := bytes.Contains(header, []byte("\ngpgsig-sha256 "))
		if o.typ == plumbing.CommitObject {
			shapes["commit with both signatures"] = shapes["commit with both signatures"] || sha1Sig && sha256Sig
			shapes["commit with only gpgsig"] = shapes["commit with only gpgsig"] || sha1Sig && !sha256Sig
			shapes["commit with only gpgsig-sha256"] = shapes["commit with only gpgsig-sha256"] || !sha1Sig && sha256Sig
			shapes["commit with mergetag"] = shapes["commit with mergetag"] || bytes.Contains(header, []byte("\nmergetag "))
			shapes["commit with encoding"] = shapes["commit with encoding"] || bytes.Contains(header, []byte("\nencoding "))
		}
		if o.typ == plumbing.TagObject {
			shapes["tag with header signature"] = shapes["tag with header signature"] || sha1Sig || sha256Sig
			start := signatureStart(o.content)
			shapes["tag with body signature"] = shapes["tag with body signature"] || start < len(o.content)
			shapes["tag with NUL in signature"] = shapes["tag with NUL in signature"] || bytes.IndexByte(o.content[start:], 0) >= 0
			shapes["tag with NUL in header"] = shapes["tag with NUL in header"] || bytes.IndexByte(header, 0) >= 0
		}
	}
	for shape, seen := range shapes {
		if !seen {
			t.Errorf("test repository has no %s", shape)
		}
	}
}

type rawObject struct {
	hash    plumbing.Hash
	typ     plumbing.ObjectType
	content []byte
}

func requireCompatGit(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"git", "ssh-keygen"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available: %v", bin, err)
		}
	}
	dir := t.TempDir()
	runGit(t, dir, nil, "init", "-q", "--object-format=sha256", ".")
	runGit(t, dir, nil, "config", "extensions.compatObjectFormat", "sha1")
	runGit(t, dir, nil, "commit", "-q", "--allow-empty", "-m", "probe")
	cmd := exec.CommandContext(t.Context(), "git", "rev-parse", "--output-object-format=sha1", "HEAD")
	cmd.Dir = dir
	cmd.Env = isolatedGitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git does not support extensions.compatObjectFormat: %v\n%s", err, out)
	}
}

// buildCompatRepo mirrors t1016's setup: plain, tagged and signed objects,
// a signed merge of signed tags, and commits and tags with one of their two
// signatures removed.
func buildCompatRepo(t *testing.T, format, compat formatcfg.ObjectFormat) string {
	t.Helper()
	repo := t.TempDir()
	key := filepath.Join(t.TempDir(), "key")
	keygen := exec.CommandContext(t.Context(), "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key)
	if out, err := keygen.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}

	git := func(args ...string) string { return runGit(t, repo, nil, args...) }
	git("init", "-q", "--object-format="+format.String(), ".")
	git("config", "core.repositoryformatversion", "1")
	git("config", "extensions.compatObjectFormat", compat.String())
	git("config", "gpg.format", "ssh")
	git("config", "user.signingkey", key)

	writeFile(t, filepath.Join(repo, "hello"), "Hello World!\n")
	writeFile(t, filepath.Join(repo, "run.sh"), "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(repo, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hello", filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, "dir", "nested"), "nested\n")
	git("add", ".")
	git("commit", "-q", "-m", "Initial commit")
	commit := strings.TrimSpace(git("rev-parse", "HEAD"))
	git("tag", "--no-sign", "-m", "This is a tag", "hellotag", strings.TrimSpace(git("rev-parse", "HEAD:hello")))
	git("tag", "--no-sign", "-m", "This is a tag", "treetag", strings.TrimSpace(git("rev-parse", "HEAD^{tree}")))
	git("tag", "--no-sign", "-m", "This is a tag", "committag", commit)
	git("commit", "-q", "--gpg-sign", "--allow-empty", "-m", "This is a signed commit")
	git("tag", "-s", "-m", "This is a signed tag", "signedtag", "HEAD")
	git("-c", "i18n.commitEncoding=ISO-8859-1", "commit", "-q", "--allow-empty", "-m", "encoded")

	git("checkout", "-q", "-b", "branch", commit)
	writeFile(t, filepath.Join(repo, "more"), "More more more give me more!\n")
	git("add", "more")
	git("commit", "-q", "-m", "Add more files!")
	git("tag", "-s", "-m", "This is another signed tag", "signedtag2", "HEAD")
	git("merge", "-q", "-S", "-m", "merge some signed tags together", "signedtag", "signedtag2")

	git("commit", "-q", "--gpg-sign", "--allow-empty", "-m", "This is an additional signed commit")
	signedCommit := []byte(git("cat-file", "commit", "HEAD"))
	for _, header := range []string{"gpgsig", "gpgsig-sha256"} {
		runGit(t, repo, deleteHeader(signedCommit, header), "hash-object", "-t", "commit", "-w", "--stdin")
	}
	git("tag", "-s", "-m", "This is an additional signed tag", "signedtag34", "HEAD")
	signedTag := []byte(git("cat-file", "tag", "signedtag34"))
	otherHeader := "gpgsig"
	if format == formatcfg.SHA1 {
		otherHeader = "gpgsig-sha256"
	}
	bodySigned := deleteHeader(signedTag, otherHeader)
	runGit(t, repo, bodySigned, "hash-object", "-t", "tag", "-w", "--stdin")
	runGit(t, repo, signedTag[:signatureStart(signedTag)], "hash-object", "-t", "tag", "-w", "--stdin")

	// add_header_signature works on C strings: it copies a signature only up
	// to a NUL, and finds no end of header past one.
	nulInSignature := bytes.Replace(bodySigned, []byte("\n-----END"), []byte("\nab\x00cd\n-----END"), 1)
	runGit(t, repo, nulInSignature, "hash-object", "--literally", "-t", "tag", "-w", "--stdin")
	nulInHeader := bytes.Replace(bodySigned, []byte("\ntag "), []byte("\ntag nul\x00"), 1)
	runGit(t, repo, nulInHeader, "hash-object", "--literally", "-t", "tag", "-w", "--stdin")
	return repo
}

// deleteHeader drops header lines named key, with their continuation lines,
// as t1016's test-tool delete-gpgsig does.
func deleteHeader(raw []byte, key string) []byte {
	var out []byte
	inHeader, dropping := true, false
	for _, line := range bytes.SplitAfter(raw, []byte{'\n'}) {
		if inHeader && len(line) > 0 && line[0] == '\n' {
			inHeader = false
		}
		if inHeader {
			if line[0] == ' ' {
				if dropping {
					continue
				}
			} else {
				dropping = bytes.HasPrefix(line, []byte(key+" "))
				if dropping {
					continue
				}
			}
		}
		out = append(out, line...)
	}
	return out
}

func catAllObjects(t *testing.T, repo string) []rawObject {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "cat-file", "--batch-all-objects", "--batch")
	cmd.Dir = repo
	cmd.Env = isolatedGitEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git cat-file --batch-all-objects: %v", err)
	}
	var objects []rawObject
	r := bufio.NewReader(bytes.NewReader(out))
	for {
		header, err := r.ReadString('\n')
		if errors.Is(err, io.EOF) {
			return objects
		}
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 {
			t.Fatalf("unexpected cat-file header %q", header)
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil {
			t.Fatal(err)
		}
		content := make([]byte, size+1)
		if _, err := io.ReadFull(r, content); err != nil {
			t.Fatal(err)
		}
		typ, err := plumbing.ParseObjectType(fields[1])
		if err != nil {
			t.Fatal(err)
		}
		objects = append(objects, rawObject{hash: plumbing.NewHash(fields[0]), typ: typ, content: content[:size]})
	}
}

func compatHashes(t *testing.T, repo string, to formatcfg.ObjectFormat, objects []rawObject) map[plumbing.Hash]plumbing.Hash {
	t.Helper()
	args := []string{"rev-parse", "--output-object-format=" + to.String()}
	for _, o := range objects {
		args = append(args, o.hash.String())
	}
	lines := strings.Fields(runGit(t, repo, nil, args...))
	if len(lines) != len(objects) {
		t.Fatalf("rev-parse returned %d hashes for %d objects", len(lines), len(objects))
	}
	out := make(map[plumbing.Hash]plumbing.Hash, len(objects))
	for i, o := range objects {
		out[o.hash] = plumbing.NewHash(lines[i])
	}
	return out
}

func runGit(t *testing.T, dir string, stdin []byte, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = isolatedGitEnv()
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return string(out)
}

// isolatedGitEnv keeps the user's git configuration, such as a signing
// setup, out of the test.
func isolatedGitEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=A U Thor", "GIT_AUTHOR_EMAIL=author@example.com",
		"GIT_COMMITTER_NAME=C O Mitter", "GIT_COMMITTER_EMAIL=committer@example.com",
		"GIT_AUTHOR_DATE=1700000000 +0000", "GIT_COMMITTER_DATE=1700000000 +0000",
	)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(fmt.Errorf("write %s: %w", path, err))
	}
}
