package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/entirehq/git-sync/internal/validation"
	"github.com/entirehq/git-sync/pkg/gitsync"
	"github.com/entirehq/git-sync/pkg/gitsync/unstable"
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
		return runSyncLike(ctx, "sync", args[1:], false, gitsync.ModeSync)
	case "replicate":
		return runSyncLike(ctx, "replicate", args[1:], false, gitsync.ModeReplicate)
	case "plan":
		return runSyncLike(ctx, "plan", args[1:], true, "")
	case "bootstrap":
		return runBootstrap(ctx, args[1:])
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

func runSyncLike(ctx context.Context, name string, args []string, dryRun bool, defaultMode gitsync.OperationMode) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var mappings multiStringFlag
	var jsonOutput bool
	var sourceAuth gitsync.EndpointAuth
	var targetAuth gitsync.EndpointAuth
	req := unstable.SyncRequest{DryRun: dryRun}

	fs.StringVar(&req.Source.URL, "source-url", "", "source repository URL")
	fs.StringVar(&req.Target.URL, "target-url", "", "target repository URL")

	fs.StringVar(&sourceAuth.Token, "source-token", envOr("GITSYNC_SOURCE_TOKEN", ""), "source token/password")
	fs.StringVar(&targetAuth.Token, "target-token", envOr("GITSYNC_TARGET_TOKEN", ""), "target token/password")
	fs.StringVar(&sourceAuth.Username, "source-username", envOr("GITSYNC_SOURCE_USERNAME", "git"), "source basic auth username")
	fs.StringVar(&targetAuth.Username, "target-username", envOr("GITSYNC_TARGET_USERNAME", "git"), "target basic auth username")
	fs.BoolVar(&sourceAuth.SkipTLSVerify, "source-insecure-skip-tls-verify", envBool("GITSYNC_SOURCE_INSECURE_SKIP_TLS_VERIFY"), "skip TLS certificate verification for the source")
	fs.BoolVar(&targetAuth.SkipTLSVerify, "target-insecure-skip-tls-verify", envBool("GITSYNC_TARGET_INSECURE_SKIP_TLS_VERIFY"), "skip TLS certificate verification for the target")

	fs.StringVar(&sourceAuth.BearerToken, "source-bearer-token", envOr("GITSYNC_SOURCE_BEARER_TOKEN", ""), "source bearer token")
	fs.StringVar(&targetAuth.BearerToken, "target-bearer-token", envOr("GITSYNC_TARGET_BEARER_TOKEN", ""), "target bearer token")

	branches := fs.String("branch", "", "comma-separated branch list; default is all source branches")
	fs.Var(&mappings, "map", "ref mapping in src:dst form; short names map branches, full refs map exact refs")
	modeValue := operationModeFlag(defaultOperationMode(defaultMode))
	if name == "plan" {
		fs.Var(&modeValue, "mode", "operation mode: sync or replicate")
	}
	fs.BoolVar(&req.Policy.IncludeTags, "tags", false, "mirror tags")
	fs.BoolVar(&req.Policy.Force, "force", false, "allow non-fast-forward branch updates and retarget tags")
	fs.BoolVar(&req.Policy.Prune, "prune", false, "delete managed target refs that no longer exist on source")
	fs.BoolVar(&req.Options.CollectStats, "stats", false, "print transfer statistics")
	fs.BoolVar(&req.Options.MeasureMemory, "measure-memory", false, "sample elapsed time and Go heap usage")
	fs.BoolVar(&jsonOutput, "json", false, "print JSON output")
	fs.IntVar(&req.Options.MaterializedMaxObjects, "materialized-max-objects", unstable.DefaultMaterializedMaxObjects, "abort non-relay materialized syncs above this many objects")
	fs.Int64Var(&req.Options.MaxPackBytes, "max-pack-bytes", 0, "abort bootstrap-relay push if the streamed source pack exceeds this many bytes")
	fs.Int64Var(&req.Options.TargetMaxPackBytes, "target-max-pack-bytes", 0, "target receive-pack body size limit; batches are planned and auto-subdivided to fit")
	protocolValue := protocolModeFlag(protocolMode(envOr("GITSYNC_PROTOCOL", validation.ProtocolAuto)))
	fs.Var(&protocolValue, "protocol", "protocol mode: auto, v1, or v2")
	fs.BoolVar(&req.Options.Verbose, "v", false, "verbose logging")

	if err := fs.Parse(args); err != nil {
		return err
	}
	req.Policy.Mode = gitsync.OperationMode(modeValue)
	req.Policy.Protocol = gitsync.ProtocolMode(protocolValue)

	positional := fs.Args()
	if req.Source.URL == "" && len(positional) > 0 {
		req.Source.URL = positional[0]
	}
	if req.Target.URL == "" && len(positional) > 1 {
		req.Target.URL = positional[1]
	}
	if len(positional) > 2 {
		return usageError("too many positional arguments")
	}

	if *branches != "" {
		req.Scope.Branches = splitCSV(*branches)
	}
	for _, raw := range mappings {
		mapping, err := validation.ParseMapping(raw)
		if err != nil {
			return err
		}
		req.Scope.Mappings = append(req.Scope.Mappings, gitsync.RefMapping{
			Source: mapping.Source,
			Target: mapping.Target,
		})
	}

	if req.Source.URL == "" || req.Target.URL == "" {
		return usageError(name + " requires source and target repository URLs")
	}

	client := unstable.New(unstable.Options{
		Auth: gitsync.StaticAuthProvider{Source: sourceAuth, Target: targetAuth},
	})
	var (
		result unstable.Result
		err    error
	)
	if dryRun {
		result, err = client.Plan(ctx, req)
	} else {
		if req.Policy.Mode == gitsync.ModeReplicate {
			result, err = client.Replicate(ctx, req)
		} else {
			result, err = client.Sync(ctx, req)
		}
	}
	if err != nil {
		return err
	}
	printOutput(jsonOutput, result)

	if !dryRun && result.Blocked > 0 {
		return errors.New("one or more branches were skipped because the target was not fast-forwardable")
	}
	return nil
}

