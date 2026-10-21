package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// writeReference writes the Markdown CLI reference (docs/cli-reference.md):
// every visible command with its usage, description and options, taken from
// the same definitions as --help, so the two cannot disagree.
func writeReference(w io.Writer, root *cobra.Command) {
	fmt.Fprintf(w, "# %s command reference\n\n", root.Name())
	fmt.Fprintf(w, "Generated from the CLI's own help text; do not edit by hand. "+
		"Regenerate with `go test ./internal/cli -run TestCLIReference -update`. "+
		"For how the options work together, see the [CLI guide](cli-guide.md).\n\n")
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w)
	commands := visibleCommands(root)
	for _, cmd := range commands {
		fmt.Fprintf(w, "- [`%s`](#%s) — %s\n", cmd.CommandPath(), anchor(cmd.CommandPath()), cmd.Short)
	}
	fmt.Fprintln(w)
	if flags := visibleFlags(root.PersistentFlags()); len(flags) > 0 {
		fmt.Fprintln(w, "## Global options")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "These work with every command.")
		fmt.Fprintln(w)
		writeFlagTable(w, flags)
	}
	for _, cmd := range commands {
		fmt.Fprintf(w, "## %s\n\n", cmd.CommandPath())
		description := cmd.Long
		if description == "" {
			description = cmd.Short
		}
		fmt.Fprintf(w, "%s\n\n", strings.TrimSpace(description))
		fmt.Fprintf(w, "```text\n%s\n```\n\n", cmd.UseLine())
		if len(cmd.Aliases) > 0 {
			fmt.Fprintf(w, "Aliases: %s\n\n", "`"+strings.Join(cmd.Aliases, "`, `")+"`")
		}
		if flags := visibleFlags(cmd.LocalNonPersistentFlags()); len(flags) > 0 {
			writeFlagTable(w, flags)
		}
	}
}

// visibleCommands lists the commands below root, depth first, without
// hidden ones and Cobra's built-in help.
func visibleCommands(root *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	for _, cmd := range root.Commands() {
		if cmd.Hidden || cmd.Name() == "help" || cmd.Name() == "completion" {
			continue
		}
		if cmd.Runnable() {
			out = append(out, cmd)
		}
		out = append(out, visibleCommands(cmd)...)
	}
	return out
}

// visibleFlags lists flags alphabetically, without hidden ones and Cobra's
// --help.
func visibleFlags(set *pflag.FlagSet) []*pflag.Flag {
	var out []*pflag.Flag
	set.VisitAll(func(f *pflag.Flag) {
		if !f.Hidden && f.Name != "help" {
			out = append(out, f)
		}
	})
	return out
}

func writeFlagTable(w io.Writer, flags []*pflag.Flag) {
	fmt.Fprintln(w, "| Option | Default | Description |")
	fmt.Fprintln(w, "| --- | --- | --- |")
	for _, f := range flags {
		name := "--" + f.Name
		if f.Shorthand != "" {
			name = "-" + f.Shorthand + ", " + name
		}
		if kind := f.Value.Type(); kind != "bool" {
			name += " " + kind
		}
		fmt.Fprintf(w, "| `%s` | %s | %s |\n", name, defaultText(f), escapeCell(f.Usage))
	}
	fmt.Fprintln(w)
}

// defaultText shows a flag's default, or nothing when it is the zero value.
func defaultText(f *pflag.Flag) string {
	switch f.DefValue {
	case "", "0", "false", "[]":
		return ""
	}
	return "`" + f.DefValue + "`"
}

func escapeCell(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "|", `\|`), "\n", " ")
}

// anchor is the GitHub heading anchor for "## text".
func anchor(text string) string {
	return strings.ReplaceAll(strings.ToLower(text), " ", "-")
}
