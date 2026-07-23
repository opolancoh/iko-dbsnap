package tui

import (
	"context"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/bubbles/spinner"

	"iko-dbsnap/core"
)

// RestoreConfig configures one interactive restore run. Targets carry the
// caller's intent (which database, from which file, with which
// no-owner/format options) but never opts.Create — the flow decides per
// target, via core.DecideRestorePlan, whether creating the database is
// safe.
type RestoreConfig struct {
	Provider    core.Provider
	Targets     []core.RestoreTarget
	Concurrency int
	AutoConfirm bool
}

// RunRestore drives the interactive restore flow: connect, check each
// backup file and what it knows about its own original database, check
// which target databases already exist, confirm, restore, summarize.
func RunRestore(parent context.Context, cfg RestoreConfig) error {
	if len(cfg.Targets) == 0 {
		return fmt.Errorf("tui: no targets given")
	}
	insp, ok := cfg.Provider.(core.Inspector)
	if !ok {
		return fmt.Errorf("tui: provider %q does not support interactive discovery", cfg.Provider.Name())
	}
	archInsp, ok := cfg.Provider.(core.ArchiveInspector)
	if !ok {
		return fmt.Errorf("tui: provider %q does not support archive inspection", cfg.Provider.Name())
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	m := newRestoreModel(ctx, cfg, insp, archInsp)
	m.cancel = cancel
	program := tea.NewProgram(m)
	m.program = program

	final, err := program.Run()
	if err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	fm := final.(*restoreModel)

	switch {
	case fm.fatalErr != nil:
		return fm.fatalErr
	case fm.userAborted:
		return ErrAborted
	case fm.failedCount > 0:
		return fmt.Errorf("%d of %d database restore(s) failed", fm.failedCount, fm.attemptedCount)
	}
	return nil
}

type restoreStage int

const (
	rstagePinging restoreStage = iota
	rstageDiscovering
	rstageReport
	rstageRestoring
	rstageDone
)

type restoreDBState struct {
	conn core.ConnectionInfo
	opts core.RestoreOptions
	name string

	archiveInfo core.ArchiveInfo
	archiveErr  error

	exists bool
	plan   core.RestorePlan // valid once both archive check and db list are in

	restoring bool
	current   string
	done      bool
	result    core.RestoreResult

	// verifying/postInsp/postInspErr cover the post-restore verification
	// step: after a successful restore, we query the target database's
	// actual schema/table/row-count shape directly, rather than just
	// trusting pg_restore's exit code — unlike backup, restore's target
	// is a live database we already have full access to, so there's no
	// need to settle for an unverified "it said it worked."
	verifying   bool
	postInsp    core.DatabaseInspection
	postInspErr error
}

// blocked reports whether this target can't proceed at all (bad or
// unreadable backup file).
func (d *restoreDBState) blocked() bool {
	return d.archiveErr != nil
}

type restoreModel struct {
	ctx     context.Context
	cancel  context.CancelFunc
	cfg     RestoreConfig
	insp    core.Inspector
	arch    core.ArchiveInspector
	program *tea.Program
	spinner spinner.Model

	stage       restoreStage
	fatalErr    error
	userAborted bool

	dbs []*restoreDBState

	archivePending int
	dbListDone     bool
	existingDBs    map[string]bool

	restorePending int
	attemptedCount int
	failedCount    int
}

func newRestoreModel(ctx context.Context, cfg RestoreConfig, insp core.Inspector, arch core.ArchiveInspector) *restoreModel {
	m := &restoreModel{
		ctx:     ctx,
		cfg:     cfg,
		insp:    insp,
		arch:    arch,
		spinner: spinner.New(spinner.WithSpinner(spinner.Dot)),
	}
	for _, t := range cfg.Targets {
		m.dbs = append(m.dbs, &restoreDBState{conn: t.Conn, opts: t.Opts, name: t.Conn.DBName})
	}
	return m
}

// --- messages ---

type rPingMsg struct{ err error }
type rDBListMsg struct {
	names []string
	err   error
}
type rArchiveMsg struct {
	index int
	info  core.ArchiveInfo
	err   error
}
type rRestoreDoneMsg struct {
	index int
	res   core.RestoreResult
}
type rVerifyMsg struct {
	index int
	insp  core.DatabaseInspection
	err   error
}
type rProgressMsg core.ProgressEvent

// --- init / commands ---

func (m *restoreModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.pingCmd())
}

