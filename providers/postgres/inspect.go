package postgres

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"iko-dbsnap/core"
)

// DefaultMaintenanceDB is the database Ping/ListDatabases connect through
// when no override is given — Postgres has no "connect to the server with
// no database" mode, so every server-level operation needs some database
// to connect to. Matches what psql/pg_dump default to.
const DefaultMaintenanceDB = "postgres"

// maxInspectConcurrency bounds how many row-count queries run at once
// while inspecting a single database, so a database with hundreds of
// tables doesn't open hundreds of simultaneous connections.
const maxInspectConcurrency = 4

func maintenanceDB(conn core.ConnectionInfo) string {
	return conn.Param("maintenanceDB", DefaultMaintenanceDB)
}

// connString builds a postgres:// connection URI. It goes through
// url.URL/url.UserPassword rather than hand-formatted key=value pairs so
// that special characters in the user/password are escaped correctly.
func connString(conn core.ConnectionInfo, dbName string) string {
	port := conn.Port
	if port == 0 {
		port = DefaultPort
	}
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(conn.User, conn.Password),
		Host:   fmt.Sprintf("%s:%d", conn.Host, port),
		Path:   "/" + dbName,
	}
	if sslmode := conn.Param("sslmode", ""); sslmode != "" {
		q := url.Values{}
		q.Set("sslmode", sslmode)
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// Ping verifies the server is reachable and the credentials work, by
// connecting to the maintenance database (default "postgres", overridable
// via conn.Params["maintenanceDB"]) rather than conn.DBName — the point of
// Ping is to catch host/auth problems before we've even determined which
// requested databases exist.
func (p *Provider) Ping(ctx context.Context, conn core.ConnectionInfo) error {
	if conn.User == "" {
		return fmt.Errorf("postgres: User is required")
	}
	c, err := pgx.Connect(ctx, connString(conn, maintenanceDB(conn)))
	if err != nil {
		return fmt.Errorf("postgres: cannot connect to %s:%d: %w", conn.Host, portOrDefault(conn.Port), err)
	}
	defer c.Close(ctx)
	return c.Ping(ctx)
}

// CreateDatabase creates a new, empty database named conn.DBName, using
// server defaults for encoding/owner/collation. Used by Restore when
// asked to create the target database itself — deliberately implemented
// as a plain CREATE DATABASE rather than delegating to `pg_restore
// --create`, since that always names the database after whatever the
// archive was originally dumped from, not whatever name was requested
// here.
func (p *Provider) CreateDatabase(ctx context.Context, conn core.ConnectionInfo) error {
	c, err := pgx.Connect(ctx, connString(conn, maintenanceDB(conn)))
	if err != nil {
		return fmt.Errorf("postgres: cannot connect to %s:%d: %w", conn.Host, portOrDefault(conn.Port), err)
	}
	defer c.Close(ctx)

	ident := pgx.Identifier{conn.DBName}.Sanitize()
	if _, err := c.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		return fmt.Errorf("postgres: creating database %q: %w", conn.DBName, err)
	}
	return nil
}