func runBootstrap(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var mappings multiStringFlag
	var jsonOutput bool
	var sourceAuth gitsync.EndpointAuth
	var targetAuth gitsync.EndpointAuth
	req := unstable.BootstrapRequest{}

	fs.StringVar(&req.Source.URL, "source-url", "", "source repository URL")
	fs.StringVar(&req.Target.URL, "target-url", "", "target repository URL")

	fs.StringVar(&sourceAuth.Token, "source-token", envOr("GITSYNC_SOURCE_TOKEN", ""), "source token/password")
	fs.StringVar(&targetAuth.Token, "target-token", envOr("GITSYNC_TARGET_TOKEN", ""), "target token/password")
	fs.StringVar(&sourceAuth.Username, "source-username", envOr("GITSYNC_SOURCE_USERNAME", "git"), "source basic auth username")
	fs.StringVar(&targetAuth.Username, "target-username", envOr("GITSYNC_TARGET_USERNAME", "git"), "target basic auth username")
	fs.BoolVar(&sourceAuth.SkipTLSVerify, "source-insecure-skip-tls-verify", envBool("GITSYNC_SOURCE_INSECURE_SKIP_TLS_VERIFY"), "skip TLS certificate verification for the source")
	fs.BoolVar(&targetAuth.SkipTLSVerify, "target-insecure-skip-tls-verify", envBool("GITSYNC_TARGET_INSECURE_SKIP_TLS_VERIFY"), "skip TLS certificate verification for the target")

	fs.StringVar(&sourceAuth.BearerToken, "source-bearer-token", envOr("GITSYNC_SOURCE_BEARER_TOKEN", ""), "source bearer token")
	fs.StringVar(&targetAuth.BearerToken, "target-bearer-token", envOr("GITSYNC_TARGET_BEARER_TOKEN", ""), "target bearer token")

	branches := fs.String("branch", "", "comma-separated branch list; default is all source branches")
	fs.Var(&mappings, "map", "ref mapping in src:dst form; short names map branches, full refs map exact refs")
	fs.BoolVar(&req.IncludeTags, "tags", false, "mirror tags")
	fs.BoolVar(&req.Options.CollectStats, "stats", false, "print transfer statistics")
	fs.BoolVar(&req.Options.MeasureMemory, "measure-memory", false, "sample elapsed time and Go heap usage")
	fs.BoolVar(&jsonOutput, "json", false, "print JSON output")
	fs.Int64Var(&req.Options.MaxPackBytes, "max-pack-bytes", 0, "abort bootstrap if the streamed source pack exceeds this many bytes")
	fs.Int64Var(&req.Options.TargetMaxPackBytes, "target-max-pack-bytes", 0, "target receive-pack body size limit; batches are planned and auto-subdivided to fit")
	bootstrapProtocol := protocolModeFlag(protocolMode(envOr("GITSYNC_PROTOCOL", validation.ProtocolAuto)))
	fs.Var(&bootstrapProtocol, "protocol", "protocol mode: auto, v1, or v2")
	fs.BoolVar(&req.Options.Verbose, "v", false, "verbose logging")

	if err := fs.Parse(args); err != nil {
		return err
	}
	req.Protocol = gitsync.ProtocolMode(bootstrapProtocol)

	positional := fs.Args()
	if req.Source.URL == "" && len(positional) > 0 {
		req.Source.URL = positional[0]
	}
	if req.Target.URL == "" && len(positional) > 1 {
		req.Target.URL = positional[1]
	}
	if len(positional) > 2 {
		return usageError("too many positional arguments")
	}

	if *branches != "" {
		req.Scope.Branches = splitCSV(*branches)
	}
	for _, raw := range mappings {
		mapping, err := validation.ParseMapping(raw)
		if err != nil {
			return err
		}
		req.Scope.Mappings = append(req.Scope.Mappings, gitsync.RefMapping{
			Source: mapping.Source,
			Target: mapping.Target,
		})
	}

	if req.Source.URL == "" || req.Target.URL == "" {
		return usageError("bootstrap requires source and target repository URLs")
	}

	result, err := unstable.New(unstable.Options{
		Auth: gitsync.StaticAuthProvider{Source: sourceAuth, Target: targetAuth},
	}).Bootstrap(ctx, req)
	if err != nil {
		return err
	}
	printOutput(jsonOutput, result)
	return nil
}

