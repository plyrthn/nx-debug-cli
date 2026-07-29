// Package tui is a terminal UI for target management and application
// lifecycle commands, built on internal/htc (the same library cmd/nxdbg's
// plain CLI mode uses). Run is what cmd/nxdbg calls when it's invoked with
// no arguments.
//
// Everything the user can trigger lives in the action registry in
// actions.go; this file is the model, the key routing and the rendering.
package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/plyrthn/nx-debug-cli/internal/htc"
)

// Run starts the TUI and blocks until the user quits.
func Run() error {
	_, err := tea.NewProgram(initialModel(), tea.WithAltScreen()).Run()
	return err
}

// ---- styles ----

var (
	styleTitle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("219")).Padding(0, 1)
	styleHelp     = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	styleSelected = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("219"))
	styleRow      = lipgloss.NewStyle()
	styleHeader   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("245"))
	stylePanel    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
	styleErr      = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	styleOk       = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleFaint    = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
)

// ---- messages ----

// peersMsg is the target list read off the HTCS control port.
type peersMsg struct {
	targets []htc.Target
	err     error
}

// panelMsg fills the detail panel. Anything with more than a line of output
// goes here rather than into the one-line status.
type panelMsg struct {
	title string
	body  string
}

// actionResultMsg is a one-line outcome: a confirmation or a failure.
type actionResultMsg struct {
	text string
	err  error
}

// ---- model ----

type model struct {
	err     error
	loading bool

	targets []htc.Target
	cursor  int

	// serve is a session this TUI started because nothing else was running.
	// It is stopped on the way out, so quitting does not leave a process
	// holding the devkit's USB interface.
	serve *os.Process
	// triedServe stops a failing session start from being retried forever.
	triedServe bool
	// serveErr is what reading the control port said, kept while a session
	// is being started so that a box where neither works reports both.
	serveErr error
	// serveLog is where the started session's output went, for when it needs
	// explaining.
	serveLog string

	panelTitle string
	panelBody  string
	statusMsg  string
	statusErr  bool

	// streamLines/streamDone are set while a Streamed command (install,
	// currently the only one) is running, so Update can keep asking for the
	// next line - see stream.go.
	streamLines <-chan string
	streamDone  <-chan error

	// palette is the catalog-driven command list, which is where everything
	// that has no hotkey lives.
	palette palette

	width, height int
}

func initialModel() model {
	return model{loading: true}
}

func (m model) Init() tea.Cmd {
	return loadPeersCmd()
}

// loadPeersCmd reads the target list straight off the HTCS control port,
// which `nxdbg serve` publishes. Without this the TUI has nothing to talk
// to.
func loadPeersCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		snap, err := htc.PortMap(ctx, htc.ControlAddr())
		if err != nil {
			return peersMsg{err: err}
		}
		targets := make([]htc.Target, 0, len(snap.Targets))
		for _, t := range snap.Targets {
			targets = append(targets, htc.Target{
				Name:                t.Peer,
				UniqueIdentifier:    t.Peer,
				HardwareType:        t.PeerType,
				CommunicationMethod: "htcs",
			})
		}
		return peersMsg{targets: targets}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case peersMsg:
		m.loading = false
		if msg.err != nil {
			// Nothing is serving the control port. Rather than stopping to
			// say so, start a session: it is this same binary, and nothing
			// else can be holding the devkit or something would have
			// answered.
			if !m.triedServe {
				m.triedServe = true
				m.loading = true
				m.statusMsg = "nothing running: starting a session over USB..."
				m.statusErr = false
				m.serveErr = msg.err
				return m, startServeCmd()
			}
			m.err = fmt.Errorf("%w\n\nand starting one failed too, so there is nothing to talk to", m.serveErr)
			return m, nil
		}
		m.err = nil
		m.targets = msg.targets
		if m.cursor >= len(m.targets) {
			m.cursor = 0
		}
		if m.serve != nil {
			m.statusMsg = "started a session over USB, and it stops when you quit"
			m.statusErr = false
		}
		return m, nil

	case serveStartedMsg:
		if msg.err != nil {
			m.loading = false
			m.err = fmt.Errorf("%w\n\nand starting one failed: %v", m.serveErr, msg.err)
			return m, nil
		}
		// Record the process before waiting on it, so quitting mid-start
		// still stops it rather than leaving it holding the USB interface.
		m.serve = msg.proc
		m.serveLog = msg.log
		m.statusMsg = "starting a session over USB, this takes a few seconds..."
		return m, waitForServeCmd(msg.exited, msg.log)

	case serveReadyMsg:
		m.loading = false
		if msg.err != nil {
			m.stopServe()
			m.serve = nil
			m.err = fmt.Errorf("%w\n\nand starting one failed: %v", m.serveErr, msg.err)
			return m, nil
		}
		// The session is up, so the target list it publishes can be read now.
		return m, loadPeersCmd()

	case panelMsg:
		m.panelTitle = msg.title
		m.panelBody = msg.body
		m.statusMsg = ""
		m.statusErr = false
		return m, nil

	case actionResultMsg:
		if msg.err != nil {
			m.statusMsg = msg.err.Error()
			m.statusErr = true
			return m, nil
		}
		m.statusMsg = msg.text
		if m.statusMsg == "" {
			m.statusMsg = "ok"
		}
		m.statusErr = false
		return m, nil

	case commandOutputMsg:
		if msg.err != nil {
			m.statusMsg = msg.err.Error()
			m.statusErr = true
			return m, nil
		}
		m.panelTitle, m.panelBody = msg.title, msg.body
		m.statusMsg = ""
		m.statusErr = false
		return m, nil

	case streamStartedMsg:
		if msg.err != nil {
			m.statusMsg = msg.err.Error()
			m.statusErr = true
			return m, nil
		}
		m.panelTitle, m.panelBody = msg.title, ""
		m.statusMsg = "running..."
		m.statusErr = false
		m.streamLines, m.streamDone = msg.lines, msg.done
		return m, waitForStreamLineCmd(msg.lines, msg.done)

	case streamLineMsg:
		if m.panelBody != "" {
			m.panelBody += "\n"
		}
		m.panelBody += msg.line
		return m, waitForStreamLineCmd(m.streamLines, m.streamDone)

	case streamDoneMsg:
		m.streamLines, m.streamDone = nil, nil
		if msg.err != nil {
			m.statusMsg = msg.err.Error()
			m.statusErr = true
			return m, nil
		}
		m.statusMsg = "ok"
		m.statusErr = false
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg.String())
	}
	return m, nil
}

