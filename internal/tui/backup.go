// Package tui drives the interactive backup flow: connect, discover which
// requested databases exist, inspect their schema/table/row-count shape,
// confirm with the user, run the backups, and show a final summary. It's
// wired to core.Provider/core.Inspector only, so it works with any
// provider that implements discovery — nothing here is Postgres-specific.
package tui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/lipgloss"

	"iko-dbsnap/core"
)

// ErrAborted is returned by Run when the user declines to continue at the
// confirmation step.
var ErrAborted = errors.New("aborted by user")

// Config configures one interactive backup run.
type Config struct {
	Provider core.Provider
	// Targets is one ConnectionInfo per requested database, sharing
	// Host/Port/User/Password/Params and differing only in DBName.
	Targets     []core.ConnectionInfo
	Opts        core.BackupOptions
	Concurrency int
	// AutoConfirm skips the confirmation prompt and proceeds straight
	// from the discovery report into backing up (for -yes).
	AutoConfirm bool
}

// Run drives the interactive flow to completion. It returns nil only if
// every requested database that was found backed up successfully. Errors
// distinguish: a hard failure before any backup ran (bad host/creds, or
// none of the requested databases exist), the user aborting at the
// confirm step (ErrAborted), or one or more backups failing.
func Run(parent context.Context, cfg Config) error {
	if len(cfg.Targets) == 0 {
		return fmt.Errorf("tui: no targets given")
	}
	insp, ok := cfg.Provider.(core.Inspector)
	if !ok {
		return fmt.Errorf("tui: provider %q does not support interactive discovery", cfg.Provider.Name())
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	m := newModel(ctx, cfg, insp)
	m.cancel = cancel
	program := tea.NewProgram(m)
	m.program = program

	final, err := program.Run()
	if err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	fm := final.(*model)

	switch {
	case fm.fatalErr != nil:
		return fm.fatalErr
	case fm.userAborted:
		return ErrAborted
	case fm.failedCount > 0:
		return fmt.Errorf("%d of %d database backup(s) failed", fm.failedCount, fm.attemptedCount)
	}
	return nil
}

type stage int

const (
	stagePinging stage = iota
	stageCheckingDBs
	stageInspecting
	stageReport
	stageBackingUp
	stageDone
)

type dbState struct {
	conn core.ConnectionInfo
	name string

	found bool

	inspecting bool
	insp       core.DatabaseInspection
	inspectErr error

	backing bool
	current string // "schema.table" pg_dump most recently reported starting

	done   bool
	result core.BackupResult
}

type model struct {
	ctx      context.Context
	cancel   context.CancelFunc
	cfg      Config
	insp     core.Inspector
	program  *tea.Program
	spinner  spinner.Model

	stage       stage
	fatalErr    error
	userAborted bool

	dbs    []*dbState
	byName map[string]*dbState

	inspectPending int
	backupPending  int

	attemptedCount int
	failedCount    int
}

func newModel(ctx context.Context, cfg Config, insp core.Inspector) *model {
	m := &model{
		ctx:     ctx,
		cfg:     cfg,
		insp:    insp,
		spinner: spinner.New(spinner.WithSpinner(spinner.Dot)),
		byName:  make(map[string]*dbState, len(cfg.Targets)),
	}
	for _, c := range cfg.Targets {
		d := &dbState{conn: c, name: c.DBName}
		m.dbs = append(m.dbs, d)
		m.byName[c.DBName] = d
	}
	return m
}

// --- messages ---

type pingMsg struct{ err error }
type dbListMsg struct {
	names []string
	err   error
}
type inspectMsg struct {
	name string
	insp core.DatabaseInspection
	err  error
}
type progressMsg core.ProgressEvent
type backupDoneMsg struct {
	name string
	res  core.BackupResult
}

// --- init / commands ---

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.pingCmd())
}