func runProbe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var jsonOutput bool
	var sourceAuth gitsync.EndpointAuth
	var targetAuth gitsync.EndpointAuth
	req := unstable.ProbeRequest{}
	fs.StringVar(&req.Source.URL, "source-url", "", "source repository URL")
	targetURL := fs.String("target-url", "", "optional target repository URL")
	fs.StringVar(&sourceAuth.Token, "source-token", envOr("GITSYNC_SOURCE_TOKEN", ""), "source token/password")
	fs.StringVar(&targetAuth.Token, "target-token", envOr("GITSYNC_TARGET_TOKEN", ""), "target token/password")
	fs.StringVar(&sourceAuth.Username, "source-username", envOr("GITSYNC_SOURCE_USERNAME", "git"), "source basic auth username")
	fs.StringVar(&targetAuth.Username, "target-username", envOr("GITSYNC_TARGET_USERNAME", "git"), "target basic auth username")
	fs.StringVar(&sourceAuth.BearerToken, "source-bearer-token", envOr("GITSYNC_SOURCE_BEARER_TOKEN", ""), "source bearer token")
	fs.StringVar(&targetAuth.BearerToken, "target-bearer-token", envOr("GITSYNC_TARGET_BEARER_TOKEN", ""), "target bearer token")
	fs.BoolVar(&sourceAuth.SkipTLSVerify, "source-insecure-skip-tls-verify", envBool("GITSYNC_SOURCE_INSECURE_SKIP_TLS_VERIFY"), "skip TLS certificate verification for the source")
	fs.BoolVar(&targetAuth.SkipTLSVerify, "target-insecure-skip-tls-verify", envBool("GITSYNC_TARGET_INSECURE_SKIP_TLS_VERIFY"), "skip TLS certificate verification for the target")
	fs.BoolVar(&req.IncludeTags, "tags", false, "include tag ref prefixes in probe")
	probeProtocol := protocolModeFlag(protocolMode(envOr("GITSYNC_PROTOCOL", validation.ProtocolAuto)))
	fs.Var(&probeProtocol, "protocol", "protocol mode: auto, v1, or v2")
	fs.BoolVar(&req.Options.CollectStats, "stats", false, "print transfer statistics")
	fs.BoolVar(&req.Options.MeasureMemory, "measure-memory", false, "sample elapsed time and Go heap usage")
	fs.BoolVar(&jsonOutput, "json", false, "print JSON output")

	if err := fs.Parse(args); err != nil {
		return err
	}
	req.Protocol = gitsync.ProtocolMode(probeProtocol)

	positional := fs.Args()
	if req.Source.URL == "" && len(positional) > 0 {
		req.Source.URL = positional[0]
	}
	if *targetURL == "" && len(positional) > 1 {
		*targetURL = positional[1]
	}
	if len(positional) > 2 {
		return usageError("too many positional arguments")
	}
	if req.Source.URL == "" {
		return usageError("probe requires a source repository URL")
	}
	if *targetURL != "" {
		req.Target = &gitsync.Endpoint{URL: *targetURL}
	}

	result, err := unstable.New(unstable.Options{
		Auth: gitsync.StaticAuthProvider{Source: sourceAuth, Target: targetAuth},
	}).Probe(ctx, req)
	if err != nil {
		return err
	}
	printOutput(jsonOutput, result)
	return nil
}

