// Command dbsnap backs up and restores databases across pluggable engines.
// Today only the postgres provider is registered; new engines register
// themselves via a blank import, same as postgres below.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"

	"golang.org/x/term"

	"iko-dbsnap/core"
	"iko-dbsnap/internal/tui"
	_ "iko-dbsnap/providers/postgres"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "backup":
		runBackup(os.Args[2:])
	case "restore":
		runRestore(os.Args[2:])
	case "-h", "--help", "help":
		printUsage()
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `dbsnap: backup and restore databases

Usage:
  dbsnap backup  -user U -db name1,name2 [flags]
  dbsnap restore -user U [-db NAME] [flags] BACKUP_FILE

Run "dbsnap backup -h" or "dbsnap restore -h" for flag details.
`)
}

// connFlags registers the connection-related flags shared by both
// subcommands and returns pointers to their values.
func connFlags(fs *flag.FlagSet) (provider, host *string, port *int, user, password *string) {
	provider = fs.String("provider", "postgres", fmt.Sprintf("db provider to use (available: %s)", strings.Join(core.Names(), ", ")))
	host = fs.String("host", "localhost", "database host")
	port = fs.Int("port", 0, "database port (0 = provider default)")
	user = fs.String("user", "", "database user (required)")
	password = fs.String("password", os.Getenv("DBSNAP_PASSWORD"), "database password (or set DBSNAP_PASSWORD)")
	return
}

func runBackup(args []string) {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	provider, host, port, user, password := connFlags(fs)
	dbNames := fs.String("db", "", "comma-separated database names to back up (required)")
	outputDir := fs.String("out", "./backups", "directory to write backup files to")
	format := fs.String("format", "", "backup format, provider-specific (empty = provider default)")
	concurrency := fs.Int("concurrency", 3, "how many databases to back up in parallel")
	yes := fs.Bool("yes", false, "skip the confirmation prompt (still shows the discovery report and progress)")
	nonInteractive := fs.Bool("non-interactive", false, "always use plain line-per-database output, even on a terminal")
	fs.Parse(args)

	if *user == "" {
		log.Fatal("missing required flag: -user")
	}
	names := splitNonEmpty(*dbNames)
	if len(names) == 0 {
		log.Fatal("missing required flag: -db (comma-separated database names)")
	}
	if err := checkPathArg("-out", *outputDir); err != nil {
		log.Fatal(err)
	}

	p, err := core.Get(*provider)
	if err != nil {
		log.Fatal(err)
	}

	conns := make([]core.ConnectionInfo, len(names))
	for i, n := range names {
		conns[i] = core.ConnectionInfo{
			Host:     *host,
			Port:     *port,
			User:     *user,
			Password: *password,
			DBName:   n,
		}
	}
	opts := core.BackupOptions{OutputDir: *outputDir, Format: *format}

	interactive := !*nonInteractive && term.IsTerminal(int(os.Stdout.Fd()))
	if _, ok := p.(core.Inspector); !ok {
		interactive = false
	}

	if interactive {
		err := tui.Run(context.Background(), tui.Config{
			Provider:    p,
			Targets:     conns,
			Opts:        opts,
			Concurrency: *concurrency,
			AutoConfirm: *yes,
		})
		if errors.Is(err, tui.ErrAborted) {
			fmt.Fprintln(os.Stderr, "Aborted.")
			os.Exit(1)
		}
		if err != nil {
			log.Fatal(err)
		}
		return
	}

	targets := make([]core.Target, len(conns))
	for i, c := range conns {
		targets[i] = core.Target{Conn: c}
	}
	results := core.BackupAll(context.Background(), p, targets, opts, *concurrency)

	failed := 0
	for _, r := range results {
		if r.OK() {
			fmt.Printf("OK   %-30s %12d bytes  %10s  -> %s\n", r.DBName, r.SizeBytes, r.Duration.Round(1e6), r.FilePath)
		} else {
			failed++
			fmt.Printf("FAIL %-30s %v\n", r.DBName, r.Err)
		}
	}
	if failed > 0 {
		os.Exit(1)
	}
}

// restoreArgs is everything `dbsnap restore` was given on the command
// line.
type restoreArgs struct {
	provider, host   string
	port             int
	user, password   string
	db, file, format string
	jobs             string
	noOwner          bool
	yes              bool
	nonInteractive   bool
}

const restoreUsage = "dbsnap restore -user U [-db NAME] [flags] BACKUP_FILE"

