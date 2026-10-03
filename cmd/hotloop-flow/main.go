// Command hotloop-flow runs the flow engine.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/HotLoop-io/hotloop-flow/internal/api"
	"github.com/HotLoop-io/hotloop-flow/internal/config"
	"github.com/HotLoop-io/hotloop-flow/internal/discover"
	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/filescope"
	"github.com/HotLoop-io/hotloop-flow/internal/flowhttp"
	"github.com/HotLoop-io/hotloop-flow/internal/history"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/nodes" // registers the built-in palette
	"github.com/HotLoop-io/hotloop-flow/internal/runtime"
	"github.com/HotLoop-io/hotloop-flow/internal/shell"
	"github.com/HotLoop-io/hotloop-flow/internal/store"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		// A command whose exit status is its answer, like diff, has already
		// said everything it has to say.
		var code exitCode
		if errors.As(err, &code) {
			os.Exit(int(code))
		}
		// A configuration refusal gets printed plainly with its remedy rather
		// than as a stack of wrapped errors. The operator reading this is
		// probably looking at a CrashLoopBackOff.
		var insecure *config.ErrInsecure
		if errors.As(err, &insecure) {
			fmt.Fprintf(os.Stderr, "\nhotloop-flow %s\n\n%s\n\n", version, insecure.Error())
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "hotloop-flow: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		switch os.Args[1] {
		case "hash-password":
			return cmdHashPassword(os.Args[2:])
		case "import":
			return cmdImport(os.Args[2:])
		case "diff":
			return cmdDiff(os.Args[2:], os.Stdout)
		case "bench":
			return cmdBench(os.Args[2:])
		case "version":
			fmt.Println(version)
			return nil
		default:
			return fmt.Errorf("unknown command %q (try: serve, hash-password, import, diff, bench, version)", os.Args[1])
		}
	}
	return cmdServe(os.Args[1:])
}

