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
        let paused = try decoder.decode(OrchestraRunnerStatus.self, from: Data("{\"active\":true,\"dir\":\"/tmp\",\"paused\":true,\"remaining\":8,\"reason\":\"low\"}".utf8))
        precondition(paused.paused == true && paused.remaining == 8 && paused.reason == "low")
        let settings = try decoder.decode(OrchestraSettings.self, from: Data("{\"account\":\"desktop\",\"min_remaining\":10}".utf8))
        precondition(settings.account == "desktop" && settings.min_remaining == 10 && settings.auto_start_runner == nil)
        let auto = try decoder.decode(OrchestraSettings.self, from: Data("{\"account\":\"desktop\",\"min_remaining\":10,\"auto_switch\":true,\"auto_start_runner\":true}".utf8))
        precondition(auto.auto_switch == true && auto.auto_start_runner == true)
        let switchStatus = try decoder.decode(OrchestraRunnerStatus.self, from: Data("{\"active\":true,\"dir\":\"/tmp\",\"auto_switch\":{\"stage\":\"uncertain\",\"source\":\"当前账号\",\"target\":\"备用账号\",\"review_required\":true}}".utf8))
        precondition(switchStatus.auto_switch?.stage == "uncertain" && switchStatus.auto_switch?.review_required == true)
        precondition(switchStatus.auto_switch?.display.contains("结果待核查") == true)
        let evidence = try decoder.decode(OrchestraTask.self, from: Data("{\"id\":\"E\",\"executor\":\"codex\",\"folder\":\"/tmp\",\"card\":\"test\",\"status\":\"receipt\",\"updated\":\"now\",\"evidence\":{\"summary\":\"检查 1/1 通过\",\"checks\":[{\"command\":\"true\",\"status\":\"passed\",\"exit\":0}],\"changes\":{\"status\":\"available\",\"changed\":[\"x\"],\"out_of_scope\":[\"x\"]}}}".utf8))
        precondition(evidence.evidence?.checks?.first?.status == "passed" && evidence.evidence?.changes?.out_of_scope == ["x"])
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
        let limited = try decoder.decode(OrchestraTask.self, from: Data("{\"id\":\"T\",\"executor\":\"codex\",\"folder\":\"/tmp\",\"card\":\"x\",\"status\":\"quota_exhausted\",\"session\":\"s\",\"updated\":\"now\"}".utf8))
        precondition(limited.canResume && limited.canRework && !limited.canAccept)
        let unconfirmed = try decoder.decode(OrchestraTask.self, from: Data("{\"id\":\"T\",\"executor\":\"codex\",\"folder\":\"/tmp\",\"card\":\"x\",\"status\":\"failed\",\"updated\":\"now\",\"unconfirmed\":true}".utf8))
        precondition(unconfirmed.canConfirmStopped && !unconfirmed.canRework)
        precondition(none == nil)
        let project = try decoder.decode(OrchestraProject.self, from: Data("{\"id\":\"p1\",\"name\":\"工作群A\",\"folder\":\"/tmp/p1\",\"executor\":\"codex\",\"reviewer\":\"claude\",\"checks\":[\"go test ./...\"],\"scope\":[\"internal/\"]}".utf8))
        precondition(project.id == "p1" && project.name == "工作群A" && project.executor == "codex" && project.reviewer == "claude" && project.checks == ["go test ./..."] && project.scope == ["internal/"] && project.sync_agents_md == nil)
        precondition(project.rulesPath == "/tmp/p1/.codex-helper/projects/p1.md")
        let syncedProject = try decoder.decode(OrchestraProject.self, from: Data("{\"id\":\"p2\",\"name\":\"工作群B\",\"folder\":\"/tmp/p2\",\"executor\":\"claude\",\"lead\":\"codex\",\"sync_agents_md\":true}".utf8))
        precondition(syncedProject.sync_agents_md == true && syncedProject.lead == "codex")
        precondition(orchestraProjectUpdateArgs(from: project, name: "工作群A", folder: "/tmp/p1", executor: "codex", lead: "", reviewer: "claude", checks: ["go test ./..."], scope: ["internal/"], syncAgentsMD: false).isEmpty, "Unchanged project must emit no update flags")
        precondition(orchestraProjectUpdateArgs(from: project, name: "工作群A", folder: "/tmp/p1", executor: "codex", lead: "", reviewer: "", checks: ["go test ./..."], scope: ["internal/"], syncAgentsMD: false) == ["--reviewer="], "Cleared reviewer must emit --reviewer=")
        precondition(orchestraProjectUpdateArgs(from: project, name: "工作群A", folder: "/tmp/p1", executor: "codex", lead: "", reviewer: "claude", checks: ["go test ./...", "go vet ./..."], scope: ["internal/"], syncAgentsMD: false) == ["--clear-checks", "--check", "go test ./...", "--check", "go vet ./..."], "Changed checks must emit --clear-checks followed by each --check")
        precondition(orchestraProjectUpdateArgs(from: syncedProject, name: "工作群B2", folder: "/tmp/p2", executor: "claude", lead: "", reviewer: "", checks: [], scope: ["cmd/"], syncAgentsMD: false) == ["--name", "工作群B2", "--lead=", "--clear-scope", "--scope", "cmd/", "--sync-agents-md=false"])
        let reviewing = try decoder.decode(OrchestraTask.self, from: Data("{\"id\":\"T2\",\"project\":\"p1\",\"executor\":\"codex\",\"folder\":\"/tmp/p1\",\"card\":\"task\",\"status\":\"reviewing\",\"review_task\":\"T2-review-1\",\"updated\":\"now\"}".utf8))
        precondition(reviewing.canAccept && reviewing.canCancel && !reviewing.canRework && reviewing.statusLabel == "审查中" && reviewing.reviewLabel == "审查中" && reviewing.project == "p1" && reviewing.review_task == "T2-review-1")
        let reviewed = try decoder.decode(OrchestraTask.self, from: Data("{\"id\":\"T3\",\"project\":\"p1\",\"executor\":\"codex\",\"folder\":\"/tmp/p1\",\"card\":\"task\",\"status\":\"receipt\",\"review_verdict\":\"changes\",\"review_text\":\"fix x\",\"updated\":\"now\"}".utf8))
        precondition(reviewed.reviewLabel == "需修改" && reviewed.review_text == "fix x" && reviewed.statusLabel == "待验收")
        let instructions = orchestraConnectionInstructions(executable: URL(fileURLWithPath: "/tmp/a b/codex-helper"), stateDir: "/tmp/test state")
        precondition(instructions.contains("CODEX_HELPER_NATIVE_PREVIEW=1") && instructions.contains("CODEX_HELPER_NATIVE_PREVIEW = \"1\""))
        precondition(instructions.contains("CODEX_HELPER_ORCHESTRA_DIR='/tmp/test state'") && instructions.contains("CODEX_HELPER_ORCHESTRA_DIR = \"/tmp/test state\""), "Both clients must use the GUI's state root")
        let fixedNow = Date(timeIntervalSince1970: 1_793_640_000)
        precondition(resolveOrchestraTaskID(" custom-id ", project: "p1", now: fixedNow) == "custom-id")
        let generated = resolveOrchestraTaskID("   ", project: "p1", now: fixedNow)
        precondition(generated.hasPrefix("p1-") && generated.count <= 64, "Blank task ID must generate a valid project-prefixed ID")
        let deduped = resolveOrchestraTaskID("", project: "p1", existingIDs: [generated], now: fixedNow)
        precondition(deduped == generated + "-2", "Generated task ID must avoid collisions with existing tasks")
        let noProjectID = resolveOrchestraTaskID("", project: nil, now: fixedNow)
        precondition(noProjectID.hasPrefix("task-"), "Blank task ID without project must generate a valid task-prefixed ID")
        let projIDFromName = resolveOrchestraProjectID("", name: "前端 Refactor 2.0", folder: "/tmp/my-folder", existingIDs: [], now: fixedNow)
        precondition(projIDFromName == "refactor-2.0", "Project ID should slugify ASCII segments from name")
        let projIDFromFolder = resolveOrchestraProjectID("", name: "纯中文项目", folder: "/Users/test/codex-helper", existingIDs: ["codex-helper"], now: fixedNow)
        precondition(projIDFromFolder == "codex-helper-2", "Project ID should fall back to folder name and deduplicate")
        let reviewChild = try decoder.decode(OrchestraTask.self, from: Data("{\"id\":\"T2-review-1\",\"project\":\"p1\",\"review_of\":\"T2\",\"executor\":\"claude\",\"folder\":\"/tmp/p1\",\"card\":\"review\",\"status\":\"running\",\"updated\":\"2026-10-31T12:00:00Z\",\"progress\":{\"state\":\"working\",\"summary\":\"第 3 步 · Bash\",\"latest_stage\":\"正在核对单元测试\",\"stages\":[\"正在核对单元测试\"],\"recent_actions\":[\"Bash\"],\"steps\":3,\"conversation_id\":\"cl-77\",\"idle_seconds\":5}}".utf8))
        precondition(reviewChild.statusLabel == "执行中(3步)" && reviewChild.formattedProgressSection?.contains("正在核对单元测试") == true && reviewChild.formattedProgressSection?.contains("cl-77") == true)
        let retryingAGY = try decoder.decode(OrchestraTask.self, from: Data("{\"id\":\"A1\",\"project\":\"p1\",\"executor\":\"antigravity\",\"folder\":\"/tmp/p1\",\"card\":\"fix ui\",\"status\":\"running\",\"updated\":\"now\",\"progress\":{\"state\":\"retrying\",\"summary\":\"第 4 步 · 模型断流重试中\",\"steps\":4,\"warning\":\"检测到 AGY 断流\"}}".utf8))
        precondition(retryingAGY.statusLabel == "断流重试" && retryingAGY.formattedProgressSection?.contains("⚠️ 监视预警") == true)
        let unassignedTask = try decoder.decode(OrchestraTask.self, from: Data("{\"id\":\"U1\",\"executor\":\"codex\",\"folder\":\"/tmp\",\"card\":\"free task\",\"status\":\"queued\",\"updated\":\"2026-10-31T11:00:00Z\"}".utf8))
        let groups = buildOrchestraProjectGroups(projects: [project], allProjects: [project], tasks: [reviewing, reviewed, reviewChild, unassignedTask])
        precondition(groups.count == 2 && groups[0].projectID == "p1" && groups[1].isUnassigned, "Tasks must be grouped into project containers plus unassigned")
        precondition(groups[0].tasks.map(\.id) == ["T2", "T2-review-1", "T3"] || groups[0].tasks.contains(where: { $0.id == "T2-review-1" }), "Review child task must stay inside its project group")
        if let idx = groups[0].tasks.firstIndex(where: { $0.id == "T2" }) {
            precondition(groups[0].tasks[idx + 1].id == "T2-review-1", "Review child task must be ordered directly after parent task")
        }
        _ = NSApplication.shared
        let hostWin = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 960, height: 720), styleMask: [.titled], backing: .buffered, defer: false)
        let controller = OrchestraWindowController(executable: URL(fileURLWithPath: "/usr/bin/true"), hostWindow: hostWin)
        let sidebar = controller.makeSidebarView()
        hostWin.contentView?.addSubview(sidebar)
        hostWin.contentView?.addSubview(controller.rootView)
        controller.testInjectProjects([project, syncedProject], tasks: [reviewing, reviewed, unassignedTask])
        precondition(controller.testSidebarRowCount() == 6, "Sidebar must list Home, Header, All, 2 Projects, and Unassigned")
        controller.setEmbeddedActive(true)
        controller.selectSidebarScope(.project("p1"))
        precondition(controller.sidebarSelection == .project("p1"), "Selecting project scope must filter to p1")
        controller.selectSidebarScope(.unassigned)
        precondition(controller.sidebarSelection == .unassigned, "Selecting unassigned scope must filter to unassigned")
        hostWin.close()
        print("Orchestra JSON decoding and task action rules passed")
    }
}