func (m *model) pingCmd() tea.Cmd {
	conn := m.dbs[0].conn
	return func() tea.Msg {
		return pingMsg{err: m.insp.Ping(m.ctx, conn)}
	}
}

func (m *model) listCmd() tea.Cmd {
	conn := m.dbs[0].conn
	return func() tea.Msg {
		names, err := m.insp.ListDatabases(m.ctx, conn)
		return dbListMsg{names: names, err: err}
	}
}

func (m *model) inspectCmd(d *dbState) tea.Cmd {
	conn := d.conn
	return func() tea.Msg {
		data, err := m.insp.Inspect(m.ctx, conn)
		return inspectMsg{name: conn.DBName, insp: data, err: err}
	}
}

// startBackups kicks off core.BackupAllWithProgress for every found
// database in the background, bridging its callbacks back into the
// bubbletea event loop via m.program.Send (safe for concurrent use from
// any goroutine, which is required here since multiple backups may be
// running at once).
func (m *model) startBackups() tea.Cmd {
	m.stage = stageBackingUp

	var targets []core.Target
	for _, d := range m.dbs {
		if d.found {
			d.backing = true
			m.backupPending++
			targets = append(targets, core.Target{Conn: d.conn})
		}
	}
	if len(targets) == 0 {
		m.stage = stageDone
		return tea.Quit
	}

	opts := m.cfg.Opts
	opts.OnProgress = func(ev core.ProgressEvent) {
		m.program.Send(progressMsg(ev))
	}
	concurrency := m.cfg.Concurrency

	return func() tea.Msg {
		core.BackupAllWithProgress(m.ctx, m.cfg.Provider, targets, opts, concurrency, func(_ int, res core.BackupResult) {
			m.program.Send(backupDoneMsg{name: res.DBName, res: res})
		})
		return nil
	}
}

// --- update ---

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case pingMsg:
		if msg.err != nil {
			m.fatalErr = fmt.Errorf("cannot connect to %s:%d: %w", m.dbs[0].conn.Host, portOf(m.dbs[0].conn), msg.err)
			return m, tea.Quit
		}
		m.stage = stageCheckingDBs
		return m, m.listCmd()

	case dbListMsg:
		if msg.err != nil {
			m.fatalErr = fmt.Errorf("listing databases: %w", msg.err)
			return m, tea.Quit
		}
		existing := make(map[string]bool, len(msg.names))
		for _, n := range msg.names {
			existing[n] = true
		}
		var cmds []tea.Cmd
		anyFound := false
		for _, d := range m.dbs {
			d.found = existing[d.name]
			if d.found {
				anyFound = true
				d.inspecting = true
				m.inspectPending++
				cmds = append(cmds, m.inspectCmd(d))
			}
		}
		if !anyFound {
			m.fatalErr = fmt.Errorf("none of the requested databases were found on %s:%d", m.dbs[0].conn.Host, portOf(m.dbs[0].conn))
			m.stage = stageReport
			return m, tea.Quit
		}
		m.stage = stageInspecting
		return m, tea.Batch(cmds...)

	case inspectMsg:
		d := m.byName[msg.name]
		d.inspecting = false
		d.insp = msg.insp
		d.inspectErr = msg.err
		m.inspectPending--
		if m.inspectPending > 0 {
			return m, nil
		}
		if m.cfg.AutoConfirm {
			return m, m.startBackups()
		}
		m.stage = stageReport
		return m, nil

	case progressMsg:
		if d, ok := m.byName[msg.DBName]; ok {
			d.current = joinSchemaTable(msg.Schema, msg.Table)
		}
		return m, nil

	case backupDoneMsg:
		d := m.byName[msg.name]
		d.backing = false
		d.done = true
		d.result = msg.res
		m.attemptedCount++
		if !msg.res.OK() {
			m.failedCount++
		}
		m.backupPending--
		if m.backupPending == 0 {
			m.stage = stageDone
			return m, tea.Quit
		}
		return m, nil
	}
	return m, nil
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if key == "ctrl+c" {
		m.userAborted = true
		m.cancel()
		return m, tea.Quit
	}
	if m.stage == stageReport {
		switch key {
		case "y", "Y", "enter":
			return m, m.startBackups()
		case "n", "N", "esc", "q":
			m.userAborted = true
			m.cancel()
			return m, tea.Quit
		}
	}
	return m, nil
}

