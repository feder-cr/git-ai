package gitcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/feder-cr/git-ai/internal/cgroups"
	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/alternates"
	"github.com/feder-cr/git-ai/internal/git/mvcc"
	"github.com/feder-cr/git-ai/internal/git/trace2"
	"github.com/feder-cr/git-ai/internal/git/trace2hooks"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/tracing"
	"github.com/prometheus/client_golang/prometheus"
	"gitlab.com/gitlab-org/labkit/correlation"
	"golang.org/x/time/rate"
)

const (
	// BigFileThresholdMB is the threshold we configure via `core.bigFileThreshold` and determines the maximum size
	// after which Git considers files to be big. Please refer to `GlobalConfiguration()` for more details.
	BigFileThresholdMB = 50
	// maxTrace2EventPerSecond is the maximum number of events that can be processed per second
	maxTrace2EventPerSecond = 40
	// Rate limiter is immediately allocated the maxBurstToken value. Burst is the maximum number of tokens
	// that can be consumed in a single call
	maxBurstToken = 40

	// FsyncComponents are the components git makes durable on every write (`core.fsync`).
	FsyncComponents = "objects,derived-metadata,reference"
	// FsyncComponentsWithoutReferences is FsyncComponents without the references, for the
	// reference writes whose durability the storage core makes itself
	// (updateref.WithDeferredReferenceDurability).
	FsyncComponentsWithoutReferences = "objects,derived-metadata"
	// ReftableLockTimeoutMilliseconds is how long git waits for the reference lock of a reftable
	// repository (`reftable.lockTimeout`).
	ReftableLockTimeoutMilliseconds = 10000
)

// CommandFactory is designed to create and run git commands in a protected and fully managed manner.
type CommandFactory interface {
	// New creates a new command for the repo repository.
	New(ctx context.Context, repo storage.Repository, sc Command, opts ...CmdOpt) (*command.Command, error)
	// NewWithoutRepo creates a command without a target repository.
	NewWithoutRepo(ctx context.Context, sc Command, opts ...CmdOpt) (*command.Command, error)
	// GetExecutionEnvironment returns parameters required to execute Git commands.
	GetExecutionEnvironment(context.Context) ExecutionEnvironment
	// HooksPath returns the path where Gitaly's Git hooks reside.
	HooksPath(context.Context) string
	// GitVersion returns the Git version used by the command factory.
	GitVersion(context.Context) (string, error)
}

type execCommandFactoryConfig struct {
	hooksPath           string
	gitBinaryPath       string
	cgroupsManager      cgroups.Manager
	trace2Hooks         []trace2.Hook
	traceRateLimiter    *rate.Limiter
	execEnvConstructors []ExecutionEnvironmentConstructor
}

// ExecCommandFactoryOption is an option that can be passed to NewExecCommandFactory.
type ExecCommandFactoryOption func(*execCommandFactoryConfig)

// WithSkipHooks will skip any use of hooks in this command factory.
func WithSkipHooks() ExecCommandFactoryOption {
	return func(cfg *execCommandFactoryConfig) {
		cfg.hooksPath = "/var/empty"
	}
}

// WithHooksPath will override the path where hooks are to be found.
func WithHooksPath(hooksPath string) ExecCommandFactoryOption {
	return func(cfg *execCommandFactoryConfig) {
		cfg.hooksPath = hooksPath
	}
}

// WithGitBinaryPath overrides the path to the Git binary that shall be executed.
func WithGitBinaryPath(path string) ExecCommandFactoryOption {
	return func(cfg *execCommandFactoryConfig) {
		cfg.gitBinaryPath = path
	}
}

// WithCgroupsManager overrides the Cgroups manager used by the command factory.
func WithCgroupsManager(cgroupsManager cgroups.Manager) ExecCommandFactoryOption {
	return func(cfg *execCommandFactoryConfig) {
		cfg.cgroupsManager = cgroupsManager
	}
}

// WithTrace2Hooks overrides default trace2 hooks used by trace2 manager
func WithTrace2Hooks(hooks []trace2.Hook) ExecCommandFactoryOption {
	return func(cfg *execCommandFactoryConfig) {
		cfg.trace2Hooks = hooks
	}
}

