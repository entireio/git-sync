// Package syncer provides the top-level orchestration for git-sync.
// It delegates to internal/gitproto for protocol, internal/planner for
// planning, and internal/auth for credentials.
package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-git/go-git/v6/storage/memory"

	"entire.io/entire/git-sync/internal/auth"
	"entire.io/entire/git-sync/internal/convert"
	"entire.io/entire/git-sync/internal/gitproto"
	"entire.io/entire/git-sync/internal/planner"
	"entire.io/entire/git-sync/internal/redact"
	"entire.io/entire/git-sync/internal/sanitize"
	bstrap "entire.io/entire/git-sync/internal/strategy/bootstrap"
	"entire.io/entire/git-sync/internal/strategy/incremental"
	"entire.io/entire/git-sync/internal/strategy/materialized"
	repstrat "entire.io/entire/git-sync/internal/strategy/replicate"
	"entire.io/entire/git-sync/internal/validation"
)

const (
	protocolModeAuto = validation.ProtocolAuto
	protocolModeV1   = validation.ProtocolV1
	protocolModeV2   = validation.ProtocolV2
	modeSync         = "sync"
	modeReplicate    = "replicate"
)

const DefaultMaterializedMaxObjects = materialized.DefaultMaxMaterializedObjects

// Endpoint holds the connection configuration for a remote.
type Endpoint struct {
	URL           string
	Username      string
	Token         string
	BearerToken   string
	SkipTLSVerify bool

	// FollowInfoRefsRedirect, when true, rewrites the effective host of
	// this endpoint to the final URL of /info/refs after HTTP redirects,
	// so follow-up upload-pack / receive-pack POSTs land on the
	// redirected node. Plumbed into gitproto.Conn.FollowInfoRefsRedirect.
	FollowInfoRefsRedirect bool
}

// RefMapping is a user-specified source:target ref mapping.
type RefMapping = validation.RefMapping

// Config holds all configuration for a sync operation.
type Config struct {
	Source                 Endpoint
	Target                 Endpoint
	HTTPClient             *http.Client
	Branches               []string
	Mappings               []RefMapping
	AllRefs                bool
	ExcludeRefPrefixes     []string
	ExcludeRefs            []string
	IncludeTags            bool
	DryRun                 bool
	Verbose                bool
	ShowStats              bool
	MeasureMemory          bool
	Progress               bool
	Mode                   string
	ForceWithLease         bool
	ForceBlind             bool
	Prune                  bool
	BestEffort             bool
	MaxPackBytes           int64
	TargetMaxPackBytes     int64
	TargetMaxRefUpdates    int
	MaterializedMaxObjects int
	ProtocolMode           string
	BootstrapStrategy      string // "" | "first-parent" | "topo"
	// SourceAssertedEmpty is the caller's authoritative statement that the
	// source repository holds no refs at all, obtained from something that
	// sees the repository's real state rather than its advertisement — git
	// cannot supply this, because ref hiding is invisible to the client by
	// design. Only consulted under AllowEmptySource, and never sufficient on
	// its own: resolveEmptyDesiredSet still requires every git-side
	// observation to corroborate it.
	SourceAssertedEmpty bool

	// TargetAssertedEmpty is the same statement about the TARGET, required for
	// the same reason rather than as belt-and-braces: receive.hideRefs omits
	// matching refs from receive-pack's advertisement, so a populated target
	// can advertise nothing but the capabilities^{} sentinel and read as
	// empty. Worse than the source case, because receive.hideRefs and
	// uploadpack.hideRefs are separate settings — a ref hidden from the push
	// side is still served to fetchers, so a target wrongly judged empty is
	// one whose readers see refs the source does not have. Verified against
	// git 2.53.
	TargetAssertedEmpty bool

	// AllowEmptySource opts into treating a VERIFIED-empty source as an
	// outcome rather than an error, in replicate mode only. Off by default:
	// with it unset, an empty source fails exactly as it always has, so no
	// existing caller changes behavior. See resolveEmptyDesiredSet for what
	// "verified" requires and which outcome each case produces.
	AllowEmptySource bool

	// progressOut overrides the writer used by the live progress ticker.
	// Defaults to os.Stderr when nil. Exposed for tests.
	progressOut io.Writer
}

// Re-export types from planner for CLI compatibility.
type (
	RefKind    = planner.RefKind
	Action     = planner.Action
	BranchPlan = planner.BranchPlan
)

const (
	RefKindBranch = planner.RefKindBranch
	RefKindTag    = planner.RefKindTag
	RefKindOther  = planner.RefKindOther
	ActionCreate  = planner.ActionCreate
	ActionUpdate  = planner.ActionUpdate
	ActionDelete  = planner.ActionDelete
	ActionSkip    = planner.ActionSkip
	ActionBlock   = planner.ActionBlock
	ActionWarn    = planner.ActionWarn
)

type RefInfo struct {
	Name string        `json:"name"`
	Hash plumbing.Hash `json:"hash"`
}

func (r RefInfo) MarshalJSON() ([]byte, error) {
	type ri struct {
		Name string `json:"name"`
		Hash string `json:"hash"`
	}
	b, err := json.Marshal(ri{Name: r.Name, Hash: r.Hash.String()})
	if err != nil {
		return nil, fmt.Errorf("marshal JSON: %w", err)
	}
	return b, nil
}

// Result holds the outcome of a sync or bootstrap operation.
type Result struct {
	Plans              []BranchPlan           `json:"plans"`
	Pushed             int                    `json:"pushed"`
	Skipped            int                    `json:"skipped"`
	Blocked            int                    `json:"blocked"`
	Deleted            int                    `json:"deleted"`
	Warned             int                    `json:"warned"`
	DryRun             bool                   `json:"dryRun"`
	OperationMode      string                 `json:"operationMode"`
	Relay              bool                   `json:"relay"`
	RelayMode          string                 `json:"relayMode"`
	RelayReason        string                 `json:"relayReason"`
	Batching           bool                   `json:"batching"`
	BatchCount         int                    `json:"batchCount"`
	PlannedBatchCount  int                    `json:"plannedBatchCount"`
	TempRefs           []string               `json:"tempRefs"`
	BootstrapSuggested bool                   `json:"bootstrapSuggested"`
	SourceHEAD         plumbing.ReferenceName `json:"sourceHead,omitempty"`
	Stats              Stats                  `json:"stats"`
	Measurement        Measurement            `json:"measurement"`
	Protocol           string                 `json:"protocol"`
	// Converged is true when the source was verified to have no refs and the
	// target had none either, so the two already agree with nothing to apply.
	// Only ever set on a successful zero-plan replicate; see
	// resolveEmptySource.
	//
	// Named for what it reports rather than for the source alone: it requires
	// BOTH sides verified, so it is false in every other empty-source outcome
	// — including the diverged one, where the source WAS verified empty. A
	// "SourceEmpty" that is false on a verified-empty source is a field that
	// invites the wrong read.
	//
	// Emitted unconditionally (no omitempty) like every other discriminating
	// bool here: this is the field that separates a converged run from an
	// ordinary no-op, so "false" and "this binary has no such field" must not
	// be the same JSON.
	Converged bool `json:"converged"`
}

func (r Result) Lines() []string {
	lines := make([]string, 0, len(r.Plans)+8)
	for _, plan := range r.Plans {
		lines = append(lines, planner.FormatPlanLine(plan))
	}
	summary := fmt.Sprintf(
		"summary: pushed=%d deleted=%d skipped=%d blocked=%d warned=%d mode=%s protocol=%s relay=%t relay-mode=%s relay-reason=%s batching=%t batch-count=%d planned-batches=%d",
		r.Pushed, r.Deleted, r.Skipped, r.Blocked, r.Warned, r.OperationMode, r.Protocol, r.Relay, r.RelayMode, r.RelayReason, r.Batching, r.BatchCount, r.PlannedBatchCount,
	)
	if r.DryRun {
		summary += " dry-run=true"
	}
	lines = append(lines, summary)
	lines = append(lines, statsLines(r.Stats)...)
	lines = append(lines, measurementLine(r.Measurement)...)
	if r.BootstrapSuggested {
		lines = append(lines, "hint: target refs are absent; bootstrap can seed them without local object storage")
	}
	if r.Batching && len(r.TempRefs) > 0 {
		lines = append(lines, "batching: temp-refs="+strings.Join(r.TempRefs, ","))
	}
	if r.SourceHEAD != "" {
		lines = append(lines, "source-head: "+r.SourceHEAD.String())
	}
	// Without this the text output of a converged empty replicate is
	// byte-identical to an ordinary sync that had no work to do — the one
	// distinction this field exists to draw, dropped on the path that
	// cmd/git-sync and every non-JSON consumer actually render.
	if r.Converged {
		lines = append(lines, "converged: source and target both verified empty; nothing to apply")
	}
	return lines
}

