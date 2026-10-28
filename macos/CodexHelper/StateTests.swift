import Foundation

@main
enum Tests {
    static func session(_ key: String, title: String = "标题", archived: Bool = false, project: String = "/project", time: Int64 = 1, scope: String? = nil) -> HelperSession {
        HelperSession(key: key, id: "thread-\(key)", title: title, title_source: "database", project: project, updated_at: time, source: "CLI（历史来源）", archived: archived, scope: scope)
    }

    static func main() throws {
        var state = SelectionState()
        state.toggleObservation()
        precondition(!state.observing, "No session cannot be observed")
        state.select("a")
        precondition(!state.observing, "Selection does not enable observation")
        state.toggleObservation()
        precondition(state.observing)
        state.reconcile([session("a", title: "已重命名")])
        precondition(state.selectedKey == "a" && state.observing, "Renaming preserves stable binding")
        state.select("b")
        precondition(!state.observing, "Changing session disables observation")
        state.toggleObservation()
        state.reconcile([session("b", archived: true)])
        precondition(state.selectedKey == "b" && !state.observing, "Archived target remains browsable, observation stops")
        state.reconcile([])
        precondition(state.selectedKey == nil && !state.observing, "Missing target is cleared")
        state.select("a"); state.toggleObservation()
        state.expandedProjects = [projectKey("/project")]; state.projectLayoutInitialized = true
        state.reconcile([], complete: false)
        precondition(state.selectedKey == "a" && !state.observing, "Partial reads retain selection and pause observation")
        state.reconcile([session("a")])
        precondition(!state.observing, "Recovery never silently resumes observation")
        precondition(filteredSessions([session("a"), session("b", archived: true)], search: "thread-B", includeArchived: true).map(\.key) == ["b"])
        precondition(filteredSessions([session("b", archived: true)], search: "", includeArchived: false).isEmpty)
        state.select("a"); state.toggleObservation()
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: dir) }
        let store = StateStore(directory: dir)
		state.handoffThreshold="8"
        try store.save(state)
        let loaded = store.load()
        precondition(loaded.selectedKey == "a" && !loaded.observing, "Restart requires fresh observation opt-in")
		precondition(loaded.handoffThreshold == "8", "Restart must retain the user's threshold")
        precondition(loaded.expandedProjects == [projectKey("/project")] && loaded.projectLayoutInitialized)
        let attrs = try FileManager.default.attributesOfItem(atPath: store.file.path)
        precondition((attrs[.posixPermissions] as? NSNumber)?.intValue == 0o600)
        try Data("corrupt".utf8).write(to: store.file)
        precondition(store.load().selectedKey == nil)
        try Data("{\"selectedKey\":\"legacy\",\"observing\":true}".utf8).write(to: store.file)
        precondition(store.load().selectedKey == "legacy" && !store.load().projectLayoutInitialized, "Existing state files must remain readable")
        let projects = groupedSessions([
            session("old", project: "/a/project", time: 1), session("new", project: "/a/project/", time: 10),
            session("same-name", project: "/b/project", time: 5), session("unknown", project: "", time: 2)
        ])
        precondition(projects.count == 3 && projects[0].sessions.map(\.key) == ["new", "old"], "Group by full normalized path and sort by activity")
        precondition(projects[0].title != projects[1].title, "Same basename projects must remain distinguishable")
        precondition(projects[2].title == "未指定项目")
        let matches = filteredSessions(projects.flatMap(\.sessions), search: "thread-same-name", includeArchived: true)
        precondition(groupedSessions(matches).count == 1 && matches[0].key == "same-name")
        let scopedProjects = groupedSessions([session("desktop-item", scope: "desktop"), session("cli-item", scope: "cli")])
        precondition(scopedProjects.count == 2 && scopedProjects[0].key != scopedProjects[1].key, "Same project in different homes must keep independent disclosure state")
        var scoped = SelectionState()
        scoped.select("desktop-item"); scoped.toggleObservation()
        scoped.changeScope("cli")
        precondition(scoped.selectedKey == nil && !scoped.observing)
        scoped.select("cli-item"); scoped.toggleObservation()
        scoped.changeScope("desktop")
        precondition(scoped.selectedKey == "desktop-item" && !scoped.observing)
        scoped.changeScope("cli")
        precondition(scoped.selectedKey == "cli-item" && !scoped.observing, "Each scope remembers its selection but requires fresh observation")
        try store.save(scoped)
        precondition(store.load().activeScope == "cli" && store.load().selectedKey == "cli-item")
        let scopedJSON = """
        {"version":"0.1.3","captured_at":"2026-10-26T00:00:00Z","codex_home":"/desktop","accounts":[],"sessions":[],"sessions_complete":false,"capabilities":{},"warnings":[],"sources":[{"id":"desktop","label":"Desktop","home":"/desktop","complete":true,"warnings":[]},{"id":"cli","label":"CLI","home":"/cli","complete":false,"warnings":["unavailable"]}]}
        """
        let scopedSnapshot = try JSONDecoder().decode(Snapshot.self, from: Data(scopedJSON.utf8))
        precondition(scopedSnapshot.complete(in: "desktop") && !scopedSnapshot.complete(in: "cli"), "CLI failure cannot disable Desktop")
        precondition(scopedSnapshot.warnings(in: "desktop").isEmpty && scopedSnapshot.warnings(in: "cli").count == 1)
        let quotaJSON = """
        {"version":"0.1.4","captured_at":"2026-10-26T00:00:00Z","codex_home":"/desktop","accounts":[{"id":"managed","name":"example","provider_names":[],"selected":false,"default":false,"quota_status":"ready","quota":{"status":"ready","queried_at":"2026-10-26T00:00:00Z","credential_sources":["CLI"],"windows":[{"seconds":18000,"used_percent":12.5},{"seconds":604800,"used_percent":100}]}}],"sessions":[],"capabilities":{},"warnings":[]}
        """
        let quotaSnapshot = try JSONDecoder().decode(Snapshot.self, from: Data(quotaJSON.utf8))
        let summary = quotaSnapshot.accounts[0].quota!.summary
        precondition(summary.contains("87.5%") && summary.contains("剩余 0%") && summary.contains("重置时间未提供"))
        var refreshed = quotaSnapshot
        refreshed.accounts[0].quota = nil
        refreshed.keepQuotaReadings(from: quotaSnapshot)
        precondition(refreshed.accounts[0].quota?.queried_at == quotaSnapshot.accounts[0].quota?.queried_at, "Local refresh must retain the actual last quota query time")
        let handoffJSON = """
        {"version":"test","captured_at":"2026-10-27T00:00:00Z","codex_home":"/test","accounts":[],"sessions":[],"capabilities":{},"warnings":[],"handoff":{"enabled":true,"scope":"cli","session_id":"fixture","threshold":10,"stage":"watching","message":"正在监控"}}
        """
        let handoffSnapshot = try JSONDecoder().decode(Snapshot.self, from: Data(handoffJSON.utf8))
        precondition(handoffSnapshot.handoff?.enabled == true && handoffSnapshot.handoff?.session_id == "fixture" && handoffSnapshot.handoff?.authorization_id == nil, "GUI status must decode without exposing authorization internals")
        let deadRunner = try JSONDecoder().decode(HandoffReport.self, from: Data("""
        {"enabled":false,"authorization_pending":true,"scope":"desktop","session_id":"fixture","threshold":8,"stage":"uncertain","message":"执行器已停止"}
        """.utf8))
        precondition(!deadRunner.enabled && deadRunner.hasAuthorization, "A stopped runner must retain the Stop control until stale authority is revoked")
		let allocation = try JSONDecoder().decode(AccountAllocationReport.self, from: Data("""
		{"clients":[{"scope":"desktop","home":"/isolated/.codex","name":"账号 A","status":"matched","message":"disk only"},{"scope":"cli","home":"/isolated/.codex-cli","name":"账号 B","status":"matched","message":"disk only"}],"distinct":true,"working_scope":"desktop","message":"两端账号不同"}
		""".utf8))
		precondition(allocation.distinct && allocation.working_scope == "desktop" && allocation.caption.contains("Desktop：账号 A") && allocation.caption.contains("CLI：账号 B"), "Dual-client allocation must decode and distinguish both scopes")
        let started = Date()
        let schedule = WarmupSchedule(enabled: true, start: "22:00", end: "08:00", time_zone: "Asia/Singapore")
        precondition(schedule.caption.contains("次日 08:00"))
        let scheduleJSON = """
        {"enabled":true,"message":"时段外等待","accounts":[],"available":true,"schedule":{"enabled":true,"start":"22:00","end":"08:00","time_zone":"Asia/Singapore"},"schedule_allowed":false,"next_opening":"2026-10-26T22:00:00+08:00"}
        """
        let waiting = try JSONDecoder().decode(WarmupReport.self, from: Data(scheduleJSON.utf8))
        precondition(waiting.summary.contains("22:00") && waiting.summary.contains("时段外等待"))
        do {
            _ = try BridgeClient.read(executable: URL(fileURLWithPath: "/bin/sleep"), arguments: ["5"], timeout: 0.1)
            preconditionFailure("Hung bridge was not timed out")
        } catch BridgeError.timedOut { }
        precondition(Date().timeIntervalSince(started) < 2, "Watchdog failed to bound execution")
        do {
            _ = try BridgeClient.read(executable: URL(fileURLWithPath: "/usr/bin/printf"), arguments: ["123456789"], maximumBytes: 4)
            preconditionFailure("Oversized bridge output accepted")
        } catch BridgeError.outputLimit { }
        let output = try BridgeClient.read(executable: URL(fileURLWithPath: "/usr/bin/printf"), arguments: ["ok"])
        precondition(String(data: output, encoding: .utf8) == "ok")
        print("Selection, persistence and bridge checks passed")
    }
}
