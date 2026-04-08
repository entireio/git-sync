package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/soph/git-sync/internal/syncer"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError("")
	}

	switch args[0] {
	case "sync":
		return runSyncLike(ctx, "sync", args[1:], false)
	case "plan":
		return runSyncLike(ctx, "plan", args[1:], true)
	case "probe":
		return runProbe(ctx, args[1:])
	case "fetch":
		return runFetch(ctx, args[1:])
	case "help", "-h", "--help":
		return usageError("")
	default:
		return usageError(fmt.Sprintf("unknown command %q", args[0]))
	}
}

func runSyncLike(ctx context.Context, name string, args []string, dryRun bool) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	cfg := syncer.Config{DryRun: dryRun}
	var mappings multiStringFlag

	fs.StringVar(&cfg.Source.URL, "source-url", "", "source repository URL")
	fs.StringVar(&cfg.Target.URL, "target-url", "", "target repository URL")

	fs.StringVar(&cfg.Source.Token, "source-token", envOr("GITSYNC_SOURCE_TOKEN", ""), "source token/password")
	fs.StringVar(&cfg.Target.Token, "target-token", envOr("GITSYNC_TARGET_TOKEN", ""), "target token/password")
	fs.StringVar(&cfg.Source.Username, "source-username", envOr("GITSYNC_SOURCE_USERNAME", "git"), "source basic auth username")
	fs.StringVar(&cfg.Target.Username, "target-username", envOr("GITSYNC_TARGET_USERNAME", "git"), "target basic auth username")

	fs.StringVar(&cfg.Source.BearerToken, "source-bearer-token", envOr("GITSYNC_SOURCE_BEARER_TOKEN", ""), "source bearer token")
	fs.StringVar(&cfg.Target.BearerToken, "target-bearer-token", envOr("GITSYNC_TARGET_BEARER_TOKEN", ""), "target bearer token")

	branches := fs.String("branch", "", "comma-separated branch list; default is all source branches")
	fs.Var(&mappings, "map", "ref mapping in src:dst form; short names map branches, full refs map exact refs")
	fs.BoolVar(&cfg.IncludeTags, "tags", false, "mirror tags")
	fs.BoolVar(&cfg.Force, "force", false, "allow non-fast-forward branch updates and retarget tags")
	fs.BoolVar(&cfg.Prune, "prune", false, "delete managed target refs that no longer exist on source")
	fs.BoolVar(&cfg.ShowStats, "stats", false, "print transfer statistics")
	fs.StringVar(&cfg.ProtocolMode, "protocol", envOr("GITSYNC_PROTOCOL", "auto"), "protocol mode: auto, v1, or v2")
	fs.BoolVar(&cfg.Verbose, "v", false, "verbose logging")

	if err := fs.Parse(args); err != nil {
		return err
	}

	positional := fs.Args()
	if cfg.Source.URL == "" && len(positional) > 0 {
		cfg.Source.URL = positional[0]
	}
	if cfg.Target.URL == "" && len(positional) > 1 {
		cfg.Target.URL = positional[1]
	}
	if len(positional) > 2 {
		return usageError("too many positional arguments")
	}

	if *branches != "" {
		cfg.Branches = splitCSV(*branches)
	}
	for _, raw := range mappings {
		mapping, err := parseMapping(raw)
		if err != nil {
			return err
		}
		cfg.Mappings = append(cfg.Mappings, mapping)
	}

	if cfg.Source.URL == "" || cfg.Target.URL == "" {
		return usageError(name + " requires source and target repository URLs")
	}

	result, err := syncer.Run(ctx, cfg)
	if err != nil {
		return err
	}

	for _, line := range result.Lines() {
		fmt.Println(line)
	}

	if !dryRun && result.Blocked > 0 {
		return errors.New("one or more branches were skipped because the target was not fast-forwardable")
	}
	return nil
}

func runProbe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	cfg := syncer.Config{}
	fs.StringVar(&cfg.Source.URL, "source-url", "", "source repository URL")
	fs.StringVar(&cfg.Target.URL, "target-url", "", "optional target repository URL")
	fs.StringVar(&cfg.Source.Token, "source-token", envOr("GITSYNC_SOURCE_TOKEN", ""), "source token/password")
	fs.StringVar(&cfg.Target.Token, "target-token", envOr("GITSYNC_TARGET_TOKEN", ""), "target token/password")
	fs.StringVar(&cfg.Source.Username, "source-username", envOr("GITSYNC_SOURCE_USERNAME", "git"), "source basic auth username")
	fs.StringVar(&cfg.Target.Username, "target-username", envOr("GITSYNC_TARGET_USERNAME", "git"), "target basic auth username")
	fs.StringVar(&cfg.Source.BearerToken, "source-bearer-token", envOr("GITSYNC_SOURCE_BEARER_TOKEN", ""), "source bearer token")
	fs.StringVar(&cfg.Target.BearerToken, "target-bearer-token", envOr("GITSYNC_TARGET_BEARER_TOKEN", ""), "target bearer token")
	fs.BoolVar(&cfg.IncludeTags, "tags", false, "include tag ref prefixes in probe")
	fs.StringVar(&cfg.ProtocolMode, "protocol", envOr("GITSYNC_PROTOCOL", "auto"), "protocol mode: auto, v1, or v2")
	fs.BoolVar(&cfg.ShowStats, "stats", false, "print transfer statistics")

	if err := fs.Parse(args); err != nil {
		return err
	}

	positional := fs.Args()
	if cfg.Source.URL == "" && len(positional) > 0 {
		cfg.Source.URL = positional[0]
	}
	if cfg.Target.URL == "" && len(positional) > 1 {
		cfg.Target.URL = positional[1]
	}
	if len(positional) > 2 {
		return usageError("too many positional arguments")
	}
	if cfg.Source.URL == "" {
		return usageError("probe requires a source repository URL")
	}

	result, err := syncer.Probe(ctx, cfg)
	if err != nil {
		return err
	}
	for _, line := range result.Lines() {
		fmt.Println(line)
	}
	return nil
}