func (m *restoreModel) pingCmd() tea.Cmd {
	conn := m.dbs[0].conn
	return func() tea.Msg {
		return rPingMsg{err: m.insp.Ping(m.ctx, conn)}
	}
}

func (m *restoreModel) listCmd() tea.Cmd {
	conn := m.dbs[0].conn
	return func() tea.Msg {
		names, err := m.insp.ListDatabases(m.ctx, conn)
		return rDBListMsg{names: names, err: err}
	}
}

func (m *restoreModel) archiveCmd(index int, d *restoreDBState) tea.Cmd {
	path := d.opts.InputPath
	return func() tea.Msg {
		if stat, err := os.Stat(path); err != nil {
			return rArchiveMsg{index: index, err: fmt.Errorf("cannot read backup at %q: %w", path, err)}
		} else if stat.Size() == 0 && !stat.IsDir() {
			return rArchiveMsg{index: index, err: fmt.Errorf("backup at %q is empty", path)}
		}
		info, err := m.arch.InspectArchive(m.ctx, path)
		return rArchiveMsg{index: index, info: info, err: err}
	}
}

func (m *restoreModel) verifyCmd(index int, conn core.ConnectionInfo) tea.Cmd {
	return func() tea.Msg {
		insp, err := m.insp.Inspect(m.ctx, conn)
		return rVerifyMsg{index: index, insp: insp, err: err}
	}
}

func (m *restoreModel) startArchiveChecks() tea.Cmd {
	m.archivePending = len(m.dbs)
	cmds := make([]tea.Cmd, 0, len(m.dbs)+1)
	for i, d := range m.dbs {
		cmds = append(cmds, m.archiveCmd(i, d))
	}
	cmds = append(cmds, m.listCmd())
	return tea.Batch(cmds...)
}

// maybeAdvanceToReport moves past rstageDiscovering once both the server's
// database list and every target's archive check have come back.
func (m *restoreModel) maybeAdvanceToReport() (tea.Model, tea.Cmd) {
	if m.archivePending > 0 || !m.dbListDone {
		return m, nil
	}
	anyRunnable := false
	for _, d := range m.dbs {
		if d.archiveErr == nil {
			d.exists = m.existingDBs[d.name]
			d.plan = core.DecideRestorePlan(d.name, d.exists)
			anyRunnable = true
		}
	}
	if !anyRunnable {
		m.fatalErr = fmt.Errorf("none of the requested restores can proceed — see the report above")
		m.stage = rstageReport
		return m, tea.Quit
	}
	if m.cfg.AutoConfirm {
		return m, m.startRestores()
	}
	m.stage = rstageReport
	return m, nil
}

func (m *restoreModel) startRestores() tea.Cmd {
	m.stage = rstageRestoring

	var targets []core.RestoreTarget
	var indices []int
	for i, d := range m.dbs {
		if d.blocked() {
			continue
		}
		d.restoring = true
		m.restorePending++
		opts := d.opts
		opts.Create = d.plan == core.RestorePlanCreate
		opts.OnProgress = func(ev core.ProgressEvent) {
			m.program.Send(rProgressMsg(ev))
		}
		targets = append(targets, core.RestoreTarget{Conn: d.conn, Opts: opts})
		indices = append(indices, i)
	}
	if len(targets) == 0 {
		m.stage = rstageDone
		return tea.Quit
	}

	concurrency := m.cfg.Concurrency
	return func() tea.Msg {
		core.RestoreAllWithProgress(m.ctx, m.cfg.Provider, targets, concurrency, func(i int, res core.RestoreResult) {
			m.program.Send(rRestoreDoneMsg{index: indices[i], res: res})
		})
		return nil
	}
}

// --- update ---

