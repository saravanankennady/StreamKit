// Command streamkit manages rooms, tokens, recordings and ingest on an Elnova StreamKit server from a terminal.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// version is this CLI's version.
const version = "0.1.0"

// command is one node in the tree: a group with subcommands, or a command that runs.
type command struct {
	name    string
	aliases []string
	usage   string
	args    string
	flags   func(fs *flag.FlagSet)
	run     func(c *call) error
	subs    []*command
}

// call is one run of a command.
type call struct {
	ctx    context.Context
	args   []string
	stdout io.Writer
	stderr io.Writer
	stdin  io.Reader
	global *globals
	// path is how the user reached this command, for messages.
	path string
}

// usageError is a mistake in how a command was called. It gets the command's usage after it.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error { return &usageError{fmt.Sprintf(format, args...)} }

// app is the whole command tree.
func app() *command {
	return &command{
		name:  "streamkit",
		usage: "manage an Elnova StreamKit server",
		subs: []*command{
			projectCommand(),
			tokenCommand(),
			roomCommand(),
			participantCommand(),
			dataCommand(),
			egressCommand(),
			ingressCommand(),
			webhookCommand(),
			perfCommand(),
			{
				name:  "version",
				usage: "print the version",
				run: func(c *call) error {
					fmt.Fprintln(c.stdout, "streamkit", version)
					return nil
				},
			},
		},
	}
}

func (cmd *command) find(name string) *command {
	for _, sub := range cmd.subs {
		if sub.name == name {
			return sub
		}
		for _, alias := range sub.aliases {
			if alias == name {
				return sub
			}
		}
	}
	return nil
}

func (cmd *command) printUsage(w io.Writer, path string, fs *flag.FlagSet) {
	if len(cmd.subs) > 0 {
		fmt.Fprintf(w, "%s: %s\n\nUsage: %s COMMAND [flags]\n\nCommands:\n", path, cmd.usage, path)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, sub := range cmd.subs {
			fmt.Fprintf(tw, "  %s\t%s\n", sub.name, sub.usage)
		}
		_ = tw.Flush()
		fmt.Fprintf(w, "\nRun '%s COMMAND --help' for more.\n", path)
		return
	}
	fmt.Fprintf(w, "%s: %s\n\nUsage: %s [flags] %s\n\nFlags:\n", path, cmd.usage, path, cmd.args)
	fs.SetOutput(w)
	fs.VisitAll(func(f *flag.Flag) {
		name, usage := flag.UnquoteUsage(f)
		dashes := "--"
		if len(f.Name) == 1 {
			dashes = "-"
		}
		line := "  " + dashes + f.Name
		if name != "" {
			line += " " + name
		}
		fmt.Fprintf(w, "%s\n      %s", line, usage)
		if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" && f.DefValue != "[]" {
			fmt.Fprintf(w, " (default %s)", f.DefValue)
		}
		fmt.Fprintln(w)
	})
}

// parseInterspersed lets flags and positional arguments come in any order.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

func (cmd *command) newFlagSet(path string, g *globals) *flag.FlagSet {
	fs := flag.NewFlagSet(path, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	g.register(fs)
	if cmd.flags != nil {
		cmd.flags(fs)
	}
	return fs
}

// execute walks the tree to the command named by args and runs it.
func (cmd *command) execute(c *call, path string, args []string) error {
	fs := cmd.newFlagSet(path, c.global)
	if len(cmd.subs) == 0 {
		positional, err := parseInterspersed(fs, args)
		if errors.Is(err, flag.ErrHelp) {
			cmd.printUsage(c.stdout, path, fs)
			return nil
		}
		if err != nil {
			fmt.Fprintf(c.stderr, "%s: %v\n\n", path, err)
			cmd.printUsage(c.stderr, path, fs)
			return errSilent
		}
		c.args, c.path = positional, path
		err = cmd.run(c)
		var usage *usageError
		if errors.As(err, &usage) {
			fmt.Fprintf(c.stderr, "%s: %s\n\n", path, usage.msg)
			cmd.printUsage(c.stderr, path, fs)
			return errSilent
		}
		return err
	}
	// A group's flags stop at the first word, which names a subcommand.
	err := fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		cmd.printUsage(c.stdout, path, fs)
		return nil
	}
	if err != nil {
		fmt.Fprintf(c.stderr, "%s: %v\n", path, err)
		return errSilent
	}
	rest := fs.Args()
	if len(rest) == 0 {
		cmd.printUsage(c.stderr, path, fs)
		return errSilent
	}
	if rest[0] == "help" {
		cmd.printUsage(c.stdout, path, fs)
		return nil
	}
	sub := cmd.find(rest[0])
	if sub == nil {
		fmt.Fprintf(c.stderr, "%s: unknown command %q\n\n", path, rest[0])
		cmd.printUsage(c.stderr, path, fs)
		return errSilent
	}
	return sub.execute(c, path+" "+sub.name, rest[1:])
}

// errSilent ends a run with a failure that has already been explained.
var errSilent = errors.New("failed")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c := &call{ctx: ctx, stdout: os.Stdout, stderr: os.Stderr, stdin: os.Stdin, global: &globals{}}
	err := app().execute(c, "streamkit", os.Args[1:])
	if err == nil {
		return
	}
	if !errors.Is(err, errSilent) {
		fmt.Fprintln(os.Stderr, "streamkit:", strings.TrimPrefix(err.Error(), "streamkit: "))
	}
	os.Exit(1)
}

// stringList is a flag that can be repeated, and takes comma-separated values.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*l = append(*l, part)
		}
	}
	return nil
}

// repeatedString is a flag that can be repeated, and keeps commas.
type repeatedString []string

func (l *repeatedString) String() string { return strings.Join(*l, ",") }

func (l *repeatedString) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// keyValues parses key=value pairs.
func keyValues(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		k, v, ok := strings.Cut(pair, "=")
		if !ok || k == "" {
			return nil, usagef("%q is not in key=value form", pair)
		}
		out[k] = v
	}
	return out, nil
}

// printProto writes a message as JSON, with the field names the protocol uses.
func printProto(w io.Writer, m proto.Message) error {
	data, err := protojson.MarshalOptions{UseProtoNames: true, Multiline: true, Indent: "  "}.Marshal(m)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}
