import Cocoa

@main enum OrchestraTests {
    static func main() throws {
        let decoder = JSONDecoder()
        let agents = try decoder.decode([OrchestraAgent].self, from: Data("""
        [{"name":"codex","label":"Codex","available":true},{"name":"antigravity","label":"Antigravity","available":false,"reason":"不可用"}]
        """.utf8))
        precondition(agents.count == 2 && agents[0].available && agents[1].reason == "不可用")
        let runner = try decoder.decode(OrchestraRunnerStatus.self, from: Data("""
        {"active":true,"dir":"/tmp/state"}
        """.utf8))
        precondition(runner.active && runner.dir == "/tmp/state")
        let login = try decoder.decode(OrchestraLoginInfo.self, from: Data("""
        {"dir":"/tmp/home/.codex-runner","created":["config.toml"],"codex_bin":"/tmp/fake","login":{"logged_in":true,"state":"logged_in","mode":"ChatGPT","detail":"已登录"}}
        """.utf8))
        precondition(login.login.logged_in && login.login.mode == "ChatGPT" && login.created == ["config.toml"])
        precondition(OrchestraLoginActions(state: "logged_in", busy: false).canLogout)
        precondition(!OrchestraLoginActions(state: "logged_in", busy: false).canLogin)
        precondition(OrchestraLoginActions(state: "not_logged_in", busy: false).canLogin)
        precondition(!OrchestraLoginActions(state: "not_logged_in", busy: false).canLogout)
        precondition(!OrchestraLoginActions(state: "not_logged_in", busy: true).canLogin && !OrchestraLoginActions(state: "logged_in", busy: true).canLogout)
        for (status, cancel, review) in [("queued",true,false),("running",true,false),("receipt",false,true),("failed",false,true),("cancelled",false,false),("accepted",false,false)] {
            let json = """
            [{"id":"T1","executor":"codex","folder":"/tmp/project","card":"task","status":"\(status)","updated":"2026-10-31T10:00:00Z","runs":[{"error":"problem"}]}]
            """
            let tasks = try decoder.decode([OrchestraTask].self, from: Data(json.utf8))
            let task = tasks[0]
            precondition(task.canCancel == cancel && task.canAccept == review && task.canRework == review, "button rules for \(status)")
            precondition(task.lastResult == "problem")
        }
        let none = try decoder.decode([OrchestraTask]?.self, from: Data("null".utf8))
        precondition(none == nil)
        let instructions = orchestraConnectionInstructions(executable: URL(fileURLWithPath: "/tmp/a b/codex-helper"), stateDir: "/tmp/test state")
        precondition(instructions.contains("CODEX_HELPER_NATIVE_PREVIEW=1") && instructions.contains("CODEX_HELPER_NATIVE_PREVIEW = \"1\""))
        precondition(instructions.contains("CODEX_HELPER_ORCHESTRA_DIR='/tmp/test state'") && instructions.contains("CODEX_HELPER_ORCHESTRA_DIR = \"/tmp/test state\""), "Both clients must use the GUI's state root")
        print("Orchestra JSON decoding and task action rules passed")
    }
}