// ---------------------------------------------------------------------------
// serve
// ---------------------------------------------------------------------------

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("hotloop-flow", flag.ContinueOnError)
	configPath := fs.String("config", os.Getenv("HOTLOOP_FLOW_CONFIG"), "path to the YAML configuration file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	log := newLogger(cfg.Logging)
	log.Info("starting", "version", version, "addr", cfg.Addr(), "dataDir", cfg.Data.Dir)
	if !cfg.Auth.Enabled {
		// Config.Validate only lets this through when HOTLOOP_FLOW_INSECURE was
		// set on purpose. It still gets said at boot, every boot, because the
		// next person to read this log may not be the one who set it.
		log.Warn("authentication is disabled by HOTLOOP_FLOW_INSECURE: anyone who can reach "+
			"this port can deploy a flow, and a flow can run commands", "addr", cfg.Addr())
	}

	// Install the discovery scope before any flow starts, so a scan node can
	// never run against an unbounded scope even for one message.
	scope, err := discover.NewScope(cfg.Discovery.Enabled, cfg.Discovery.AllowedCIDRs)
	if err != nil {
		return fmt.Errorf("discovery: %w", err)
	}
	nodes.Scope = scope
	if cfg.Discovery.Enabled {
		log.Warn("network discovery is enabled",
			"allowedCIDRs", strings.Join(cfg.Discovery.AllowedCIDRs, ","))
	}

	// Same treatment for the exec node: the policy is installed before any flow
	// starts, so a node can never run for even one message against an unset one.
	commands, err := shell.NewPolicy(cfg.Exec.Enabled, cfg.Exec.AllowedCommands)
	if err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	nodes.Commands = commands
	if cfg.Exec.Enabled {
		log.Warn("the exec node is enabled",
			"allowedCommands", strings.Join(commands.Allowed(), ","))
		// Said at boot rather than at the first message: an operator who
		// misspelled a command should find out while they are still looking.
		if missing := commands.Unresolved(); len(missing) > 0 {
			log.Warn("some allowed commands were not found on the PATH; they will be "+
				"resolved again if a flow uses one, in case they are mounted later",
				"commands", strings.Join(missing, ","))
		}
	}

	if err := os.MkdirAll(cfg.Data.Dir, 0o700); err != nil {
		return fmt.Errorf("creating data directory %s: %w", cfg.Data.Dir, err)
	}

	// After the data directory exists, so its symlinks resolve — on Kubernetes
	// the mount path is usually a link into the kubelet's tree, and a scope
	// built before the mkdir would compare the wrong string.
	fileScope, err := filescope.NewScope(cfg.Data.Dir, cfg.Files.AllowedPaths)
	if err != nil {
		return fmt.Errorf("files: %w", err)
	}
	nodes.Files = fileScope
	if len(cfg.Files.AllowedPaths) > 0 {
		log.Warn("the file nodes may reach outside the data directory",
			"allowedPaths", strings.Join(fileScope.Roots(), ","))
	}

	// The flow route table, with the editor and the admin API reserved so a flow
	// cannot claim a path that would make them unreachable.
	adminRoot := strings.TrimSuffix(cfg.Server.AdminRoot, "/")
	reserved := []string{
		adminRoot + "/health", adminRoot + "/ready", adminRoot + "/auth",
		adminRoot + "/settings", adminRoot + "/nodes", adminRoot + "/flows",
		adminRoot + "/runtime", adminRoot + "/inject", adminRoot + "/comms",
		adminRoot + "/deployments",
	}
	if cfg.Metrics.Enabled {
		reserved = append(reserved, adminRoot+cfg.Metrics.Path)
	}
	nodes.Routes = flowhttp.NewRouter(cfg.Server.HTTPRoot, reserved)

	flowStore := store.NewFlowStore(cfg.FlowPath())
	flowStore.SetBackupGenerations(cfg.Data.BackupGenerations)

	creds := store.NewCredentialStore(cfg.CredentialsPath(), cfg.Data.CredentialSecret)
	if err := creds.Load(); err != nil {
		return fmt.Errorf("loading credentials: %w", err)
	}
	if creds.MigratedFromLegacy() {
		// Worth saying out loud: the operator should know their credentials
		// arrived in Node-RED's format and are about to be re-encrypted.
		log.Warn("credentials were read in Node-RED's AES-256-CTR format and will be " +
			"re-encrypted with AES-256-GCM on the next deploy")
	}
	if !creds.HasSecret() {
		log.Warn("no credential secret is set; node credentials are stored in plaintext")
	}

	deployments, warnings, err := history.Open(cfg.HistoryDir(), cfg.History.Retain)
	if err != nil {
		return fmt.Errorf("opening the deployment log: %w", err)
	}
	for _, w := range warnings {
		log.Error("deployment log", "detail", w)
	}

	app := &application{
		cfg:       cfg,
		log:       log,
		flowStore: flowStore,
		creds:     creds,
		history:   deployments,
		registry:  node.Default,
		contexts:  store.NewScopedContexts(),
	}

	srv := api.New(api.Deps{
		Config:      cfg,
		Registry:    node.Default,
		Flows:       flowStore,
		Credentials: creds,
		History:     deployments,
		Logger:      log,
		Runtime:     app.currentRuntime,
		Deploy:      app.deploy,
		FlowRoutes:  nodes.Routes,
		Version:     version,
	})
	app.hub = srv.Hub()

	// Load and start whatever is on disk before opening the port, so the first
	// request never races the runtime coming up.
	flows, err := flowStore.Load()
	if err != nil {
		return fmt.Errorf("loading flows: %w", err)
	}
	if recovered, from := flowStore.Recovered(); recovered {
		log.Error("the flow file was unparseable and was recovered from a backup",
			"backup", from, "corruptFileKept", cfg.FlowPath()+".corrupt")
	}
	for _, w := range flows.Warnings {
		log.Warn("flow warning", "detail", w)
	}
	app.recordBaseline(flows.Rev)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Per-node failures are logged inside start and are not fatal: a flow with
	// one bad node still runs the other nodes, and refusing to boot over a
	// single typo would take a line down for no reason.
	app.start(ctx, flows)

	httpServer := &http.Server{
		Addr:         cfg.Addr(),
		Handler:      srv.Handler(),
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr(), "adminRoot", cfg.Server.AdminRoot)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		log.Info("shutting down")
	}

	// Stop accepting requests first, then stop the flows. The other order would
	// let a deploy land against a runtime that is halfway through stopping.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown did not complete cleanly", "error", err)
	}
	app.stop(shutdownCtx)
	log.Info("stopped")
	return nil
}

