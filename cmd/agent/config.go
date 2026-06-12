package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// config is the resolved runtime configuration for the agent.
type config struct {
	server   string
	token    string
	user     string
	machine  string
	pin      string
	interval int
	graceMin int
}

// validate returns a helpful error listing every required field that is unset.
func (c config) validate() error {
	var missing []string
	if c.server == "" {
		missing = append(missing, "server")
	}
	if c.token == "" {
		missing = append(missing, "token")
	}
	if c.user == "" {
		missing = append(missing, "user")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required settings: %s (set via flag, env, or config file)",
			strings.Join(missing, ", "))
	}
	return nil
}

// action is the one-shot mode an invocation requested, if any.
type action int

const (
	actionRun              action = iota // standalone: heartbeat + lock loop (talks to server directly)
	actionInstall                        // persist config + register the standalone logon task, then exit
	actionUninstall                      // remove the logon task + config, then exit
	actionVersion                        // print version and exit
	actionWriteConfig                    // write the resolved config to its file and exit
	actionService                        // run as the brain: Windows service owning accounting + lock state
	actionFace                           // run as the face: per-session UI that mirrors the brain over a pipe
	actionInstallService                 // persist config + install the brain Windows service, then exit
	actionUninstallService               // remove the service (and config), then exit
)

// resolved bundles everything resolveConfig produces.
type resolved struct {
	cfg        config
	configPath string
	action     action
}

// resolveConfig layers configuration sources with this precedence (highest
// first): command-line flag > environment variable > config file > built-in
// default. It is pure (args + getenv in, config out) so it is fully testable.
func resolveConfig(args []string, getenv func(string) string) (resolved, error) {
	host, _ := os.Hostname()

	// The config file location is itself resolved with the same precedence,
	// minus the file (it cannot point at itself): flag > env > OS default.
	configPath := firstNonEmpty(scanArgValue(args, "config"), getenv("REWARDD_CONFIG"), defaultConfigPath(getenv))
	file := readConfigFile(configPath)

	pick := func(envKey, fileKey, def string) string {
		return firstNonEmpty(getenv(envKey), file[fileKey], def)
	}

	var cfg config
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // caller decides how to surface errors

	cfgPathFlag := fs.String("config", configPath, "path to a key=value config file")
	fs.StringVar(&cfg.server, "server", pick("REWARDD_SERVER", "server", ""), "server base URL")
	fs.StringVar(&cfg.token, "token", pick("REWARDD_TOKEN", "token", ""), "API bearer token")
	fs.StringVar(&cfg.user, "user", pick("REWARDD_USER", "user", ""), "kid name")
	fs.StringVar(&cfg.machine, "machine", pick("REWARDD_MACHINE", "machine", host), "machine label")
	fs.StringVar(&cfg.pin, "pin", pick("REWARDD_PIN", "pin", ""), "local emergency PIN (offline only)")
	fs.IntVar(&cfg.interval, "interval", pickInt(pick("REWARDD_INTERVAL", "interval", ""), 20), "heartbeat seconds")
	fs.IntVar(&cfg.graceMin, "grace", pickInt(pick("REWARDD_GRACE", "grace", ""), 30), "minutes per offline emergency unlock")
	installFlag := fs.Bool("install", false, "persist config + register the standalone logon task, then exit")
	uninstallFlag := fs.Bool("uninstall", false, "remove the logon task + config, then exit")
	versionFlag := fs.Bool("version", false, "print version and exit")
	writeConfigFlag := fs.Bool("writeconfig", false, "write resolved settings to the config file and exit")
	serviceFlag := fs.Bool("service", false, "run as the brain (Windows service)")
	faceFlag := fs.Bool("face", false, "run as the face (per-session UI client of the service)")
	installSvcFlag := fs.Bool("install-service", false, "persist config + install the brain service, then exit")
	uninstallSvcFlag := fs.Bool("uninstall-service", false, "remove the brain service (+ config), then exit")

	if err := fs.Parse(args); err != nil {
		return resolved{}, err
	}
	// The MSI passes -machine "" when MACHINE is unset; treat that as "use hostname"
	// rather than an empty label (flag.Parse does not apply flag defaults for "").
	if cfg.machine == "" {
		cfg.machine = host
	}

	out := resolved{cfg: cfg, configPath: *cfgPathFlag}
	switch {
	case *versionFlag:
		out.action = actionVersion
	case *writeConfigFlag:
		out.action = actionWriteConfig
	case *uninstallSvcFlag:
		out.action = actionUninstallService
	case *installSvcFlag:
		out.action = actionInstallService
	case *serviceFlag:
		out.action = actionService
	case *faceFlag:
		out.action = actionFace
	case *uninstallFlag:
		out.action = actionUninstall
	case *installFlag:
		out.action = actionInstall
	}
	return out, nil
}

// writeConfigFile materializes the agent config to a key=value file, creating
// the parent directory. The MSI uses `agent.exe -writeconfig` so the installer
// never has to template a text file itself.
func writeConfigFile(path string, c config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# rewardd agent configuration (managed). Edit and restart the logon task.\n")
	write := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s = %s\n", k, v)
		}
	}
	write("server", c.server)
	write("token", c.token)
	write("user", c.user)
	write("machine", c.machine)
	write("pin", c.pin)
	fmt.Fprintf(&b, "interval = %d\n", c.interval)
	fmt.Fprintf(&b, "grace = %d\n", c.graceMin)
	// 0600: the file holds the API token. On Windows the bits are advisory;
	// restrict the parent directory's ACL via the installer for real control.
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// defaultConfigPath is where the agent looks for its config file when no flag
// or env var overrides it. On Windows this is the directory the MSI writes to.
func defaultConfigPath(getenv func(string) string) string {
	if runtime.GOOS == "windows" {
		base := getenv("ProgramData")
		if base == "" {
			base = `C:\ProgramData`
		}
		return filepath.Join(base, "rewardd", "agent.conf")
	}
	return "/etc/rewardd/agent.conf"
}

// readConfigFile parses a simple "key = value" file. Blank lines and lines
// starting with '#' are ignored; surrounding whitespace and matching quotes are
// stripped. A missing or unreadable file yields an empty map (not an error):
// the file is always optional.
func readConfigFile(path string) map[string]string {
	out := map[string]string{}
	if path == "" {
		return out
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.ToLower(key))
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		if key != "" {
			out[key] = val
		}
	}
	return out
}

// scanArgValue extracts the value of a flag from a raw argument slice without a
// full parse, supporting "-name value", "-name=value" and their "--" forms. It
// lets us discover -config before building the flag set whose defaults depend
// on the file -config points at.
func scanArgValue(args []string, name string) string {
	forms := []string{"-" + name, "--" + name}
	for i := 0; i < len(args); i++ {
		a := args[i]
		for _, f := range forms {
			if a == f {
				if i+1 < len(args) {
					return args[i+1]
				}
				return ""
			}
			if strings.HasPrefix(a, f+"=") {
				return a[len(f)+1:]
			}
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func pickInt(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}
