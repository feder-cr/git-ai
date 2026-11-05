package main

import (
	"strings"

	"github.com/feder-cr/git-ai/internal/graft/displaytext"
)

// displayText returns s in a form that a terminal shows as text (displaytext.Text). --json output
// carries the exact text instead.
func displayText(s string) string {
	return displaytext.Text(s)
}

// displayList returns the items as one comma-separated line, each made safe
// with displayText.
func displayList(items []string) string {
	shown := make([]string, len(items))
	for i, item := range items {
		shown[i] = displayText(item)
	}

	return strings.Join(shown, ", ")
}
