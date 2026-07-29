package tui

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/plyrthn/nx-debug-cli/internal/commands"
)

// The palette is how everything nxdbg can do is reachable from the TUI without
// eleven more keybindings. It is built from the command catalog, so a command
// added to the CLI shows up here with no second registration - which is the
// whole point: the two used to drift, and the TUI offered a tenth of what the
// CLI could do.
//
// Commands run by invoking this same binary rather than by calling into the
// library, for the same reason the video window does: the CLI already knows how
// to parse the arguments and format the output, and duplicating that here would
// be a second implementation to keep in step.

// paletteRows is how many commands are listed at once. More than this and the
// list pushes the target table off a normal terminal.
const paletteRows = 12

// paletteStage is where in the run-a-command flow the palette is.
type paletteStage int

const (
	paletteClosed paletteStage = iota
	paletteChoosing
	paletteArguments
	paletteConfirming
)

type palette struct {
	stage  paletteStage
	filter string
	cursor int

	// chosen is the command being filled in, once one is picked.
	chosen commands.Command
	args   string
}

// paletteCommands is every command the palette offers, in catalog order.
func paletteCommands() []commands.Command { return commands.All() }

// matches filters on the command path and its summary, so both "reboot" and
// "power off" find what you would expect.
func (p palette) matches() []commands.Command {
	all := paletteCommands()
	if p.filter == "" {
		return all
	}
	needle := strings.ToLower(p.filter)
	var out []commands.Command
	for _, c := range all {
		hay := strings.ToLower(c.Path() + " " + c.Summary)
		if strings.Contains(hay, needle) {
			out = append(out, c)
		}
	}
	return out
}

func (p palette) selected() (commands.Command, bool) {
	m := p.matches()
	if p.cursor < 0 || p.cursor >= len(m) {
		return commands.Command{}, false
	}
	return m[p.cursor], true
}

// handleKey routes a keystroke while the palette is up. It returns the updated
// model and a command, plus whether it consumed the key at all.
func (m model) paletteKey(key string) (model, tea.Cmd, bool) {
	if m.palette.stage == paletteClosed {
		return m, nil, false
	}
	switch m.palette.stage {
	case paletteChoosing:
		return m.paletteChooseKey(key)
	case paletteArguments:
		return m.paletteArgumentKey(key)
	case paletteConfirming:
		return m.paletteConfirmKey(key)
	}
	return m, nil, false
}

func (m model) paletteChooseKey(key string) (model, tea.Cmd, bool) {
	switch key {
	case "esc", "ctrl+c":
		m.palette = palette{}
		return m, nil, true
	case "up", "ctrl+p":
		if m.palette.cursor > 0 {
			m.palette.cursor--
		}
		return m, nil, true
	case "down", "ctrl+n":
		if m.palette.cursor < len(m.palette.matches())-1 {
			m.palette.cursor++
		}
		return m, nil, true
	case "backspace":
		if n := len(m.palette.filter); n > 0 {
			m.palette.filter = m.palette.filter[:n-1]
			m.palette.cursor = 0
		}
		return m, nil, true
	case "enter":
		cmd, ok := m.palette.selected()
		if !ok {
			return m, nil, true
		}
		return m.palettePick(cmd)
	}
	if text, ok := typedText(key); ok {
		m.palette.filter += text
		m.palette.cursor = 0
	}
	return m, nil, true
}

// typedText picks the keystrokes that are text out of the ones that are key
// names. "a" is a letter, "pgup" is not, and space is reported either way
// depending on the bubbletea version.
func typedText(key string) (string, bool) {
	if key == "space" {
		return " ", true
	}
	if len([]rune(key)) == 1 {
		return key, true
	}
	return "", false
}

// palettePick moves a chosen command to the next stage: asking for arguments
// if it takes any, confirming if it changes target state, or running it.
func (m model) palettePick(c commands.Command) (model, tea.Cmd, bool) {
	if _, ok := m.targetFor(c); !ok {
		m.palette = palette{}
		m.statusMsg = "no target selected, and " + c.Path() + " needs one"
		m.statusErr = true
		return m, nil, true
	}
	m.palette.chosen = c
	m.palette.args = ""
	if c.Args != "" {
		m.palette.stage = paletteArguments
		return m, nil, true
	}
	if c.Destructive {
		m.palette.stage = paletteConfirming
		return m, nil, true
	}
	return m.paletteRun()
}

func (m model) paletteArgumentKey(key string) (model, tea.Cmd, bool) {
	switch key {
	case "esc":
		m.palette.stage = paletteChoosing
		return m, nil, true
	case "ctrl+c":
		m.palette = palette{}
		return m, nil, true
	case "backspace":
		if n := len(m.palette.args); n > 0 {
			m.palette.args = m.palette.args[:n-1]
		}
		return m, nil, true
	case "enter":
		if m.palette.chosen.Destructive {
			m.palette.stage = paletteConfirming
			return m, nil, true
		}
		return m.paletteRun()
	}
	if text, ok := typedText(key); ok {
		m.palette.args += text
	}
	return m, nil, true
}

func (m model) paletteConfirmKey(key string) (model, tea.Cmd, bool) {
	switch key {
	case "y", "Y":
		return m.paletteRun()
	case "n", "N", "esc", "ctrl+c":
		m.palette = palette{}
		m.statusMsg = "cancelled"
		m.statusErr = false
		return m, nil, true
	}
	return m, nil, true
}

