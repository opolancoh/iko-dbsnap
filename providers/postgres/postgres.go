// Package postgres implements core.Provider for PostgreSQL by shelling out
// to the standard pg_dump client binary. All Postgres-specific knowledge
// (flags, formats, default port, env vars) lives in this package; nothing
// outside it should need to know pg_dump exists.
package postgres

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"iko-dbsnap/core"
)

// Name is this provider's registry key ("-provider postgres").
const Name = "postgres"

// DefaultPort is used whenever ConnectionInfo.Port is left at 0.
const DefaultPort = 5432

// Backup formats accepted in BackupOptions.Format. Custom is the default:
// it's compressed and the only format pg_restore can run with -j (parallel
// restore).
const (
	FormatCustom    = "custom"
	FormatPlain     = "plain"
	FormatDirectory = "directory"
	FormatTar       = "tar"
)

var formatFlag = map[string]string{
	FormatCustom:    "c",
	FormatPlain:     "p",
	FormatTar:       "t",
	FormatDirectory: "d",
}

// formatExt is the file extension for single-file formats. Directory
// format has no entry: pg_dump -F d writes a directory, not a file.
var formatExt = map[string]string{
	FormatCustom: ".dump",
	FormatPlain:  ".sql",
	FormatTar:    ".tar",
}

// Provider backs up and restores PostgreSQL databases via the standard
// pg_dump / pg_restore / psql client binaries.
type Provider struct {
	// DumpBin, RestoreBin and PsqlBin override the pg_dump / pg_restore /
	// psql executable names/paths. Each defaults to its plain name,
	// resolved from PATH.
	DumpBin    string
	RestoreBin string
	PsqlBin    string
}

// New returns a Provider configured to use pg_dump/pg_restore/psql from PATH.
func New() *Provider {
	return &Provider{DumpBin: "pg_dump", RestoreBin: "pg_restore", PsqlBin: "psql"}
}

func init() { core.Register(New()) }

// Name identifies this provider in the core registry.
func (p *Provider) Name() string { return Name }

// Backup runs pg_dump for a single database. Recognized opts.Params keys:
//   - "compressLevel": passed as pg_dump -Z (ignored for FormatPlain)
//   - "jobs":          passed as pg_dump -j (only valid with FormatDirectory)
//
// Recognized conn.Params keys:
//   - "sslmode": passed via PGSSLMODE
func (p *Provider) Backup(ctx context.Context, conn core.ConnectionInfo, opts core.BackupOptions) (core.BackupResult, error) {
	start := time.Now()
	res := core.BackupResult{DBName: conn.DBName, StartedAt: start}

	if conn.DBName == "" {
		return res, fmt.Errorf("postgres: DBName is required")
	}
	if conn.User == "" {
		return res, fmt.Errorf("postgres: User is required")
	}

	bin := firstNonEmpty(p.DumpBin, "pg_dump")
	if err := requireBin(bin); err != nil {
		return res, err
	}

	format := opts.Format
	if format == "" {
		format = FormatCustom
	}
	flag, ok := formatFlag[format]
	if !ok {
		return res, fmt.Errorf("postgres: unsupported format %q (want one of custom, plain, directory, tar)", format)
	}

	outDir := opts.OutputDir
	if outDir == "" {
		outDir = "."
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return res, fmt.Errorf("postgres: error creating backup file for %q: could not create output directory %q: %w", conn.DBName, outDir, err)
	}

	stamp := start.Format("20060102T150405")
	outPath := filepath.Join(outDir, fmt.Sprintf("%s_%s%s", conn.DBName, stamp, formatExt[format]))

	args := connArgs(conn)
	args = append(args, "-F", flag, "-f", outPath, "-v")
	if lvl := opts.Param("compressLevel", ""); lvl != "" && format != FormatPlain {
		args = append(args, "-Z", lvl)
	}
	if jobs := opts.Param("jobs", ""); jobs != "" {
		if format != FormatDirectory {
			return res, fmt.Errorf("postgres: \"jobs\" is only valid with format=directory")
		}
		args = append(args, "-j", jobs)
	}
	args = append(args, conn.DBName)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = connEnv(conn)

	stderr, runErr := runWithProgress(cmd, func(line string) {
		if opts.OnProgress == nil {
			return
		}
		if schema, table, ok := parseDumpingTable(line); ok {
			opts.OnProgress(core.ProgressEvent{DBName: conn.DBName, Schema: schema, Table: table})
		}
	})
	res.Duration = time.Since(start)
	if runErr != nil {
		if _, statErr := os.Stat(outPath); statErr != nil {
			return res, fmt.Errorf("postgres: error creating backup file %q for %q: pg_dump failed: %w: %s", outPath, conn.DBName, runErr, stderr)
		}
		return res, fmt.Errorf("postgres: pg_dump failed for %q partway through writing %q (an incomplete file was left behind — delete it before retrying): %w: %s", conn.DBName, outPath, runErr, stderr)
	}

	info, err := os.Stat(outPath)
	if err != nil {
		return res, fmt.Errorf("postgres: error creating backup file %q for %q: pg_dump reported success but no output was found: %w", outPath, conn.DBName, err)
	}
	size := sizeOf(outPath, info)
	if size == 0 {
		return res, fmt.Errorf("postgres: error creating backup file %q for %q: pg_dump reported success but the output is empty", outPath, conn.DBName)
	}

	res.FilePath = outPath
	res.SizeBytes = size
	return res, nil
}