func (m *restoreModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case rPingMsg:
		if msg.err != nil {
			m.fatalErr = fmt.Errorf("cannot connect to %s:%d: %w", m.dbs[0].conn.Host, portOf(m.dbs[0].conn), msg.err)
			return m, tea.Quit
		}
		m.stage = rstageDiscovering
		return m, m.startArchiveChecks()

	case rDBListMsg:
		if msg.err != nil {
			m.fatalErr = fmt.Errorf("listing databases: %w", msg.err)
			return m, tea.Quit
		}
		m.existingDBs = make(map[string]bool, len(msg.names))
		for _, n := range msg.names {
			m.existingDBs[n] = true
		}
		m.dbListDone = true
		return m.maybeAdvanceToReport()

	case rArchiveMsg:
		d := m.dbs[msg.index]
		d.archiveInfo = msg.info
		d.archiveErr = msg.err
		m.archivePending--
		return m.maybeAdvanceToReport()

	case rProgressMsg:
		for _, d := range m.dbs {
			if d.conn.DBName == msg.DBName && d.restoring {
				d.current = joinSchemaTable(msg.Schema, msg.Table)
			}
		}
		return m, nil

	case rRestoreDoneMsg:
		d := m.dbs[msg.index]
		d.restoring = false
		d.done = true
		d.result = msg.res
		m.attemptedCount++
		if !msg.res.OK() {
			m.failedCount++
			m.restorePending--
			if m.restorePending == 0 {
				m.stage = rstageDone
				return m, tea.Quit
			}
			return m, nil
		}
		d.verifying = true
		return m, m.verifyCmd(msg.index, d.conn)

	case rVerifyMsg:
		d := m.dbs[msg.index]
		d.verifying = false
		d.postInsp = msg.insp
		d.postInspErr = msg.err
		m.restorePending--
		if m.restorePending == 0 {
			m.stage = rstageDone
			return m, tea.Quit
		}
		return m, nil
	}
	return m, nil
}

func (m *restoreModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if key == "ctrl+c" {
		m.userAborted = true
		m.cancel()
		return m, tea.Quit
	}
	if m.stage == rstageReport {
		switch key {
		case "y", "Y", "enter":
			return m, m.startRestores()
		case "n", "N", "esc", "q":
			m.userAborted = true
			m.cancel()
			return m, tea.Quit
		}
	}
	return m, nil
}

// --- view ---

func (m *restoreModel) View() string {
	// Checked first, regardless of stage: a fatal error can be set while
	// still "in" an earlier stage (e.g. discovery finishes with nothing
	// runnable), and the reader needs to see why, not a frozen spinner.
	if m.fatalErr != nil && m.stage != rstageReport {
		return styleFail.Render("Error: "+m.fatalErr.Error()) + "\n"
	}

	switch m.stage {
	case rstagePinging:
		return fmt.Sprintf("%s Connecting to %s...\n", m.spinner.View(), connLabel(m.dbs[0].conn))
	case rstageDiscovering:
		return m.renderTarget() + fmt.Sprintf("%s Reading backup files and checking target databases...\n", m.spinner.View())
	case rstageReport:
		if m.fatalErr != nil {
			return m.renderTarget() + m.renderReport() + "\n" + styleFail.Render(m.fatalErr.Error()) + "\n"
		}
		return m.renderTarget() + m.renderReport() + "\n" + styleBold.Render("Continue with restore? [y/N] ")
	case rstageRestoring:
		return m.renderTarget() + m.renderReport() + "\n" + m.renderProgress()
	case rstageDone:
		return m.renderTarget() + m.renderSummary()
	}
	return ""
}

// renderTarget shows which server and user this restore is running
// against — restoring into the wrong server is a much costlier mistake
// than backing up the wrong one, so this stays visible on every screen
// past the initial connect, not just while connecting.
func (m *restoreModel) renderTarget() string {
	return styleDim.Render("Target server: "+connLabel(m.dbs[0].conn)) + "\n\n"
}