// ---------------------------------------------------------------------------
// application
// ---------------------------------------------------------------------------

// application owns the running runtime and the deploy cycle.
type application struct {
	cfg       config.Config
	log       *slog.Logger
	flowStore *store.FlowStore
	creds     *store.CredentialStore
	history   *history.Log
	registry  *node.Registry
	contexts  *store.ScopedContexts
	hub       interface{ Broadcast(runtime.Event) }

	mu      sync.Mutex
	rt      *runtime.Runtime
	pumpCtx context.CancelFunc

	// deployMu serialises deploys. Each one reads the revision it replaces,
	// writes two files, appends to the deployment log and swaps the runtime,
	// and two of those interleaved would record a parent that was never live.
	deployMu sync.Mutex
}

func (a *application) currentRuntime() *runtime.Runtime {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rt
}

func (a *application) runtimeOptions() runtime.Options {
	return runtime.Options{
		InboxCapacity: a.cfg.Runtime.InboxCapacity,
		Overflow:      runtime.OverflowPolicy(a.cfg.Runtime.Overflow),
		BlockTimeout:  a.cfg.Runtime.BlockTimeout,
		CloseTimeout:  a.cfg.Runtime.CloseTimeout,
	}
}

// start builds a runtime for a flow set and starts it, returning the per-node
// failures it tolerated. A non-empty result does not mean the start failed —
// one node with a bad config or an unknown type must not take the deploy down.
func (a *application) start(ctx context.Context, flows *engine.Flows) []runtime.StartError {
	rt := runtime.New(a.registry, flows, a.runtimeOptions())
	rt.SetContexts(a.contexts)
	rt.SetCredentials(func(nodeID string) map[string]string {
		return a.creds.Get(nodeID)
	})

	// Pump runtime events into the websocket hub before starting, so nothing
	// emitted during start-up is lost.
	pumpCtx, cancel := context.WithCancel(ctx)
	go a.pump(pumpCtx, rt)

	// Said before starting: an operator whose subflow declares an unreachable
	// input should find out at boot, not from a message that never arrives.
	for _, w := range rt.Warnings() {
		a.log.Warn("subflow warning", "detail", w)
	}

	failures := rt.Start(ctx)
	for _, f := range failures {
		a.log.Error("node failed to start", "node", f.NodeID, "type", f.Type, "error", f.Err)
	}

	a.mu.Lock()
	a.rt = rt
	a.pumpCtx = cancel
	a.mu.Unlock()

	a.log.Info("flows started",
		"nodes", len(flows.Nodes), "tabs", len(flows.Tabs),
		"subflowInstances", len(rt.Instances()), "failures", len(failures))
	return failures
}

// warnings returns the running runtime's subflow warnings, for the deploy
// response. The editor shows them next to the per-node failures.
func (a *application) warnings() []string {
	rt := a.currentRuntime()
	if rt == nil {
		return nil
	}
	return rt.Warnings()
}

// pump forwards runtime events to connected editors.
func (a *application) pump(ctx context.Context, rt *runtime.Runtime) {
	events := rt.Events()
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-events:
			if !ok {
				return
			}
			if a.hub != nil {
				a.hub.Broadcast(e)
			}
		}
	}
}