// DefaultTrace2HooksFor creates a list of all Trace2 hooks. It doesn't mean all hooks are triggered.
// Each hook's activation status will be evaluated before the command starts.
func DefaultTrace2HooksFor(ctx context.Context, subCmd string, logger log.Logger, rl *rate.Limiter) []trace2.Hook {
	var hooks []trace2.Hook
	if tracing.IsSampled(ctx) {
		hooks = append(hooks, trace2hooks.NewTracingExporter())
	}
	if subCmd == "pack-objects" {
		hooks = append(hooks, trace2hooks.NewPackObjectsMetrics())
	}
	if subCmd == "receive-pack" && featureflag.ReceivePackTrace2Hook.IsEnabled(ctx) {
		hooks = append(hooks, trace2hooks.NewReceivePack())
	}
	if featureflag.LogGitTraces.IsEnabled(ctx) {
		hooks = append(hooks, trace2hooks.NewLogExporter(rl, logger))
	}
	return hooks
}

// WithExecutionEnvironmentConstructors overrides the default Git execution environments used by the
// command factory.
func WithExecutionEnvironmentConstructors(constructors ...ExecutionEnvironmentConstructor) ExecCommandFactoryOption {
	return func(cfg *execCommandFactoryConfig) {
		cfg.execEnvConstructors = constructors
	}
}

type hookDirectories struct {
	tempHooksPath string
}

type cachedGitVersion struct {
	version string
	stat    os.FileInfo
}

// ExecCommandFactory knows how to properly construct different types of commands.
type ExecCommandFactory struct {
	locator               storage.Locator
	cfg                   config.Cfg
	execEnvs              []ExecutionEnvironment
	logger                log.Logger
	cgroupsManager        cgroups.Manager
	trace2Hooks           []trace2.Hook
	traceRateLimiter      *rate.Limiter
	invalidCommandsMetric *prometheus.CounterVec
	hookDirs              hookDirectories

	cachedGitVersionLock     sync.RWMutex
	cachedGitVersionByBinary map[string]cachedGitVersion
}

// NewExecCommandFactory returns a new instance of initialized ExecCommandFactory. The returned
// cleanup function shall be executed when the server shuts down.
func NewExecCommandFactory(cfg config.Cfg, logger log.Logger, opts ...ExecCommandFactoryOption) (_ *ExecCommandFactory, _ func(), returnedErr error) {
	var factoryCfg execCommandFactoryConfig
	for _, opt := range opts {
		opt(&factoryCfg)
	}

	var cleanups []func()
	runCleanups := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}

	defer func() {
		if returnedErr != nil {
			runCleanups()
		}
	}()

	hookDirectories, cleanup, err := setupHookDirectories(cfg, factoryCfg, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("setting up hooks: %w", err)
	}
	cleanups = append(cleanups, cleanup)

	execEnvs, cleanup, err := setupGitExecutionEnvironments(cfg, factoryCfg, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("setting up Git execution environment: %w", err)
	}
	cleanups = append(cleanups, cleanup)

	cgroupsManager := factoryCfg.cgroupsManager
	if cgroupsManager == nil {
		cgroupsManager = cgroups.NewManager(cfg.Cgroups, logger, os.Getpid())
	}

	traceRateLimiter := factoryCfg.traceRateLimiter
	if traceRateLimiter == nil {
		traceRateLimiter = rate.NewLimiter(maxTrace2EventPerSecond, maxBurstToken)
	}

	gitCmdFactory := &ExecCommandFactory{
		cfg:              cfg,
		execEnvs:         execEnvs,
		logger:           logger,
		locator:          config.NewLocator(cfg),
		cgroupsManager:   cgroupsManager,
		trace2Hooks:      factoryCfg.trace2Hooks,
		traceRateLimiter: traceRateLimiter,
		invalidCommandsMetric: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gitaly_invalid_commands_total",
				Help: "Total number of invalid arguments tried to execute",
			},
			[]string{"command"},
		),
		hookDirs:                 hookDirectories,
		cachedGitVersionByBinary: make(map[string]cachedGitVersion),
	}

	return gitCmdFactory, runCleanups, nil
}

