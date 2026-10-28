package assistant

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func quotaToken(user, workspace string, expiry int64) string {
	data, _ := json.Marshal(map[string]any{"iss": "https://auth.openai.com", "exp": expiry, "https://api.openai.com/auth": map[string]string{"chatgpt_user_id": user, "chatgpt_account_id": workspace}})
	return "e30." + base64.RawURLEncoding.EncodeToString(data) + ".fake-test-signature"
}
func quotaFixtures(t *testing.T, home string) {
	t.Helper()
	expires := time.Now().Add(time.Hour).Unix()
	store := map[string]any{"version": 2, "accounts": map[string]any{
		"managed-a": map[string]any{"account_id": "managed-a", "chatgpt_account_id": "workspace-a", "id_token": quotaToken("user-a", "workspace-a", expires), "refresh_token": "REFRESH_MUST_NOT_BE_USED"},
		"managed-b": map[string]any{"account_id": "managed-b", "chatgpt_account_id": "workspace-b", "id_token": quotaToken("user-b", "workspace-b", expires), "refresh_token": "REFRESH_MUST_NOT_BE_USED"},
	}}
	data, _ := json.Marshal(store)
	writeFixture(t, filepath.Join(home, ".cc-switch/codex_oauth_auth.json"), string(data))
	for i, dir := range []string{".codex", ".codex-cli"} {
		letter := []string{"a", "b"}[i]
		token := quotaToken("user-"+letter, "workspace-"+letter, expires)
		auth := map[string]any{"auth_mode": "chatgpt", "tokens": map[string]string{"access_token": token, "id_token": token, "account_id": "workspace-" + letter, "refresh_token": "DO_NOT_REFRESH"}}
		data, _ = json.Marshal(auth)
		writeFixture(t, filepath.Join(home, dir, "auth.json"), string(data))
	}
}

type quotaRoundTrip func(*http.Request) (*http.Response, error)