func (a *application) stop(ctx context.Context) {
	a.mu.Lock()
	rt, cancel := a.rt, a.pumpCtx
	a.rt = nil
	a.mu.Unlock()

	if rt != nil {
		for _, err := range rt.Stop(ctx) {
			a.log.Warn("error stopping a node", "error", err)
		}
	}
	if cancel != nil {
		cancel()
	}

	// Two process-wide registries outlive a runtime, because both resolve
	// across flows and cannot be rebuilt from the graph alone. Clearing them
	// here is what makes a redeploy see only the nodes that still exist.
	//
	// Without it, a deleted Link In stays registered and keeps the emitter of a
	// runtime that has stopped, so a Link Out in the new flow that still names
	// it delivers into a dead runner and the message goes nowhere quietly. The
	// same applies to a route left behind by an HTTP In node whose Close did
	// not run.
	nodes.Links.Reset()
	nodes.Routes.Reset()
	nodes.TCPReplies.Reset()
}

// recordBaseline writes what is on disk into the deployment log when the log
// doesn't already end with it: the first start with a log, or a flow file that
// was changed by hand while the process was down. Without it the first deploy
// would have nothing to diff against or roll back to, and a hand edit would
// quietly become history nobody recorded.
func (a *application) recordBaseline(rev string) {
	if a.history == nil {
		return
	}
	data := a.flowStore.Bytes()
	latest, ok := a.history.Latest()
	if ok && latest.Rev == rev {
		return
	}
	if !ok && data == nil {
		// A fresh volume. Nothing has ever run, so there is nothing to record.
		return
	}
	creds, err := a.creds.Snapshot()
	if err != nil {
		a.log.Error("could not snapshot credentials for the deployment log", "error", err)
	}
	note := "the flow file on disk at startup"
	if ok {
		note = "the flow file changed outside Flow since deployment " + strconv.FormatInt(latest.Seq, 10)
	}
	rec, err := a.history.Append(history.Record{
		Kind: history.KindBaseline, Rev: rev, ParentRev: latest.Rev,
		Note: note, Flows: data, Credentials: creds,
	})
	if err != nil {
		a.log.Error("could not record the startup flow file in the deployment log", "error", err)
		return
	}
	a.log.Info("recorded the startup flow file in the deployment log", "deployment", rec.Seq, "rev", rev)
}

