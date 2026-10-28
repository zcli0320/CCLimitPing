package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitTestHomeMustHaveMarkerAndNeverFallsBack(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HELPER_TEST_HOME", root)
	if _, err := NewReader(); err == nil {
		t.Fatal("invalid test root silently read real user data")
	}
	writeFixture(t, filepath.Join(root, ".codex-helper-test-root"), "isolated-test")
	r, err := NewReader()
	expected, _ := filepath.EvalSymlinks(root)
	if err != nil || r.Home != expected {
		t.Fatalf("test root not applied: %v", err)
	}
}

func fixtureDB(t *testing.T, path, sql string) {
	t.Helper()
	if _, err := os.Stat("/usr/bin/sqlite3"); err != nil {
		t.Skip("sqlite3 not installed")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/usr/bin/sqlite3", path, sql).CombinedOutput(); err != nil {
		t.Fatalf("fixture sqlite: %v %s", err, out)
	}
}

func TestSnapshotProjectsMetadataAndTitlesWithoutCredentials(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".codex")
	cc := filepath.Join(home, ".cc-switch")
	writeFixture(t, filepath.Join(cc, "settings.json"), `{"currentProviderCodex":"p1"}`)
	writeFixture(t, filepath.Join(cc, "codex_oauth_auth.json"), `{"version":2,"default_account_id":"b","accounts":{"a":{"account_id":"a","email":"person@example.test","refresh_token":"REFRESH_SECRET","id_token":"JWT_SECRET"},"b":{"account_id":"b"}}}`)
	fixtureDB(t, filepath.Join(cc, "cc-switch.db"), `CREATE TABLE providers(id TEXT,name TEXT,app_type TEXT,meta TEXT,is_current INTEGER,sort_index INTEGER,settings_config TEXT);
INSERT INTO providers VALUES('p1','Official person@example.test','codex','{"authBinding":{"authProvider":"codex_oauth","accountId":"a"}}',0,1,'AUTH_SECRET');
INSERT INTO providers VALUES('p2','Backup','codex','{"authBinding":{"authProvider":"codex_oauth","accountId":"b"}}',1,2,'AUTH_SECRET');`)
	writeFixture(t, filepath.Join(root, "session_index.jsonl"), `{"id":"one","thread_name":"旧名字"}
{"id":"one","thread_name":"索引名字"}
{"id":"two","thread_name":"索引重命名"}
broken`)
	fixtureDB(t, filepath.Join(root, "state_5.sqlite"), `CREATE TABLE threads(id TEXT,title TEXT,first_user_message TEXT,cwd TEXT,updated_at INTEGER,source TEXT,archived INTEGER,rollout_path TEXT);
INSERT INTO threads VALUES('one','数据库重命名','原始问题','/project/一',30,'cli',0,'');
INSERT INTO threads VALUES('two','原始问题','原始问题','/project/二',20,'vscode',0,'');
INSERT INTO threads VALUES('three','','如何运行实验？','/project/三',10,'cli',0,'');
INSERT INTO threads VALUES('sub','隐藏','隐藏','/project',40,'{"subagent":{}}',0,'');`)
	r := Reader{Home: home, SQLite: "/usr/bin/sqlite3"}
	s := r.Snapshot(context.Background())
	if len(s.Accounts) != 2 || !s.Accounts[0].Selected || s.Accounts[1].Selected {
		t.Fatalf("selection must prefer settings: %+v", s.Accounts)
	}
	if len(s.Sessions) != 3 {
		t.Fatalf("sessions: %+v; warnings %v", s.Sessions, s.Warnings)
	}
	if s.Sessions[0].Title != "数据库重命名" || s.Sessions[1].Title != "索引重命名" || s.Sessions[2].Title != "如何运行实验？" {
		t.Fatalf("title precedence: %+v", s.Sessions)
	}
	for name, c := range s.Capabilities {
		if c.Available && name != "account_quota" && name != "account_warmup" && name != "desktop_resume" && name != "account_switch" && name != "cli_handoff" && name != "desktop_handoff" {
			t.Fatalf("unverified capability enabled: %s", name)
		}
	}
	if !s.Capabilities["cli_handoff"].Available || !s.Capabilities["desktop_handoff"].Available {
		t.Fatal("verified CLI and Desktop handoff capabilities unavailable")
	}
	data, _ := json.Marshal(s)
	for _, secret := range []string{"REFRESH_SECRET", "JWT_SECRET", "AUTH_SECRET", "person@example.test"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("sensitive field escaped into snapshot")
		}
	}
	before, _ := os.ReadFile(filepath.Join(cc, "codex_oauth_auth.json"))
	_ = r.Snapshot(context.Background())
	after, _ := os.ReadFile(filepath.Join(cc, "codex_oauth_auth.json"))
	if string(before) != string(after) {
		t.Fatal("credential source modified")
	}
}

