package assistant

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeDisplayAccountsIncludesCCSwitchMetadataWithoutAuthorizingIt(t *testing.T) {
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	home := t.TempDir()
	r := Reader{Home: home}
	writeFixture(t, r.managedAccountsStorePath(), `{"version":2,"accounts":{"native":{"account_id":"native","email":"person@example.test"}}}`)
	root, err := filepath.EvalSymlinks(filepath.Join(r.helperStateRoot(), "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(root))
	dir := filepath.Join("/tmp", fmt.Sprintf("codex-helper-account-%d-%x", os.Getuid(), sum[:8]))
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "manager.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, e := listener.Accept()
		if e != nil {
			return
		}
		defer conn.Close()
		scan := bufio.NewScanner(conn)
		for scan.Scan() {
			var req struct {
				ID int `json:"id"`
			}
			if json.Unmarshal(scan.Bytes(), &req) != nil {
				return
			}
			resp := map[string]any{"id": req.ID, "result": map[string]any{
				"pending": false, "cc_switch_status": "ready", "accounts": []any{
					map[string]any{"id": "native", "name": "p***@example.test", "managed": true},
					map[string]any{"id": "cc:" + strings.Repeat("a", 64), "name": "c***@example.test", "managed": false},
				},
			}}
			data, _ := json.Marshal(resp)
			_, _ = conn.Write(append(data, '\n'))
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	accounts, warnings := r.nativeDisplayAccounts(ctx, nil)
	if len(warnings) != 0 || len(accounts) != 2 || accounts[0].ExternalOnly || !accounts[1].ExternalOnly || accounts[1].Name != "c***@example.test" {
		t.Fatalf("read-only account union incorrect: %+v warnings=%v", accounts, warnings)
	}
	automation := r.automationAccounts(ctx)
	if len(automation) != 1 || automation[0].ID != "native" {
		t.Fatal("displaying CC Switch metadata changed the automation account set")
	}
	<-done
}