func runFetch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	cfg := syncer.Config{}
	var haveRefs multiStringFlag
	var haveHashesRaw multiStringFlag

	fs.StringVar(&cfg.Source.URL, "source-url", "", "source repository URL")
	fs.StringVar(&cfg.Source.Token, "source-token", envOr("GITSYNC_SOURCE_TOKEN", ""), "source token/password")
	fs.StringVar(&cfg.Source.Username, "source-username", envOr("GITSYNC_SOURCE_USERNAME", "git"), "source basic auth username")
	fs.StringVar(&cfg.Source.BearerToken, "source-bearer-token", envOr("GITSYNC_SOURCE_BEARER_TOKEN", ""), "source bearer token")
	branches := fs.String("branch", "", "comma-separated branch list; default is all source branches")
	fs.BoolVar(&cfg.IncludeTags, "tags", false, "include tags in the fetch request")
	fs.StringVar(&cfg.ProtocolMode, "protocol", envOr("GITSYNC_PROTOCOL", "auto"), "protocol mode: auto, v1, or v2")
	fs.BoolVar(&cfg.ShowStats, "stats", false, "print transfer statistics")
	fs.Var(&haveRefs, "have-ref", "source ref name to advertise as have; short names map to branches")
	fs.Var(&haveHashesRaw, "have", "explicit object hash to advertise as have")

	if err := fs.Parse(args); err != nil {
		return err
	}

	positional := fs.Args()
	if cfg.Source.URL == "" && len(positional) > 0 {
		cfg.Source.URL = positional[0]
	}
	if len(positional) > 1 {
		return usageError("too many positional arguments")
	}
	if cfg.Source.URL == "" {
		return usageError("fetch requires a source repository URL")
	}
	if *branches != "" {
		cfg.Branches = splitCSV(*branches)
	}

	haveHashes := make([]plumbing.Hash, 0, len(haveHashesRaw))
	for _, raw := range haveHashesRaw {
		hash := plumbing.NewHash(strings.TrimSpace(raw))
		if hash.IsZero() {
			return fmt.Errorf("invalid --have %q", raw)
		}
		haveHashes = append(haveHashes, hash)
	}

	result, err := syncer.Fetch(ctx, cfg, haveRefs, haveHashes)
	if err != nil {
		return err
	}
	for _, line := range result.Lines() {
		fmt.Println(line)
	}
	return nil
}

type multiStringFlag []string

func (m *multiStringFlag) String() string {
	return strings.Join(*m, ",")
}

func (m *multiStringFlag) Set(value string) error {
	*m = append(*m, value)
	return nil
}

func parseMapping(raw string) (syncer.RefMapping, error) {
	parts := strings.SplitN(raw, ":", 2)
	if len(parts) != 2 {
		return syncer.RefMapping{}, fmt.Errorf("invalid --map %q, expected src:dst", raw)
	}
	source := strings.TrimSpace(parts[0])
	target := strings.TrimSpace(parts[1])
	if source == "" || target == "" {
		return syncer.RefMapping{}, fmt.Errorf("invalid --map %q, expected src:dst", raw)
	}
	return syncer.RefMapping{Source: source, Target: target}, nil
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func envOr(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

func usageError(message string) error {
	usage := "usage:\n  git-sync sync [flags] <source-url> <target-url>\n  git-sync plan [flags] <source-url> <target-url>\n  git-sync probe [flags] <source-url> [target-url]\n  git-sync fetch [flags] <source-url>\n\nsync/plan flags:\n  --branch main,dev\n  --map main:stable\n  --tags\n  --force\n  --prune\n  --stats\n  --protocol auto|v1|v2\n  --source-token ...\n  --target-token ...\n  --source-username git\n  --target-username git\n  --source-bearer-token ...\n  --target-bearer-token ...\n  -v\n\nprobe flags:\n  --tags\n  --stats\n  --protocol auto|v1|v2\n  --source-token ...\n  --source-username git\n  --source-bearer-token ...\n  --target-token ...\n  --target-username git\n  --target-bearer-token ...\n\nfetch flags:\n  --branch main,dev\n  --tags\n  --stats\n  --protocol auto|v1|v2\n  --have-ref main\n  --have <hash>\n  --source-token ...\n  --source-username git\n  --source-bearer-token ...\n"
	if message == "" {
		return errors.New(strings.TrimSpace(usage))
	}
	return fmt.Errorf("%s\n\n%s", message, usage)
}