// setupGitExecutionEnvironments assembles a Git execution environment that can be used to run Git
// commands. It warns if no path was specified in the configuration.
func setupGitExecutionEnvironments(cfg config.Cfg, factoryCfg execCommandFactoryConfig, logger log.Logger) ([]ExecutionEnvironment, func(), error) {
	sharedEnvironment := []string{
		// Force English locale for consistency on output messages and to help us debug in
		// case we get bug reports from customers whose system-locale would be different.
		"LANG=en_US.UTF-8",
		// Ask Git to never prompt us for any information like e.g. credentials.
		"GIT_TERMINAL_PROMPT=0",
		// Prevent the environment from affecting git calls by ignoring the configuration files.
		// This should be done always but we have to wait until 15.0 due to backwards compatibility
		// concerns.
		//
		// See https://gitlab.com/gitlab-org/gitaly/-/issues/3617.
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"XDG_CONFIG_HOME=/dev/null",
	}

	if factoryCfg.gitBinaryPath != "" {
		return []ExecutionEnvironment{
			{BinaryPath: factoryCfg.gitBinaryPath, EnvironmentVariables: sharedEnvironment},
		}, func() {}, nil
	}

	constructors := factoryCfg.execEnvConstructors
	if factoryCfg.execEnvConstructors == nil {
		constructors = defaultExecutionEnvironmentConstructors
	}

	var execEnvs []ExecutionEnvironment
	for _, constructor := range constructors {
		execEnv, err := constructor.Construct(cfg)
		if err != nil {
			// In case the environment has not been configured by the user we simply
			// skip it.
			if errors.Is(err, ErrNotConfigured) {
				continue
			}

			// But if it has been configured and we fail to set it up then it signifies
			// a real error.
			return nil, nil, fmt.Errorf("constructing Git environment: %w", err)
		}

		execEnv.EnvironmentVariables = append(execEnv.EnvironmentVariables, sharedEnvironment...)

		execEnvs = append(execEnvs, execEnv)
	}

	if len(execEnvs) == 0 {
		execEnv, err := FallbackGitEnvironmentConstructor{}.Construct(cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("could not set up any Git execution environments")
		}
		execEnv.EnvironmentVariables = append(execEnv.EnvironmentVariables, sharedEnvironment...)

		logger.WithFields(log.Fields{
			"resolvedPath": execEnv.BinaryPath,
		}).Warn("Git has not been properly configured, falling back to Git found on PATH")

		execEnvs = append(execEnvs, execEnv)
	}

	return execEnvs, func() {
		for _, execEnv := range execEnvs {
			if err := execEnv.Cleanup(); err != nil {
				logger.WithError(err).Error("execution environment cleanup failed")
			}
		}
	}, nil
}

// Describe is used to describe Prometheus metrics.
func (cf *ExecCommandFactory) Describe(descs chan<- *prometheus.Desc) {
	prometheus.DescribeByCollect(cf, descs)
}

// Collect is used to collect Prometheus metrics.
func (cf *ExecCommandFactory) Collect(metrics chan<- prometheus.Metric) {
	cf.invalidCommandsMetric.Collect(metrics)
}

// New creates a new command for the repo repository.
func (cf *ExecCommandFactory) New(ctx context.Context, repo storage.Repository, sc Command, opts ...CmdOpt) (*command.Command, error) {
	return cf.newCommand(ctx, repo, sc, opts...)
}

// NewWithoutRepo creates a command without a target repository.
func (cf *ExecCommandFactory) NewWithoutRepo(ctx context.Context, sc Command, opts ...CmdOpt) (*command.Command, error) {
	return cf.newCommand(ctx, nil, sc, opts...)
}

// GetExecutionEnvironment returns parameters required to execute Git commands.
func (cf *ExecCommandFactory) GetExecutionEnvironment(ctx context.Context) ExecutionEnvironment {
	// We first go through all execution environments and check whether any of them is enabled
	// in the current context, which most importantly will check their respective feature flags.
	for _, execEnv := range cf.execEnvs {
		if execEnv.IsEnabled(ctx) {
			return execEnv
		}
	}

	// If none is enabled though, we simply use the first execution environment, which is also
	// the one with the highest priority. This can for example happen in case we only were able to
	// construct a single execution environment that is currently feature flagged.
	return cf.execEnvs[0]
}

// HooksPath returns the path where Gitaly's Git hooks reside.
func (cf *ExecCommandFactory) HooksPath(ctx context.Context) string {
	return cf.hookDirs.tempHooksPath
}

func setupHookDirectories(cfg config.Cfg, factoryCfg execCommandFactoryConfig, logger log.Logger) (hookDirectories, func(), error) {
	if factoryCfg.hooksPath != "" {
		return hookDirectories{
			tempHooksPath: factoryCfg.hooksPath,
		}, func() {}, nil
	}

	if cfg.BinDir == "" {
		return hookDirectories{}, nil, errors.New("binary directory required to set up hooks")
	}

	// This sets up the new hook location. Hooks now live in a temporary directory, where all
	// hooks are symlinks to the `gitaly-hooks` binary.
	tempHooksPath, err := os.MkdirTemp(cfg.RuntimeDir, "hooks-*.d")
	if err != nil {
		return hookDirectories{}, nil, fmt.Errorf("creating temporary hooks directory: %w", err)
	}

	// And now we symlink all required hooks to the wrapper script.
	for _, hook := range []string{"pre-receive", "post-receive", "update", "reference-transaction"} {
		if err := os.Symlink(cfg.BinaryPath("gitaly-hooks"), filepath.Join(tempHooksPath, hook)); err != nil {
			return hookDirectories{}, nil, fmt.Errorf("creating symlink for %s hook: %w", hook, err)
		}
	}

	return hookDirectories{tempHooksPath: tempHooksPath}, func() {
		if err := os.RemoveAll(tempHooksPath); err != nil {
			logger.WithError(err).Error("cleaning up temporary hooks path")
		}
	}, nil
}

