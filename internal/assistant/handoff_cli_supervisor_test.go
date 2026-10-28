package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fake speaks the real local WebSocket framing and runs as a separate
// process. It uses only invented credentials and never reaches a model API.
const fakeHandoffCLI = `#!/usr/bin/python3
import os,sys,time,json,socket,hashlib,base64,struct,signal
home=os.environ['FAKE_HANDOFF_HOME']
state_path=os.path.join(home,'fake-state.json')
def load():
    with open(state_path) as f:return json.load(f)
def save(x):
    tmp=state_path+'.tmp'
    with open(tmp,'w') as f:json.dump(x,f)
    os.replace(tmp,state_path)
if sys.argv[1]=='resume':
    while True: time.sleep(1)
if sys.argv[1]!='app-server':sys.exit(2)
path=sys.argv[3].replace('unix://','',1)
try:os.unlink(path)
except FileNotFoundError:pass
listener=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM)
listener.bind(path);listener.listen(8)
def readn(c,n):
    out=b''
    while len(out)<n:
        v=c.recv(n-len(out))
        if not v:raise EOFError()
        out+=v
    return out
def getframe(c):
    a,b=readn(c,2);n=b&127
    if n==126:n=struct.unpack('!H',readn(c,2))[0]
    elif n==127:n=struct.unpack('!Q',readn(c,8))[0]
    mask=readn(c,4) if b&128 else None
    data=readn(c,n)
    if mask:data=bytes(x^mask[i%4] for i,x in enumerate(data))
    return json.loads(data)
def sendframe(c,obj):
    data=json.dumps(obj,separators=(',',':')).encode();n=len(data)
    head=b'\x81'+(bytes([n]) if n<126 else b'\x7e'+struct.pack('!H',n) if n<65536 else b'\x7f'+struct.pack('!Q',n))
    c.sendall(head+data)
def serve(c):
    head=b''
    while b'\r\n\r\n' not in head:head+=c.recv(1)
    key=[x.split(b':',1)[1].strip() for x in head.split(b'\r\n') if x.lower().startswith(b'sec-websocket-key:')][0]
    accept=base64.b64encode(hashlib.sha1(key+b'258EAFA5-E914-47DA-95CA-C5AB0DC85B11').digest())
    c.sendall(b'HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: '+accept+b'\r\n\r\n')
    while True:
        req=getframe(c)
        if 'id' not in req:continue
        method=req['method'];p=req.get('params') or {};st=load();result={}
        settings=st['settings']
        if method=='thread/read':result={'thread':st['thread']}
        elif method=='thread/resume':result={'thread':{'id':'fixture-thread'},**settings}
        elif method=='turn/steer':
            assert p['expectedTurnId']=='turn-1'
            st['steer']=st.get('steer',0)+1;turn=st['thread']['turns'][-1];turn['status']='completed';turn['items']=[{'type':'userMessage','clientId':p['clientUserMessageId']},{'type':'agentMessage'}];st['thread']['status']={'type':'idle'};save(st);result={'turnId':'turn-1'}
        elif method=='thread/loaded/list':result={'data':['fixture-thread']}
        elif method=='thread/backgroundTerminals/list':result={'data':[],'nextCursor':None}
        elif method=='account/read':
            with open(os.path.join(home,'.codex-cli','auth.json')) as f: auth=json.load(f)
            import base64 as b64
            part=auth['tokens']['access_token'].split('.')[1];claims=json.loads(b64.urlsafe_b64decode(part+'='*(-len(part)%4)))
            user=claims['https://api.openai.com/auth']['chatgpt_user_id']
            result={'account':{'type':'chatgpt','email':user+'@example.test','planType':'plus'},'requiresOpenaiAuth':True}
        elif method=='account/rateLimits/read':result={'rateLimits':{}}
        elif method=='thread/settings/update':st['updated']=True;save(st)
        elif method=='turn/start':
            assert st.get('continued',0)==0
            st['continued']=1;st['thread']['status']={'type':'active','activeFlags':[]};st['thread']['turns'].append({'id':'turn-2','status':'inProgress','startedAt':1,'items':[],'itemsView':'full'});save(st);result={'turn':{'id':'turn-2','status':'inProgress'}}
        elif method=='initialize':result={'userAgent':'fake'}
        else:raise RuntimeError('unexpected method')
        sendframe(c,{'id':req['id'],'result':result})
while True:
    conn,_=listener.accept()
    try:serve(conn)
    except (EOFError,ConnectionError,BrokenPipeError):pass
    finally:conn.close()
`

