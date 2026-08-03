package cli

import (
	"fmt"
	"os"

	"github.com/charmbracelet/lipgloss"
)

// Centralized styling for the CLI. lipgloss's default renderer auto-detects the
// TTY and honors NO_COLOR / non-TTY output (it strips all ANSI when piped), so
// callers never need to check for a terminal themselves.

// Status glyphs, centralized so every command renders the same symbols.
const (
	glyphUp        = "●"
	glyphDown      = "○"
	glyphUnclaimed = "◆"
	glyphOK        = "✓"
	glyphFail      = "✗"
)

var (
	// Status colors.
	styleUp        = lipgloss.NewStyle().Foreground(lipgloss.Color("42")) // green
	styleDown      = lipgloss.NewStyle().Faint(true)                      // dim/gray
	styleUnclaimed = lipgloss.NewStyle().Foreground(lipgloss.Color("214")) // amber

	// Field styles.
	stylePort = lipgloss.NewStyle().Foreground(lipgloss.Color("44")) // cyan
	styleName = lipgloss.NewStyle().Bold(true)
	styleURL  = lipgloss.NewStyle().Foreground(lipgloss.Color("246")) // soft gray
	styleTier = lipgloss.NewStyle().Faint(true)

	// Section headers and summaries.
	styleSummary = lipgloss.NewStyle().Faint(true)
	styleBrand   = lipgloss.NewStyle().Bold(true)

	// Feedback.
	styleOK   = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	styleFail = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Bold(true) // red
	styleDim  = lipgloss.NewStyle().Faint(true)
)

// isTTY reports whether stdout is an interactive terminal.
func isTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// progress prints an inline "⏳ <verb> <name>…" line (only on a TTY). Call
// progressClear afterwards to erase it before printing the final result.
func progress(verb, name string) {
	if !isTTY() {
		return
	}
	fmt.Fprintf(os.Stdout, "%s %s %s…", styleDim.Render("⏳"), verb, styleName.Render(name))
}

func progressClear() {
	if !isTTY() {
		return
	}
	fmt.Fprint(os.Stdout, "\r\033[K")
}