func TestRolloutFallbackFiltersInjectedPromptsAndDeduplicates(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".codex")
	writeFixture(t, filepath.Join(root, "sessions", "rollout-one.jsonl"), `{"type":"session_meta","payload":{"id":"one","cwd":"/project","source":"cli"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions\nThis is injected"}]}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>injected</environment_context>"}]}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"真正的问题"}]}}
{"unfinished":`)
	writeFixture(t, filepath.Join(root, "sessions", "rollout-sub.jsonl"), `{"type":"session_meta","payload":{"id":"sub","source":{"subagent":{}}}}`)
	r := Reader{Home: home, SQLite: "/usr/bin/sqlite3"}
	sessions, warnings, complete := r.sessions(context.Background(), root, nil)
	if complete {
		t.Fatal("fallback cannot claim a complete authoritative list")
	}
	if len(sessions) != 1 || sessions[0].Title != "真正的问题" || sessions[0].ID != "one" {
		t.Fatalf("fallback: %+v", sessions)
	}
	if len(warnings) == 0 {
		t.Fatal("missing database must be visible")
	}
	key := sessions[0].Key
	writeFixture(t, filepath.Join(root, "session_index.jsonl"), `{"id":"one","thread_name":"新的标题"}`)
	sessions, _, _ = r.sessions(context.Background(), root, nil)
	if sessions[0].Title != "新的标题" || sessions[0].Key != key {
		t.Fatal("rename changed binding")
	}
	if sessionKey(filepath.Join(home, "isolated"), "one") == key {
		t.Fatal("isolated histories collided")
	}
}

func TestConfiguredHistoryAndSQLiteHome(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".codex")
	dbRoot := filepath.Join(home, "custom-db")
	ccRoot := filepath.Join(home, ".codex-cli")
	settings, _ := json.Marshal(map[string]string{"codexConfigDir": ccRoot})
	writeFixture(t, filepath.Join(home, ".cc-switch", "settings.json"), string(settings))
	writeFixture(t, filepath.Join(root, "config.toml"), "sqlite_home = \""+dbRoot+"\"\n")
	fixtureDB(t, filepath.Join(dbRoot, "state_5.sqlite"), `CREATE TABLE threads(id TEXT,title TEXT,first_user_message TEXT,cwd TEXT,updated_at INTEGER,source TEXT,archived INTEGER,rollout_path TEXT); INSERT INTO threads VALUES('x','','问题','/custom',1,'cli',0,'');`)
	s := (Reader{Home: home, SQLite: "/usr/bin/sqlite3"}).Snapshot(context.Background())
	if s.CodexHome != ccRoot || len(s.Sessions) != 1 || s.Sessions[0].Title != "问题" || s.Sessions[0].Scope != "desktop" {
		t.Fatalf("custom paths ignored: %+v", s)
	}
}