func joinSchemaTable(schema, table string) string {
	if schema == "" {
		return table
	}
	return schema + "." + table
}

func portOf(c core.ConnectionInfo) int {
	if c.Port == 0 {
		return 5432
	}
	return c.Port
}

// connLabel formats a connection as "user@host:port" for display — used
// throughout both the backup and restore flows so it's always visible
// which server and user an operation is targeting, not just during the
// initial connect.
func connLabel(c core.ConnectionInfo) string {
	return fmt.Sprintf("%s@%s:%d", c.User, c.Host, portOf(c))
}

// --- view ---

var (
	styleBold = lipgloss.NewStyle().Bold(true)
	styleOK   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleFail = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleDim  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleWarn = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
)

func (m *model) View() string {
	// Checked first, regardless of stage: a fatal error can be set while
	// still "in" an earlier stage (e.g. discovery finishes with nothing
	// found), and the reader needs to see why, not a frozen spinner.
	if m.fatalErr != nil && m.stage != stageReport {
		return styleFail.Render("Error: "+m.fatalErr.Error()) + "\n"
	}

	switch m.stage {
	case stagePinging:
		return fmt.Sprintf("%s Connecting to %s...\n", m.spinner.View(), connLabel(m.dbs[0].conn))
	case stageCheckingDBs:
		return fmt.Sprintf("%s Looking for requested databases...\n", m.spinner.View())
	case stageInspecting:
		return m.renderDestination() + m.renderReport()
	case stageReport:
		if m.fatalErr != nil {
			return m.renderDestination() + m.renderReport() + "\n" + styleFail.Render(m.fatalErr.Error()) + "\n"
		}
		return m.renderDestination() + m.renderReport() + "\n" +
			styleBold.Render(fmt.Sprintf("Continue with backup to %s? [y/N] ", m.outputDir()))
	case stageBackingUp:
		return m.renderDestination() + m.renderReport() + "\n" + m.renderProgress()
	case stageDone:
		return m.renderDestination() + m.renderSummary()
	}
	return ""
}

// outputDir returns the configured backup destination, defaulting to "."
// the same way the postgres provider does when OutputDir is unset.
func (m *model) outputDir() string {
	if m.cfg.Opts.OutputDir == "" {
		return "."
	}
	return m.cfg.Opts.OutputDir
}

// renderDestination shows where backup files will land, resolved to an
// absolute path — the configured OutputDir alone is easy to misread when
// it's relative and you're not sure what directory the command was run
// from.
func (m *model) renderDestination() string {
	dir := m.outputDir()
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	target := styleDim.Render("Target server: " + connLabel(m.dbs[0].conn))
	dest := styleDim.Render(fmt.Sprintf("Backup destination: %s (%s)", dir, abs))
	return target + "\n" + dest + "\n\n"
}

