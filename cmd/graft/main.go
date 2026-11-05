// Command graft is the command-line client for sparse, server-centric working
// copies of Graft repositories. See doc/graft for the design.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
	// exitInterrupted is the conventional exit code after an interrupt (128+SIGINT).
	exitInterrupted = 130
)

// target identifies the repository a command talks to.
type target struct {
	Address string
	Token   string
	Storage string
	Repo    string
	// User is the acting user ID (gl_id) sent with writes. See sdkBackend.user.
	User string
}

// backendFactory returns a workspace.Backend bound to the repository in t. The
// production implementation talks to the server through the sdk; tests replace
// it with workspace.MemoryBackend.
var backendFactory = dialBackend

// options holds the flags and environment shared by all commands.
type options struct {
	address string
	token   string
	storage string
	user    string
	author  string
	dir     string
	branch  string
	sparse  string
	message string
	land    string
	abort   bool
	resolve bool
	// conflictsDir and resolved are the directories of graft land --conflicts-dir and
	// --resolved.
	conflictsDir string
	resolved     string
	strategy     string
	revision     string
	after        uint64
	limit        int
	jsonOut      bool
	// stderr receives warnings of commands that succeed.
	stderr io.Writer
}

// usageError marks a command line that cannot be run as written.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// command is one subcommand: its own flags and its implementation.
type command struct {
	bind func(fs *flag.FlagSet, o *options)
	run  func(ctx context.Context, out io.Writer, o *options, args []string) error
}

func commandTable() map[string]command {
	return map[string]command{
		"clone":   {bind: bindClone, run: cmdClone},
		"status":  {run: cmdStatus},
		"diff":    {run: cmdDiff},
		"commit":  {bind: bindCommit, run: cmdCommit},
		"land":    {bind: bindLand, run: cmdLand},
		"revert":  {run: cmdRevert},
		"update":  {run: cmdUpdate},
		"sparse":  {run: cmdSparse},
		"log":     {bind: bindLog, run: cmdLog},
		"watch":   {bind: bindWatch, run: cmdWatch},
		"show":    {bind: bindShow, run: cmdShow},
		"blame":   {bind: bindRevision, run: cmdBlame},
		"history": {bind: bindHistory, run: cmdHistory},
		"cat":     {run: cmdCat},
		"search":  newSearchCommand(),
		"review":  reviewCommand(),
		"storage": storageCommand(),
		"repo":    repoCommand(),
		"policy":  policyCommand(),
	}
}

func main() {
	// An interrupt cancels the command instead of killing the process, so that
	// it stops at the next server call and records what it already wrote.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := runContext(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run executes one graft command line and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	return runContext(context.Background(), args, stdout, stderr)
}

// runContext is run with a context that cancels the command.
func runContext(parent context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return exitUsage
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printUsage(stdout)
		return exitOK
	}
	name := args[0]
	cmd, ok := commandTable()[name]
	if !ok {
		fmt.Fprintf(stderr, "graft: unknown command %q\n\n", name)
		printUsage(stderr)
		return exitUsage
	}

	ctx, release := withClosers(parent)
	defer release()

	o := defaultOptions()
	o.stderr = stderr
	fs := flag.NewFlagSet("graft "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	bindCommon(fs, &o)
	if cmd.bind != nil {
		cmd.bind(fs, &o)
	}
	positional, err := parseArgs(fs, args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}

	err = cmd.run(ctx, stdout, &o, positional)
	if err == nil {
		return exitOK
	}
	if parent.Err() != nil {
		// Interrupted: the error is the canceled call, which says nothing useful.
		fmt.Fprintf(stderr, "graft %s: interrupted\n", name)
		return exitInterrupted
	}
	fmt.Fprintf(stderr, "graft %s: %v\n", name, err)
	var ue *usageError
	if errors.As(err, &ue) {
		return exitUsage
	}
	return exitError
}

// parseArgs parses the flags of a command and returns its positional arguments.
// Flags may appear before or after positional arguments, as in
// "graft clone ADDR REPO DIR --branch main". A "--" ends the flags.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			return append(positional, rest...), nil
		}
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// defaultOptions reads the environment. Flags override it.
func defaultOptions() options {
	return options{
		address: os.Getenv("GRAFT_ADDRESS"),
		token:   os.Getenv("GRAFT_TOKEN"),
		storage: os.Getenv("GRAFT_STORAGE"),
		user:    os.Getenv("GRAFT_USER"),
		author:  os.Getenv("GRAFT_AUTHOR"),
	}
}

