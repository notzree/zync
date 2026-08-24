// Package tui is the interactive lease dashboard. It is a pure frontend over
// the ops package: everything it does is also available as a CLI subcommand.
package tui

import (
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/notzree/zync/internal/cliconf"
	"github.com/notzree/zync/internal/ops"
	"github.com/notzree/zync/internal/protocol"
)

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212")).Padding(0, 1)
	helpStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Padding(0, 1)
	msgStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("114")).Padding(0, 1)
	errStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Padding(0, 1)
	baseStyle  = lipgloss.NewStyle().BorderStyle(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("240"))
)

type leasesMsg struct {
	leases []protocol.LeaseInfo
	err    error
}

type actionMsg struct {
	verb string
	err  error
}

type tickMsg time.Time

type model struct {
	o        *ops.Ops
	registry cliconf.Registry
	table    table.Model
	leases   []protocol.LeaseInfo
	status   string
	isErr    bool
	busy     bool
}

func Run(o *ops.Ops) error {
	reg, err := cliconf.LoadRegistry()
	if err != nil {
		return err
	}
	columns := []table.Column{
		{Title: "Workspace", Width: 18},
		{Title: "Branch", Width: 16},
		{Title: "State", Width: 10},
		{Title: "Holder", Width: 18},
		{Title: "Gen", Width: 5},
		{Title: "Updated (UTC)", Width: 20},
	}
	t := table.New(table.WithColumns(columns), table.WithFocused(true), table.WithHeight(12))
	s := table.DefaultStyles()
	s.Header = s.Header.Bold(true).BorderStyle(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("240")).BorderBottom(true)
	s.Selected = s.Selected.Foreground(lipgloss.Color("229")).Background(lipgloss.Color("57")).Bold(true)
	t.SetStyles(s)

	m := model{o: o, registry: reg, table: t, status: "loading..."}
	_, err = tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

func (m model) fetchLeases() tea.Cmd {
	return func() tea.Msg {
		leases, err := m.o.Client.ListLeases()
		return leasesMsg{leases: leases, err: err}
	}
}

func tick() tea.Cmd {
	return tea.Tick(3*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.fetchLeases(), tick())
}

func (m model) selectedLease() *protocol.LeaseInfo {
	i := m.table.Cursor()
	if i < 0 || i >= len(m.leases) {
		return nil
	}
	return &m.leases[i]
}

func (m model) runAction(verb string, lease protocol.LeaseInfo, force bool) tea.Cmd {
	dir, ok := m.registry[lease.Workspace]
	if !ok {
		return func() tea.Msg {
			return actionMsg{verb: verb, err: fmt.Errorf("workspace %q has no local checkout on this replica (run `zync clone %s`)", lease.Workspace, lease.Workspace)}
		}
	}
	return func() tea.Msg {
		var err error
		switch verb {
		case "take":
			err = m.o.Take(dir, lease.Branch, force)
		case "handoff":
			err = m.o.Handoff(dir, lease.Branch)
		}
		return actionMsg{verb: verb, err: err}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "r":
			return m, m.fetchLeases()
		case "t", "T":
			if m.busy {
				return m, nil
			}
			if l := m.selectedLease(); l != nil {
				m.busy = true
				m.status = fmt.Sprintf("taking %s/%s...", l.Workspace, l.Branch)
				m.isErr = false
				return m, m.runAction("take", *l, msg.String() == "T")
			}
		case "h":
			if m.busy {
				return m, nil
			}
			if l := m.selectedLease(); l != nil {
				m.busy = true
				m.status = fmt.Sprintf("handing off %s/%s...", l.Workspace, l.Branch)
				m.isErr = false
				return m, m.runAction("handoff", *l, false)
			}
		}
	case tickMsg:
		if m.busy {
			return m, tick()
		}
		return m, tea.Batch(m.fetchLeases(), tick())
	case leasesMsg:
		if msg.err != nil {
			m.status, m.isErr = "hub error: "+msg.err.Error(), true
			return m, nil
		}
		m.leases = msg.leases
		rows := make([]table.Row, len(msg.leases))
		for i, l := range msg.leases {
			holder := l.Holder
			if holder == m.o.Client.Replica() {
				holder = holder + " *"
			}
			rows[i] = table.Row{l.Workspace, l.Branch, l.State, holder, fmt.Sprint(l.Generation), l.UpdatedAt}
		}
		m.table.SetRows(rows)
		if !m.isErr {
			m.status = fmt.Sprintf("%d lease(s) - refreshed %s", len(msg.leases), time.Now().Format("15:04:05"))
		}
		return m, nil
	case actionMsg:
		m.busy = false
		if msg.err != nil {
			m.status, m.isErr = msg.verb+" failed: "+msg.err.Error(), true
		} else {
			m.status, m.isErr = msg.verb+" succeeded", false
		}
		return m, m.fetchLeases()
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m model) View() string {
	status := msgStyle.Render(m.status)
	if m.isErr {
		status = errStyle.Render(m.status)
	}
	return titleStyle.Render(fmt.Sprintf("zync - replica %q", m.o.Client.Replica())) + "\n" +
		baseStyle.Render(m.table.View()) + "\n" +
		status + "\n" +
		helpStyle.Render("t take - T force-take - h handoff - r refresh - q quit  (* = this replica)")
}