func statDiffers(a, b os.FileInfo) bool {
	return a.Size() != b.Size() || a.ModTime() != b.ModTime() || a.Mode() != b.Mode()
}

// GitVersion returns the Git version in use. The version is cached as long as the binary remains
// unchanged as determined by stat(3P).
func (cf *ExecCommandFactory) GitVersion(ctx context.Context) (string, error) {
	gitBinary := cf.GetExecutionEnvironment(ctx).BinaryPath

	stat, err := os.Stat(gitBinary)
	if err != nil {
		return "", fmt.Errorf("cannot stat Git binary: %w", err)
	}

	cf.cachedGitVersionLock.RLock()
	cachedVersion, upToDate := cf.cachedGitVersionByBinary[gitBinary]
	if upToDate {
		upToDate = !statDiffers(stat, cachedVersion.stat)
	}
	cf.cachedGitVersionLock.RUnlock()

	if upToDate {
		return cachedVersion.version, nil
	}

	cf.cachedGitVersionLock.Lock()
	defer cf.cachedGitVersionLock.Unlock()

	execEnv := cf.GetExecutionEnvironment(ctx)

	// We cannot reuse the stat(3P) information from above given that it wasn't acquired under
	// the write-lock. As such, it may have been invalidated by a concurrent thread which has
	// already updated the Git version information.
	stat, err = os.Stat(execEnv.BinaryPath)
	if err != nil {
		return "", fmt.Errorf("cannot stat Git binary: %w", err)
	}

	// There is a race here: if the Git executable has changed between calling stat(3P) on the
	// binary and executing it, then we may report the wrong Git version. This race is inherent
	// though: it can also happen after `GitVersion()` was called, so it doesn't really help to
	// retry version detection here. Instead, we just live with this raciness -- the next call
	// to `GitVersion()` would detect the version being out-of-date anyway and thus correct it.
	//
	// Furthermore, note that we're not using `newCommand()` but instead hand-craft the command.
	// This is required to avoid a cyclic dependency when we need to check the version in
	// `newCommand()` itself.
	var versionBuffer bytes.Buffer
	cmd, err := command.New(ctx, cf.logger, []string{execEnv.BinaryPath, "version"},
		command.WithEnvironment(execEnv.EnvironmentVariables),
		command.WithStdout(&versionBuffer),
	)
	if err != nil {
		return "", fmt.Errorf("spawning version command: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("waiting for version: %w", err)
	}

	version := strings.TrimSpace(versionBuffer.String())
	if suffix, ok := strings.CutPrefix(version, "git version "); ok {
		version = suffix
	}

	cf.cachedGitVersionByBinary[gitBinary] = cachedGitVersion{
		version: version,
		stat:    stat,
	}

	return version, nil
}

// newCommand creates a new command.Command for the given git command. If a repo is given, then the
// command will be run in the context of that repository. Note that this sets up arguments and
// environment variables for git, but doesn't run in the directory itself. If a directory
// is given, then the command will be run in that directory.
func (cf *ExecCommandFactory) newCommand(ctx context.Context, repo storage.Repository, sc Command, opts ...CmdOpt) (_ *command.Command, returnedErr error) {
	// In Git change 2386535511, we introduced a feature, "attr: read attributes from HEAD when bare repo".
	// This causes a performance degradation. Current workaround is to get rid of the default behavior
	// in bare repos of reading from HEAD by setting attr.tree to empty tree.
	// See https://lore.kernel.org/git/xmqqzft6aozg.fsf_-_@gitster.g/ and
	// https://gitlab.com/gitlab-org/gitaly/-/issues/6064 for details.
	//
	// For gitaly, there are commands that specifically need attr.tree to be set to HEAD. We have a white list
	// of commands who should be excluded, see AttrTreeConfig. Those commands in the white list
	// need to set "attr.tree" to HEAD
	//
	// This can be removed once https://gitlab.com/gitlab-org/git/-/issues/316 is implemented and put in git upstream

	attrTreeConfig, err := cf.AttrTreeConfig(ctx, repo, sc, opts...)
	if err != nil {
		return nil, err
	}
	if attrTreeConfig != nil {
		opts = append(opts, WithConfig(*attrTreeConfig))
	}

	// For new repositories being created, we want to ensure that the
	// right reference backend is used.
	//
	// Make sure we don't override the env variable, if set.
	_, refFormatSet := os.LookupEnv("GIT_DEFAULT_REF_FORMAT")
	if (sc.Name == "clone" || sc.Name == "init") && !refFormatSet {
		// The ref-format has to match the backend on the context. That backend also
		// selects the binary, and only the MVCC binary can read an MVCC repository.
		backend := git.ReferenceBackendFiles
		if ctxBackend, ok := referenceBackendFromContext(ctx); ok && ctxBackend.Name != "" {
			backend = ctxBackend
		}

		sc.Flags = append(sc.Flags, Flag{Name: fmt.Sprintf("--ref-format=%s", backend.Name)})
	}

	// An agent's walk of git's pseudo references leaves out the references agents do not see.
	walk := agentRefWalk(ctx, sc)
	if walk {
		sc = excludeHiddenRefs(sc)
	}

	config, err := cf.combineOpts(ctx, sc, opts)
	if err != nil {
		return nil, err
	}
	if walk {
		config.globals = append(config.globals, agentWalkConfig()...)
		if readsRevisionsFromStdin(sc) {
			config.commandOpts = append(config.commandOpts, command.WithStdinFilter(excludeHiddenRefsOnStdin))
		}
	}

	cmdGitVersion, err := cf.GitVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting Git version: %w", err)
	}

	args, err := cf.combineArgs(ctx, sc, config)
	if err != nil {
		return nil, err
	}

	env := config.env

	// If the repository uses an MVCC reference backend then wait for the
	// cache to be warmed and inject the environment variables in the command.
	mvccCache := mvcc.CacheFromContext(ctx)
	if mvccCache != nil {
		err = mvccCache.WaitUntilReady(ctx)
		if err != nil {
			return nil, err
		}

		for _, envVar := range mvccCache.Environment() {
			env = append(env, envVar.String())
		}
	}

	var repoPath string
	if repo != nil {
		var err error
		repoPath, err = cf.locator.GetRepoPath(ctx, repo)
		if err != nil {
			return nil, err
		}

		env = append(alternates.Env(repoPath, repo.GetGitObjectDirectory(), repo.GetGitAlternateObjectDirectories()), env...)
	}

	if config.worktreePath != "" {
		args = append([]string{"-C", config.worktreePath}, args...)
	} else if repoPath != "" {
		args = append([]string{"--git-dir", repoPath}, args...)
	}

	execEnv := cf.GetExecutionEnvironment(ctx)

	env = append(env, execEnv.EnvironmentVariables...)

	var cgroupsAddCommandOpts []cgroups.AddCommandOption
	if repo != nil {
		cgroupRepo := repo
		if tx := storage.ExtractTransaction(ctx); tx != nil {
			cgroupRepo = tx.OriginalRepository(repo)
		}
		cgroupsAddCommandOpts = []cgroups.AddCommandOption{
			cgroups.WithCgroupKey(cgroups.RepositoryKey(cgroupRepo)),
		}
	}

	commandOpts := config.commandOpts

	trace2Hooks := cf.trace2Hooks
	if trace2Hooks == nil {
		trace2Hooks = DefaultTrace2HooksFor(ctx, sc.Name, cf.logger, cf.traceRateLimiter)
	}
	if len(trace2Hooks) != 0 {
		trace2Manager, err := trace2.NewManager(correlation.ExtractFromContextOrGenerate(ctx), trace2Hooks)
		if err != nil {
			return nil, fmt.Errorf("creating trace2 manager: %w", err)
		}

		env = trace2Manager.Inject(env)
		commandOpts = append(commandOpts, command.WithFinalizer(cf.trace2Finalizer(trace2Manager)))
	}

	commandOpts = append(
		commandOpts,
		command.WithEnvironment(env),
		command.WithCommandName("git", sc.Name),
		command.WithCgroup(cf.cgroupsManager, cgroupsAddCommandOpts...),
		command.WithCommandGitVersion(cmdGitVersion),
		command.WithSubprocessLogger(cf.cfg.Logging.Config),
	)

	// The invocation lives as long as the process: the exit hook ends it however the process
	// ends, and a process that did not start (New failed or panicked) ends it here. Ending an
	// invocation twice does nothing.
	endInvocation := BeginInvocation(config.invocation)
	started := false
	defer func() {
		if !started {
			endInvocation()
		}
	}()
	commandOpts = append(commandOpts, command.WithExitHook(endInvocation))

	command, err := command.New(ctx, cf.logger, append([]string{execEnv.BinaryPath}, args...), commandOpts...)
	if err != nil {
		return nil, err
	}
	started = true

	return command, nil
}

