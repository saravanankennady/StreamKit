package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	streamkit "elnova-streamkit.local/sdks/server-go"
)

// Development credentials, the same ones a server started with --dev accepts. They are public on purpose.
const (
	devURL    = "ws://127.0.0.1:7880"
	devKey    = "devkey"
	devSecret = "secret"
)

// globals are the flags every command takes.
type globals struct {
	url, apiKey, apiSecret, project string
	dev, json                       bool
}

// quietString is a string flag whose value never shows up as a default in help text. Global flags are registered again
// at each level of the command tree, so otherwise a secret given early would be printed by --help later.
type quietString struct{ p *string }

func (q quietString) String() string { return "" }

func (q quietString) Set(v string) error {
	*q.p = v
	return nil
}

func (g *globals) register(fs *flag.FlagSet) {
	fs.Var(quietString{&g.url}, "url", "`URL` of the server. Also STREAMKIT_URL")
	fs.Var(quietString{&g.apiKey}, "api-key", "API `KEY`. Also STREAMKIT_API_KEY")
	fs.Var(quietString{&g.apiSecret}, "api-secret", "API `SECRET`. Other users can see command lines, so prefer STREAMKIT_API_SECRET")
	fs.Var(quietString{&g.project}, "project", "`NAME` of a project from 'streamkit project add'")
	fs.BoolVar(&g.dev, "dev", g.dev, "use the development server and keys: "+devURL)
	fs.BoolVar(&g.json, "json", g.json, "print JSON")
	fs.BoolVar(&g.json, "j", g.json, "print JSON")
}

// credentials say which server to talk to, and how to sign for it.
type credentials struct {
	URL, APIKey string
	secret      string
}

// String keeps the secret out of anything that prints these.
func (c credentials) String() string { return fmt.Sprintf("{url:%s key:%s}", c.URL, c.APIKey) }

// credentials work out what to use, in this order: a project named with --project, then flags and the environment, then
// --dev, then the default project. Flags and the environment override single fields of the default project.
func (g *globals) credentials() (credentials, error) {
	st, err := loadStore()
	if err != nil {
		return credentials{}, err
	}
	fromProject := func(p *projectEntry) (credentials, error) {
		secret, err := st.secretOf(p)
		if err != nil {
			return credentials{}, err
		}
		return credentials{URL: p.URL, APIKey: p.APIKey, secret: secret}, nil
	}
	if g.project != "" {
		p := st.find(g.project)
		if p == nil {
			return credentials{}, fmt.Errorf("no project named %q. See 'streamkit project list'", g.project)
		}
		return fromProject(p)
	}

	out := credentials{
		URL:    firstSet(g.url, os.Getenv("STREAMKIT_URL")),
		APIKey: firstSet(g.apiKey, os.Getenv("STREAMKIT_API_KEY")),
		secret: firstSet(g.apiSecret, os.Getenv("STREAMKIT_API_SECRET")),
	}
	if out.URL != "" && out.APIKey != "" && out.secret != "" {
		return out, nil
	}
	if g.dev {
		return credentials{URL: firstSet(out.URL, devURL), APIKey: firstSet(out.APIKey, devKey), secret: firstSet(out.secret, devSecret)}, nil
	}
	if st.data.DefaultProject != "" {
		p := st.find(st.data.DefaultProject)
		if p == nil {
			return credentials{}, fmt.Errorf("the default project %q is not in the config", st.data.DefaultProject)
		}
		// A key on its own says nothing about which secret goes with it, so a project only fills in a whole pair.
		base, err := fromProject(p)
		if err != nil && out.APIKey == "" {
			return credentials{}, err
		}
		out.URL = firstSet(out.URL, base.URL)
		if out.APIKey == "" {
			out.APIKey, out.secret = base.APIKey, firstSet(out.secret, base.secret)
		}
	}
	return out, nil
}

