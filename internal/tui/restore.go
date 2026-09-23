package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"iko-dbsnap/core"
)

// RestoreConfig configures one interactive restore run. The pre-flight
// checks (core.PlanRestore) have already run by the time this is built:
// Conn.DBName is the resolved target, known not to exist yet.
type RestoreConfig struct {
	Provider    core.Provider
	Conn        core.ConnectionInfo
	Opts        core.RestoreOptions
	Plan        core.RestorePlan
	AutoConfirm bool
}

// RunRestore drives the interactive restore flow: show the plan, confirm,
// create the database and restore into it, then verify and summarize.
func RunRestore(parent context.Context, cfg RestoreConfig) error {
	insp, ok := cfg.Provider.(core.Inspector)
	if !ok {
		return fmt.Errorf("tui: provider %q does not support interactive discovery", cfg.Provider.Name())
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	m := newRestoreModel(ctx, cfg, insp)
	m.cancel = cancel
	program := tea.NewProgram(m)
	m.program = program

	final, err := program.Run()
	if err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	fm := final.(*restoreModel)

	switch {
	case fm.userAborted && fm.stage == rstageReport:
		return ErrAborted
	case fm.userAborted:
		return fmt.Errorf("aborted mid-restore — database %q may have been created and partially restored; drop it before retrying", cfg.Conn.DBName)
	case !fm.result.OK():
		return fmt.Errorf("restore of %q failed", cfg.Conn.DBName)
	}
	return nil
}

type restoreStage int

const (
	rstageReport restoreStage = iota
	rstageRestoring
	rstageVerifying
	rstageDone
)

type restoreModel struct {
	ctx     context.Context
	cancel  context.CancelFunc
	cfg     RestoreConfig
	insp    core.Inspector
	program *tea.Program
	spinner spinner.Model

	stage       restoreStage
	userAborted bool

	current string
	result  core.RestoreResult

	// postInsp/postInspErr cover the post-restore verification step:
	// after a successful restore, we query the new database's actual
	// schema/table/row-count shape directly, rather than just trusting
	// pg_restore's exit code — unlike backup, restore's target is a live
	// database we already have full access to, so there's no need to
	// settle for an unverified "it said it worked."
	postInsp    core.DatabaseInspection
	postInspErr error
}

func newRestoreModel(ctx context.Context, cfg RestoreConfig, insp core.Inspector) *restoreModel {
	return &restoreModel{
		ctx:     ctx,
		cfg:     cfg,
		insp:    insp,
		spinner: spinner.New(spinner.WithSpinner(spinner.Dot)),
	}
}

// --- messages ---

type rRestoreDoneMsg struct{ res core.RestoreResult }
type rVerifyMsg struct {
	insp core.DatabaseInspection
	err  error
}
type rProgressMsg core.ProgressEvent

// --- init / commands ---

func (m *restoreModel) Init() tea.Cmd {
	if m.cfg.AutoConfirm {
		return tea.Batch(m.spinner.Tick, m.startRestore())
	}
	return m.spinner.Tick
}

func (m *restoreModel) startRestore() tea.Cmd {
	m.stage = rstageRestoring
	opts := m.cfg.Opts
	opts.OnProgress = func(ev core.ProgressEvent) {
		m.program.Send(rProgressMsg(ev))
	}
	return func() tea.Msg {
		res, err := m.cfg.Provider.Restore(m.ctx, m.cfg.Conn, opts)
		if err != nil && res.Err == nil {
			res.Err = err
		}
		return rRestoreDoneMsg{res: res}
	}
}

func (m *restoreModel) verifyCmd() tea.Cmd {
	return func() tea.Msg {
		insp, err := m.insp.Inspect(m.ctx, m.cfg.Conn)
		return rVerifyMsg{insp: insp, err: err}
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

	case rProgressMsg:
		m.current = joinSchemaTable(msg.Schema, msg.Table)
		return m, nil

	case rRestoreDoneMsg:
		m.result = msg.res
		if !msg.res.OK() {
			m.stage = rstageDone
			return m, tea.Quit
		}
		m.stage = rstageVerifying
		return m, m.verifyCmd()

	case rVerifyMsg:
		m.postInsp = msg.insp
		m.postInspErr = msg.err
		m.stage = rstageDone
		return m, tea.Quit
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
			return m, m.startRestore()
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
	switch m.stage {
	case rstageReport:
		if m.userAborted {
			return m.renderTarget() + m.renderReport() + "\nAborted — nothing was created or restored.\n"
		}
		return m.renderTarget() + m.renderReport() + "\n" + styleBold.Render("Continue with restore? [y/N] ")
	case rstageRestoring:
		cur := m.current
		if cur == "" {
			cur = "starting..."
		}
		return m.renderTarget() + m.renderReport() + fmt.Sprintf("\n%s Restoring %s — %s\n", m.spinner.View(), m.cfg.Conn.DBName, cur)
	case rstageVerifying:
		return m.renderTarget() + m.renderReport() + fmt.Sprintf("\n%s Verifying %s...\n", m.spinner.View(), m.cfg.Conn.DBName)
	case rstageDone:
		return m.renderTarget() + m.renderReport() + "\n" + m.renderSummary()
	}
	return ""
}

// renderTarget shows which server and user this restore is running
// against — restoring into the wrong server is a much costlier mistake
// than backing up the wrong one, so this stays visible on every screen.
func (m *restoreModel) renderTarget() string {
	return styleDim.Render("Target server: "+connLabel(m.cfg.Conn)) + "\n\n"
}

func (m *restoreModel) renderReport() string {
	var b strings.Builder
	name := m.cfg.Conn.DBName
	b.WriteString(styleBold.Render("Restore plan") + "\n")
	note := ""
	switch {
	case m.cfg.Plan.NameFromArchive:
		note = " (name taken from the backup — pass -db to choose another)"
	case m.cfg.Plan.Archive.DBName != "" && m.cfg.Plan.Archive.DBName != name:
		note = fmt.Sprintf(" (backup was originally database %q)", m.cfg.Plan.Archive.DBName)
	}
	b.WriteString("  " + styleOK.Render(fmt.Sprintf("+ %s — new database, will be created%s", name, note)) + "\n")
	b.WriteString("      " + styleDim.Render("<- "+m.cfg.Opts.InputPath) + "\n")
	return b.String()
}

func (m *restoreModel) renderSummary() string {
	var b strings.Builder
	name := m.cfg.Conn.DBName
	dur := m.result.Duration.Round(1e6)
	b.WriteString(styleBold.Render("Summary") + "\n")

	if !m.result.OK() {
		b.WriteString("  " + styleFail.Render(fmt.Sprintf("✗ %s — FAILED (%s): %v", name, dur, m.result.Err)) + "\n")
		return b.String()
	}

	if m.postInspErr != nil {
		b.WriteString("  " + styleOK.Render("✓") + fmt.Sprintf(" %s — restored (%s)\n", name, dur))
		b.WriteString("      " + styleFail.Render("could not verify row counts: "+m.postInspErr.Error()) + "\n")
	} else {
		b.WriteString("  " + styleOK.Render("✓") + fmt.Sprintf(" %s — %d schemas, %d tables, %d rows restored (%s)\n",
			name, len(m.postInsp.Schemas), m.postInsp.TableCount(), m.postInsp.RowCount(), dur))
		for _, s := range m.postInsp.Schemas {
			b.WriteString("      " + styleDim.Render("schema "+s.Name) + "\n")
			for _, t := range s.Tables {
				b.WriteString(fmt.Sprintf("        %-40s %10d rows\n", t.Name, t.RowCount))
			}
		}
	}

	if len(m.result.Warnings) > 0 {
		b.WriteString("\n" + styleWarn.Render(fmt.Sprintf("⚠ %d statement(s) failed and were skipped:", len(m.result.Warnings))) + "\n")
		for _, w := range m.result.Warnings {
			b.WriteString("  - " + w + "\n")
		}
		b.WriteString(styleDim.Render("  Often harmless (e.g. a setting the target server's version doesn't know) — check the list above.") + "\n")
	}
	return b.String()
}
