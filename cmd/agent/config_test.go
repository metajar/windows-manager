package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noEnv is a getenv that returns nothing, so tests are isolated from the host.
func noEnv(string) string { return "" }

func envFromMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveDefaults(t *testing.T) {
	res, err := resolveConfig([]string{"-config", "/nonexistent/none.conf"}, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if res.action != actionRun {
		t.Fatalf("action=%v, want actionRun", res.action)
	}
	if res.cfg.interval != 20 || res.cfg.graceMin != 30 {
		t.Fatalf("defaults: interval=%d grace=%d, want 20/30", res.cfg.interval, res.cfg.graceMin)
	}
}

func TestResolvePrecedenceFlagOverEnvOverFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.conf")
	os.WriteFile(path, []byte("server = http://from-file:8080\ntoken = file-token\nuser = file-user\n"), 0o644)

	env := envFromMap(map[string]string{
		"REWARDD_CONFIG": path,
		"REWARDD_TOKEN":  "env-token",
	})
	// Flag overrides env+file for server; env overrides file for token; file
	// supplies user since neither flag nor env set it.
	res, err := resolveConfig([]string{"-server", "http://flag:9090"}, env)
	if err != nil {
		t.Fatal(err)
	}
	cfg := res.cfg
	if cfg.server != "http://flag:9090" {
		t.Fatalf("server=%q, want flag value", cfg.server)
	}
	if cfg.token != "env-token" {
		t.Fatalf("token=%q, want env value", cfg.token)
	}
	if cfg.user != "file-user" {
		t.Fatalf("user=%q, want file value", cfg.user)
	}
}

func TestResolveConfigPathViaFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.conf")
	os.WriteFile(path, []byte("# comment\nuser = leo\ninterval = 45\npin = \"1234\"\n"), 0o644)

	res, err := resolveConfig([]string{"-config", path}, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := res.cfg
	if cfg.user != "leo" || cfg.interval != 45 || cfg.pin != "1234" {
		t.Fatalf("file parse: user=%q interval=%d pin=%q", cfg.user, cfg.interval, cfg.pin)
	}
}

func TestResolveActions(t *testing.T) {
	cases := []struct {
		args []string
		want action
	}{
		{[]string{"-install"}, actionInstall},
		{[]string{"-version"}, actionVersion},
		{[]string{"-writeconfig"}, actionWriteConfig},
		{[]string{}, actionRun},
	}
	for _, c := range cases {
		res, err := resolveConfig(c.args, noEnv)
		if err != nil {
			t.Fatal(err)
		}
		if res.action != c.want {
			t.Fatalf("args %v: action=%v, want %v", c.args, res.action, c.want)
		}
	}
}

func TestWriteConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.conf")
	in := config{server: "http://h:8080", token: "tok", user: "leo", machine: "PC", pin: "1234", interval: 25, graceMin: 45}
	if err := writeConfigFile(path, in); err != nil {
		t.Fatal(err)
	}
	res, err := resolveConfig([]string{"-config", path}, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	got := res.cfg
	if got.server != in.server || got.token != in.token || got.user != in.user ||
		got.machine != in.machine || got.pin != in.pin || got.interval != in.interval || got.graceMin != in.graceMin {
		t.Fatalf("round trip mismatch:\n in=%+v\nout=%+v", in, got)
	}
}

func TestValidateReportsMissing(t *testing.T) {
	err := config{}.validate()
	if err == nil {
		t.Fatal("expected error for empty config")
	}
	for _, want := range []string{"server", "token", "user"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err.Error(), want)
		}
	}
	if err := (config{server: "s", token: "t", user: "u"}).validate(); err != nil {
		t.Fatalf("complete config should validate, got %v", err)
	}
}

func TestScanArgValue(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"-config", "a.conf"}, "a.conf"},
		{[]string{"-config=b.conf"}, "b.conf"},
		{[]string{"--config", "c.conf"}, "c.conf"},
		{[]string{"--config=d.conf"}, "d.conf"},
		{[]string{"-server", "x"}, ""},
	}
	for _, c := range cases {
		if got := scanArgValue(c.args, "config"); got != c.want {
			t.Fatalf("scanArgValue(%v) = %q, want %q", c.args, got, c.want)
		}
	}
}

func TestReadConfigFileMissingIsEmpty(t *testing.T) {
	if m := readConfigFile("/does/not/exist.conf"); len(m) != 0 {
		t.Fatalf("missing file should yield empty map, got %v", m)
	}
}