func runFetch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var haveRefs multiStringFlag
	var haveHashesRaw multiStringFlag
	var jsonOutput bool
	var sourceAuth gitsync.EndpointAuth
	req := unstable.FetchRequest{}

	fs.StringVar(&req.Source.URL, "source-url", "", "source repository URL")
	fs.StringVar(&sourceAuth.Token, "source-token", envOr("GITSYNC_SOURCE_TOKEN", ""), "source token/password")
	fs.StringVar(&sourceAuth.Username, "source-username", envOr("GITSYNC_SOURCE_USERNAME", "git"), "source basic auth username")
	fs.StringVar(&sourceAuth.BearerToken, "source-bearer-token", envOr("GITSYNC_SOURCE_BEARER_TOKEN", ""), "source bearer token")
	fs.BoolVar(&sourceAuth.SkipTLSVerify, "source-insecure-skip-tls-verify", envBool("GITSYNC_SOURCE_INSECURE_SKIP_TLS_VERIFY"), "skip TLS certificate verification for the source")
	branches := fs.String("branch", "", "comma-separated branch list; default is all source branches")
	fs.BoolVar(&req.IncludeTags, "tags", false, "include tags in the fetch request")
	fetchProtocol := protocolModeFlag(protocolMode(envOr("GITSYNC_PROTOCOL", validation.ProtocolAuto)))
	fs.Var(&fetchProtocol, "protocol", "protocol mode: auto, v1, or v2")
	fs.BoolVar(&req.Options.CollectStats, "stats", false, "print transfer statistics")
	fs.BoolVar(&req.Options.MeasureMemory, "measure-memory", false, "sample elapsed time and Go heap usage")
	fs.BoolVar(&jsonOutput, "json", false, "print JSON output")
	fs.Var(&haveRefs, "have-ref", "source ref name to advertise as have; short names map to branches")
	fs.Var(&haveHashesRaw, "have", "explicit object hash to advertise as have")

	if err := fs.Parse(args); err != nil {
		return err
	}
	req.Protocol = gitsync.ProtocolMode(fetchProtocol)

	positional := fs.Args()
	if req.Source.URL == "" && len(positional) > 0 {
		req.Source.URL = positional[0]
	}
	if len(positional) > 1 {
		return usageError("too many positional arguments")
	}
	if req.Source.URL == "" {
		return usageError("fetch requires a source repository URL")
	}
	if *branches != "" {
		req.Scope.Branches = splitCSV(*branches)
	}

	haveHashes := make([]plumbing.Hash, 0, len(haveHashesRaw))
	for _, raw := range haveHashesRaw {
		hash := plumbing.NewHash(strings.TrimSpace(raw))
		if hash.IsZero() {
			return fmt.Errorf("invalid --have %q", raw)
		}
		haveHashes = append(haveHashes, hash)
	}

	req.HaveRefs = append(req.HaveRefs, haveRefs...)
	req.HaveHashes = append(req.HaveHashes, haveHashes...)
	result, err := unstable.New(unstable.Options{
		Auth: gitsync.StaticAuthProvider{Source: sourceAuth},
	}).Fetch(ctx, req)
	if err != nil {
		return err
	}
	printOutput(jsonOutput, result)
	return nil
}

func printOutput(jsonOutput bool, value interface{ Lines() []string }) {
	if jsonOutput {
		data, err := marshalOutput(value)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: encode JSON output: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		return
	}

	for _, line := range value.Lines() {
		fmt.Println(line)
	}
}

func marshalOutput(value interface{}) ([]byte, error) {
	return json.MarshalIndent(value, "", "  ")
}

type multiStringFlag []string

func (m *multiStringFlag) String() string {
	return strings.Join(*m, ",")
}