// ProbeResult holds the outcome of a probe operation.
type ProbeResult struct {
	SourceURL     string                 `json:"sourceUrl"`
	TargetURL     string                 `json:"targetUrl,omitempty"`
	RequestedMode string                 `json:"requestedMode"`
	Protocol      string                 `json:"protocol"`
	RefPrefixes   []string               `json:"refPrefixes"`
	Capabilities  []string               `json:"sourceCapabilities"`
	TargetCaps    []string               `json:"targetCapabilities,omitempty"`
	Refs          []RefInfo              `json:"refs"`
	SourceHEAD    plumbing.ReferenceName `json:"sourceHead,omitempty"`
	// SourceHeadUnborn is true when the source reported HEAD as unborn: its
	// symref target does not exist. Diagnostic only — it is emphatically NOT a
	// statement that the repository is empty (git emits the line for any
	// dangling HEAD), and acting on emptiness needs the far stricter path
	// behind SyncPolicy.AllowEmptySource.
	//
	// Surfaced here because probe is the diagnostic surface and the ls-refs
	// request already carries the unborn argument on every v2 listing: an
	// operator asking "why will this mirror not converge" can see whether the
	// source reported unborn at all, which is otherwise invisible. False for a
	// v1 source, or a v2 source that does not advertise ls-refs=unborn,
	// however empty it is.
	SourceHeadUnborn bool        `json:"sourceHeadUnborn"`
	Stats            Stats       `json:"stats"`
	Measurement      Measurement `json:"measurement"`
}

func (r ProbeResult) Lines() []string {
	lines := []string{
		"source: " + r.SourceURL,
		"requested-protocol: " + r.RequestedMode,
		"negotiated-protocol: " + r.Protocol,
	}
	if len(r.RefPrefixes) > 0 {
		lines = append(lines, "ref-prefixes: "+strings.Join(r.RefPrefixes, ", "))
	}
	if r.SourceHEAD != "" {
		lines = append(lines, "source-head: "+r.SourceHEAD.String())
	}
	if r.SourceHeadUnborn {
		lines = append(lines, "source-head-unborn: true")
	}
	if len(r.Capabilities) > 0 {
		lines = append(lines, "source-capabilities: "+strings.Join(r.Capabilities, ", "))
	}
	if r.TargetURL != "" {
		lines = append(lines, "target: "+r.TargetURL)
	}
	if len(r.TargetCaps) > 0 {
		lines = append(lines, "target-capabilities: "+strings.Join(r.TargetCaps, ", "))
	}
	lines = append(lines, fmt.Sprintf("refs: %d", len(r.Refs)))
	for _, ref := range r.Refs {
		lines = append(lines, fmt.Sprintf("ref: %s %s", ref.Hash.String(), ref.Name))
	}
	lines = append(lines, statsLines(r.Stats)...)
	lines = append(lines, measurementLine(r.Measurement)...)
	return lines
}

// FetchResult holds the outcome of a fetch operation.
type FetchResult struct {
	SourceURL      string          `json:"sourceUrl"`
	RequestedMode  string          `json:"requestedMode"`
	Protocol       string          `json:"protocol"`
	Wants          []RefInfo       `json:"wants"`
	Haves          []plumbing.Hash `json:"haves"`
	FetchedObjects int             `json:"fetchedObjects"`
	Stats          Stats           `json:"stats"`
	Measurement    Measurement     `json:"measurement"`
}