func (m model) handleKey(key string) (tea.Model, tea.Cmd) {
	// The palette takes every key while it is up, including the ones that are
	// hotkeys otherwise - typing "q" into a filter must not quit.
	if next, cmd, handled := m.paletteKey(key); handled {
		return next, cmd
	}

	switch key {
	case "ctrl+c", "q":
		m.stopServe()
		return m, tea.Quit
	case ":":
		m.palette = palette{stage: paletteChoosing}
		return m, nil
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil
	case "down", "j":
		if m.cursor < len(m.targets)-1 {
			m.cursor++
		}
		return m, nil
	case "esc":
		m.panelTitle, m.panelBody = "", ""
		return m, nil
	}

	action, ok := lookupAction(key)
	if !ok {
		return m, nil
	}
	var target htc.Target
	if action.NeedsTarget {
		t, ok := m.selectedTarget()
		if !ok {
			m.statusMsg = "no target selected"
			m.statusErr = true
			return m, nil
		}
		target = t
	}
	m.statusMsg = action.Busy
	m.statusErr = false
	return m, action.Run(target)
}

func (m model) selectedTarget() (htc.Target, bool) {
	if m.cursor < 0 || m.cursor >= len(m.targets) {
		return htc.Target{}, false
	}
	return m.targets[m.cursor], true
}

func (m model) View() string {
	var b strings.Builder

	where := "  target management TUI  (" + htc.ControlAddr() + ")"
	if m.serve != nil {
		where = "  target management TUI  (own session over USB, " + htc.ControlAddr() + ")"
	}
	b.WriteString(styleTitle.Render("nxdbg") + styleFaint.Render(where))
	b.WriteString("\n\n")

	if m.err != nil {
		// Wrap to the terminal. These messages say what to start, and that
		// part lands past column 80 in the default terminal, which is exactly
		// where it was getting cut off.
		b.WriteString(styleErr.Width(m.errWidth()).Render("error: "+m.err.Error()) + "\n")
		b.WriteString(styleHelp.Render("q: quit"))
		return b.String()
	}

	if m.loading {
		b.WriteString("loading...\n")
		return b.String()
	}

	if len(m.targets) == 0 {
		b.WriteString("no targets connected\n\n")
	} else {
		header := fmt.Sprintf("  %-18s %-18s %s", "SERIAL", "HARDWARE", "TRANSPORT")
		b.WriteString(styleHeader.Render(truncate(header, m.errWidth())) + "\n")
		for i, t := range m.targets {
			row := fmt.Sprintf("  %-18s %-18s %s", truncate(t.Name, 18), truncate(t.HardwareType, 18), t.CommunicationMethod)
			// Clip rather than wrap. The selected row carries a background
			// colour, and a wrapped one paints a second ragged line of it.
			row = truncate(row, m.errWidth())
			if i == m.cursor {
				b.WriteString(styleSelected.Render(row) + "\n")
			} else {
				b.WriteString(styleRow.Render(row) + "\n")
			}
		}
	}

	// The palette replaces the panel while it is up rather than stacking below
	// it, so a long panel can't push the list off the screen.
	switch {
	case m.palette.stage != paletteClosed:
		b.WriteString("\n")
		b.WriteString(stylePanel.Render(m.paletteView()))
		b.WriteString("\n")
	case m.panelBody != "":
		b.WriteString("\n")
		body := m.panelBody
		if m.panelTitle != "" {
			body = styleHeader.Render(m.panelTitle) + "\n" + body
		}
		b.WriteString(stylePanel.Render(m.clipPanel(body)))
		b.WriteString("\n")
	}

	if m.statusMsg != "" {
		b.WriteString("\n")
		if m.statusErr {
			b.WriteString(styleErr.Render(m.statusMsg))
		} else {
			b.WriteString(styleOk.Render(m.statusMsg))
		}
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(styleHelp.Width(m.errWidth()).Render(helpLine()))

	return b.String()
}

// errWidth is what a message gets wrapped to. The window size arrives as a
// message, so it can still be zero on the first frame - which is when a
// connection error is most likely to be the thing being shown.
//
// The help line needs this as much as the errors do: it lists every hotkey and
// runs well past 80 columns, so without a width it spills off the side and the
// last few keys are simply invisible.
func (m model) errWidth() int {
	if m.width > 20 {
		return m.width - 2
	}
	return 78
}

// clipPanel keeps a long panel (a target log, mostly) from pushing the help
// line off the screen, showing the tail since that's the interesting end.
func (m model) clipPanel(body string) string {
	// Rows taken by the header, target table, status line and help.
	overhead := len(m.targets) + 9
	avail := m.height - overhead
	if m.height == 0 || avail < 3 {
		avail = 3
	}
	lines := strings.Split(body, "\n")
	if len(lines) <= avail {
		return body
	}
	return "…\n" + strings.Join(lines[len(lines)-avail:], "\n")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