func TestOwnedCLISupervisorCompletesHandoffAgainstFakeBackend(t *testing.T) {
	r, _ := handoffAuthFixture(t)
	// Fixture defaults put A in Desktop and B in CLI; swap complete records.
	a, err := os.ReadFile(filepath.Join(r.Home, ".codex/auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var source map[string]any
	if json.Unmarshal(a, &source) != nil {
		t.Fatal("bad fixture")
	}
	source["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)
	source["tokens"].(map[string]any)["refresh_token"] = "REFRESH_MUST_NOT_BE_USED"
	a, _ = json.Marshal(source)
	b, err := os.ReadFile(filepath.Join(r.Home, ".codex-cli/auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var target map[string]any
	if json.Unmarshal(b, &target) != nil {
		t.Fatal("bad fixture")
	}
	target["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)
	target["tokens"].(map[string]any)["refresh_token"] = "REFRESH_MUST_NOT_BE_USED"
	b, _ = json.Marshal(target)
	writeFixture(t, filepath.Join(r.Home, ".codex-cli/auth.json"), string(a))
	writeFixture(t, filepath.Join(r.Home, ".codex/auth.json"), string(b))
	var store map[string]any
	data, _ := os.ReadFile(filepath.Join(r.Home, ".cc-switch/codex_oauth_auth.json"))
	_ = json.Unmarshal(data, &store)
	accounts := store["accounts"].(map[string]any)
	accounts["managed-a"].(map[string]any)["email"] = "user-a@example.test"
	accounts["managed-b"].(map[string]any)["email"] = "user-b@example.test"
	data, _ = json.Marshal(store)
	writeFixture(t, filepath.Join(r.Home, ".cc-switch/codex_oauth_auth.json"), string(data))
	work := filepath.Join(r.Home, "work")
	if os.Mkdir(work, 0700) != nil {
		t.Fatal("fixture directory")
	}
	settings := map[string]any{"cwd": work, "model": "fixture-model", "reasoningEffort": "low", "approvalPolicy": "never", "approvalsReviewer": "user", "sandbox": map[string]any{"type": "readOnly"}, "activePermissionProfile": nil, "collaborationMode": nil, "serviceTier": nil, "disabledPluginIds": []any{}}
	fakeState := map[string]any{"settings": settings, "thread": map[string]any{"id": "fixture-thread", "cwd": work, "modelProvider": "openai", "status": map[string]any{"type": "active", "activeFlags": []any{}}, "turns": []any{map[string]any{"id": "turn-1", "status": "inProgress", "items": []any{}, "itemsView": "full"}}}}
	data, _ = json.Marshal(fakeState)
	writeFixture(t, filepath.Join(r.Home, "fake-state.json"), string(data))
	script := filepath.Join(r.Home, "fake-codex")
	if err = os.WriteFile(script, []byte(fakeHandoffCLI), 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp", "codex-helper-cli-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	sourceUsed := 91
	httpClient := &http.Client{Transport: quotaRoundTrip(func(req *http.Request) (*http.Response, error) {
		claims, ok := decodeTokenClaims(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
		if !ok {
			return nil, fmt.Errorf("invalid test token")
		}
		used := sourceUsed
		if claims.Auth.Workspace == "workspace-b" {
			used = 20
		}
		body := fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":%d,"limit_window_seconds":18000},"secondary_window":{"used_percent":10,"limit_window_seconds":604800},"limit_reached":false}}`, used)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	env := append(cliEnvironment(r.Home), "FAKE_HANDOFF_HOME="+r.Home)
	s := &cliHandoffSupervisor{r: r, ctx: ctx, id: "fixture-thread", cwd: work, exe: script, socket: filepath.Join(root, "control.sock"), env: env, provider: "openai", input: &cliHandoffInput{}, sourceID: "managed-a", targetID: "managed-b", client: httpClient}
	if err = s.start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stop, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = s.stop(stop)
		_ = os.Remove(r.cliRunnerPath())
	}()
	if _, err = r.ConfigureHandoff("cli", s.id, 10, true); err != nil {
		t.Fatal(err)
	}
	var state HandoffState
	for i := 0; i < 16; i++ {
		state, err = r.AdvanceHandoff(ctx, s)
		if err != nil {
			t.Fatalf("step %d failed at %s: %v", i, state.Stage, err)
		}
		if state.Stage == "uncertain" {
			t.Fatalf("step %d became uncertain", i)
		}
		if state.Stage == "completed" {
			break
		}
	}
	if state.Stage != "completed" {
		t.Fatalf("handoff stalled at %s", state.Stage)
	}
	live, err := readProtectedHandoffFile(filepath.Join(r.Home, ".codex-cli/auth.json"))
	if err != nil || !r.handoffCurrentMatchesAccount(live, "managed-b") {
		t.Fatal("target auth not installed")
	}
	var result struct {
		Continued int  `json:"continued"`
		Updated   bool `json:"updated"`
	}
	if readJSON(filepath.Join(r.Home, "fake-state.json"), &result) != nil || result.Continued != 1 || !result.Updated {
		t.Fatal("resume was not issued exactly once with restored settings")
	}
	if err = r.finishHandoffAuth(ctx, s.verifyAccountID); err != nil {
		t.Fatalf("verified backup cleanup failed: %v", err)
	}

	// In a second isolated run the observed active turn hits its usage limit
	// before the next poll; the supervisor must not send an extra stop prompt.
	if err := s.stop(ctx); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(r.Home, ".codex-cli/auth.json"), string(a))
	fakeState["steer"] = 0
	data, _ = json.Marshal(fakeState)
	writeFixture(t, filepath.Join(r.Home, "fake-state.json"), string(data))
	s.preparedOK = false
	s.armed = false
	s.seenActiveTurn = ""
	s.continuedTurn = ""
	sourceUsed = 20
	if err = s.start(); err != nil {
		t.Fatal(err)
	}
	if _, err = r.ConfigureHandoff("cli", s.id, 10, true); err != nil {
		t.Fatal(err)
	}
	state, err = r.AdvanceHandoff(ctx, s)
	if err != nil || state.Stage != "watching" || s.seenActiveTurn != "turn-1" {
		t.Fatal("active turn was not observed")
	}
	fakeState["thread"] = map[string]any{"id": "fixture-thread", "cwd": work, "modelProvider": "openai", "status": map[string]any{"type": "idle"}, "turns": []any{map[string]any{"id": "turn-1", "status": "failed", "error": map[string]any{"codexErrorInfo": "usageLimitExceeded"}, "items": []any{}, "itemsView": "full"}}}
	data, _ = json.Marshal(fakeState)
	writeFixture(t, filepath.Join(r.Home, "fake-state.json"), string(data))
	sourceUsed = 100
	state, err = r.AdvanceHandoff(ctx, s)
	if err != nil || state.Stage != "ready_exit" {
		t.Fatalf("usage-limit fallback did not proceed: %s %v", state.Stage, err)
	}
	for i := 0; i < 12; i++ {
		state, err = r.AdvanceHandoff(ctx, s)
		if err != nil || state.Stage == "uncertain" {
			t.Fatalf("fallback failed at %s", state.Stage)
		}
		if state.Stage == "completed" {
			break
		}
	}
	if state.Stage != "completed" {
		t.Fatalf("fallback stalled at %s", state.Stage)
	}
	var fallback struct {
		Continued int `json:"continued"`
		Steer     int `json:"steer"`
	}
	if readJSON(filepath.Join(r.Home, "fake-state.json"), &fallback) != nil || fallback.Continued != 1 || fallback.Steer != 0 {
		t.Fatal("limit fallback repeated or sent a stop prompt")
	}
}