func (r FetchResult) MarshalJSON() ([]byte, error) {
	type fr struct {
		SourceURL      string      `json:"sourceUrl"`
		RequestedMode  string      `json:"requestedMode"`
		Protocol       string      `json:"protocol"`
		Wants          []RefInfo   `json:"wants"`
		Haves          []string    `json:"haves"`
		FetchedObjects int         `json:"fetchedObjects"`
		Stats          Stats       `json:"stats"`
		Measurement    Measurement `json:"measurement"`
	}
	haves := make([]string, 0, len(r.Haves))
	for _, h := range r.Haves {
		haves = append(haves, h.String())
	}
	b, err := json.Marshal(fr{
		SourceURL: r.SourceURL, RequestedMode: r.RequestedMode,
		Protocol: r.Protocol, Wants: r.Wants, Haves: haves,
		FetchedObjects: r.FetchedObjects, Stats: r.Stats, Measurement: r.Measurement,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal JSON: %w", err)
	}
	return b, nil
}

func (r FetchResult) Lines() []string {
	lines := []string{
		"source: " + r.SourceURL,
		"requested-protocol: " + r.RequestedMode,
		"negotiated-protocol: " + r.Protocol,
		fmt.Sprintf("wants: %d", len(r.Wants)),
		fmt.Sprintf("haves: %d", len(r.Haves)),
		fmt.Sprintf("fetched-objects: %d", r.FetchedObjects),
	}
	for _, w := range r.Wants {
		lines = append(lines, fmt.Sprintf("want: %s %s", w.Hash.String(), w.Name))
	}
	for _, h := range r.Haves {
		lines = append(lines, "have: "+h.String())
	}
	lines = append(lines, statsLines(r.Stats)...)
	lines = append(lines, measurementLine(r.Measurement)...)
	return lines
}

func statsLines(s Stats) []string {
	if !s.Enabled {
		return nil
	}
	keys := make([]string, 0, len(s.Items))
	for k := range s.Items {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var lines []string
	for _, k := range keys {
		item := s.Items[k]
		lines = append(lines, fmt.Sprintf(
			"stats: %s requests=%d request-bytes=%d response-bytes=%d wants=%d haves=%d commands=%d",
			item.Name, item.Requests, item.RequestBytes, item.ResponseBytes, item.Wants, item.Haves, item.Commands,
		))
	}
	if line := throughputLine(s); line != "" {
		lines = append(lines, line)
	}
	return lines
}

// throughputLine renders a one-line per-side throughput summary using
// the wall-clock window the stats collector observed. Returns "" when
// no side bytes were recorded so the line stays out of the way for
// metadata-only operations like probe.
func throughputLine(s Stats) string {
	if len(s.Sides) == 0 || s.ElapsedNanos <= 0 {
		return ""
	}
	sides := make([]SideBytes, 0, len(s.Sides))
	for _, side := range s.Sides {
		if side.Bytes <= 0 {
			continue
		}
		sides = append(sides, side)
	}
	if len(sides) == 0 {
		return ""
	}
	sort.Slice(sides, func(i, j int) bool { return sides[i].Label < sides[j].Label })
	dur := time.Duration(s.ElapsedNanos)
	parts := make([]string, 0, len(sides))
	for _, side := range sides {
		// End-of-run line uses the active-window average; passing 0
		// for instant rate makes formatSide skip the sliding window
		// and fall back to ActiveNanos-based formatting.
		parts = append(parts, formatSide(side, dur, 0, true))
	}
	return "throughput: " + strings.Join(parts, sideSeparator)
}

func measurementLine(m Measurement) []string {
	if !m.Enabled {
		return nil
	}
	return []string{fmt.Sprintf(
		"measurement: elapsed-ms=%d peak-alloc-bytes=%d peak-heap-inuse-bytes=%d total-alloc-bytes=%d gc-count=%d",
		m.ElapsedMillis, m.PeakAllocBytes, m.PeakHeapInuseBytes, m.TotalAllocBytes, m.GCCount,
	)}
}

// effectiveMaxObjects resolves the materialized object ceiling for a config:
// the caller's value when set, otherwise the same default the materialized
// strategy applies, so the streaming guard and the closure check agree.
func effectiveMaxObjects(cfg Config) int {
	if cfg.MaterializedMaxObjects > 0 {
		return cfg.MaterializedMaxObjects
	}
	return DefaultMaterializedMaxObjects
}

// --- Session setup ---

func newConn(raw Endpoint, label string, stats *statsCollector, httpClient *http.Client) (gitproto.Conn, error) {
	ep, err := transport.ParseURL(raw.URL)
	if err != nil {
		// url.Parse wraps the raw URL in its error (`parse "<url>": ...`),
		// which would echo credentials embedded in userinfo into status
		// output, logs, and CI. Report the cause without the URL string.
		return nil, fmt.Errorf("parse endpoint: %w", redact.URLError(err))
	}
	// Dispatch on scheme: ssh has a native transport; http/https fall through
	// to the HTTP builder below. Any other scheme (e.g. entire://) is handed to
	// a git remote helper named git-remote-<scheme> if one is installed — the
	// helper owns auth and the network round trips while git-sync drives the
	// smart protocol over its stateless-connect bridge. http/https deliberately
	// stay on the native transport rather than git's own git-remote-http(s).
	switch ep.Scheme {
	case "ssh", "git+ssh":
		stats.setSideDisplay(label, hostnameFromURL(raw.URL))
		conn, err := gitproto.NewSSHConn(ep, label)
		if err != nil {
			return nil, fmt.Errorf("new SSH connection: %w", err)
		}
		return conn, nil
	case "http", "https":
		// native HTTP transport, built below
	default:
		if helperPath, ok := gitproto.LookupRemoteHelper(ep.Scheme); ok {
			stats.setSideDisplay(label, hostnameFromURL(raw.URL))
			return gitproto.NewHelperConn(helperPath, ep, raw.URL, label), nil
		}
		// No helper: fall through to the HTTP builder, preserving the prior
		// behavior of surfacing an unsupported-scheme error from there.
	}
	authEp := auth.Endpoint{
		Username:      raw.Username,
		Token:         raw.Token,
		BearerToken:   raw.BearerToken,
		SkipTLSVerify: raw.SkipTLSVerify,
	}
	authMethod := auth.Resolve(authEp, ep)
	stats.setSideDisplay(label, hostnameFromURL(raw.URL))
	client := instrumentHTTPClient(httpClient, raw.SkipTLSVerify, label, stats)
	conn := gitproto.NewHTTPConnWithClient(ep, label, authMethod, client)
	conn.FollowInfoRefsRedirect = raw.FollowInfoRefsRedirect
	conn.InsecureSkipTLSVerify = raw.SkipTLSVerify
	// Installed even alongside an explicit token. Explicit credentials are
	// withheld from a redirect target outside the endpoint's site, and with no
	// helper there would be nothing to fall back to — the 401 would be final,
	// contradicting both the warning git-sync prints and the documented
	// remedy. For same-site requests the explicit token still wins, so the
	// helper is only ever consulted where the token is deliberately not sent.
	conn.CredentialHelper = auth.GitCredentialHelper{}
	return conn, nil
}

// hostnameFromURL returns the host portion of an endpoint URL, used to
// label sides in progress and throughput output. Returns "" for malformed
// URLs so callers can fall back to the internal label.
func hostnameFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func instrumentHTTPClient(base *http.Client, skipTLS bool, label string, stats *statsCollector) *http.Client {
	if base == nil {
		base = &http.Client{Transport: gitproto.NewHTTPTransport(skipTLS)}
	}
	clone := *base
	baseRT := clone.Transport
	if baseRT == nil {
		baseRT = gitproto.NewHTTPTransport(skipTLS)
	}
	clone.Transport = &countingRoundTripper{base: baseRT, label: label, stats: stats}
	return &clone
}

// finalizeCounts applies any best-effort rejections to both the push slice
// and the result.Plans the caller will return, then tallies Pushed/Deleted
// counters from the (now-classified) push plans.
func (s *syncSession) finalizeCounts(pushPlans []BranchPlan, result *Result) {
	if !s.cfg.DryRun {
		if warned := s.applyRejections(pushPlans); warned > 0 {
			s.applyRejections(result.Plans)
			result.Warned += warned
		}
	}
	pushed, deleted := tallyActions(pushPlans)
	result.Pushed += pushed
	result.Deleted += deleted
}

// classifyPlans splits planned actions into pushable plans, tallying skips and
// blocks into result. Under dry-run, pushable plans are counted as skipped and
// none are returned. ActionWarn is not produced by planning; it is only set
// after a push by applyRejections.
func classifyPlans(plans []BranchPlan, dryRun bool, result *Result) []BranchPlan {
	pushPlans := make([]BranchPlan, 0, len(plans))
	for _, plan := range plans {
		switch plan.Action {
		case ActionCreate, ActionUpdate, ActionDelete:
			if dryRun {
				result.Skipped++
				continue
			}
			pushPlans = append(pushPlans, plan)
		case ActionSkip:
			result.Skipped++
		case ActionBlock:
			result.Blocked++
		case ActionWarn:
		}
	}
	return pushPlans
}

// tallyActions counts ref pushes and deletes from a classified plan slice.
// ActionWarn/Skip/Block don't contribute; rejections are tracked separately
// in Result.Warned via applyRejections.
func tallyActions(plans []BranchPlan) (pushed, deleted int) {
	for _, plan := range plans {
		switch plan.Action {
		case ActionCreate, ActionUpdate:
			pushed++
		case ActionDelete:
			deleted++
		case ActionWarn, ActionSkip, ActionBlock:
		}
	}
	return pushed, deleted
}

// leaseFailureError surfaces receive-pack lease misses as a fatal error even
// when BestEffort would otherwise downgrade them. Without this, a sync with
// both --force-with-lease and --all-refs (which implies BestEffort) would
// silently treat a concurrent target update as a warning, defeating the lease.
//
// Scoped to --force-with-lease only. The lease-failure marker set in gitproto
// is intentionally broad to cover phrasing across servers (stale info / fetch
// first / non-fast-forward / does not match), but under --force-blind or
// non-force runs those same messages can mean ordinary policy rejection rather
// than a stale lease, which BestEffort is allowed to downgrade. The contract
// is only made when the user explicitly opts in.
func (s *syncSession) leaseFailureError() error {
	if !s.cfg.ForceWithLease {
		return nil
	}
	if len(s.rejections) == 0 {
		return nil
	}
	var refs []string
	for name, status := range s.rejections {
		if gitproto.IsLeaseFailure(status) {
			refs = append(refs, name.String())
		}
	}
	if len(refs) == 0 {
		return nil
	}
	sort.Strings(refs)
	// Wrap the sentinel so this BestEffort+ForceWithLease escalation is reachable
	// via errors.Is(err, ErrTargetRefMoved) like the non-BestEffort path. Under
	// explicit ForceWithLease the "ambiguous marker" caveat does not apply — the
	// caller opted into lease semantics, so a lease-marker rejection here is
	// definitionally a concurrent target move.
	return fmt.Errorf("lease failure on %d ref(s) (%s) — target moved during sync; rerun, or use --force-blind to overwrite: %w", len(refs), strings.Join(refs, ", "), gitproto.ErrTargetRefMoved)
}

// refOutcome reports what the target did with name in the push that just
// returned: applied it, refused it, or said nothing about it. Answers
// bootstrap.Params.RefOutcome, whose doc has the why; read after a push
// returns, from the same goroutine the pusher's callback ran on.
//
// Scoped to that one push rather than to s.rejections, which accumulates over
// the session: a strategy asking "did the ref I just pushed land" must not be
// answered with a rejection from an earlier request for the same name.
//
// The reason is sanitized here, as the OnRejection callback does for
// s.rejections: it is free-form text the target wrote, and from here it reaches
// the terminal in a notice and an error message, where an escape sequence could
// redraw the line a warning was printed on.
func (s *syncSession) refOutcome(name plumbing.ReferenceName) (gitproto.RefOutcome, string) {
	outcome, reason := s.target.pusher.LastOutcome(name)
	return outcome, sanitize.Text(reason)
}

// targetRefsNow re-reads the target's refs mid-run. Answers
// bootstrap.Params.TargetRefsNow, which asks it only to settle whether a branch
// whose create no push could confirm is actually there — one advertisement
// round trip against a transfer measured in gigabytes.
//
// It is the receive-pack advertisement, so receive.hideRefs can omit a ref that
// is really there (the same caveat Config.TargetAssertedEmpty documents). That
// direction is safe for the one question asked here: a hidden branch reads as
// absent, which keeps a resume marker that was not needed rather than dropping
// one that was.
func (s *syncSession) targetRefsNow(ctx context.Context) (map[plumbing.ReferenceName]plumbing.Hash, error) {
	if s.target == nil || s.target.conn == nil {
		return nil, errors.New("no target connection")
	}
	adv, err := gitproto.AdvertisedRefsV1(ctx, s.target.conn, transport.ReceivePackService)
	if err != nil {
		return nil, fmt.Errorf("list target refs: %w", err)
	}
	// Skipped names are dropped without a second warning: newSession already
	// reported them for this target, and a name git would reject cannot be one
	// of the branches this run created.
	refs, _, err := gitproto.AdvRefsToSlice(adv)
	if err != nil {
		return nil, fmt.Errorf("decode target refs: %w", err)
	}
	return gitproto.RefHashMap(refs), nil
}

// applyRejections downgrades plans whose ref was rejected by the target to
// ActionWarn and returns the count.
func (s *syncSession) applyRejections(plans []BranchPlan) int {
	if len(s.rejections) == 0 {
		return 0
	}
	warned := 0
	for i := range plans {
		status, ok := s.rejections[plans[i].TargetRef]
		if !ok {
			continue
		}
		plans[i].Action = ActionWarn
		if status == "" {
			plans[i].Reason = "target rejected ref update"
		} else {
			plans[i].Reason = "target rejected ref update: " + status
		}
		warned++
	}
	return warned
}

func planConfig(cfg Config) planner.PlanConfig {
	return planner.PlanConfig{
		Branches:           cfg.Branches,
		Mappings:           cfg.Mappings,
		IncludeTags:        cfg.IncludeTags,
		AllRefs:            cfg.AllRefs,
		ExcludeRefPrefixes: cfg.ExcludeRefPrefixes,
		ExcludeRefs:        cfg.ExcludeRefs,
		Force:              cfg.ForceAny(),
		Prune:              cfg.Prune,
	}
}

// ForceAny reports whether either force flag is set. Used by call sites that
// only care about "is this run allowed to do non-fast-forward updates?" rather
// than the lease-vs-blind distinction.
func (c Config) ForceAny() bool { return c.ForceWithLease || c.ForceBlind }

// needsLocalSourceClosure reports whether the sync must populate the
// in-memory store with the full source closure before running. The fetch
// is required when:
//   - Force or prune is set: incremental relay is disabled, so the
//     materialized fallback will run and needs the closure.
//   - Any desired ref already exists on target at a different hash: a
//     branch fast-forward check will need ancestry data, or a tag retarget
//     will route to materialized.
//
// When all desired refs are either skips (target hash matches source) or
// creates (target hash is zero), incremental relay handles the push without
// the closure — the upfront fetch would just be wasted bandwidth, since
// relay does its own FetchPack on the source.
func needsLocalSourceClosure(
	cfg Config,
	desired map[plumbing.ReferenceName]planner.DesiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
) bool {
	if cfg.ForceAny() || cfg.Prune {
		return true
	}
	for targetRef, want := range desired {
		targetHash := targetRefs[targetRef]
		if targetHash.IsZero() {
			continue
		}
		if targetHash == want.SourceHash {
			continue
		}
		return true
	}
	return false
}

func sshStatsWarning(cfg Config, sourceConn, targetConn gitproto.Conn) string {
	if !cfg.Progress && !cfg.ShowStats {
		return ""
	}
	hasSSH := false
	if _, ok := sourceConn.(*gitproto.SSHConn); ok {
		hasSSH = true
	}
	if !hasSSH && targetConn != nil {
		if _, ok := targetConn.(*gitproto.SSHConn); ok {
			hasSSH = true
		}
	}
	if !hasSSH {
		return ""
	}
	return "warning: SSH transport does not yet expose byte-counted throughput; --progress and --stats output will omit SSH transfer bytes"
}

// --- Session setup (issue #12) ---

// syncSession holds the shared state for a sync operation, reducing
// setup duplication across Run, Bootstrap, Probe, and Fetch.
type syncSession struct {
	cfg             Config
	stats           *statsCollector
	logger          *slog.Logger
	sourceConn      gitproto.Conn
	sourceService   *gitproto.RefService
	sourceRefMap    map[plumbing.ReferenceName]plumbing.Hash
	target          *targetSession
	measurementDone func() Measurement
	progress        *progressReporter
	// rejections records target ng statuses; nil unless BestEffort.
	rejections map[plumbing.ReferenceName]string
}

// finish releases the resources owned by the session: the live progress
// ticker, the memory-measurement ticker goroutine, and the source/target
// transports (SSH transports spawn processes). Idempotent and safe to call
// from defer in callers that also produce results in the happy path —
// measurementDone is sync.Once-guarded, so an error path that never built a
// Result still stops its goroutine without disturbing the happy-path value.
func (s *syncSession) finish() {
	if s.progress != nil {
		s.progress.terminate()
	}
	if s.measurementDone != nil {
		_ = s.measurementDone()
	}
	if s.sourceConn != nil {
		_ = s.sourceConn.Close()
	}
	if s.target != nil && s.target.conn != nil {
		_ = s.target.conn.Close()
	}
}

// notice surfaces a one-line human-readable event during a sync. When
// the live progress ticker is active it prints above the current frame;
// otherwise it falls back to plain stderr so the message is still seen
// when --progress is off or the destination is not a TTY.
func (s *syncSession) notice(msg string) {
	if s.progress != nil {
		s.progress.notify(msg)
		return
	}
	fmt.Fprintln(os.Stderr, msg)
}

type targetSession struct {
	conn     gitproto.Conn
	adv      *packp.AdvRefs
	refMap   map[plumbing.ReferenceName]plumbing.Hash
	features gitproto.TargetFeatures
	policy   planner.RelayTargetPolicy
	pusher   *gitproto.Pusher
	// skippedRefCount is how many advertised target ref names were dropped as
	// invalid. Retained, not just warned about, because a non-zero count is
	// load-bearing for any caller reasoning about an EMPTY target: names
	// dropped here leave refMap empty while the target plainly holds refs.
	// A count rather than the names, for the reason on
	// gitproto.RefService.SkippedRefCount — this struct outlives the pack
	// transfer and only a boolean is ever read from it.
	skippedRefCount int
}

// newSession performs the shared setup: protocol validation, mapping validation,
// connection creation, and ref discovery.
func newSession(ctx context.Context, cfg Config, needTarget bool) (*syncSession, error) {
	mode, err := validation.NormalizeProtocolMode(cfg.ProtocolMode)
	if err != nil {
		return nil, fmt.Errorf("normalize protocol mode: %w", err)
	}
	cfg.ProtocolMode = mode
	switch cfg.Mode {
	case "", modeSync:
		cfg.Mode = modeSync
	case modeReplicate:
	default:
		return nil, fmt.Errorf("unsupported operation mode %q", cfg.Mode)
	}
	if err := validateEmptySourcePolicy(cfg); err != nil {
		return nil, err
	}
	if _, err := validation.ValidateMappings(cfg.Mappings, cfg.AllRefs); err != nil {
		return nil, fmt.Errorf("validate mappings: %w", err)
	}
	if cfg.ForceWithLease && cfg.ForceBlind {
		return nil, errors.New("--force-with-lease and --force-blind are mutually exclusive")
	}
	if cfg.Mode == modeReplicate && cfg.ForceAny() {
		return nil, errors.New("replicate does not support force flags; use sync instead")
	}
	if needTarget {
		if err := validation.ValidateEndpoints(cfg.Source.URL, cfg.Target.URL); err != nil {
			return nil, fmt.Errorf("validate endpoints: %w", err)
		}
	}

	s := &syncSession{
		cfg:             cfg,
		stats:           newStats(cfg.ShowStats),
		measurementDone: startMeasurement(cfg.MeasureMemory),
	}
	// startMeasurement spawned a ticker goroutine and the steps below open
	// transports (SSH spawns a process). If we return an error partway through
	// setup the caller has no session to finish(), so release everything here
	// unless we hand the session back.
	success := false
	defer func() {
		if !success {
			s.finish()
		}
	}()
	var warnedSSHStats bool
	warnSSHStats := func(sourceConn, targetConn gitproto.Conn) {
		if warnedSSHStats {
			return
		}
		warning := sshStatsWarning(cfg, sourceConn, targetConn)
		if warning == "" {
			return
		}
		warnedSSHStats = true
		out := cfg.progressOut
		if out == nil {
			out = os.Stderr
		}
		_, _ = fmt.Fprintln(out, warning)
	}
	if cfg.Verbose {
		s.logger = slog.New(slog.NewTextHandler(&sessionStderr{s: s}, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		}))
	}

	s.sourceConn, err = newConn(cfg.Source, "source", s.stats, cfg.HTTPClient)
	if err != nil {
		return nil, fmt.Errorf("create source transport: %w", err)
	}
	s.sourceConn.SetProgressWriter(&sessionStderr{s: s})
	warnSSHStats(s.sourceConn, nil)

	refPrefixes := planner.RefPrefixes(planConfig(cfg))
	sourceRefs, sourceService, err := gitproto.ListSourceRefs(ctx, s.sourceConn, cfg.ProtocolMode, refPrefixes)
	if err != nil {
		return nil, fmt.Errorf("list source refs: %w", err)
	}
	sourceService.Verbose = cfg.Verbose
	s.sourceService = sourceService
	s.sourceRefMap = gitproto.RefHashMap(sourceRefs)

	if needTarget {
		targetConn, err := newConn(cfg.Target, "target", s.stats, cfg.HTTPClient)
		if err != nil {
			return nil, fmt.Errorf("create target transport: %w", err)
		}
		// Hand the conn to the session immediately so the deferred cleanup
		// closes it even if a ref-listing step below fails.
		s.target = &targetSession{conn: targetConn}
		targetConn.SetProgressWriter(&sessionStderr{s: s})
		warnSSHStats(s.sourceConn, targetConn)
		targetAdv, err := gitproto.AdvertisedRefsV1(ctx, targetConn, transport.ReceivePackService)
		if err != nil {
			return nil, fmt.Errorf("list target refs: %w", err)
		}
		targetRefSlice, skippedTargetRefs, err := gitproto.AdvRefsToSlice(targetAdv)
		if err != nil {
			return nil, fmt.Errorf("decode target refs: %w", err)
		}
		// A skipped target ref is also one the planner will not see, so it is
		// never picked as a prune candidate — the safe direction, but the
		// operator should know the target holds a name git would reject.
		gitproto.WarnSkippedRefNames(targetConn.ProgressWriter(), "target", skippedTargetRefs)
		s.target.skippedRefCount = len(skippedTargetRefs)
		targetRefMap := gitproto.RefHashMap(targetRefSlice)
		targetFeatures := gitproto.TargetFeaturesFromAdvRefs(targetAdv)
		s.target.adv = targetAdv
		s.target.refMap = targetRefMap
		s.target.features = targetFeatures
		s.target.policy = planner.RelayTargetPolicy{
			CapabilitiesKnown: targetFeatures.Known,
			NoThin:            targetFeatures.NoThin,
		}
		s.target.pusher = gitproto.NewPusher(targetConn, targetAdv, cfg.Verbose)
		s.target.pusher.MaxRefUpdates = cfg.TargetMaxRefUpdates
		if cfg.BestEffort {
			s.rejections = make(map[plumbing.ReferenceName]string)
			s.target.pusher.OnRejection = func(name plumbing.ReferenceName, status string) {
				// Server-authored text that becomes BranchPlan.Reason, which is
				// printed by FormatPlanLine and marshalled as "reason". This is
				// the designed path under --all-refs, where per-ref rejections
				// surface as warnings rather than errors, so it carries hostile
				// text to the terminal and to --json by intent. It never passes
				// through asRefRejectedError, so filtering has to happen here.
				// IsLeaseFailure and applyRejections only substring-match, so
				// this cannot change how a rejection is classified.
				s.rejections[name] = sanitize.Text(status)
			}
		}
	}
	// Start the live progress ticker only after auth resolution and the
	// initial ref-listing round trips have completed. The auth path may
	// shell out to `git credential fill`, which inherits our stderr and
	// can prompt the user; an interactive prompt and a '\r'-redrawing
	// ticker writing to the same tty would clobber each other. Deferring
	// the ticker until newSession returns guarantees no concurrent writer
	// is active when prompts happen and also avoids leaking a goroutine
	// when newSession fails partway through setup.
	if cfg.Progress {
		out := cfg.progressOut
		if out == nil {
			out = os.Stderr
		}
		// Render only when the destination is a real terminal. Pipes,
		// log files, and CI captures get nothing rather than a flood
		// of '\r'-prefixed control sequences.
		if out != os.Stderr || stderrIsTTY() {
			s.progress = newProgressReporter(out, s.stats, 0)
			go s.progress.run()
		}
	}

	success = true
	return s, nil
}

// --- Public API ---

// Run executes a sync or plan operation.
func Run(ctx context.Context, cfg Config) (Result, error) {
	s, err := newSession(ctx, cfg, true)
	if err != nil {
		return Result{}, err
	}
	defer s.finish()
	if s.cfg.Mode == modeReplicate {
		return s.runReplicate(ctx)
	}
	return s.runSync(ctx)
}

func (s *syncSession) runSync(ctx context.Context) (Result, error) {
	measurementDone := s.measurementDone
	stats := s.stats
	sourceService := s.sourceService
	sourceRefMap := s.sourceRefMap
	targetRefMap := s.target.refMap

	desiredRefs, managedTargets, err := planner.BuildDesiredRefs(sourceRefMap, planConfig(s.cfg))
	if err != nil {
		return Result{}, fmt.Errorf("build desired refs: %w", err)
	}
	if len(desiredRefs) == 0 {
		return Result{}, errors.New("no source refs matched")
	}

	// Check for bootstrap opportunity (before allocating in-memory repo)
	if ok, reason := planner.CanBootstrapRelay(s.cfg.ForceAny(), s.cfg.Prune, desiredRefs, targetRefMap); ok {
		if s.cfg.DryRun {
			plans, err := planner.BuildBootstrapPlans(desiredRefs, targetRefMap)
			if err != nil {
				return Result{}, fmt.Errorf("build bootstrap plans: %w", err)
			}
			return Result{
				Plans: plans, DryRun: true, RelayReason: reason,
				OperationMode: modeSync, BootstrapSuggested: true, Stats: stats.snapshot(),
				Measurement: measurementDone(), Protocol: sourceService.Protocol,
				SourceHEAD: s.sourceService.HeadTarget,
			}, nil
		}
		return bootstrapWithInputs(ctx, s, desiredRefs, targetRefMap, reason)
	}

	// Normal sync: allocate in-memory repo. The source closure is fetched
	// lazily — only when planning needs ancestry data (FF detection on a
	// divergent branch) or when the materialized fallback ends up running.
	// Pure skip/create plans that take incremental relay never decode
	// source objects locally, so the upfront fetch would be a wasted
	// full-pack round trip.
	// Bound the store before anything is fetched into it: the object limit used
	// to be checked on the closure afterwards, which cannot prevent the memory
	// growth it exists to prevent.
	repo, err := git.Init(newBoundedStorer(memory.NewStorage(), effectiveMaxObjects(s.cfg)), nil)
	if err != nil {
		return Result{}, fmt.Errorf("init in-memory repository: %w", err)
	}
	gpDesired := convert.DesiredRefs(desiredRefs)
	closureFetched := false
	fetchClosure := func() error {
		if closureFetched {
			return nil
		}
		if err := sourceService.FetchToStore(ctx, repo.Storer, s.sourceConn, gpDesired, targetRefMap); err != nil {
			if !errors.Is(err, git.NoErrAlreadyUpToDate) {
				return fmt.Errorf("fetch to store: %w", err)
			}
		}
		closureFetched = true
		return nil
	}
	if needsLocalSourceClosure(s.cfg, desiredRefs, targetRefMap) {
		if err := fetchClosure(); err != nil {
			return Result{}, err
		}
	}

	plans, err := planner.BuildPlans(repo.Storer, desiredRefs, targetRefMap, managedTargets, planConfig(s.cfg))
	if err != nil {
		return Result{}, fmt.Errorf("build plans: %w", err)
	}

	result := Result{
		Plans: plans, DryRun: s.cfg.DryRun, OperationMode: modeSync, Protocol: sourceService.Protocol,
		Stats: stats.snapshot(), Measurement: measurementDone(),
		SourceHEAD: s.sourceService.HeadTarget,
	}

	pushPlans := classifyPlans(plans, s.cfg.DryRun, &result)

	if !s.cfg.DryRun && result.Blocked > 0 {
		return result, fmt.Errorf("blocked %d ref update(s); rerun with --force-with-lease (or --force-blind) where appropriate", result.Blocked)
	}
	result.RelayReason = planner.RelayFallbackReason(s.cfg.ForceAny(), s.cfg.Prune, s.cfg.DryRun, pushPlans, s.target.policy)

	if !s.cfg.DryRun {
		// Try incremental relay first
		incResult, err := s.executeIncremental(ctx, desiredRefs, pushPlans)
		if err != nil {
			return result, err
		}
		if incResult.Relay {
			result.Relay = incResult.Relay
			result.RelayMode = incResult.RelayMode
			result.RelayReason = incResult.RelayReason
		} else if len(pushPlans) > 0 {
			// Materialized fallback. needsLocalSourceClosure may have skipped
			// the upfront fetch when relay looked eligible from the plan
			// shape alone, but CanIncrementalRelay can still reject (e.g.
			// when target capabilities are unknown). Fetch on demand so
			// materialized doesn't run against an empty store.
			if err := fetchClosure(); err != nil {
				return result, err
			}
			if err := s.executeMaterialized(ctx, repo.Storer, desiredRefs, pushPlans); err != nil {
				return result, err
			}
		}
	}

	s.finalizeCounts(pushPlans, &result)
	if err := s.leaseFailureError(); err != nil {
		return result, err
	}
	result.Stats = stats.snapshot()
	result.Measurement = measurementDone()
	return result, nil
}

func (s *syncSession) runReplicate(ctx context.Context) (Result, error) {
	// An empty source advertisement is resolved BEFORE planning. Under AllRefs
	// the advertisement covers refs/, so "nothing advertised" is already a
	// complete observation and planning cannot add to it — while
	// BuildDesiredRefs errors on a mapping whose source ref is absent, which
	// on a genuinely empty source is every mapping. Deciding after planning
	// left the whole policy inert for any request that pinned refs by mapping:
	// the caller got "source ref X not found" matching no sentinel, on exactly
	// the state the policy exists to make succeed.
	//
	// Gated on the opt-in so a caller that never asked for this still gets the
	// planner's error verbatim.
	//
	// The relay check runs FIRST, above both this intercept and planning. It is
	// a property of the target, not of the work: replicate refuses a non-relay
	// target outright, and a converged result claims Relay: true on the
	// strength of that refusal. Deciding emptiness ahead of it would let the
	// one path that makes the claim be the one path that never checked it.
	if ok, reason := planner.SupportsReplicateRelay(s.target.policy); !ok {
		return Result{OperationMode: modeReplicate}, fmt.Errorf("replicate requires relay-capable target: %s; use sync instead", reason)
	}
	if s.cfg.AllowEmptySource && s.cfg.AllRefs && len(s.sourceRefMap) == 0 {
		return s.resolveEmptySource()
	}
	desiredRefs, managedTargets, err := planner.BuildDesiredRefs(s.sourceRefMap, planConfig(s.cfg))
	if err != nil {
		return Result{}, fmt.Errorf("build desired refs: %w", err)
	}
	if len(desiredRefs) == 0 {
		return s.resolveEmptyDesiredSet()
	}

	takeBootstrap, bootstrapReason := s.replicateBootstrapRoute(desiredRefs, s.sourceService.SupportsBootstrapBatch())
	if takeBootstrap {
		if s.cfg.DryRun {
			plans, err := planner.BuildBootstrapPlans(desiredRefs, s.target.refMap)
			if err != nil {
				return Result{}, fmt.Errorf("build bootstrap plans: %w", err)
			}
			return Result{
				Plans:              plans,
				DryRun:             true,
				OperationMode:      modeReplicate,
				RelayReason:        bootstrapReason,
				BootstrapSuggested: true,
				Stats:              s.stats.snapshot(),
				Measurement:        s.measurementDone(),
				Protocol:           s.sourceService.Protocol,
				SourceHEAD:         s.sourceService.HeadTarget,
			}, nil
		}
		return bootstrapWithInputs(ctx, s, desiredRefs, s.target.refMap, bootstrapReason)
	}

	plans, err := planner.BuildReplicationPlans(desiredRefs, s.target.refMap, managedTargets, planConfig(s.cfg))
	if err != nil {
		return Result{}, fmt.Errorf("build replication plans: %w", err)
	}

	result := Result{
		Plans:         plans,
		DryRun:        s.cfg.DryRun,
		OperationMode: modeReplicate,
		Protocol:      s.sourceService.Protocol,
		Stats:         s.stats.snapshot(),
		Measurement:   s.measurementDone(),
		SourceHEAD:    s.sourceService.HeadTarget,
	}

	pushPlans := classifyPlans(plans, s.cfg.DryRun, &result)
	relayPlans := make([]BranchPlan, 0, len(pushPlans))
	for _, plan := range pushPlans {
		if plan.Action != ActionDelete {
			relayPlans = append(relayPlans, plan)
		}
	}

	if !s.cfg.DryRun && len(pushPlans) > 0 {
		if len(relayPlans) > 0 {
			ok, reason := planner.CanReplicateRelay(relayPlans)
			if !ok {
				return result, fmt.Errorf("replicate requires relay-capable target: %s; use sync instead", reason)
			}
		}
		repResult, err := s.executeReplicate(ctx, desiredRefs, pushPlans)
		if err != nil {
			return result, fmt.Errorf("replicate relay failed: %w", err)
		}
		result.Relay = repResult.Relay
		result.RelayMode = repResult.RelayMode
		result.RelayReason = repResult.RelayReason
	}

	s.finalizeCounts(pushPlans, &result)
	if err := s.leaseFailureError(); err != nil {
		return result, err
	}
	result.Stats = s.stats.snapshot()
	result.Measurement = s.measurementDone()
	return result, nil
}

// replicateBootstrapRoute decides whether a replicate run should take the
// bootstrap strategy instead of building replication plans, and with which
// relay reason.
//
// Besides the emptiness heuristic (desiredTargetRefsAbsent plus
// pruneDeletesNothingInScope), a leftover
// refs/gitsync/bootstrap/* temp ref routes to bootstrap directly. The
// namespace has exactly one writer — batched bootstrap, which deletes its
// marker when the branch is finalized — so a surviving marker means a
// bootstrap started and did not finish. Without this route, the marker is
// just an undesired target ref that disqualifies the emptiness heuristic
// under prune: the artifact of the one strategy that can resume is what
// locked that strategy out, and the repo re-synced as a single unbatched
// pack forever (ENT-2054).
//
// Marker routing deliberately ignores other stray target refs — an aborted
// push or a branch deleted upstream mid-bootstrap must not re-wedge the
// repo — but still requires every desired target ref to be absent, because
// the bootstrap planner refuses targets whose desired refs exist. A marker
// surviving next to its completed branch is stale; that state takes the
// replicate path, which prunes it.
//
// Bootstrap does not prune, so every run this route claims is a run that
// deletes nothing. That is one deferred pass in the normal case — the
// bootstrap completes and the next run prunes — but a bootstrap that keeps
// failing keeps the marker alive, re-fires this route, and starves prune for
// as long as it fails, where pre-route runs at least pruned while getting the
// transfer wrong. Accepted: a target stuck mid-bootstrap has a half-imported
// repository to finish, which outranks reaping its stale refs, and the refs
// in question are unreachable-but-harmless until it lands.
//
// The marker check runs before the emptiness heuristic so a resume is
// labeled bootstrap-resume-marker even in configs where both would route to
// bootstrap (without prune the heuristic has no walk to disqualify it).
//
// batchableSource gates the marker route on the source supporting batched
// bootstrap (protocol v2 + fetch filter). Markers are only ever written by
// batched bootstrap, so a non-batchable source seeing one is a corner (the
// capability regressed since the marker was written). Standing down sends
// the run wherever the remaining heuristics point: under prune, to replicate
// — which keeps the marker as a fetch have (the planner's live-marker prune
// guard preserves it); without prune, the emptiness heuristic still selects
// the one-shot bootstrap, which likewise fetches with the marker as a have.
// Either way the transfer stays marker-anchored instead of committing to a
// batched plan the source cannot serve.
func (s *syncSession) replicateBootstrapRoute(
	desiredRefs map[plumbing.ReferenceName]planner.DesiredRef,
	batchableSource bool,
) (bool, string) {
	if !s.desiredTargetRefsAbsent(desiredRefs) {
		return false, ""
	}
	if batchableSource && s.hasBootstrapResumeMarker(desiredRefs) {
		return true, planner.ReasonBootstrapResumeMarker
	}
	if s.pruneDeletesNothingInScope(desiredRefs) {
		return true, planner.ReasonEmptyTargetManagedRefs
	}
	return false, ""
}

// hasBootstrapResumeMarker reports whether the target carries a batched-
// bootstrap temp ref for a branch in the desired set — resume state for work
// this run would actually do. An orphan marker (its branch deleted upstream
// mid-bootstrap) is stale, not resume state: routing on it would relabel an
// ordinary replicate as a resume and push the marker's prune out by a run,
// where replicate creates the desired branches and prunes the orphan in the
// same pass. The route's caller has already established that every desired
// target ref is absent, so a matching marker is live by definition.
//
// Exclusions are honored so a caller that carved the namespace out of its
// scope keeps the routing it asked for (the emptiness heuristic then skips
// the marker for the same reason, and the bootstrap it selects still resumes
// — the marker stays in the target ref map).
func (s *syncSession) hasBootstrapResumeMarker(desiredRefs map[plumbing.ReferenceName]planner.DesiredRef) bool {
	for targetRef, hash := range s.target.refMap {
		if hash.IsZero() {
			continue
		}
		branch, ok := planner.BootstrapTempRefTarget(targetRef)
		if !ok {
			continue
		}
		if _, desired := desiredRefs[branch]; !desired {
			continue
		}
		if planner.IsRefExcluded(targetRef, s.cfg.ExcludeRefPrefixes, s.cfg.ExcludeRefs) {
			continue
		}
		return true
	}
	return false
}

func (s *syncSession) desiredTargetRefsAbsent(desiredRefs map[plumbing.ReferenceName]planner.DesiredRef) bool {
	for targetRef := range desiredRefs {
		if !s.target.refMap[targetRef].IsZero() {
			return false
		}
	}
	return true
}

// pruneDeletesNothingInScope reports whether a prune would leave every
// undesired target ref alone — either because prune is off, or because no
// such ref falls inside the request's scope. A single in-scope deletion means
// the target is not the blank slate bootstrap plans for.
func (s *syncSession) pruneDeletesNothingInScope(desiredRefs map[plumbing.ReferenceName]planner.DesiredRef) bool {
	if !s.cfg.Prune {
		return true
	}
	for targetRef, hash := range s.target.refMap {
		if hash.IsZero() {
			continue
		}
		if _, ok := desiredRefs[targetRef]; ok {
			continue
		}
		if planner.IsRefExcluded(targetRef, s.cfg.ExcludeRefPrefixes, s.cfg.ExcludeRefs) {
			continue
		}
		// AllRefs overrides per-namespace allowlists: under "all refs" a
		// stale branch matters even when a Branches filter is set.
		branchScopeCovers := s.cfg.AllRefs || len(s.cfg.Branches) == 0
		switch {
		case targetRef.IsTag() && (s.cfg.IncludeTags || s.cfg.AllRefs):
			return false
		case targetRef.IsBranch() && len(s.cfg.Mappings) == 0 && branchScopeCovers:
			return false
		case s.cfg.AllRefs && planner.RefKindFromName(targetRef) == planner.RefKindOther && len(s.cfg.Mappings) == 0:
			return false
		}
	}
	return true
}

// Bootstrap seeds an empty target with relay behavior.
func Bootstrap(ctx context.Context, cfg Config) (Result, error) {
	if cfg.ForceAny() {
		return Result{}, errors.New("bootstrap does not support force flags")
	}
	if cfg.Prune {
		return Result{}, errors.New("bootstrap does not support --prune")
	}
	if cfg.DryRun {
		return Result{}, errors.New("bootstrap does not support dry-run; use plan or sync")
	}

	s, err := newSession(ctx, cfg, true)
	if err != nil {
		return Result{}, err
	}
	defer s.finish()

	desiredRefs, _, err := planner.BuildDesiredRefs(s.sourceRefMap, planConfig(cfg))
	if err != nil {
		return Result{}, fmt.Errorf("build desired refs: %w", err)
	}
	if len(desiredRefs) == 0 {
		return Result{}, errors.New("no source refs matched")
	}

	_, reason := planner.CanBootstrapRelay(cfg.ForceAny(), cfg.Prune, desiredRefs, s.target.refMap)
	result, err := bootstrapWithInputs(ctx, s, desiredRefs, s.target.refMap, reason)
	result.Measurement = s.measurementDone()
	return result, err
}

// Probe inspects source and optionally target remotes.
func Probe(ctx context.Context, cfg Config) (ProbeResult, error) {
	if cfg.Source.URL == "" {
		return ProbeResult{}, errors.New("source repository URL is required")
	}

	s, err := newSession(ctx, cfg, cfg.Target.URL != "")
	if err != nil {
		return ProbeResult{}, err
	}
	defer s.finish()
	return s.newProbeResult(), nil
}

// Fetch exercises source-side fetch negotiation.
func Fetch(ctx context.Context, cfg Config, haveRefs []string, haveHashes []plumbing.Hash) (FetchResult, error) {
	if cfg.Source.URL == "" {
		return FetchResult{}, errors.New("source repository URL is required")
	}

	s, err := newSession(ctx, cfg, false)
	if err != nil {
		return FetchResult{}, err
	}
	defer s.finish()

	// Bounded only when the caller asks, with no default — unlike the sync path
	// above. Fetch exists to exercise source-side negotiation and, without
	// --have, deliberately pulls a full pack, so a default ceiling would fail a
	// previously working operation at the limit. The sync path's fetch is
	// have-bounded by the target's refs, so there the cap tracks the diff being
	// pushed, which is what the limit is for.
	repo, err := git.Init(newBoundedStorer(memory.NewStorage(), cfg.MaterializedMaxObjects), nil)
	if err != nil {
		return FetchResult{}, fmt.Errorf("init in-memory repository: %w", err)
	}
	desiredRefs, err := s.buildDesiredRefs()
	if err != nil {
		return FetchResult{}, fmt.Errorf("build desired refs: %w", err)
	}
	targetRefMap, err := s.buildHaveRefMap(haveRefs, haveHashes)
	if err != nil {
		return FetchResult{}, fmt.Errorf("build have ref map: %w", err)
	}

	gpDesired := convert.DesiredRefs(desiredRefs)
	if err := s.sourceService.FetchToStore(ctx, repo.Storer, s.sourceConn, gpDesired, targetRefMap); err != nil {
		if !errors.Is(err, git.NoErrAlreadyUpToDate) {
			return FetchResult{}, fmt.Errorf("fetch to store: %w", err)
		}
	}

	objectCount, err := countObjects(repo.Storer)
	if err != nil {
		return FetchResult{}, fmt.Errorf("count fetched objects: %w", err)
	}
	return s.newFetchResult(objectCount, desiredRefs, targetRefMap), nil
}

// --- Bootstrap implementation ---

func bootstrapWithInputs(
	ctx context.Context,
	s *syncSession,
	desiredRefs map[plumbing.ReferenceName]planner.DesiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
	relayReason string,
) (Result, error) {
	bResult, err := bstrap.Execute(ctx, bstrap.Params{
		SourceConn: s.sourceConn, SourceService: s.sourceService, TargetPusher: s.target.pusher,
		DesiredRefs: desiredRefs, TargetRefs: targetRefs,
		SourceHeadTarget: s.sourceService.HeadTarget,
		MaxPackBytes:     s.cfg.MaxPackBytes, TargetMaxPack: s.cfg.TargetMaxPackBytes,
		Verbose: s.cfg.Verbose, Logger: s.logger,
		Strategy:   s.cfg.BootstrapStrategy,
		OnPhase:    s.stats.setPhase,
		OnNotice:   s.notice,
		RefOutcome: s.refOutcome,
		// Where a push leaves a create in doubt — refused, or a target that
		// reports nothing — the strategy asks the target which refs it has
		// rather than guessing from the wording of a rejection.
		TargetRefsNow: s.targetRefsNow,
	}, relayReason)
	if err != nil {
		return Result{}, fmt.Errorf("bootstrap execute: %w", err)
	}
	plans := bResult.Plans
	warned := s.applyRejections(plans)
	pushed, _ := tallyActions(plans)
	return Result{
		Plans: plans, Pushed: pushed, Warned: warned, OperationMode: s.cfg.Mode,
		Relay: bResult.Relay, RelayMode: bResult.RelayMode, RelayReason: bResult.RelayReason,
		Batching: bResult.Batching, BatchCount: bResult.BatchCount,
		PlannedBatchCount: bResult.PlannedBatchCount, TempRefs: bResult.TempRefs,
		Stats: s.stats.snapshot(), Measurement: s.measurementDone(), Protocol: s.sourceService.Protocol,
		SourceHEAD: s.sourceService.HeadTarget,
	}, nil
}

func (s *syncSession) executeIncremental(
	ctx context.Context,
	desiredRefs map[plumbing.ReferenceName]planner.DesiredRef,
	pushPlans []planner.BranchPlan,
) (incremental.Result, error) {
	incResult, incErr := incremental.Execute(ctx, incremental.Params{
		SourceConn: s.sourceConn, SourceService: s.sourceService, TargetPusher: s.target.pusher,
		DesiredRefs: desiredRefs, TargetRefs: s.target.refMap,
		PushPlans: pushPlans, MaxPackBytes: s.cfg.MaxPackBytes,
		ForceBlind: s.cfg.ForceBlind,
		CanRelay: func(force, prune, dryRun bool, plans []planner.BranchPlan) (bool, string) {
			return planner.CanIncrementalRelay(force, prune, dryRun, plans, s.target.policy)
		},
		CanTagRelay: planner.CanFullTagCreateRelay,
	}, planConfig(s.cfg))
	if incErr != nil {
		return incResult, fmt.Errorf("incremental execute: %w", incErr)
	}
	return incResult, nil
}

func (s *syncSession) executeMaterialized(
	ctx context.Context,
	store storer.Storer,
	desiredRefs map[plumbing.ReferenceName]planner.DesiredRef,
	pushPlans []planner.BranchPlan,
) error {
	if err := materialized.Execute(ctx, materialized.Params{
		Store: store, SourceConn: s.sourceConn, SourceService: s.sourceService, TargetPusher: s.target.pusher,
		DesiredRefs: desiredRefs, TargetRefs: s.target.refMap,
		PushPlans: pushPlans, MaxObjects: s.cfg.MaterializedMaxObjects,
		ForceBlind: s.cfg.ForceBlind,
	}); err != nil {
		return fmt.Errorf("materialized execute: %w", err)
	}
	return nil
}

func (s *syncSession) executeReplicate(
	ctx context.Context,
	desiredRefs map[plumbing.ReferenceName]planner.DesiredRef,
	pushPlans []planner.BranchPlan,
) (repstrat.Result, error) {
	repResult, repErr := repstrat.Execute(ctx, repstrat.Params{
		SourceConn: s.sourceConn, SourceService: s.sourceService, TargetPusher: s.target.pusher,
		DesiredRefs: desiredRefs, TargetRefs: s.target.refMap,
		PushPlans: pushPlans, MaxPackBytes: s.cfg.MaxPackBytes,
	})
	if repErr != nil {
		return repResult, fmt.Errorf("replicate execute: %w", repErr)
	}
	return repResult, nil
}

func (s *syncSession) buildDesiredRefs() (map[plumbing.ReferenceName]planner.DesiredRef, error) {
	desiredRefs, _, err := planner.BuildDesiredRefs(s.sourceRefMap, planConfig(s.cfg))
	if err != nil {
		return nil, fmt.Errorf("build desired refs: %w", err)
	}
	if len(desiredRefs) == 0 {
		return nil, errors.New("no source refs matched")
	}
	return desiredRefs, nil
}

func (s *syncSession) buildHaveRefMap(haveRefs []string, haveHashes []plumbing.Hash) (map[plumbing.ReferenceName]plumbing.Hash, error) {
	targetRefMap := make(map[plumbing.ReferenceName]plumbing.Hash)
	for _, raw := range haveRefs {
		name := validation.ParseHaveRef(raw)
		hash, ok := s.sourceRefMap[name]
		if !ok {
			return nil, fmt.Errorf("have-ref %q not found on source", raw)
		}
		targetRefMap[name] = hash
	}
	for idx, hash := range haveHashes {
		targetRefMap[plumbing.ReferenceName(fmt.Sprintf("refs/haves/%d", idx))] = hash
	}
	return targetRefMap, nil
}

func (s *syncSession) newProbeResult() ProbeResult {
	refInfos := make([]RefInfo, 0, len(s.sourceRefMap))
	for name, hash := range s.sourceRefMap {
		if planner.IsRefExcluded(name, s.cfg.ExcludeRefPrefixes, s.cfg.ExcludeRefs) {
			continue
		}
		refInfos = append(refInfos, RefInfo{Name: name.String(), Hash: hash})
	}
	sort.Slice(refInfos, func(i, j int) bool { return refInfos[i].Name < refInfos[j].Name })

	result := ProbeResult{
		// Redacted: an endpoint URL may carry credentials in its userinfo
		// (https://user:token@host, or the token-as-username form), and this
		// value is printed to stdout and serialized into --json output.
		SourceURL:     redact.URL(s.cfg.Source.URL),
		RequestedMode: s.cfg.ProtocolMode,
		Protocol:      s.sourceService.Protocol,
		RefPrefixes:   planner.RefPrefixes(planConfig(s.cfg)),
		Capabilities:  s.sourceService.Capabilities(),
		Refs:          refInfos,
		SourceHEAD:    s.sourceService.HeadTarget,
		// Reported even though nothing in Probe acts on it: it is the one
		// wire fact that explains an unconvergeable empty mirror, and it is
		// otherwise unobservable from outside.
		SourceHeadUnborn: s.sourceService.HeadUnborn,
		Stats:            s.stats.snapshot(),
		Measurement:      s.measurementDone(),
	}
	if s.target != nil {
		result.TargetURL = redact.URL(s.cfg.Target.URL)
		result.TargetCaps = gitproto.AdvRefsCaps(s.target.adv)
	}
	return result
}

func (s *syncSession) newFetchResult(
	objectCount int,
	desiredRefs map[plumbing.ReferenceName]planner.DesiredRef,
	targetRefMap map[plumbing.ReferenceName]plumbing.Hash,
) FetchResult {
	wants := make([]RefInfo, 0, len(desiredRefs))
	for _, ref := range desiredRefs {
		wants = append(wants, RefInfo{Name: ref.SourceRef.String(), Hash: ref.SourceHash})
	}
	sort.Slice(wants, func(i, j int) bool { return wants[i].Name < wants[j].Name })

	haveValues := make([]plumbing.Hash, 0, len(targetRefMap))
	for _, h := range targetRefMap {
		if !h.IsZero() {
			haveValues = append(haveValues, h)
		}
	}

	return FetchResult{
		SourceURL:      redact.URL(s.cfg.Source.URL),
		RequestedMode:  s.cfg.ProtocolMode,
		Protocol:       s.sourceService.Protocol,
		Wants:          wants,
		Haves:          gitproto.SortedUniqueHashes(haveValues),
		FetchedObjects: objectCount,
		Stats:          s.stats.snapshot(),
		Measurement:    s.measurementDone(),
	}
}

func countObjects(store storer.EncodedObjectStorer) (int, error) {
	iter, err := store.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		return 0, fmt.Errorf("iterate encoded objects: %w", err)
	}
	defer iter.Close()
	count := 0
	if err := iter.ForEach(func(_ plumbing.EncodedObject) error {
		count++
		return nil
	}); err != nil {
		return 0, fmt.Errorf("count encoded objects: %w", err)
	}
	return count, nil
}
