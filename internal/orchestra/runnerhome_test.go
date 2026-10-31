package orchestra

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestRunnerHomeCreatesPrivateFilesWithoutOverwriting(t *testing.T) {
	home := t.TempDir()
	dir, created, err := EnsureRunnerHome(home)
	if err != nil || len(created) != 2 {
		t.Fatalf("setup: %q %v %v", dir, created, err)
	}
	for _, name := range []string{"config.toml", "AGENTS.md"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("%s: %v %v", name, info, err)
		}
	}
	info, _ := os.Stat(dir)
	if info.Mode().Perm() != 0700 {
		t.Fatal(info.Mode())
	}
	config := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(config, []byte("user configuration"), 0600); err != nil {
		t.Fatal(err)
	}
	_, created, err = EnsureRunnerHome(home)
	if err != nil || len(created) != 0 {
		t.Fatalf("second setup: %v %v", created, err)
	}
	b, _ := os.ReadFile(config)
	if string(b) != "user configuration" {
		t.Fatal("existing file overwritten")
	}
}

func TestRunnerHomeRejectsSymlink(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(home, ".codex-runner")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnsureRunnerHome(home); err == nil {
		t.Fatal("symlinked runner home accepted")
	}
}

func TestRunnerLoginReportsOnlySanitizedState(t *testing.T) {
	home := t.TempDir()
	binary := filepath.Join(home, "fake-codex")
	script := "#!/bin/sh\ncase \"$CODEX_HOME\" in */.codex-runner) ;; *) exit 42;; esac\ncase \"$TEST_LOGIN_MODE\" in chatgpt) echo 'Logged in using ChatGPT user@example.test secret';; api) echo 'Logged in using an API key';; none) echo 'Not logged in'; exit 1;; nonezero) echo 'Not logged in'; exit 0;; *) echo 'private error user@example.test'; exit 2;; esac\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ input, state, mode string }{{"chatgpt", "logged_in", "ChatGPT"}, {"api", "logged_in", "API key"}, {"none", "not_logged_in", ""}, {"nonezero", "not_logged_in", ""}, {"broken", "unknown", ""}} {
		t.Setenv("TEST_LOGIN_MODE", tc.input)
		got := RunnerLogin(home, binary)
		if got.State != tc.state || got.Mode != tc.mode || strings.Contains(got.Detail, "example.test") || strings.Contains(got.Detail, "secret") {
			t.Fatalf("%s: %+v", tc.input, got)
		}
	}
}

func TestDefaultCodexNeverFallsBackToDesktopHome(t *testing.T) {
	home := t.TempDir()
	codex := DefaultAgents(home)["codex"]
	if len(codex.Env) != 1 || codex.Env[0] != "CODEX_HOME="+filepath.Join(home, ".codex-runner") {
		t.Fatalf("missing isolated home: %#v", codex.Env)
	}
}

func TestRunnerConfigCarriesOnlySafeSettingsFromUserConfig(t *testing.T) {
	home := t.TempDir()
	codexDir := filepath.Join(home, ".codex")
	os.MkdirAll(codexDir, 0o700)
	os.MkdirAll(filepath.Join(home, "go", "pkg", "mod"), 0o700)
	user := `model = "m-1"
model_provider = "corp"
notify = ["/bin/echo"]
experimental_bearer_token = "SECRET-1"
[model_providers.corp]
name = "Corp"
base_url = "https://llm.example.test/v1"
wire_api = "responses"
http_headers = { Authorization = "Bearer SECRET-2" }
env_key = "CORP_KEY"
[features]
respect_system_proxy = true
memories = true
[shell_environment_policy.set]
HTTPS_PROXY = "http://127.0.0.1:3128"
OPENAI_API_KEY = "SECRET-3"
[mcp_servers.x]
command = "/bin/x"
`
	os.WriteFile(filepath.Join(codexDir, "config.toml"), []byte(user), 0o600)
	os.WriteFile(filepath.Join(codexDir, "AGENTS.md"), []byte("# Mine\nKeep this.\n<!-- agent-bridge:start -->\nbridge stuff\n<!-- agent-bridge:end -->\nTail.\n"), 0o600)
	dir, _, err := EnsureRunnerHome(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	got := string(cfg)
	var parsed map[string]any
	if _, err := toml.Decode(got, &parsed); err != nil {
		t.Fatalf("generated config is not valid TOML: %v\n%s", err, got)
	}
	for _, want := range []string{`model = "m-1"`, `model_provider = "corp"`, `base_url = "https://llm.example.test/v1"`, `HTTPS_PROXY = "http://127.0.0.1:3128"`, `respect_system_proxy = true`, `sandbox_mode = "workspace-write"`, `approval_policy = "never"`, filepath.Join(home, "go", "pkg", "mod")} {
		if !strings.Contains(got, want) {
			t.Errorf("config lacks %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"SECRET", "http_headers", "env_key", "notify", "mcp_servers", "memories", ".cargo"} {
		if strings.Contains(got, bad) {
			t.Errorf("config must not carry %q:\n%s", bad, got)
		}
	}
	agents, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if a := string(agents); !strings.Contains(a, "Keep this.") || !strings.Contains(a, "Tail.") || strings.Contains(a, "bridge stuff") || !strings.Contains(a, "# Codex runner") {
		t.Fatalf("AGENTS.md:\n%s", a)
	}
}

func TestRunnerConfigWithoutUserConfig(t *testing.T) {
	home := t.TempDir()
	dir, _, err := EnsureRunnerHome(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	if strings.Contains(string(cfg), "model_provider") || !strings.Contains(string(cfg), `sandbox_mode = "workspace-write"`) {
		t.Fatalf("defaults:\n%s", cfg)
	}
}