func (cf *ExecCommandFactory) combineOpts(ctx context.Context, sc Command, opts []CmdOpt) (cmdCfg, error) {
	var config cmdCfg

	commandDescription, ok := commandDescriptions[sc.Name]
	if !ok {
		return cmdCfg{}, fmt.Errorf("invalid sub command name %q: %w", sc.Name, ErrInvalidArg)
	}

	for _, opt := range opts {
		if err := opt(ctx, cf.cfg, cf, &config); err != nil {
			return cmdCfg{}, err
		}
	}

	if !config.hooksConfigured && commandDescription.mayUpdateRef() {
		return cmdCfg{}, fmt.Errorf("subcommand %q: %w", sc.Name, ErrHookPayloadRequired)
	}

	return config, nil
}

func (cf *ExecCommandFactory) combineArgs(ctx context.Context, sc Command, cc cmdCfg) (_ []string, err error) {
	var args []string

	defer func() {
		if err != nil && IsInvalidArgErr(err) && len(args) > 0 {
			cf.invalidCommandsMetric.WithLabelValues(sc.Name).Inc()
		}
	}()

	commandDescription, ok := commandDescriptions[sc.Name]
	if !ok {
		return nil, fmt.Errorf("invalid sub command name %q: %w", sc.Name, ErrInvalidArg)
	}

	globalConfig, err := cf.GlobalConfiguration(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting global Git configuration: %w", err)
	}

	var commandOpts []GlobalOption
	if commandDescription.opts != nil {
		commandOpts = commandDescription.opts(ctx)
	}

	combinedGlobals := make([]GlobalOption, 0, len(globalConfig)+len(commandOpts)+len(cc.globals)+len(cf.cfg.Git.Config))
	for _, configPair := range globalConfig {
		combinedGlobals = append(combinedGlobals, configPair)
	}
	combinedGlobals = append(combinedGlobals, commandOpts...)
	combinedGlobals = append(combinedGlobals, cc.globals...)
	for _, configPair := range cf.cfg.Git.Config {
		combinedGlobals = append(combinedGlobals, configPair)
	}

	for _, global := range combinedGlobals {
		globalArgs, err := global.GlobalArgs()
		if err != nil {
			return nil, err
		}
		args = append(args, globalArgs...)
	}

	scArgs, err := sc.CommandArgs()
	if err != nil {
		return nil, err
	}

	return append(args, scArgs...), nil
}