// dumpingTableRe matches pg_dump -v's per-table progress line, e.g.:
//
//	pg_dump: dumping contents of table "public.foo"
var dumpingTableRe = regexp.MustCompile(`dumping contents of table "([^"]+)"`)

// parseDumpingTable extracts the schema-qualified table name from a
// pg_dump -v progress line, if that's what the line is.
func parseDumpingTable(line string) (schema, table string, ok bool) {
	m := dumpingTableRe.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	if schema, table, found := strings.Cut(m[1], "."); found {
		return schema, table, true
	}
	return "", m[1], true
}

// runWithProgress runs cmd, invoking onLine for each line written to
// stderr as it arrives (rather than only after the process exits), while
// still collecting the full stderr text for error reporting.
func runWithProgress(cmd *exec.Cmd, onLine func(line string)) (stderr string, err error) {
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("creating stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(io.TeeReader(stderrPipe, &buf))
		for scanner.Scan() {
			line := scanner.Text()
			if onLine != nil {
				onLine(line)
			}
		}
	}()

	runErr := cmd.Wait()
	<-done
	return buf.String(), runErr
}

// Restore creates a new database named exactly conn.DBName and loads a
// backup produced by Backup into it. Creation goes through a plain CREATE
// DATABASE, not pg_restore's own --create, so the result is always named
// what was asked for rather than whatever the archive was originally
// dumped from — and since CREATE DATABASE fails if the name is taken, an
// existing database is never restored into. Plain-format dumps (a .sql
// file, or any dump with Format explicitly set to "plain") are executed
// via psql; every other format is handed to pg_restore, which
// auto-detects custom/tar/directory archives itself. If pg_restore only
// skipped individual failing statements ("errors ignored on restore"),
// the restore counts as OK and those errors come back as
// RestoreResult.Warnings.
//
// Recognized opts.Params keys:
//   - "jobs": passed as pg_restore -j (parallel restore; only valid for
//     format=custom or format=directory, rejected otherwise)
//
// Recognized conn.Params keys:
//   - "sslmode": passed via PGSSLMODE
func (p *Provider) Restore(ctx context.Context, conn core.ConnectionInfo, opts core.RestoreOptions) (core.RestoreResult, error) {
	start := time.Now()
	res := core.RestoreResult{DBName: conn.DBName, StartedAt: start}

	if conn.DBName == "" {
		return res, fmt.Errorf("postgres: DBName is required")
	}
	if len(conn.DBName) > maxIdentifierLen {
		return res, fmt.Errorf("postgres: database name %q is longer than %d bytes, which Postgres would silently truncate", conn.DBName, maxIdentifierLen)
	}
	if conn.User == "" {
		return res, fmt.Errorf("postgres: User is required")
	}
	if opts.InputPath == "" {
		return res, fmt.Errorf("postgres: InputPath is required")
	}
	info, err := os.Stat(opts.InputPath)
	if err != nil {
		return res, fmt.Errorf("postgres: cannot read backup at %q: %w", opts.InputPath, err)
	}
	if size := sizeOf(opts.InputPath, info); size == 0 {
		return res, fmt.Errorf("postgres: backup at %q is empty", opts.InputPath)
	}

	format, err := resolveRestoreFormat(opts.Format, opts.InputPath)
	if err != nil {
		return res, err
	}

	var cmd *exec.Cmd
	if format == FormatPlain {
		bin := firstNonEmpty(p.PsqlBin, "psql")
		if err := requireBin(bin); err != nil {
			return res, err
		}
		args := connArgs(conn)
		args = append(args, "-d", conn.DBName, "-v", "ON_ERROR_STOP=1", "-f", opts.InputPath)
		cmd = exec.CommandContext(ctx, bin, args...)
	} else {
		bin := firstNonEmpty(p.RestoreBin, "pg_restore")
		if err := requireBin(bin); err != nil {
			return res, err
		}
		args := connArgs(conn)
		args = append(args, "-d", conn.DBName, "-v")
		if opts.NoOwner {
			args = append(args, "--no-owner")
		}
		if jobs := opts.Param("jobs", ""); jobs != "" {
			if format != FormatCustom && format != FormatDirectory {
				return res, fmt.Errorf("postgres: \"jobs\" is only valid with format=custom or format=directory, not %q", format)
			}
			args = append(args, "-j", jobs)
		}
		args = append(args, opts.InputPath)
		cmd = exec.CommandContext(ctx, bin, args...)
	}
	cmd.Env = connEnv(conn)

	if err := p.CreateDatabase(ctx, conn); err != nil {
		return res, err
	}

	stderr, runErr := runWithProgress(cmd, func(line string) {
		if opts.OnProgress == nil {
			return
		}
		if schema, table, ok := parseProcessingTable(line); ok {
			opts.OnProgress(core.ProgressEvent{DBName: conn.DBName, Schema: schema, Table: table})
		}
	})
	res.Duration = time.Since(start)
	if runErr != nil {
		if warnings, ok := parseIgnoredErrors(stderr); ok && format != FormatPlain {
			res.Warnings = warnings
			return res, nil
		}
		return res, fmt.Errorf("postgres: restore failed for %q (the database was created and may be partially restored — drop it before retrying): %w: %s", conn.DBName, runErr, stderr)
	}
	return res, nil
}