// ListDatabases returns every non-template database on the server.
func (p *Provider) ListDatabases(ctx context.Context, conn core.ConnectionInfo) ([]string, error) {
	c, err := pgx.Connect(ctx, connString(conn, maintenanceDB(conn)))
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot connect to %s:%d: %w", conn.Host, portOrDefault(conn.Port), err)
	}
	defer c.Close(ctx)

	rows, err := c.Query(ctx, `SELECT datname FROM pg_catalog.pg_database WHERE NOT datistemplate ORDER BY datname`)
	if err != nil {
		return nil, fmt.Errorf("postgres: listing databases: %w", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("postgres: scanning database name: %w", err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

type tableRef struct{ schema, name string }

// Inspect connects to conn.DBName and reports every user table (schemas
// pg_catalog/information_schema/pg_toast* are excluded, matching what a
// backup actually captures) along with an exact row count for each,
// obtained via SELECT count(*). That's a full scan per table — accurate,
// but not cheap on large tables, which is why counts run concurrently
// (bounded by maxInspectConcurrency) rather than one at a time.
func (p *Provider) Inspect(ctx context.Context, conn core.ConnectionInfo) (core.DatabaseInspection, error) {
	result := core.DatabaseInspection{DBName: conn.DBName}
	if conn.DBName == "" {
		return result, fmt.Errorf("postgres: DBName is required")
	}

	pool, err := pgxpool.New(ctx, connString(conn, conn.DBName))
	if err != nil {
		return result, fmt.Errorf("postgres: cannot connect to database %q: %w", conn.DBName, err)
	}
	defer pool.Close()

	rows, err := pool.Query(ctx, `
		SELECT schemaname, tablename
		FROM pg_catalog.pg_tables
		WHERE schemaname NOT IN ('pg_catalog', 'information_schema')
		  AND schemaname NOT LIKE 'pg\_toast%'
		ORDER BY schemaname, tablename`)
	if err != nil {
		return result, fmt.Errorf("postgres: listing tables for %q: %w", conn.DBName, err)
	}
	var tables []tableRef
	for rows.Next() {
		var t tableRef
		if err := rows.Scan(&t.schema, &t.name); err != nil {
			rows.Close()
			return result, fmt.Errorf("postgres: scanning table row: %w", err)
		}
		tables = append(tables, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("postgres: listing tables for %q: %w", conn.DBName, err)
	}

	counts := make([]int64, len(tables))
	errs := make([]error, len(tables))
	sem := make(chan struct{}, maxInspectConcurrency)
	var wg sync.WaitGroup
	for i, t := range tables {
		wg.Add(1)
		go func(i int, t tableRef) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			counts[i], errs[i] = countRows(ctx, pool, t.schema, t.name)
		}(i, t)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return result, e
		}
	}

	schemaIdx := make(map[string]int)
	for i, t := range tables {
		idx, ok := schemaIdx[t.schema]
		if !ok {
			idx = len(result.Schemas)
			schemaIdx[t.schema] = idx
			result.Schemas = append(result.Schemas, core.SchemaInspection{Name: t.schema})
		}
		result.Schemas[idx].Tables = append(result.Schemas[idx].Tables, core.TableInspection{
			Schema: t.schema, Name: t.name, RowCount: counts[i],
		})
	}
	return result, nil
}

func countRows(ctx context.Context, pool *pgxpool.Pool, schema, table string) (int64, error) {
	ident := pgx.Identifier{schema, table}.Sanitize()
	var count int64
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+ident).Scan(&count); err != nil {
		return 0, fmt.Errorf("postgres: counting rows in %s: %w", ident, err)
	}
	return count, nil
}

func portOrDefault(port int) int {
	if port == 0 {
		return DefaultPort
	}
	return port
}

// archiveDBNameRe matches the "dbname:" line in a pg_restore -l header,
// e.g.:
//
//	;     dbname: appdb
var archiveDBNameRe = regexp.MustCompile(`(?m)^;\s*dbname:\s*(.+?)\s*$`)

// InspectArchive reads a backup file's own metadata without connecting to
// any server. For plain-format (.sql) dumps there's nothing reliable to
// read — pg_dump only embeds a CREATE DATABASE/database name when run
// with -C, which this tool doesn't do by default — so DBName comes back
// empty for those. For archive formats (custom/tar/directory), the
// original database name is read from `pg_restore -l`'s header, which
// pg_dump always records regardless of how the dump was invoked.
func (p *Provider) InspectArchive(ctx context.Context, path string) (core.ArchiveInfo, error) {
	format, err := resolveRestoreFormat("", path)
	if err != nil {
		return core.ArchiveInfo{}, err
	}
	if format == FormatPlain {
		return core.ArchiveInfo{Format: format}, nil
	}

	bin := firstNonEmpty(p.RestoreBin, "pg_restore")
	if err := requireBin(bin); err != nil {
		return core.ArchiveInfo{}, err
	}

	cmd := exec.CommandContext(ctx, bin, "-l", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return core.ArchiveInfo{}, fmt.Errorf("postgres: reading archive metadata from %q: %w: %s", path, err, stderr.String())
	}

	info := core.ArchiveInfo{Format: format}
	if m := archiveDBNameRe.FindStringSubmatch(stdout.String()); m != nil {
		info.DBName = m[1]
	}
	return info, nil
}