// errBadFlags wraps errors from the flag package itself, which has already
// printed the problem and the usage text by the time it returns one.
var errBadFlags = errors.New("invalid flags")

// parseRestoreArgs parses `dbsnap restore`'s arguments: flags plus exactly
// one positional backup file. Flags may appear before or after the file —
// Go's flag package stops at the first positional argument, so whatever
// follows it is parsed a second time.
func parseRestoreArgs(args []string) (restoreArgs, error) {
	var a restoreArgs
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s\n\nFlags:\n", restoreUsage)
		fs.PrintDefaults()
	}
	provider, host, port, user, password := connFlags(fs)
	fs.StringVar(&a.db, "db", "", "name of the new database to create and restore into (default: the name recorded in the backup)")
	fs.StringVar(&a.format, "format", "", "backup format, provider-specific (empty = infer from the file)")
	fs.BoolVar(&a.noOwner, "no-owner", false, "skip restoring ownership/ACLs")
	fs.StringVar(&a.jobs, "jobs", "", "parallel restore jobs (provider-specific; postgres: custom/directory formats only)")
	fs.BoolVar(&a.yes, "yes", false, "skip the confirmation prompt (still shows the restore plan and progress)")
	fs.BoolVar(&a.nonInteractive, "non-interactive", false, "always use plain output, even on a terminal")

	if err := fs.Parse(args); err != nil {
		return a, fmt.Errorf("%w: %w", errBadFlags, err)
	}
	rest := fs.Args()
	if len(rest) > 0 {
		a.file = rest[0]
		if err := fs.Parse(rest[1:]); err != nil {
			return a, fmt.Errorf("%w: %w", errBadFlags, err)
		}
		rest = fs.Args()
	}
	a.provider, a.host, a.port, a.user, a.password = *provider, *host, *port, *user, *password

	if strings.Contains(a.db, ",") {
		return a, fmt.Errorf("-db takes a single database name — restore one backup per command\nusage: %s", restoreUsage)
	}
	if strings.Contains(a.db, "=") {
		name, path, _ := strings.Cut(a.db, "=")
		return a, fmt.Errorf("-db now takes only the name of the database to create; pass the backup file as an argument instead, e.g.:\n  dbsnap restore -user %s -db %s %s",
			firstNonEmpty(a.user, "U"), shellQuote(name), shellQuote(path))
	}
	if len(rest) > 0 {
		return a, fmt.Errorf("unexpected argument %q — restore takes exactly one backup file\nusage: %s", rest[0], restoreUsage)
	}
	if a.file == "" {
		return a, fmt.Errorf("missing the backup file to restore\nusage: %s", restoreUsage)
	}
	if a.user == "" {
		return a, fmt.Errorf("missing required flag: -user")
	}
	if err := checkPathArg("the backup file path", a.file); err != nil {
		return a, err
	}
	if err := checkPathArg("-db", a.db); err != nil {
		return a, err
	}
	return a, nil
}

func runRestore(args []string) {
	a, err := parseRestoreArgs(args)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if errors.Is(err, errBadFlags) {
		os.Exit(2)
	}
	if err != nil {
		exitWith(err.Error())
	}

	p, err := core.Get(a.provider)
	if err != nil {
		log.Fatal(err)
	}

	params := map[string]string{}
	if a.jobs != "" {
		params["jobs"] = a.jobs
	}
	conn := core.ConnectionInfo{
		Host:     a.host,
		Port:     a.port,
		User:     a.user,
		Password: a.password,
		DBName:   a.db,
	}
	opts := core.RestoreOptions{
		InputPath: a.file,
		Format:    a.format,
		NoOwner:   a.noOwner,
		Params:    params,
	}

	// Pre-flight, identical for interactive and non-interactive runs:
	// resolve the target name and refuse if it already exists, before
	// anything is created.
	plan, err := core.PlanRestore(context.Background(), p, conn, opts)
	if err != nil {
		exitWith(restoreErrorMessage(os.Args[0], a, conn, err))
	}
	conn.DBName = plan.DBName

	interactive := !a.nonInteractive && term.IsTerminal(int(os.Stdout.Fd()))
	if _, ok := p.(core.Inspector); !ok {
		interactive = false
	}

	if interactive {
		err := tui.RunRestore(context.Background(), tui.RestoreConfig{
			Provider:    p,
			Conn:        conn,
			Opts:        opts,
			Plan:        plan,
			AutoConfirm: a.yes,
		})
		if errors.Is(err, tui.ErrAborted) {
			fmt.Fprintln(os.Stderr, "Aborted.")
			os.Exit(1)
		}
		if err != nil {
			log.Fatal(err)
		}
		return
	}

	res, err := p.Restore(context.Background(), conn, opts)
	if err != nil && res.Err == nil {
		res.Err = err
	}
	if !res.OK() {
		fmt.Printf("FAIL %-30s %v\n", conn.DBName, res.Err)
		os.Exit(1)
	}
	fmt.Printf("OK   %-30s %10s\n", conn.DBName, res.Duration.Round(1e6))
	for _, w := range res.Warnings {
		fmt.Printf("WARN %-30s skipped failing statement: %s\n", conn.DBName, w)
	}
}