// deploy replaces the running flows.
//
// The order matters and is not the obvious one. Credentials are split out and
// the flow file is written *before* the old runtime is stopped, so that a
// failure to persist leaves the previous flows running rather than taking the
// line down for a bad save.
//
// A full deploy stops everything and starts a fresh runtime. Anything else is a
// partial deploy: the running runtime restarts only the nodes that changed, and
// the MQTT sessions, listeners and queues of everything else carry on.
func (a *application) deploy(ctx context.Context, req api.DeployRequest) (api.DeployResult, error) {
	a.deployMu.Lock()
	defer a.deployMu.Unlock()

	flows, expectedRev, mode := req.Flows, req.ExpectedRev, req.Mode
	if mode == "" {
		mode = runtime.DeployFull
	}
	parentRev := a.flowStore.Rev()
	incoming := flows.StripCredentials()
	credsChanged := map[string]bool{}
	for id, c := range incoming {
		before := a.creds.Get(id)
		a.creds.Merge(id, c)
		if !maps.Equal(before, a.creds.Get(id)) {
			credsChanged[id] = true
		}
	}

	live := make(map[string]bool, len(flows.Nodes))
	for id := range flows.Nodes {
		live[id] = true
	}
	if removed := a.creds.Prune(live); removed > 0 {
		a.log.Info("pruned credentials for deleted nodes", "count", removed)
	}

	rev, err := a.flowStore.Save(flows, expectedRev)
	if err != nil {
		return api.DeployResult{}, err
	}
	if err := a.creds.Save(); err != nil {
		return api.DeployResult{}, fmt.Errorf("saving credentials: %w", err)
	}

	// Recorded after the save and before the swap. A log that can't be written
	// doesn't stop the deploy: the flow file is already saved, and refusing now
	// would leave the disk and the running flows disagreeing until the next
	// restart, which is worse than a missing record. It fails loudly instead,
	// in the log and in the deploy response.
	var warnings []string
	seq, err := a.record(history.Record{
		Kind: history.KindDeploy, Rev: rev, ParentRev: parentRev,
		User: req.User, Remote: req.Remote, Note: req.Note,
	})
	if err != nil {
		a.log.Error("the deployment log could not be written; this deploy has no record", "rev", rev, "error", err)
		warnings = append(warnings, "the deployment log could not be written, so this deploy has no record: "+err.Error())
	}

	// Drop context belonging to nodes and flows that no longer exist, so
	// redeploying repeatedly does not accumulate state for things that are gone.
	liveFlows := make(map[string]bool, len(flows.Tabs)+len(flows.Subflows))
	for id := range flows.Tabs {
		liveFlows[id] = true
	}
	for id := range flows.Subflows {
		liveFlows[id] = true
	}

	if rt := a.currentRuntime(); rt != nil && mode != runtime.DeployFull {
		up, err := rt.Update(ctx, flows, runtime.UpdateOptions{Mode: mode, Credentials: credsChanged})
		if err != nil {
			return api.DeployResult{}, err
		}
		for _, err := range up.CloseErrors {
			a.log.Warn("error stopping a node", "error", err)
		}
		for _, f := range up.Failures {
			a.log.Error("node failed to start", "node", f.NodeID, "type", f.Type, "error", f.Err)
		}
		a.contexts.Clean(live, liveFlows)

		a.log.Info("deployed", "rev", rev, "deployment", seq, "user", req.User, "type", string(mode),
			"started", len(up.Started), "restarted", len(up.Restarted),
			"stopped", len(up.Stopped), "unchanged", up.Unchanged, "failures", len(up.Failures))
		return api.DeployResult{
			Rev:        rev,
			Deployment: seq,
			Warnings:   append(append(append([]string(nil), flows.Warnings...), a.warnings()...), warnings...),
			Failures:   up.Failures,
			Update:     up,
		}, nil
	}

	a.stop(ctx)
	a.contexts.Clean(live, liveFlows)

	// Start against the background context, not the request's: the request is
	// about to complete and its cancellation must not tear down the flows.
	failures := a.start(context.Background(), flows)

	a.log.Info("deployed", "rev", rev, "deployment", seq, "user", req.User,
		"type", string(runtime.DeployFull), "failures", len(failures))
	return api.DeployResult{
		Rev:        rev,
		Deployment: seq,
		Warnings:   append(append(append([]string(nil), flows.Warnings...), a.warnings()...), warnings...),
		Failures:   failures,
		Update:     a.fullUpdate(),
	}, nil
}

// fullUpdate describes a full deploy in the same terms as a partial one: every
// node running now was started from scratch.
func (a *application) fullUpdate() runtime.UpdateResult {
	up := runtime.UpdateResult{Mode: runtime.DeployFull}
	if rt := a.currentRuntime(); rt != nil {
		up.Restarted = rt.RunningIDs()
	}
	return up
}

// record appends to the deployment log with the flow file and credentials as
// they now stand on disk.
func (a *application) record(r history.Record) (int64, error) {
	if a.history == nil {
		return 0, nil
	}
	creds, err := a.creds.Snapshot()
	if err != nil {
		return 0, fmt.Errorf("snapshotting credentials: %w", err)
	}
	r.Flows = a.flowStore.Bytes()
	r.Credentials = creds
	rec, err := a.history.Append(r)
	if err != nil {
		return 0, err
	}
	return rec.Seq, nil
}

// ---------------------------------------------------------------------------
// hash-password
// ---------------------------------------------------------------------------