// GlobalConfiguration returns the global Git configuration that should be applied to every Git
// command.
func (cf *ExecCommandFactory) GlobalConfiguration(ctx context.Context) ([]ConfigPair, error) {
	// As global options may cancel out each other, we have a clearly defined order in which
	// globals get applied. The order is similar to how git handles configuration options from
	// most general to most specific. This allows callsites to override options which would
	// otherwise be set up automatically. The exception to this is configuration specified by
	// the admin, which always overrides all other items. The following order of precedence
	// applies:
	//
	// 1. Globals which get set up by default for all git commands.
	// 2. Globals which get set up by default for a given git command.
	// 3. Globals passed via command options, e.g. as set up by
	//    `WithReftxHook()`.
	// 4. Configuration as provided by the admin in Gitaly's config.toml.
	config := []ConfigPair{
		// Disable automatic garbage collection as we handle scheduling
		// of it ourselves.
		{Key: "gc.auto", Value: "0"},

		// Disable automatic maintenance as we never enable any tasks.
		{Key: "maintenance.auto", Value: "0"},

		// CRLF line endings will get replaced with LF line endings when writing blobs to the
		// object database. No conversion is done when reading blobs from the object database.
		// This is required for the web editor. With feature flag "autocrlf_false" enabled
		// CRLF line endings will not get replaced and be left alone.
		{Key: "core.autocrlf", Value: "false"},

		// Git allows the use of replace refs, where a given object ID can be replaced with a
		// different one. The result is that Git commands would use the new object instead of the
		// old one in almost all contexts. This is a security threat: an adversary may use this
		// mechanism to replace malicious commits with seemingly benign ones. We thus globally
		// disable this mechanism.
		{Key: "core.useReplaceRefs", Value: "false"},

		// Change the size of files we consider to be big from 512MB to 50MB. This setting influences a bunch of
		// things for blobs that are larger than this size:
		//
		// - They will not be slurped into memory anymore, but will instead use streaming interfaces. This
		//   should reduce memory consumption as we don't have to allocate up to 512MB buffers anymore.
		//
		// - They will not be diffed anymore. This should significantly reduce the time it
		//   takes to computes diffs when such diffs contain huge blobs. This is of course at the cost of not
		//   being able to show any such diffs anymore, but overall it seems unreasonable to compute diffs for
		//   any such huge files anyway.
		//
		// - They will not be deltified anymore. This should ultimately be a no-op for us as we have already
		//   been setting `pack.windowSize=100m` already, which restricts the maximum window size. The value of
		//   50MB has thus been chosen such that it matches 2 times the window size.
		//
		// So ultimately, this should not lead to larger packfiles as we have already been restricting the
		// packfile window anyway while it should on the other hand lead to lower memory consumption and faster
		// computation of diffs when large blobs are involved.
		{Key: "core.bigFileThreshold", Value: fmt.Sprintf("%dm", BigFileThresholdMB)},

		// When generating packfiles, Git tries to reuse parts of the packfile verbatim so that it does not have
		// to recompute deltas. The goal of this is to speed up the generation of the packfile while reducing
		// computational resources required. This optimization only works for the preferred packfile though,
		// which typically is the largest packfile in the repository. Especially larger repositories may have a
		// significant amount of objects stored in different packfiles, so none of those objects would be
		// reused.
		//
		// Git has recently introduced multi-pack reuse of objects to address this inefficiency and make reuse
		// of objects work across multiple packfiles as long as those objects are mapped by a multi-pack index
		// with an accompanying bitmap. As this is a recent addition to Git this feature is not enabled by
		// default yet, but needs to be opted in with a configuration.
		{Key: "pack.allowPackReuse", Value: "multi"},

		// Force MIDX v1 during the transition period where GIT_VERSION_PREV writes
		// version 1 while GIT_VERSION_MASTER and nightly builds write version 2
		// (upstream 105a22cf69, "Merge branch 'tb/incremental-midx-part-3.2'", 2026-03-25).
		// Once GIT_VERSION_PREV is bumped past that commit, this config can be removed
		// and the test updated to expect version 2.
		{Key: "midx.version", Value: "1"},
	}

	config = append(config,
		// We configure for what data should be fsynced and how that should happen.
		// Synchronize object files, packed-refs and loose refs to disk to lessen the
		// likelihood of repository corruption in case the server crashes.
		ConfigPair{Key: "core.fsync", Value: FsyncComponents},
		ConfigPair{Key: "core.fsyncMethod", Value: "fsync"},

		// When deleting references, Git needs to rewrite the `packed-refs` file to evict
		// the reference from it. In order to not race with concurrent writers it thus needs
		// to lock the file for concurrent access. This lock is thus a shared resource, and
		// in high-activity repositories we see a lot of contention around this lock: for
		// once because we typically have many writes there, but second because these repos
		// tend to have many references and thus rewriting the `packed-refs` file takes
		// proportionally longer.
		//
		// Git has a default timeout of 1 second to try and lock the file. In practice
		// though we see that this is not sufficient, and especially the `DeleteRefs` RPC is
		// erroring out very frequently. We thus increase the timeout to 10 seconds. While
		// comparatively high, context cancellation would still cause us to exit early in
		// case the caller doesn't want to wait this long.
		ConfigPair{Key: "core.packedRefsTimeout", Value: "10000"},
		// The reftable backend takes a single lock for the whole repository on every
		// reference transaction, so it is a shared resource like the `packed-refs` lock
		// above and gets the same timeout instead of Git's default of 100 milliseconds.
		// Context cancellation still ends the wait early.
		ConfigPair{Key: "reftable.lockTimeout", Value: fmt.Sprint(ReftableLockTimeoutMilliseconds)},
		// Similarly, for loose references we bump the limit from 100 milliseconds to 1 second. We aim for a
		// lower limit here as the locking for loose references is typically a lot more fine-grained. We have
		// still observed lock contention around them though, but mostly in cases where the host system was
		// heavily loaded by a storm of incoming RPCs.
		ConfigPair{Key: "core.filesRefLockTimeout", Value: "1000"},
	)

	return config, nil
}