func (m *multiStringFlag) Set(value string) error {
	*m = append(*m, value)
	return nil
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

func envBool(key string) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func usageError(message string) error {
	usage := fmt.Sprintf("usage:\n  git-sync sync [flags] <source-url> <target-url>\n  git-sync replicate [flags] <source-url> <target-url>\n  git-sync plan [flags] <source-url> <target-url>\n  git-sync bootstrap [flags] <source-url> <target-url>\n  git-sync probe [flags] <source-url> [target-url]\n  git-sync fetch [flags] <source-url>\n\nsync flags:\n  --branch main,dev\n  --map main:stable\n  --tags\n  --force\n  --prune\n  --stats\n  --measure-memory\n  --json\n  --materialized-max-objects %d\n  --max-pack-bytes <bytes>\n  --target-max-pack-bytes <bytes>\n  --protocol auto|v1|v2\n  --source-token ...\n  --target-token ...\n  --source-username git\n  --target-username git\n  --source-bearer-token ...\n  --target-bearer-token ...\n  --source-insecure-skip-tls-verify\n  --target-insecure-skip-tls-verify\n  -v\n\nreplicate flags:\n  --branch main,dev\n  --map main:stable\n  --tags\n  --prune\n  --stats\n  --measure-memory\n  --json\n  --max-pack-bytes <bytes>\n  --target-max-pack-bytes <bytes>\n  --protocol auto|v1|v2\n  --source-token ...\n  --target-token ...\n  --source-username git\n  --target-username git\n  --source-bearer-token ...\n  --target-bearer-token ...\n  --source-insecure-skip-tls-verify\n  --target-insecure-skip-tls-verify\n  -v\n\nplan flags:\n  --mode sync|replicate\n  --branch main,dev\n  --map main:stable\n  --tags\n  --force\n  --prune\n  --stats\n  --measure-memory\n  --json\n  --max-pack-bytes <bytes>\n  --target-max-pack-bytes <bytes>\n  --protocol auto|v1|v2\n  --source-token ...\n  --target-token ...\n  --source-username git\n  --target-username git\n  --source-bearer-token ...\n  --target-bearer-token ...\n  --source-insecure-skip-tls-verify\n  --target-insecure-skip-tls-verify\n  -v\n\nbootstrap flags:\n  --branch main,dev\n  --map main:stable\n  --tags\n  --max-pack-bytes 104857600\n  --target-max-pack-bytes 1073741824\n  --stats\n  --measure-memory\n  --json\n  --protocol auto|v1|v2\n  --source-token ...\n  --target-token ...\n  --source-username git\n  --target-username git\n  --source-bearer-token ...\n  --target-bearer-token ...\n  --source-insecure-skip-tls-verify\n  --target-insecure-skip-tls-verify\n  -v\n\nprobe flags:\n  --tags\n  --stats\n  --measure-memory\n  --json\n  --protocol auto|v1|v2\n  --source-token ...\n  --source-username git\n  --source-bearer-token ...\n  --target-token ...\n  --target-username git\n  --target-bearer-token ...\n  --source-insecure-skip-tls-verify\n  --target-insecure-skip-tls-verify\n\nfetch flags:\n  --branch main,dev\n  --tags\n  --stats\n  --measure-memory\n  --json\n  --protocol auto|v1|v2\n  --have-ref main\n  --have <hash>\n  --source-token ...\n  --source-username git\n  --source-bearer-token ...\n  --source-insecure-skip-tls-verify\n", unstable.DefaultMaterializedMaxObjects)
	if message == "" {
		return errors.New(strings.TrimSpace(usage))
	}
	return fmt.Errorf("%s\n\n%s", message, usage)
}

type protocolMode gitsync.ProtocolMode
type operationMode gitsync.OperationMode

type protocolModeFlag protocolMode
type operationModeFlag operationMode

func (p *protocolModeFlag) String() string {
	return string(*p)
}

func (p *protocolModeFlag) Set(value string) error {
	mode, err := validation.NormalizeProtocolMode(value)
	if err != nil {
		return err
	}
	*p = protocolModeFlag(protocolMode(gitsync.ProtocolMode(mode)))
	return nil
}

func (m *operationModeFlag) String() string {
	return string(*m)
}

func (m *operationModeFlag) Set(value string) error {
	switch gitsync.OperationMode(value) {
	case gitsync.ModeSync, gitsync.ModeReplicate:
		*m = operationModeFlag(operationMode(value))
		return nil
	default:
		return fmt.Errorf("unsupported mode %q", value)
	}
}

// defaultOperationMode returns the starting value for the --mode flag.
// Subcommands that pin a mode (sync, replicate) pass it in; plan passes ""
// and gets sync as the default, letting --mode override it.
func defaultOperationMode(defaultMode gitsync.OperationMode) operationMode {
	if defaultMode != "" {
		return operationMode(defaultMode)
	}
	return operationMode(gitsync.ModeSync)
}