func cmdHashPassword(args []string) error {
	fs := flag.NewFlagSet("hash-password", flag.ContinueOnError)
	pass := fs.String("password", "", "password to hash (omit to read from HOTLOOP_FLOW_PASSWORD)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	plain := *pass
	if plain == "" {
		plain = os.Getenv("HOTLOOP_FLOW_PASSWORD")
	}
	if plain == "" {
		return errors.New("no password given: pass -password, or set HOTLOOP_FLOW_PASSWORD")
	}

	hash, err := config.HashPassword(plain)
	if err != nil {
		return err
	}
	fmt.Println(hash)
	return nil
}

// ---------------------------------------------------------------------------
// import
// ---------------------------------------------------------------------------

// cmdImport reports what would happen to a Node-RED flow file before anyone
// deploys it, which is the difference between finding out now and finding out
// when a line stops.
func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: hotloop-flow import <flows.json>")
	}

	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	flows, err := engine.ParseFlows(data)
	if err != nil {
		return err
	}

	// Counted against the expanded graph rather than the file, so a subflow
	// whose internals need a node this build does not have is reported. Counting
	// the file would call an instance "supported" and say nothing about what is
	// inside it, which is exactly the report an operator would act on and then
	// find out the hard way.
	expansion := engine.ExpandSubflows(flows)
	graph := expansion.Flows

	supported := map[string]int{}
	unsupported := map[string]int{}
	partial := map[string]string{}

	for _, id := range graph.Order {
		n, ok := graph.Nodes[id]
		if !ok {
			continue
		}
		if _, isInstance := n.SubflowTemplateID(); isInstance {
			supported["subflow instance"]++
			continue
		}
		reg, known := node.Default.Lookup(n.Type)
		if !known {
			unsupported[n.Type]++
			continue
		}
		supported[n.Type]++
		if reg.Descriptor.Compatibility.Level != node.CompatFull {
			partial[n.Type] = reg.Descriptor.Compatibility.Notes
		}
	}

	fmt.Printf("%s\n\n", fs.Arg(0))
	fmt.Printf("  %d entries: %d nodes, %d tabs, %d subflows, %d groups\n",
		len(flows.Order), len(flows.Nodes), len(flows.Tabs), len(flows.Subflows), len(flows.Groups))
	if n := len(expansion.Instances); n > 0 {
		fmt.Printf("  %d subflow instance(s), expanding to %d nodes in total\n",
			n, len(graph.Nodes))
	}
	fmt.Println()

	warnings := append(append([]string(nil), flows.Warnings...), expansion.Warnings...)
	if len(warnings) > 0 {
		fmt.Printf("Warnings\n")
		for _, w := range warnings {
			fmt.Printf("  - %s\n", w)
		}
		fmt.Println()
	}

	fmt.Printf("Supported node types\n")
	for _, t := range sortedCounts(supported) {
		fmt.Printf("  %-32s %d\n", t.name, t.count)
	}
	fmt.Println()

	if len(partial) > 0 {
		fmt.Printf("Partially supported — read these before deploying\n")
		for t, notes := range partial {
			fmt.Printf("  %s\n      %s\n", t, notes)
		}
		fmt.Println()
	}

	if len(unsupported) > 0 {
		fmt.Printf("NOT supported — these nodes will not start\n")
		for _, t := range sortedCounts(unsupported) {
			fmt.Printf("  %-32s %d\n", t.name, t.count)
		}
		fmt.Printf("\nThe rest of the flow still runs. See docs/compatibility.md.\n")
		return nil
	}

	fmt.Printf("Every node type in this flow is implemented.\n")
	return nil
}

type nameCount struct {
	name  string
	count int
}

func sortedCounts(m map[string]int) []nameCount {
	out := make([]nameCount, 0, len(m))
	for k, v := range m {
		out = append(out, nameCount{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].name < out[j].name
	})
	return out
}

// ---------------------------------------------------------------------------
// logging
// ---------------------------------------------------------------------------

func newLogger(cfg config.Logging) *slog.Logger {
	var level slog.Level
	switch cfg.Level {
	case "error":
		level = slog.LevelError
	case "warn":
		level = slog.LevelWarn
	case "debug", "trace":
		level = slog.LevelDebug
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if cfg.Format == "json" {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
}