func firstSet(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// needSigning requires a key and secret.
func (c credentials) needSigning() error {
	if c.APIKey == "" || c.secret == "" {
		return errors.New("no API key and secret. Set STREAMKIT_API_KEY and STREAMKIT_API_SECRET, use --project, or run 'streamkit project add'")
	}
	return nil
}

// needServer requires a key, a secret and an address.
func (c credentials) needServer() error {
	if err := c.needSigning(); err != nil {
		return err
	}
	if c.URL == "" {
		return errors.New("no server address. Set STREAMKIT_URL, use --url or --project, or run 'streamkit project add'")
	}
	return nil
}

// client returns SDK clients for the server these globals name.
func (g *globals) client() (*streamkit.Client, credentials, error) {
	creds, err := g.credentials()
	if err != nil {
		return nil, creds, err
	}
	if err := creds.needServer(); err != nil {
		return nil, creds, err
	}
	return streamkit.NewClient(creds.URL, creds.APIKey, creds.secret), creds, nil
}

func projectCommand() *command {
	return &command{
		name:  "project",
		usage: "add, list and remove the servers this CLI knows",
		subs:  []*command{projectAddCommand(), projectListCommand(), projectRemoveCommand(), projectSetDefaultCommand()},
	}
}

func projectAddCommand() *command {
	var secretEnv string
	var secretStdin, makeDefault bool
	return &command{
		name:  "add",
		usage: "add a project",
		args:  "NAME",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&secretEnv, "api-secret-env", "", "keep only the name of this environment `VARIABLE` in the config, and read the secret from it each time")
			fs.BoolVar(&secretStdin, "api-secret-stdin", false, "read the secret from standard input")
			fs.BoolVar(&makeDefault, "default", false, "make this the default project")
		},
		run: func(c *call) error {
			if len(c.args) != 1 {
				return usagef("give one project name")
			}
			name := c.args[0]
			serverURL := firstSet(c.global.url, os.Getenv("STREAMKIT_URL"))
			apiKey := firstSet(c.global.apiKey, os.Getenv("STREAMKIT_API_KEY"))
			if serverURL == "" || apiKey == "" {
				return usagef("--url and --api-key are required")
			}
			entry := &projectEntry{Name: name, URL: serverURL, APIKey: apiKey}
			st, err := loadStore()
			if err != nil {
				return err
			}
			switch {
			case secretEnv != "":
				entry.SecretEnv = secretEnv
			default:
				secret := firstSet(c.global.apiSecret, os.Getenv("STREAMKIT_API_SECRET"))
				if secretStdin {
					line, err := bufio.NewReader(c.stdin).ReadString('\n')
					if line = strings.TrimRight(line, "\r\n"); line == "" {
						if err != nil {
							return fmt.Errorf("could not read the secret from standard input: %w", err)
						}
						return errors.New("the secret on standard input is empty")
					}
					secret = line
				}
				if secret == "" {
					return usagef("give the secret with --api-secret-stdin, --api-secret-env, --api-secret or STREAMKIT_API_SECRET")
				}
				if entry.SecretSealed, err = st.seal(name, secret); err != nil {
					return err
				}
			}
			if existing := st.find(name); existing != nil {
				*existing = *entry
			} else {
				st.data.Projects = append(st.data.Projects, entry)
			}
			if makeDefault || st.data.DefaultProject == "" {
				st.data.DefaultProject = name
			}
			if err := st.save(); err != nil {
				return err
			}
			fmt.Fprintf(c.stderr, "Saved project %s\n", name)
			return nil
		},
	}
}

// projectView is what listing a project shows. The secret is not part of it, only where it is kept.
type projectView struct {
	Name         string `json:"name"`
	URL          string `json:"url"`
	APIKey       string `json:"api_key"`
	SecretSource string `json:"secret_source"`
	Default      bool   `json:"default"`
}

func projectListCommand() *command {
	return &command{
		name:    "list",
		aliases: []string{"ls"},
		usage:   "list projects",
		run: func(c *call) error {
			st, err := loadStore()
			if err != nil {
				return err
			}
			views := make([]projectView, 0, len(st.data.Projects))
			for _, p := range st.data.Projects {
				source := "sealed"
				if p.SecretEnv != "" {
					source = "env:" + p.SecretEnv
				}
				views = append(views, projectView{p.Name, p.URL, p.APIKey, source, p.Name == st.data.DefaultProject})
			}
			if c.global.json {
				return writeJSON(c.stdout, views)
			}
			if len(views) == 0 {
				fmt.Fprintln(c.stderr, "No projects yet. Add one with 'streamkit project add'.")
				return nil
			}
			tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tURL\tAPI KEY\tSECRET\tDEFAULT")
			for _, v := range views {
				mark := ""
				if v.Default {
					mark = "*"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", v.Name, v.URL, v.APIKey, v.SecretSource, mark)
			}
			return tw.Flush()
		},
	}
}

func projectRemoveCommand() *command {
	return &command{
		name:    "remove",
		aliases: []string{"rm"},
		usage:   "remove a project from the config",
		args:    "NAME",
		run: func(c *call) error {
			if len(c.args) != 1 {
				return usagef("give one project name")
			}
			st, err := loadStore()
			if err != nil {
				return err
			}
			kept := st.data.Projects[:0]
			found := false
			for _, p := range st.data.Projects {
				if p.Name == c.args[0] {
					found = true
					continue
				}
				kept = append(kept, p)
			}
			if !found {
				return fmt.Errorf("no project named %q", c.args[0])
			}
			st.data.Projects = kept
			if st.data.DefaultProject == c.args[0] {
				st.data.DefaultProject = ""
			}
			if err := st.save(); err != nil {
				return err
			}
			fmt.Fprintf(c.stderr, "Removed project %s\n", c.args[0])
			return nil
		},
	}
}

func projectSetDefaultCommand() *command {
	return &command{
		name:  "set-default",
		usage: "choose the project commands use when none is named",
		args:  "NAME",
		run: func(c *call) error {
			if len(c.args) != 1 {
				return usagef("give one project name")
			}
			st, err := loadStore()
			if err != nil {
				return err
			}
			if st.find(c.args[0]) == nil {
				return fmt.Errorf("no project named %q", c.args[0])
			}
			st.data.DefaultProject = c.args[0]
			if err := st.save(); err != nil {
				return err
			}
			fmt.Fprintf(c.stderr, "Default project is now %s\n", c.args[0])
			return nil
		},
	}
}

func writeJSON(w interface{ Write([]byte) (int, error) }, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