func (m *model) renderReport() string {
	var b strings.Builder
	b.WriteString(styleBold.Render("Databases") + "\n")
	for _, d := range m.dbs {
		// renderReport is only reached after dbListMsg has set d.found for
		// every entry (stagePinging/stageCheckingDBs render their own text
		// instead), so !d.found here reliably means "checked, not present".
		switch {
		case !d.found:
			b.WriteString("  " + styleFail.Render("✗ "+d.name+" — not found") + "\n")
		case d.inspecting:
			b.WriteString(fmt.Sprintf("  %s %s — inspecting...\n", m.spinner.View(), d.name))
		case d.inspectErr != nil:
			b.WriteString("  " + styleFail.Render(fmt.Sprintf("✗ %s — inspect failed: %v", d.name, d.inspectErr)) + "\n")
		case d.found:
			b.WriteString("  " + styleOK.Render("✓ "+d.name) +
				fmt.Sprintf("  (%d schemas, %d tables, %d rows)\n", len(d.insp.Schemas), d.insp.TableCount(), d.insp.RowCount()))
			for _, s := range d.insp.Schemas {
				b.WriteString("      " + styleDim.Render("schema "+s.Name) + "\n")
				for _, t := range s.Tables {
					b.WriteString(fmt.Sprintf("        %-40s %10d rows\n", t.Name, t.RowCount))
				}
			}
		}
	}
	return b.String()
}

func (m *model) renderProgress() string {
	var b strings.Builder
	b.WriteString(styleBold.Render("Backing up") + "\n")
	for _, d := range m.dbs {
		if !d.found {
			continue
		}
		switch {
		case d.done && d.result.OK():
			b.WriteString("  " + styleOK.Render(fmt.Sprintf("✓ %s — done in %s -> %s", d.name, d.result.Duration.Round(1e6), d.result.FilePath)) + "\n")
		case d.done:
			b.WriteString("  " + styleFail.Render(fmt.Sprintf("✗ %s — failed: %v", d.name, d.result.Err)) + "\n")
		case d.backing:
			cur := d.current
			if cur == "" {
				cur = "starting..."
			}
			b.WriteString(fmt.Sprintf("  %s %s — %s\n", m.spinner.View(), d.name, cur))
		}
	}
	return b.String()
}

func (m *model) renderSummary() string {
	if m.fatalErr != nil {
		return styleFail.Render("Error: "+m.fatalErr.Error()) + "\n"
	}
	if m.userAborted {
		return "Aborted — no backups were run.\n"
	}

	var b strings.Builder
	b.WriteString(styleBold.Render("Summary") + "\n")
	var totalFound, totalBackedUp int64
	var okDBs, foundDBs int
	for _, d := range m.dbs {
		if !d.found {
			b.WriteString("  " + styleFail.Render("✗ "+d.name+" — not found, skipped") + "\n")
			continue
		}
		foundDBs++
		ok := d.result.OK()
		if ok {
			okDBs++
		}
		status := styleOK.Render("backed up")
		backedUp := d.insp.RowCount()
		if !ok {
			status = styleFail.Render("FAILED: " + d.result.Err.Error())
			backedUp = 0
		}
		icon := "✓"
		iconStyle := styleOK
		if !ok {
			icon, iconStyle = "✗", styleFail
		}
		b.WriteString("  " + iconStyle.Render(icon) + fmt.Sprintf(" %s — %d schemas, %d tables, %d rows found — %s\n",
			d.name, len(d.insp.Schemas), d.insp.TableCount(), d.insp.RowCount(), status))
		if ok {
			b.WriteString("      " + styleDim.Render("-> "+d.result.FilePath) + "\n")
		}
		for _, s := range d.insp.Schemas {
			b.WriteString("      " + styleDim.Render("schema "+s.Name) + "\n")
			for _, t := range s.Tables {
				got := int64(0)
				if ok {
					got = t.RowCount
				}
				tableIcon, tableStyle := "✓", styleOK
				if got != t.RowCount {
					tableIcon, tableStyle = "✗", styleFail
				}
				b.WriteString(fmt.Sprintf("        %-40s found %10d   backed up %10d  %s\n",
					t.Name, t.RowCount, got, tableStyle.Render(tableIcon)))
			}
		}
		totalFound += d.insp.RowCount()
		totalBackedUp += backedUp
	}
	b.WriteString(fmt.Sprintf("\nTotal: %d/%d databases backed up, %d rows found, %d rows backed up\n",
		okDBs, foundDBs, totalFound, totalBackedUp))
	return b.String()
}