// maxIdentifierLen is Postgres's default NAMEDATALEN-1: longer database
// names are truncated (with only a NOTICE), which would leave the restore
// under a different name than the one asked for.
const maxIdentifierLen = 63

// ignoredErrorsRe matches the summary pg_restore prints when it finished
// the whole archive but skipped statements that failed, e.g.:
//
//	pg_restore: warning: errors ignored on restore: 1
var ignoredErrorsRe = regexp.MustCompile(`errors ignored on restore: \d+`)

// parseIgnoredErrors reports whether pg_restore's stderr ends in an
// "errors ignored on restore" summary — meaning it ran to completion and
// only skipped individual failing statements — and if so returns each
// skipped error, with the statement that caused it when pg_restore shows
// one. Anything else (no summary) is a real failure, not a warning.
func parseIgnoredErrors(stderr string) ([]string, bool) {
	if !ignoredErrorsRe.MatchString(stderr) {
		return nil, false
	}
	var warnings []string
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "pg_restore: error: "):
			warnings = append(warnings, strings.TrimPrefix(line, "pg_restore: error: "))
		case strings.HasPrefix(line, "Command was: ") && len(warnings) > 0:
			warnings[len(warnings)-1] += " (" + line + ")"
		}
	}
	return warnings, true
}

// processingTableRe matches pg_restore -v's per-table data-loading
// progress line, e.g.:
//
//	pg_restore: processing data for table "public.foo"
var processingTableRe = regexp.MustCompile(`processing data for table "([^"]+)"`)

func parseProcessingTable(line string) (schema, table string, ok bool) {
	m := processingTableRe.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	if schema, table, found := strings.Cut(m[1], "."); found {
		return schema, table, true
	}
	return "", m[1], true
}

// connArgs builds the -h/-p/-U flags shared by pg_dump, pg_restore and psql.
func connArgs(conn core.ConnectionInfo) []string {
	port := conn.Port
	if port == 0 {
		port = DefaultPort
	}
	return []string{
		"-h", conn.Host,
		"-p", fmt.Sprintf("%d", port),
		"-U", conn.User,
	}
}

// connEnv builds the environment for a pg_dump/pg_restore/psql invocation.
// The password travels via PGPASSWORD (rather than a -W/-w flag) so it
// never appears in the process argument list, where it'd be visible via
// `ps`.
func connEnv(conn core.ConnectionInfo) []string {
	env := append(os.Environ(), "PGPASSWORD="+conn.Password)
	if sslmode := conn.Param("sslmode", ""); sslmode != "" {
		env = append(env, "PGSSLMODE="+sslmode)
	}
	return env
}

// resolveRestoreFormat determines which restore path to take: explicit if
// set, otherwise inferred from InputPath (a directory means the Postgres
// "directory" format; a ".sql" extension means "plain"; anything else
// defaults to a pg_restore-handled archive, which auto-detects custom vs.
// tar from the file's own header).
func resolveRestoreFormat(explicit, inputPath string) (string, error) {
	if explicit != "" {
		if _, ok := formatFlag[explicit]; !ok {
			return "", fmt.Errorf("postgres: unsupported format %q (want one of custom, plain, directory, tar)", explicit)
		}
		return explicit, nil
	}
	if info, err := os.Stat(inputPath); err == nil && info.IsDir() {
		return FormatDirectory, nil
	}
	if strings.EqualFold(filepath.Ext(inputPath), ".sql") {
		return FormatPlain, nil
	}
	return FormatCustom, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func requireBin(bin string) error {
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("postgres: %s not found in PATH: %w", bin, err)
	}
	return nil
}

// sizeOf returns a file's size, or the total size of its contents if it's
// a directory (relevant for FormatDirectory, where pg_dump writes a
// directory rather than a single file).
func sizeOf(path string, info os.FileInfo) int64 {
	if !info.IsDir() {
		return info.Size()
	}
	var total int64
	_ = filepath.Walk(path, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			total += fi.Size()
		}
		return nil
	})
	return total
}