func (cf *ExecCommandFactory) trace2Finalizer(manager *trace2.Manager) func(context.Context, *command.Command) {
	return func(ctx context.Context, cmd *command.Command) {
		manager.Finish(ctx)
		customFields := log.CustomFieldsFromContext(ctx)
		if customFields != nil {
			customFields.RecordMetadata("trace2.activated", "true")
			customFields.RecordMetadata("trace2.hooks", strings.Join(manager.HookNames(), ","))
			if manager.Error() != nil {
				customFields.RecordMetadata("trace2.error", manager.Error().Error())
			}
		}
	}
}

// AttrTreeConfig adds `attr.tree = HEAD` to the commands in the whiteList have,
// others has `attr.tree = emptyTreeHash`.
//
// The empty tree depends on the object format of the repository, which is read from the
// configuration file of its common git directory, as git reads it, without starting a git process.
// An error means that the object format cannot be determined: the command fails with it, because
// running it without attr.tree would make git read the attributes from HEAD, the slow path that
// attr.tree exists to avoid.
func (cf *ExecCommandFactory) AttrTreeConfig(ctx context.Context, repo storage.Repository, sc Command, opts ...CmdOpt) (*ConfigPair, error) {
	repoPath := cf.findRepoPath(ctx, repo, sc, opts...)
	if len(repoPath) == 0 {
		// Add tree config only when repo exists, because some git command, e.g. diff
		// can exec without a repo, If we add an attr.tree when a command is executing
		// without a repo, it leads to error, e.g. attempting to get main_ref_store outside of repository.
		return nil, nil
	}

	whiteList := map[string]struct{}{
		"diff":         {},
		"merge":        {},
		"merge-tree":   {},
		"check-attr":   {},
		"worktree":     {},
		"archive":      {},
		"log":          {},
		"format-patch": {},
		// Below commands read attributes only on request (cat-file --filters and --textconv),
		// or not at all. They keep attr.tree = HEAD so that cat-file's filters see the
		// attributes of HEAD.
		"cat-file":   {},
		"rev-parse":  {},
		"update-ref": {},
	}
	if _, ok := whiteList[sc.Name]; ok {
		return &ConfigPair{Key: "attr.tree", Value: "HEAD"}, nil
	}

	objectHash, err := repositoryObjectHash(repoPath)
	if err != nil {
		return nil, fmt.Errorf("detecting the object format for attr.tree: %w", err)
	}

	return &ConfigPair{Key: "attr.tree", Value: objectHash.EmptyTreeOID.String()}, nil
}

