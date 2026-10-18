package main

import (
	"cmp"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
)

func printMailboxDiagnostics(output io.Writer, kind string, paths map[string]string, selected []string) {
	ids := selected
	all := selected == nil
	if all {
		ids = make([]string, 0, len(paths))
		for id := range paths {
			ids = append(ids, id)
		}
	} else {
		ids = slices.Clone(ids)
	}
	slices.SortFunc(ids, func(a, b string) int {
		if order := cmp.Compare(paths[a], paths[b]); order != 0 {
			return order
		}
		return cmp.Compare(a, b)
	})
	fmt.Fprintf(output, "verbose: %s Fastmail folders (%d)", kind, len(ids))
	if kind == "selected" && all {
		fmt.Fprint(output, " (all mail)")
	}
	fmt.Fprintln(output, ":")
	for _, id := range ids {
		fmt.Fprintf(output, "verbose:   path=%q id=%q\n", paths[id], id)
	}
}

func printMailboxSelectorHints(output io.Writer, selectors []string) {
	fmt.Fprintln(output, "verbose: mailbox selectors are case-sensitive full paths; exact paths select one folder; terminal /* selects descendants, excluding the parent.")
	for _, selector := range selectors {
		if before, ok := strings.CutSuffix(selector, "/"); ok {
			fmt.Fprintf(output, "verbose: selector %q ends in /; remove the trailing slash for an exact folder (%q), or append * for descendants (%q).\n", selector, before, selector+"*")
		}
	}
}

// Resolve display paths without changing configuration or file operations.
func diagnosticPath(path string) string {
	if absolute, err := filepath.Abs(path); err == nil {
		return absolute
	}
	return path
}
