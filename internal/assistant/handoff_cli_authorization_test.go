package assistant

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPrepareCLIHandoffBindsAndRevokesExactSession(t *testing.T) {
	r, backend, _ := nativeFixture(t)
	work := filepath.Join(r.Home, "work")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(r.Home, ".codex-cli", "state_1.sqlite")
	fixtureDB(t, db, `CREATE TABLE threads(id TEXT,title TEXT,first_user_message TEXT,cwd TEXT,updated_at INTEGER,source TEXT,archived INTEGER,rollout_path TEXT);`)
	// SQLite values are synthetic and contain no credentials or user data.
	if out, err := exec.Command("/usr/bin/sqlite3", db, `INSERT INTO threads VALUES('session-a','Title','Question','`+work+`',1,'cli',0,''); INSERT INTO threads VALUES('archived','Old','Old','`+work+`',1,'cli',1,'');`).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %s", out)
	}
	if _, err := r.prepareCLIHandoff(context.Background(), "missing", 10, backend); err == nil {
		t.Fatal("missing session accepted")
	}
	if _, err := r.prepareCLIHandoff(context.Background(), "archived", 10, backend); err == nil {
		t.Fatal("archived session accepted")
	}
	state, err := r.prepareCLIHandoff(context.Background(), "session-a", 10, backend)
	if err != nil || !state.Enabled || state.AuthorizationID == "" || state.SessionID != "session-a" {
		t.Fatal("valid session was not bound")
	}
	if err := r.RunCLIHandoff(context.Background(), "session-a", 10, "wrong-authorization"); err == nil {
		t.Fatal("wrong authorization started a terminal")
	}
	if _, err := r.ConfigureHandoff("", "", 0, false); err != nil {
		t.Fatal(err)
	}
	if err := r.RunCLIHandoff(context.Background(), "session-a", 10, state.AuthorizationID); err == nil {
		t.Fatal("cancelled authorization started a terminal")
	}
}
