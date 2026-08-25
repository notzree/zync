// Package tui is the interactive lease dashboard. It is a pure frontend over
// the ops package: everything it does is also available as a CLI subcommand.
package tui

import (
	"fmt"
	"os/exec"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/atotto/clipboard"
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
	leases   []protocol.LeaseInfo
	replicas []protocol.ReplicaInfo
	err      error
}

type actionMsg struct {
	verb string
	err  error
}

type execDoneMsg struct{ err error }

type tickMsg time.Time

type model struct {
	o        *ops.Ops
	registry cliconf.Registry
	table    table.Model
	leases   []protocol.LeaseInfo
	replicas []protocol.ReplicaInfo
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
		if err != nil {
			return leasesMsg{err: err}
		}
		replicas, err := m.o.Client.ListReplicas()
		return leasesMsg{leases: leases, replicas: replicas, err: err}
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

// openCommand opens the selected lease in opencode, following the lease:
//  1. held by this replica            -> local opencode in the local checkout
//  2. held by a replica advertising   -> attach to the holder's server
//     an opencode server
//  3. released, local checkout exists -> local opencode (take happens on
//     demand via the plugin when the agent edits)
//  4. otherwise                       -> attach to any advertising replica
func (m model) openCommand(lease protocol.LeaseInfo) (*exec.Cmd, string, error) {
	attach := func(url, workspacesDir string) (*exec.Cmd, string) {
		dir := path.Join(workspacesDir, lease.Workspace)
		argv := []string{"opencode", "attach", url, "--dir", dir}
		return exec.Command(argv[0], argv[1:]...), strings.Join(argv, " ")
	}
	local := func(dir string) (*exec.Cmd, string) {
		cmd := exec.Command("opencode")
		cmd.Dir = dir
		return cmd, "cd " + dir + " && opencode"
	}
	localDir, hasLocal := m.registry[lease.Workspace]

	if lease.State == "held" {
		if lease.Holder == m.o.Client.Replica() && hasLocal {
			cmd, s := local(localDir)
			return cmd, s, nil
		}
		if lease.HolderOpencodeURL != "" {
			cmd, s := attach(lease.HolderOpencodeURL, lease.HolderWorkspacesDir)
			return cmd, s, nil
		}
	}
	if hasLocal {
		cmd, s := local(localDir)
		return cmd, s, nil
	}
	candidates := make([]protocol.ReplicaInfo, 0)
	for _, r := range m.replicas {
		if r.OpencodeURL != "" {
			candidates = append(candidates, r)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
	if len(candidates) > 0 {
		cmd, s := attach(candidates[0].OpencodeURL, candidates[0].WorkspacesDir)
		return cmd, s, nil
	}
	return nil, "", fmt.Errorf("no local checkout of %q and no replica advertises an opencode server", lease.Workspace)
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
		case "o":
			if l := m.selectedLease(); l != nil {
				cmd, human, err := m.openCommand(*l)
				if err != nil {
					m.status, m.isErr = err.Error(), true
					return m, nil
				}
				m.status, m.isErr = "opencode closed - back in zync", false
				_ = human
				return m, tea.ExecProcess(cmd, func(err error) tea.Msg { return execDoneMsg{err} })
			}
		case "y":
			if l := m.selectedLease(); l != nil {
				_, human, err := m.openCommand(*l)
				if err != nil {
					m.status, m.isErr = err.Error(), true
					return m, nil
				}
				if err := clipboard.WriteAll(human); err != nil {
					m.status, m.isErr = "clipboard error: "+err.Error(), true
				} else {
					m.status, m.isErr = "copied: "+human, false
				}
				return m, nil
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
		m.replicas = msg.replicas
		rows := make([]table.Row, len(msg.leases))
		for i, l := range msg.leases {
			holder := "-"
			if l.State == "held" {
				holder = l.Holder
				if holder == m.o.Client.Replica() {
					holder = holder + " *"
				}
			} else if l.Holder != "" {
				holder = "- (last: " + l.Holder + ")"
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
	case execDoneMsg:
		if msg.err != nil {
			m.status, m.isErr = "opencode exited: "+msg.err.Error(), true
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
		helpStyle.Render("t take - T force-take - h handoff - o open in opencode - y copy open cmd - r refresh - q quit  (* = this replica)")
}