func bindCommon(fs *flag.FlagSet, o *options) {
	fs.StringVar(&o.address, "address", o.address, "server address (env GRAFT_ADDRESS)")
	fs.StringVar(&o.token, "token", o.token, "access token (env GRAFT_TOKEN)")
	fs.StringVar(&o.storage, "storage", o.storage, `storage name (env GRAFT_STORAGE, default "default")`)
	fs.StringVar(&o.user, "user", o.user, `acting user ID of writes, normally the agent name of the token (env GRAFT_USER, default "`+anonymousUser+`")`)
	fs.StringVar(&o.author, "author", o.author, `commit author as "Name <email>" (env GRAFT_AUTHOR)`)
	fs.StringVar(&o.dir, "C", "", "workspace directory (default: search upward from the current directory)")
}

func bindClone(fs *flag.FlagSet, o *options) {
	fs.StringVar(&o.branch, "branch", "main", "branch to check out")
	fs.StringVar(&o.sparse, "sparse", "", "comma-separated paths to materialize (default: whole repository)")
}

func bindCommit(fs *flag.FlagSet, o *options) {
	fs.StringVar(&o.message, "m", "", "commit message")
	fs.StringVar(&o.land, "land", "", "land the commit onto this branch")
	fs.StringVar(&o.strategy, "strategy", workspace.StrategyRebase, "land strategy: rebase, merge or ff_only")
	fs.BoolVar(&o.resolve, "resolve", false, "with --land: let the server resolve merge conflicts")
}

func bindLand(fs *flag.FlagSet, o *options) {
	fs.StringVar(&o.strategy, "strategy", workspace.StrategyRebase, "land strategy: rebase, merge or ff_only")
	fs.BoolVar(&o.abort, "abort", false, "drop the pending commit without landing it")
	fs.BoolVar(&o.resolve, "resolve", false, "let the server resolve merge conflicts")
	fs.StringVar(&o.conflictsDir, "conflicts-dir", "", "with --resolve: write the conflicts the server did not resolve into this directory")
	fs.StringVar(&o.resolved, "resolved", "", "land the resolution in this directory (written by --conflicts-dir, then edited)")
}

func bindLog(fs *flag.FlagSet, o *options) {
	fs.Uint64Var(&o.after, "after", 0, "list changesets with a sequence above this value")
	fs.IntVar(&o.limit, "limit", 100, "maximum number of changesets")
	fs.BoolVar(&o.jsonOut, "json", false, "print one JSON object per line")
}

func bindWatch(fs *flag.FlagSet, o *options) {
	fs.Uint64Var(&o.after, "after", 0, "start after this changeset sequence")
	fs.BoolVar(&o.jsonOut, "json", false, "print one JSON object per line, flushed as each changeset arrives")
}

func bindRevision(fs *flag.FlagSet, o *options) {
	fs.StringVar(&o.revision, "rev", "", "revision to read (default: the workspace branch)")
}

func bindHistory(fs *flag.FlagSet, o *options) {
	fs.StringVar(&o.revision, "rev", "", "revision to start from (default: the workspace branch)")
	fs.IntVar(&o.limit, "limit", 20, "maximum number of commits")
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `usage: graft <command> [flags] [arguments]

commands:
  clone ADDR REPO DIR    materialize REPO into DIR (--branch, --sparse a/,b/)
  status                 list local changes
  diff                   show line changes against the base commit
  commit -m MSG          commit local changes (--land BRANCH to land them, --resolve for conflicts)
  land [BRANCH]          land the pending commit (--strategy, --resolve [--conflicts-dir DIR],
                         --resolved DIR; --abort drops it)
  revert [PATH...]       discard local changes to PATHs (default: all) and restore them
  update                 move the workspace to the tip of its branch
  sparse [PREFIX...]     restrict the workspace to PREFIX paths
  log                    list changesets (--after, --limit, --json)
  watch                  follow the changeset log (--after, --json)
  show REV               describe a commit: parents, message, trailers, files (--json)
  blame PATH             attribute each line of a file (--rev)
  history PATH           list commits that changed a path (--rev, --limit)
  cat REV:PATH           print a file at a revision
  search QUERY           search file contents (--regex, -i, --path, --context, -n, --rev, --json)
  review SUBCOMMAND      create, show, list, comment, approve, request-changes, update, close (--branch, --title, --json)
  storage SUBCOMMAND     import, state, verify, checkpoint, gc, delete-conflicting-ref a repository in the storage core; gc --removed for a storage (--connectivity, --dry-run, --json)
  repo create REPO       create a repository, with the shared token (--default-branch, --object-format,
                         --initial-commit [--branch NAME], --json)
  policy check FILE      validate the access policy of a configuration file before a reload, and list
                         what it would deny among recent changesets (-repo, --limit, --json)

environment: GRAFT_ADDRESS, GRAFT_TOKEN, GRAFT_STORAGE, GRAFT_USER, GRAFT_AUTHOR

GRAFT_USER (-user) is the user ID of writes and names the temporary branches of
commit (graft/USER/...). Set it to the agent name of GRAFT_TOKEN. With the shared
token it is also the user that the server's access policy checks.
`)
}
