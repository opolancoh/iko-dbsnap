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
	"sort"
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
  dbsnap restore -user U -db "name1=path1.dump,name2=path2.dump" [flags]

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

func runRestore(args []string) {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	provider, host, port, user, password := connFlags(fs)
	restoreSpec := fs.String("db", "", `comma-separated "dbname=path" pairs to restore (required), e.g. "app=./backups/app_20260721T101500.dump"`)
	format := fs.String("format", "", "backup format, provider-specific (empty = infer per file)")
	noOwner := fs.Bool("no-owner", false, "skip restoring ownership/ACLs")
	jobs := fs.String("jobs", "", "parallel restore jobs (provider-specific; postgres: custom/directory formats only)")
	concurrency := fs.Int("concurrency", 3, "how many databases to restore in parallel")
	yes := fs.Bool("yes", false, "skip the confirmation prompt (still shows the discovery report and progress)")
	nonInteractive := fs.Bool("non-interactive", false, "always use plain line-per-database output, even on a terminal")
	fs.Parse(args)

	if *user == "" {
		log.Fatal("missing required flag: -user")
	}
	pairs, err := parsePairs(*restoreSpec)
	if err != nil {
		log.Fatal(err)
	}
	if len(pairs) == 0 {
		log.Fatal(`missing required flag: -db "dbname=path[,dbname=path...]"`)
	}

	p, err := core.Get(*provider)
	if err != nil {
		log.Fatal(err)
	}

	params := map[string]string{}
	if *jobs != "" {
		params["jobs"] = *jobs
	}

	dbNames := make([]string, 0, len(pairs))
	for db := range pairs {
		dbNames = append(dbNames, db)
	}
	sort.Strings(dbNames)

	targets := make([]core.RestoreTarget, 0, len(pairs))
	for _, db := range dbNames {
		targets = append(targets, core.RestoreTarget{
			Conn: core.ConnectionInfo{
				Host:     *host,
				Port:     *port,
				User:     *user,
				Password: *password,
				DBName:   db,
			},
			Opts: core.RestoreOptions{
				InputPath: pairs[db],
				Format:    *format,
				NoOwner:   *noOwner,
				Params:    params,
			},
		})
	}

	interactive := !*nonInteractive && term.IsTerminal(int(os.Stdout.Fd()))
	if _, ok := p.(core.Inspector); !ok {
		interactive = false
	}
	if _, ok := p.(core.ArchiveInspector); !ok {
		interactive = false
	}

	if interactive {
		err := tui.RunRestore(context.Background(), tui.RestoreConfig{
			Provider:    p,
			Targets:     targets,
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

	results := core.RestoreAll(context.Background(), p, targets, *concurrency)

	failed := 0
	for _, r := range results {
		if r.OK() {
			fmt.Printf("OK   %-30s %10s\n", r.DBName, r.Duration.Round(1e6))
		} else {
			failed++
			fmt.Printf("FAIL %-30s %v\n", r.DBName, r.Err)
		}
	}
	if failed > 0 {
		os.Exit(1)
	}
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

// parsePairs parses "db1=path1,db2=path2" into a map, rejecting malformed
// entries and duplicate database names up front rather than failing later
// mid-restore.
func parsePairs(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, entry := range splitNonEmpty(s) {
		db, path, ok := strings.Cut(entry, "=")
		db, path = strings.TrimSpace(db), strings.TrimSpace(path)
		if !ok || db == "" || path == "" {
			return nil, fmt.Errorf(`invalid -db entry %q, want "dbname=path"`, entry)
		}
		if _, dup := out[db]; dup {
			return nil, fmt.Errorf("duplicate database %q in -db", db)
		}
		if err := checkPathArg(db+"'s path in -db", path); err != nil {
			return nil, err
		}
		out[db] = path
	}
	return out, nil
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