// repositoryObjectHash returns the object hash of the repository that git -C path would use, where
// path is a git directory (a bare repository) or the root of a worktree, as findRepoPath returns
// them. Its object format is the extensions.objectFormat of the configuration file of the common
// git directory. A repository without a configuration file uses SHA-1, as git does.
func repositoryObjectHash(path string) (git.ObjectHash, error) {
	commonDir, err := commonGitDir(path)
	if err != nil {
		return git.ObjectHash{}, err
	}

	objectHash, err := DetectObjectHash(context.Background(), commonDir)
	if errors.Is(err, fs.ErrNotExist) {
		return git.ObjectHashSHA1, nil
	}

	return objectHash, err
}

// commonGitDir returns the common git directory of the repository at path. path is the git
// directory itself unless it holds a .git entry: a .git directory is the git directory of a main
// worktree, and a .git file ("gitdir: <dir>") names the git directory of a linked worktree. A
// linked worktree's git directory names its common directory in its commondir file. Relative paths
// are relative to the directory holding the file, as git resolves them.
func commonGitDir(path string) (string, error) {
	gitDir := path

	dotGit := filepath.Join(path, ".git")
	info, err := os.Stat(dotGit)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return "", fmt.Errorf("stat .git: %w", err)
	case info.IsDir():
		gitDir = dotGit
	default:
		content, err := os.ReadFile(dotGit)
		if err != nil {
			return "", fmt.Errorf("reading .git file: %w", err)
		}

		target, ok := strings.CutPrefix(strings.TrimSpace(string(content)), "gitdir:")
		if !ok {
			return "", fmt.Errorf("invalid .git file %q", dotGit)
		}

		gitDir = strings.TrimSpace(target)
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(path, gitDir)
		}
	}

	commonDir, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return gitDir, nil
	case err != nil:
		return "", fmt.Errorf("reading commondir: %w", err)
	}

	common := strings.TrimSpace(string(commonDir))
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitDir, common)
	}

	return common, nil
}

// findRepoPath find the repo to execute git rev-parse --show-object-format
func (cf *ExecCommandFactory) findRepoPath(ctx context.Context, repo storage.Repository, sc Command, opts ...CmdOpt) string {
	config, _ := cf.combineOpts(ctx, sc, opts)

	var repoPath string
	if repo != nil {
		repoPath, _ = cf.locator.GetRepoPath(ctx, repo)
	}

	if config.worktreePath != "" {
		return config.worktreePath
	} else if repoPath != "" {
		return repoPath
	}
	return ""
}