func TestDesktopAndCLIKeepDuplicateSessionIDsAndTitlesSeparate(t *testing.T) {
	home := t.TempDir()
	for _, dir := range []string{".codex", ".codex-cli"} {
		dbPath := filepath.Join(home, dir, "state_5.sqlite")
		if dir == ".codex-cli" {
			custom := filepath.Join(home, "custom-cli-db")
			writeFixture(t, filepath.Join(home, dir, "config.toml"), "sqlite_home = \""+custom+"\"\n")
			dbPath = filepath.Join(custom, "state_5.sqlite")
		}
		fixtureDB(t, dbPath, `CREATE TABLE threads(id TEXT,title TEXT,first_user_message TEXT,cwd TEXT,updated_at INTEGER,source TEXT,archived INTEGER,rollout_path TEXT);
INSERT INTO threads VALUES('same-id','', '问题','/shared/project',1,'cli',0,'');`)
		name := "Desktop renamed"
		if dir == ".codex-cli" {
			name = "CLI renamed"
		}
		writeFixture(t, filepath.Join(home, dir, "session_index.jsonl"), fmt.Sprintf(`{"id":"same-id","thread_name":%q}`, name))
	}
	s := (Reader{Home: home, SQLite: "/usr/bin/sqlite3"}).Snapshot(context.Background())
	if len(s.Sessions) != 2 || len(s.Sources) != 2 || !s.SessionsComplete {
		t.Fatalf("unexpected dual home result: %+v", s)
	}
	a, b := s.Sessions[0], s.Sessions[1]
	if a.Scope != "desktop" || b.Scope != "cli" || a.Key == b.Key || a.Title != "Desktop renamed" || b.Title != "CLI renamed" {
		t.Fatal("scopes, stable keys or title indexes were mixed")
	}
	if a.Key != sessionKey(filepath.Join(home, ".codex"), "same-id") {
		t.Fatal("existing Desktop selection must remain valid")
	}
	if a.Source != "CLI（历史来源）" {
		t.Fatal("history source and directory scope must be independent")
	}
}

func TestMissingOrSharedCLIHomeDoesNotImplyIsolation(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".codex")
	fixtureDB(t, filepath.Join(root, "state_5.sqlite"), `CREATE TABLE threads(id TEXT,title TEXT,first_user_message TEXT,cwd TEXT,updated_at INTEGER,source TEXT,archived INTEGER,rollout_path TEXT);INSERT INTO threads VALUES('one','','问题','/project',1,'vscode',0,'');`)
	r := Reader{Home: home, SQLite: "/usr/bin/sqlite3"}
	s := r.Snapshot(context.Background())
	if !s.Sources[0].Complete || s.Sources[1].Complete || len(s.Sources[1].Warnings) == 0 {
		t.Fatal("one unavailable scope must not invalidate the other")
	}
	if err := os.Symlink(root, filepath.Join(home, ".codex-cli")); err != nil {
		t.Fatal(err)
	}
	s = r.Snapshot(context.Background())
	if len(s.Sessions) != 2 || s.Sessions[0].Key == s.Sessions[1].Key || len(s.Sources[0].Warnings) == 0 || len(s.Sources[1].Warnings) == 0 {
		t.Fatal("shared physical history must be reported and keep scoped identities")
	}
}

func TestRejectsUntrustedRolloutAndUnknownStore(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".codex")
	outside := filepath.Join(home, "private.jsonl")
	writeFixture(t, outside, `secret`)
	if trustedRollout(root, outside) {
		t.Fatal("accepted external path")
	}
	writeFixture(t, filepath.Join(home, ".cc-switch", "codex_oauth_auth.json"), `{"version":99,"accounts":{"a":{"account_id":"a"}}}`)
	a, w := (Reader{Home: home, SQLite: "/usr/bin/sqlite3"}).accounts(context.Background(), filepath.Join(home, ".cc-switch"), "", nil)
	if len(a) != 0 || len(w) != 1 {
		t.Fatal("unknown schema accepted")
	}
}

func TestInvalidJSONDoesNotLeavePartialSettings(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	writeFixture(t, p, `{"codexConfigDir":"/should-not-be-used","currentProviderCodex":42}`)
	var settings struct {
		Root    string `json:"codexConfigDir"`
		Current string `json:"currentProviderCodex"`
	}
	if readJSON(p, &settings) == nil {
		t.Fatal("invalid settings accepted")
	}
	if settings.Root != "" {
		t.Fatal("partially decoded directory escaped a failed read")
	}
}