func (f quotaRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestManagedCredentialsMatchUserAndWorkspaceNotJustWorkspace(t *testing.T) {
	home := t.TempDir()
	quotaFixtures(t, home)
	r := Reader{Home: home}
	credentials := r.quotaCredentials(time.Now())
	if len(credentials) != 2 || credentials["managed-a"].sources[0] != "Desktop" || credentials["managed-b"].sources[0] != "CLI" {
		t.Fatal("managed identities did not resolve separately")
	}
	var auth map[string]any
	data, _ := os.ReadFile(filepath.Join(home, ".codex-cli/auth.json"))
	_ = json.Unmarshal(data, &auth)
	tokens := auth["tokens"].(map[string]any)
	tokens["access_token"] = quotaToken("intruder", "workspace-b", time.Now().Add(time.Hour).Unix())
	data, _ = json.Marshal(auth)
	writeFixture(t, filepath.Join(home, ".codex-cli/auth.json"), string(data))
	if len(r.quotaCredentials(time.Now())) != 1 {
		t.Fatal("wrong user in same workspace accepted")
	}
	tokens["access_token"] = quotaToken("user-b", "workspace-b", time.Now().Add(-time.Hour).Unix())
	data, _ = json.Marshal(auth)
	writeFixture(t, filepath.Join(home, ".codex-cli/auth.json"), string(data))
	if len(r.quotaCredentials(time.Now())) != 1 {
		t.Fatal("expired credential accepted")
	}
}

func TestStashedLoginFallbackIsValidatedAndNeverUsedForSessionIdentity(t *testing.T) {
	home := t.TempDir()
	quotaFixtures(t, home)
	now := time.Now()
	// Both live directories belong to A; B exists only in the CC Switch stash.
	authA, _ := os.ReadFile(filepath.Join(home, ".codex/auth.json"))
	authB, _ := os.ReadFile(filepath.Join(home, ".codex-cli/auth.json"))
	writeFixture(t, filepath.Join(home, ".codex-cli/auth.json"), string(authA))
	var a, b map[string]any
	_ = json.Unmarshal(authA, &a)
	_ = json.Unmarshal(authB, &b)
	writeStash := func(version int, logins map[string]any) {
		data, _ := json.Marshal(map[string]any{"version": version, "logins": logins})
		writeFixture(t, filepath.Join(home, ".cc-switch/codex-login-stash.json"), string(data))
	}
	writeStash(1, map[string]any{"a": a, "b": b})
	r := Reader{Home: home}
	creds := r.quotaCredentials(now)
	if len(creds) != 2 || strings.Join(creds["managed-a"].sources, ",") != "Desktop,CLI" || strings.Join(creds["managed-b"].sources, ",") != "CC Switch 暂存" {
		t.Fatal("expected live A and stashed B")
	}
	if _, ok := r.quotaCredentialsFrom(now, ".codex-cli")["managed-b"]; ok {
		t.Fatal("stash must never establish a running session identity")
	}
	writeStash(2, map[string]any{"b": b})
	if len(r.quotaCredentials(now)) != 1 {
		t.Fatal("unknown stash format accepted")
	}
	bt := b["tokens"].(map[string]any)
	bt["access_token"] = quotaToken("user-b", "workspace-b", now.Add(-time.Minute).Unix())
	writeStash(1, map[string]any{"b": b})
	if len(r.quotaCredentials(now)) != 1 {
		t.Fatal("expired stashed token accepted")
	}
	bt["access_token"] = quotaToken("intruder", "workspace-b", now.Add(time.Hour).Unix())
	writeStash(1, map[string]any{"b": b})
	if len(r.quotaCredentials(now)) != 1 {
		t.Fatal("stashed token with mismatched identity accepted")
	}
}

func TestStashedCredentialActivityChecksBothSessionRoots(t *testing.T) {
	home := t.TempDir()
	r := Reader{Home: home}
	path := filepath.Join(home, ".codex-cli/sessions/active.jsonl")
	writeFixture(t, path, "{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\"}}\n")
	if !r.recentActivity([]string{"CC Switch 暂存"}, time.Now()) {
		t.Fatal("stashed credential must account for CLI activity too")
	}
}

func TestQuotaQueryUsesCorrectAccountAndNeverExportsCredentials(t *testing.T) {
	home := t.TempDir()
	quotaFixtures(t, home)
	before, _ := os.ReadFile(filepath.Join(home, ".cc-switch/codex_oauth_auth.json"))
	var mu sync.Mutex
	seen := map[string]int{}
	client := &http.Client{Transport: quotaRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.String() != codexQuotaURL {
			t.Error("unexpected credential destination or method")
		}
		claims, ok := decodeTokenClaims(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if !ok || r.Header.Get("ChatGPT-Account-Id") != claims.Auth.Workspace {
			t.Error("mismatched token and account header")
		}
		mu.Lock()
		seen[claims.Auth.Workspace]++
		mu.Unlock()
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"rate_limit":{"primary_window":{"used_percent":12.5,"limit_window_seconds":18000,"reset_at":1900000000},"secondary_window":{"used_percent":30,"limit_window_seconds":604800,"reset_at":1900005000},"limit_reached":false}}`))}, nil
	})}
	s := Snapshot{Accounts: []Account{{ID: "managed-a"}, {ID: "managed-b"}}, Capabilities: capabilities()}
	(Reader{Home: home}).readAccountQuotas(context.Background(), &s, client)
	if seen["workspace-a"] != 1 || seen["workspace-b"] != 1 {
		t.Fatal("requests did not use both distinct accounts")
	}
	for _, a := range s.Accounts {
		if a.Quota == nil || a.Quota.Status != "ready" || len(a.Quota.Windows) != 2 || a.Quota.Windows[0].UsedPercent != 12.5 {
			t.Fatal("quota not attached")
		}
	}
	output, _ := json.Marshal(s)
	for _, secret := range []string{"REFRESH_MUST_NOT_BE_USED", "DO_NOT_REFRESH", "fake-test-signature", "access_token", "id_token", "workspace-a", "workspace-b"} {
		if strings.Contains(string(output), secret) {
			t.Fatalf("sensitive data exported: %s", secret)
		}
	}
	after, _ := os.ReadFile(filepath.Join(home, ".cc-switch/codex_oauth_auth.json"))
	if string(before) != string(after) {
		t.Fatal("source changed")
	}
}

func TestQuotaNetworkAndHTTPFailuresStayBoundedAndSanitized(t *testing.T) {
	for _, code := range []int{401, 403, 429, 500, 302} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			client := &http.Client{Transport: quotaRoundTrip(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader("SECRET_RESPONSE_BODY"))}, nil
			})}
			q := queryAccountQuota(context.Background(), client, quotaCredential{access: "SECRET_BEARER", workspace: "private"})
			out, _ := json.Marshal(q)
			if q.Status == "ready" || strings.Contains(string(out), "SECRET") {
				t.Fatal("error exposed data or reported success")
			}
			if code == 429 && q.Status != "rate_limited" {
				t.Fatal("429 misclassified")
			}
		})
	}
	client := &http.Client{Transport: quotaRoundTrip(func(r *http.Request) (*http.Response, error) { return nil, fmt.Errorf("SECRET_ERROR") })}
	q := queryAccountQuota(context.Background(), client, quotaCredential{access: "SECRET_BEARER"})
	out, _ := json.Marshal(q)
	if q.Status != "network_error" || strings.Contains(string(out), "SECRET") {
		t.Fatal("network error exposed data")
	}
	// No credential means no network request, and does not borrow another account.
	s := Snapshot{Accounts: []Account{{ID: "not-present"}}, Capabilities: capabilities()}
	(Reader{Home: t.TempDir()}).readAccountQuotas(context.Background(), &s, &http.Client{Transport: quotaRoundTrip(func(_ *http.Request) (*http.Response, error) {
		t.Error("requested without matching credential")
		return nil, nil
	})})
	if s.Accounts[0].Quota.Status != "credential_unavailable" {
		t.Fatal("missing identity not reported")
	}
}

func TestQuotaWindowsUseDurationAndRejectUnknownOrInvalidShape(t *testing.T) {
	w, _, ok := parseQuotaWindows([]byte(`{"rate_limit":{"primary_window":{"used_percent":80,"limit_window_seconds":604800},"secondary_window":{"used_percent":0,"limit_window_seconds":18000}}}`))
	if !ok || len(w) != 2 || w[0].Seconds != 18000 || w[0].UsedPercent != 0 {
		t.Fatal("window duration mapping failed")
	}
	for _, raw := range []string{`{}`, `{"rate_limit":{}}`, `{"rate_limit":{"primary_window":{"used_percent":0}}}`, `{"rate_limit":{"primary_window":{"used_percent":101,"limit_window_seconds":18000}}}`, `{"rate_limit":{"primary_window":{"limit_window_seconds":18000}}}`} {
		if _, _, ok := parseQuotaWindows([]byte(raw)); ok {
			t.Fatal("invalid response interpreted as unused quota")
		}
	}
}