func (m *restoreModel) renderReport() string {
	var b strings.Builder
	b.WriteString(styleBold.Render("Restore plan") + "\n")
	for _, d := range m.dbs {
		switch {
		case d.archiveErr != nil:
			b.WriteString("  " + styleFail.Render(fmt.Sprintf("✗ %s — %v", d.name, d.archiveErr)) + "\n")
		case d.plan == core.RestorePlanCreate:
			note := ""
			if d.archiveInfo.DBName != "" && d.archiveInfo.DBName != d.name {
				note = fmt.Sprintf(" (backup was originally database %q)", d.archiveInfo.DBName)
			}
			b.WriteString("  " + styleOK.Render(fmt.Sprintf("✓ %s — does not exist, will be created%s", d.name, note)) + "\n")
		case d.plan == core.RestorePlanIntoExisting:
			b.WriteString("  " + styleOK.Render(fmt.Sprintf("✓ %s — exists, will restore into it", d.name)) + "\n")
		}
		b.WriteString("      " + styleDim.Render("<- "+d.opts.InputPath) + "\n")
	}
	return b.String()
}

func (m *restoreModel) renderProgress() string {
	var b strings.Builder
	b.WriteString(styleBold.Render("Restoring") + "\n")
	for _, d := range m.dbs {
		if d.blocked() {
			continue
		}
		switch {
		case d.verifying:
			b.WriteString(fmt.Sprintf("  %s %s — verifying...\n", m.spinner.View(), d.name))
		case d.done && d.result.OK():
			b.WriteString("  " + styleOK.Render(fmt.Sprintf("✓ %s — done in %s", d.name, d.result.Duration.Round(1e6))) + "\n")
		case d.done:
			b.WriteString("  " + styleFail.Render(fmt.Sprintf("✗ %s — failed: %v", d.name, d.result.Err)) + "\n")
		case d.restoring:
			cur := d.current
			if cur == "" {
				cur = "starting..."
			}
			b.WriteString(fmt.Sprintf("  %s %s — %s\n", m.spinner.View(), d.name, cur))
		}
	}
	return b.String()
}

func (m *restoreModel) renderSummary() string {
	if m.fatalErr != nil {
		return styleFail.Render("Error: "+m.fatalErr.Error()) + "\n"
	}
	if m.userAborted {
		return "Aborted — no restores were run.\n"
	}

	var b strings.Builder
	b.WriteString(styleBold.Render("Summary") + "\n")
	var ok, attempted int
	var totalRows int64
	for _, d := range m.dbs {
		if d.blocked() {
			b.WriteString("  " + styleFail.Render("✗ "+d.name+" — skipped, see plan above") + "\n")
			continue
		}
		attempted++
		icon, style := "✓", styleOK
		status := "restored"
		if !d.result.OK() {
			icon, style = "✗", styleFail
			status = "FAILED: " + d.result.Err.Error()
		} else {
			ok++
		}
		action := "into existing database"
		if d.plan == core.RestorePlanCreate {
			action = "into newly created database"
		}

		if !d.result.OK() {
			b.WriteString("  " + style.Render(icon) + fmt.Sprintf(" %s — %s, %s (%s)\n", d.name, action, status, d.result.Duration.Round(1e6)))
			continue
		}
		if d.postInspErr != nil {
			b.WriteString("  " + style.Render(icon) + fmt.Sprintf(" %s — %s, %s (%s)\n", d.name, action, status, d.result.Duration.Round(1e6)))
			b.WriteString("      " + styleFail.Render("could not verify row counts: "+d.postInspErr.Error()) + "\n")
			continue
		}
		b.WriteString("  " + style.Render(icon) + fmt.Sprintf(" %s — %s, %d schemas, %d tables, %d rows restored (%s)\n",
			d.name, action, len(d.postInsp.Schemas), d.postInsp.TableCount(), d.postInsp.RowCount(), d.result.Duration.Round(1e6)))
		for _, s := range d.postInsp.Schemas {
			b.WriteString("      " + styleDim.Render("schema "+s.Name) + "\n")
			for _, t := range s.Tables {
				b.WriteString(fmt.Sprintf("        %-40s %10d rows\n", t.Name, t.RowCount))
			}
		}
		totalRows += d.postInsp.RowCount()
	}
	b.WriteString(fmt.Sprintf("\nTotal: %d/%d databases restored, %d rows restored\n", ok, attempted, totalRows))
	return b.String()
}