// paletteRun closes the palette and starts the command.
func (m model) paletteRun() (model, tea.Cmd, bool) {
	c := m.palette.chosen
	target, _ := m.targetFor(c)
	args := strings.Fields(m.palette.args)
	m.palette = palette{}
	m.statusMsg = "running " + c.Path() + "..."
	m.statusErr = false
	return m, runCommand(c, target, args), true
}

// targetFor works out what to pass the command as its target, and reports
// false when the command needs one and nothing is selected.
func (m model) targetFor(c commands.Command) (string, bool) {
	t, selected := m.selectedTarget()
	switch c.Target {
	case commands.SerialTarget:
		if !selected {
			return "", false
		}
		serial := targetSerial(t)
		if serial == "" {
			return "", false
		}
		return serial, true
	case commands.OptionalSerialTarget:
		if !selected {
			return "", true
		}
		return targetSerial(t), true
	}
	return "", true
}

// commandOutputMsg is a finished command's output.
type commandOutputMsg struct {
	title string
	body  string
	err   error
}

// runCommand invokes this binary with the command's own argument line.
//
// Most of the long-running ones (a window, a stream, a watch) are started
// and left alone: blocking the UI until the user closed one would wedge it.
// Their output goes nowhere, because a detached child writing to this
// terminal would scribble over the UI, so the message says how to see it.
// A Streamed one (install, currently the only one) is different - it opens
// no window of its own, so there's nothing else to look at, and its output
// is exactly the progress worth watching - so that one's fed into the panel
// live instead; see stream.go. Everything else is run to completion and its
// output goes in the panel all at once.
func runCommand(c commands.Command, target string, extra []string) tea.Cmd {
	return func() tea.Msg {
		self, err := selfPath()
		if err != nil {
			return actionResultMsg{err: fmt.Errorf("locate own binary: %w", err)}
		}
		argv := append(c.Argv(target), extra...)

		if c.Long && c.Streamed {
			return startStreamCmd(self, argv)()
		}

		if c.Long {
			// Stdout and Stderr are left nil deliberately, which sends them
			// to the null device rather than into this terminal.
			cmd := exec.Command(self, argv...)
			if err := cmd.Start(); err != nil {
				return actionResultMsg{err: err}
			}
			// Nothing waits on it: it belongs to the user now.
			go cmd.Wait()
			line := "nxdbg " + strings.Join(argv, " ")
			return panelMsg{
				title: "started in the background",
				body:  "  " + line + "\n\n" + "Its output is not shown here. Run the same line in a terminal\nto watch it.",
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, self, argv...).CombinedOutput()
		body := strings.TrimRight(string(out), "\r\n")
		if err != nil {
			if body == "" {
				return commandOutputMsg{err: err}
			}
			// The CLI already said what went wrong, in better words than
			// "exit status 1", so show that rather than the exit code.
			return commandOutputMsg{err: fmt.Errorf("%s", firstLine(body))}
		}
		if body == "" {
			return actionResultMsg{text: "nxdbg " + strings.Join(argv, " ") + ": ok"}
		}
		return commandOutputMsg{title: "nxdbg " + strings.Join(argv, " "), body: body}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// paletteView renders whichever stage the palette is in.
func (m model) paletteView() string {
	switch m.palette.stage {
	case paletteChoosing:
		return m.paletteListView()
	case paletteArguments:
		c := m.palette.chosen
		return styleHeader.Render("nxdbg "+c.Path()) + "\n" +
			styleFaint.Render(c.Usage()) + "\n\n" +
			"arguments: " + m.palette.args + "▌\n\n" +
			styleHelp.Render("enter: run   esc: back")
	case paletteConfirming:
		c := m.palette.chosen
		line := "nxdbg " + strings.Join(append(c.Argv(m.paletteTargetText()), strings.Fields(m.palette.args)...), " ")
		return styleHeader.Render("this changes the target") + "\n\n" +
			"  " + line + "\n  " + styleFaint.Render(c.Summary) + "\n\n" +
			styleHelp.Render("y: run it   n: cancel")
	}
	return ""
}

func (m model) paletteTargetText() string {
	target, _ := m.targetFor(m.palette.chosen)
	return target
}

func (m model) paletteListView() string {
	matches := m.palette.matches()
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s▌\n", styleHeader.Render("run:"), m.palette.filter)
	if len(matches) == 0 {
		b.WriteString(styleFaint.Render("  nothing matches") + "\n")
	}

	// Scroll so the cursor stays on screen without the list jumping around
	// when it does not have to.
	start := 0
	if m.palette.cursor >= paletteRows {
		start = m.palette.cursor - paletteRows + 1
	}
	end := start + paletteRows
	if end > len(matches) {
		end = len(matches)
	}

	width := 0
	for _, c := range matches[start:end] {
		if n := len(c.Path()); n > width {
			width = n
		}
	}
	for i := start; i < end; i++ {
		c := matches[i]
		row := fmt.Sprintf("  %-*s  %s", width, c.Path(), c.Summary)
		if i == m.palette.cursor {
			b.WriteString(styleSelected.Render(row) + "\n")
		} else {
			b.WriteString(styleRow.Render(row) + "\n")
		}
	}
	if len(matches) > end {
		fmt.Fprintf(&b, "%s\n", styleFaint.Render(fmt.Sprintf("  … %d more", len(matches)-end)))
	}
	b.WriteString("\n" + styleHelp.Render("type to filter   ↑/↓ select   enter: run   esc: close"))
	return b.String()
}

// paletteHint is the line in the main help that says the rest of the tool is
// in here. The count is worth stating: the hotkeys look like the whole of what
// the TUI can do, and they are a tenth of it.
func paletteHint() string {
	return fmt.Sprintf(":: all %d commands", len(paletteCommands()))
}