func TestSessionIndexUsesLatestRenamesBeyondReadLimit(t *testing.T) {
	p := filepath.Join(t.TempDir(), "session_index.jsonl")
	padding := strings.Repeat("{\"id\":\"padding\",\"thread_name\":\"padding\"}\n", 220000)
	writeFixture(t, p, `{"id":"one","thread_name":"old"}`+"\n"+padding+`{"id":"one","thread_name":"latest"}`+"\n")
	titles, err := indexTitles(p)
	if got := titles["one"]; got != "latest" {
		t.Fatalf("latest rename ignored: %q", got)
	}
	if err == nil {
		t.Fatal("partial index read must report its limit")
	}
}

func TestAmbiguousCurrentProviderIsNotReportedAsTwoSelectedAccounts(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".cc-switch")
	writeFixture(t, filepath.Join(root, "codex_oauth_auth.json"), `{"version":2,"accounts":{"a":{"account_id":"a"},"b":{"account_id":"b"}}}`)
	fixtureDB(t, filepath.Join(root, "cc-switch.db"), `CREATE TABLE providers(id TEXT,name TEXT,app_type TEXT,meta TEXT,is_current INTEGER,sort_index INTEGER);
INSERT INTO providers VALUES('p1','A','codex','{"authBinding":{"authProvider":"codex_oauth","accountId":"a"}}',1,20);
INSERT INTO providers VALUES('p2','B','codex','{"authBinding":{"authProvider":"codex_oauth","accountId":"b"}}',1,10);`)
	r := Reader{Home: home, SQLite: "/usr/bin/sqlite3"}
	a, w := r.accounts(context.Background(), root, "", nil)
	if len(a) != 2 || a[0].ID != "b" {
		t.Fatal("CC Switch provider order was lost")
	}
	for _, account := range a {
		if account.Selected {
			t.Fatal("ambiguous database flag treated as confirmed selection")
		}
	}
	if len(w) == 0 {
		t.Fatal("ambiguity was hidden")
	}
	a, w = r.accounts(context.Background(), root, "third-party", nil)
	if len(w) == 0 {
		t.Fatal("unmatched current provider was hidden")
	}
	for _, account := range a {
		if account.Selected {
			t.Fatal("unmatched current provider used a stale DB flag")
		}
	}
}

func TestFallbackRetainsRecentRolloutsBeyondLimit(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".codex")
	for i := 0; i < 4001; i++ {
		path := filepath.Join(root, "sessions", fmt.Sprintf("rollout-%04d.jsonl", i))
		writeFixture(t, path, fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":\"%d\",\"source\":\"cli\"}}\n", i))
		stamp := time.Unix(int64(i+1), 0)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	r := Reader{Home: home, SQLite: "/usr/bin/sqlite3"}
	sessions, w := r.rolloutSessions(context.Background(), root, nil, nil)
	if len(sessions) != 4000 {
		t.Fatalf("fallback count %d", len(sessions))
	}
	if _, ok := sessions["4000"]; !ok {
		t.Fatal("newest session was omitted")
	}
	if _, ok := sessions["0"]; ok {
		t.Fatal("oldest session took a slot from a newer one")
	}
	if len(w) == 0 {
		t.Fatal("partial result was not reported")
	}
}

func TestOlderDatabaseResultIsMarkedIncomplete(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".codex")
	writeFixture(t, filepath.Join(root, "state_6.sqlite"), "invalid sqlite data")
	fixtureDB(t, filepath.Join(root, "state_5.sqlite"), `CREATE TABLE threads(id TEXT,title TEXT,first_user_message TEXT,cwd TEXT,updated_at INTEGER,source TEXT,archived INTEGER,rollout_path TEXT); INSERT INTO threads VALUES('old','','问题','/project',1,'cli',0,'');`)
	s, w, complete := (Reader{Home: home, SQLite: "/usr/bin/sqlite3"}).sessions(context.Background(), root, nil)
	if complete || len(s) != 1 || len(w) == 0 {
		t.Fatal("older database was presented as a complete current result")
	}
}