// restoreErrorMessage turns a PlanRestore error into what the user should
// see — for the two "pick a name" cases, including a ready-to-run command
// that fixes it.
func restoreErrorMessage(prog string, a restoreArgs, conn core.ConnectionInfo, err error) string {
	var exists *core.TargetExistsError
	switch {
	case errors.As(err, &exists):
		origin := ""
		if a.db == "" {
			origin = " (name taken from the backup)"
		}
		return fmt.Sprintf("Database %q%s already exists on %s — nothing was restored.\n"+
			"dbsnap only restores into a new database it creates itself. Choose another name with -db, e.g.:\n  %s",
			exists.DBName, origin, serverLabel(conn), restoreCommand(prog, a, exists.Suggestion))
	case errors.Is(err, core.ErrNoDBName):
		return fmt.Sprintf("This backup does not record a database name, so there's nothing to name the new database after.\n"+
			"Pass the name of the database to create with -db, e.g.:\n  %s",
			restoreCommand(prog, a, "mydb"))
	}
	return err.Error()
}

// restoreCommand rebuilds the restore command the user ran, with -db set
// to dbName, so an error can offer something they can copy and run. The
// password is left out on purpose — it comes from DBSNAP_PASSWORD.
func restoreCommand(prog string, a restoreArgs, dbName string) string {
	parts := []string{prog, "restore"}
	if a.provider != "postgres" {
		parts = append(parts, "-provider", shellQuote(a.provider))
	}
	if a.host != "localhost" {
		parts = append(parts, "-host", shellQuote(a.host))
	}
	if a.port != 0 {
		parts = append(parts, "-port", fmt.Sprint(a.port))
	}
	parts = append(parts, "-user", shellQuote(a.user))
	if a.noOwner {
		parts = append(parts, "-no-owner")
	}
	parts = append(parts, "-db", shellQuote(dbName), shellQuote(a.file))
	return strings.Join(parts, " ")
}

func serverLabel(c core.ConnectionInfo) string {
	if c.Port == 0 {
		return c.Host
	}
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

// shellSafe matches strings that need no quoting in a POSIX shell.
var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./:@%+,=-]+$`)

// shellQuote single-quotes s if a POSIX shell would otherwise split or
// interpret it.
func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// exitWith prints msg to stderr and exits with status 1. Used instead of
// log.Fatal for messages meant to be read (and copied from) as-is.
func exitWith(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// smartQuotes are the curly quote characters rich-text editors, chat
// apps, and "smart quotes" autocorrect commonly substitute for plain
// ASCII quotes. None of them are shell quoting characters, so a shell
// passes them straight through as literal bytes in the argument — which
// silently corrupts a path (e.g. a value like “/foo/bar” no longer starts
// with "/", so it stops being an absolute path).
var smartQuotes = []rune{'‘', '’', '“', '”'}

// checkPathArg rejects flag values containing smart-quote characters,
// which almost always means the command was copy-pasted from somewhere
// that "helpfully" reformatted straight quotes — rather than silently
// using the corrupted value (which can turn an absolute path into a
// relative one nested somewhere unexpected), fail with an explanation.
func checkPathArg(flagDesc, val string) error {
	for _, r := range smartQuotes {
		if strings.ContainsRune(val, r) {
			return fmt.Errorf(
				"%s contains a curly quote (%q) instead of a plain one: %q\n"+
					"this usually happens when a command is copy-pasted from an app with \"smart quotes\" autocorrect — "+
					"retype the quotes directly in the terminal, or just remove them (quotes are only needed if the path contains spaces)",
				flagDesc, string(r), val)
		}
	}
	return nil
}
