import Foundation

struct Capability: Decodable {
    let available: Bool
    let reason: String
}

struct HelperAccount: Decodable {
    let id: String
    let name: String
    let provider_names: [String]
    let selected: Bool
    let `default`: Bool
    var quota_status: String
    var quota: AccountQuota?
}

struct QuotaWindow: Decodable {
    let seconds: Int64
    let used_percent: Double
    let reset_at: Int64?
    var title: String {
        switch seconds { case 18000: return "5 小时"; case 604800: return "7 天"; default: return "\(seconds / 3600) 小时" }
    }
    var remainingPercent: Double { used_percent.isFinite ? max(0, min(100, 100-used_percent)) : 0 }
    var remainingText: String {
        let value = remainingPercent
        return value.rounded(.down) == value ? String(format: "%.0f%%",value) : String(format: "%.1f%%",value)
    }
    var resetCaption: String {
        guard let reset_at = reset_at else { return "重置时间未提供" }
        let date = Date(timeIntervalSince1970: Double(reset_at))
        if date <= Date() { return "已到重置时间 · 等待更新" }
        let formatter = DateFormatter()
        if Calendar.current.isDateInToday(date) { formatter.dateFormat = "今天 HH:mm" }
        else if Calendar.current.isDateInTomorrow(date) { formatter.dateFormat = "明天 HH:mm" }
        else { formatter.dateFormat = "MM/dd HH:mm" }
        return formatter.string(from: date) + " 重置"
    }
}
struct AccountQuota: Decodable {
    let status: String
    let message: String?
    let queried_at: String
    let credential_sources: [String]
    let windows: [QuotaWindow]
    let limit_reached: Bool?
    var summary: String {
        guard status == "ready" else { return message ?? "额度暂不可用，请刷新重试。" }
        return windows.map { window in
            let percent = max(0,min(100,100-window.used_percent))
            let number = percent.rounded(.down) == percent ? String(format: "%.0f",percent) : String(format: "%.1f",percent)
            let reset: String
            if let unix = window.reset_at {
                let formatter = DateFormatter(); formatter.dateFormat = "MM/dd HH:mm"
                reset = " · \(formatter.string(from: Date(timeIntervalSince1970: Double(unix)))) 重置"
            } else { reset = " · 重置时间未提供" }
            return "\(window.title)：剩余 \(number)%\(reset)"
        }.joined(separator: "\n")
    }
}

struct HelperSession: Decodable {
    let key: String
    let id: String
    let title: String
    let title_source: String
    let project: String
    let updated_at: Int64
    let source: String
    let archived: Bool
    let scope: String?
    var sessionScope: String { scope ?? "desktop" }
}

struct SessionSource: Decodable {
    let id: String
    let label: String
    let home: String
    let complete: Bool
    let warnings: [String]
}

struct WarmupUsage: Decodable { let input_tokens: Int64; let output_tokens: Int64; let total_tokens: Int64 }
struct WarmupExecution: Decodable {
    let submitted: Bool
    let completed: Bool
    let model: String?
    let usage: WarmupUsage?
}
struct WarmupAccount: Decodable {
    let key: String
    let name: String
    let status: String
    let message: String
    let last_attempt: String?
    let next_check: String?
    let window_reset: Int64?
    let attempts: Int
    let execution: WarmupExecution?

    var statusTitle: String {
        switch status {
        case "confirmed": return "已完成"
        case "probe_verified": return "测试完成"
        case "window_running": return "窗口运行中"
        case "window_uncertain", "window_check_pending", "verification_pending": return "等待核验"
        case "confirming_idle": return "确认账号空闲"
        case "awaiting_reset": return "等待额度重置"
        case "account_busy": return "账号活动中"
        case "activity_unknown": return "活动状态未知"
        case "quota_unavailable": return "额度暂不可读"
        case "credential_unavailable": return "凭证不可用"
        case "weekly_exhausted": return "周额度耗尽"
        case "account_limited": return "账号受限"
        case "window_unknown": return "窗口信息不足"
        case "setup_failed": return "启动失败"
        default: return message
        }
    }

    var timingCaption: String {
        var values: [String] = []
        if let window_reset = window_reset, window_reset > 0 {
            values.append("窗口重置 \(warmupClock(Date(timeIntervalSince1970: Double(window_reset))))")
        }
        if let next_check = warmupDate(next_check) {
            values.append("下次评估 \(warmupClock(next_check))")
        }
        if let last_attempt = warmupDate(last_attempt) {
            values.append("最近请求 \(warmupClock(last_attempt))")
        }
        return values.joined(separator: " · ")
    }

    var reasonCaption: String {
        switch status {
        case "confirmed": return "请求已完成并核验；窗口运行期间不会重复发送。"
        case "probe_verified": return "测试请求已完成并核验；自动调度仍按窗口状态判断。"
        case "window_running": return "窗口已运行；等待窗口结束后再评估是否需要请求。"
        case "window_uncertain", "window_check_pending", "verification_pending": return message
        case "confirming_idle": return "正在复查窗口状态，避免重复发送。"
        case "awaiting_reset": return message
        default: return message
        }
    }

    var dashboardLine: String {
        let timing = timingCaption
        return "\(name)  ·  \(statusTitle)" + (timing.isEmpty ? "" : "  ·  \(timing)") + "\n  \(reasonCaption)"
    }
}

private func warmupDate(_ value: String?) -> Date? {
    guard let value = value else { return nil }
    let parser = ISO8601DateFormatter()
    parser.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    return parser.date(from: value) ?? ISO8601DateFormatter().date(from: value)
}

private func warmupClock(_ date: Date) -> String {
    let formatter = DateFormatter()
    formatter.locale = Locale(identifier: "en_US_POSIX")
    formatter.timeZone = TimeZone(identifier: "Asia/Singapore")
    formatter.dateFormat = "HH:mm"
    return formatter.string(from: date)
}
struct WarmupSchedule: Decodable {
    let enabled: Bool
    let start: String
    let end: String
    let time_zone: String
    var caption: String {
        guard enabled else { return "全天允许 · UTC+8" }
        return "每日 \(start)–\(end < start ? "次日 " : "")\(end) · UTC+8"
    }
}
struct WarmupReport: Decodable {
    let enabled: Bool
    let message: String
    let accounts: [WarmupAccount]
    let available: Bool
    let schedule: WarmupSchedule?
    let schedule_allowed: Bool?
    let next_opening: String?
    let error: String?
    var headline: String {
        guard enabled else { return "自动预热已关闭" }
        if schedule_allowed == false { return "后台已开启 · 当前时段外" }
        guard available else { return "后台已开启 · 预热程序不可用" }
        guard !accounts.isEmpty else { return "后台运行中 · 等待首次检查" }
        if accounts.allSatisfy({ $0.status == "confirmed" }) { return "后台运行中 · 本轮预热已完成" }
        if accounts.allSatisfy({ ["confirmed", "probe_verified", "window_running"].contains($0.status) }) {
            return "后台运行中 · 窗口已开启，等待结束后评估"
        }
        let blocked = accounts.filter { ["quota_unavailable", "credential_unavailable", "weekly_exhausted", "account_limited", "activity_unknown", "setup_failed", "window_unknown"].contains($0.status) }.count
        if blocked > 0 { return "后台运行中 · \(blocked) 个账号需要关注" }
        return "后台运行中 · 正在评估账号条件"
    }

    var details: String {
        guard enabled else { return "开启后会按时段、额度窗口和账号活动判断是否需要发送预热请求。" }
        if schedule_allowed == false {
            guard let date = warmupDate(next_opening) else { return "当前不在允许预热时段；到达设定时段后重新评估。" }
            return "当前不在允许预热时段；下次开放 \(warmupClock(date))（UTC+8）。"
        }
        if accounts.isEmpty { return "已开启，等待首次检查；仅已启用预热的账号会参与。" }
        return accounts.map(\.dashboardLine).joined(separator: "\n")
    }

    var summary: String { ([headline] + [details]).joined(separator: "\n") }

    var footerSummary: String {
        guard !accounts.isEmpty else { return "预热后台运行 · 等待首次检查" }
        let completed = accounts.filter { ["confirmed", "probe_verified", "window_running"].contains($0.status) }.count
        if completed == accounts.count {
            return "预热后台运行 · \(completed) 个账号窗口已核验"
        }
        return "预热后台运行 · \(accounts.count) 个账号监控中"
    }
}

struct HandoffPreflightReport: Decodable {
	let scope: String?
    let threshold: Double
    let selected: Bool
    let source_name: String?
    let source_remaining: Double?
    let target_name: String?
    let target_remaining: Double?
    let trigger_reached: Bool
    let quota_ready: Bool
	let candidate_ready: Bool?
	let candidate_source: String?
    let execution_ready: Bool
    let message: String
    let limitations: [String]

    var summary: String {
        if scope == "desktop" {
            if !execution_ready { return "条件未满足，暂不能开启 Desktop 监控。" }
            return trigger_reached ? "检查通过，已达到阈值，可以开启监控。" : "检查通过，可以开启监控；当前额度未达到触发阈值。"
        }
        if !execution_ready { return "检查未通过，暂不能开启自动切号。" }
        return trigger_reached ? "检查通过，已达到阈值，可以开启自动切号。" : "检查通过，可以开启自动切号；当前额度未达到触发阈值。"
    }

    var details: String {
        var lines = [message]
		if scope == "desktop" { lines.append(execution_ready ? "当前检查通过，可以手动开启监控。" : "当前条件尚未满足，暂不能开启 Desktop 监控。") }
        if let name = source_name, let value = source_remaining {
            lines.append("当前目录账号 \(name)：剩余 \(String(format: "%.1f", value))%\(trigger_reached ? "，已低于阈值" : "，未低于阈值")。")
        }
        if let name = target_name, let value = target_remaining {
            lines.append("备用账号 \(name)：五小时剩余 \(String(format: "%.1f", value))%，额度符合检查条件。")
        } else if selected { lines.append("没有确认额度符合条件的备用账号。") }
        if candidate_ready == true, let source = candidate_source { lines.append("完整凭证候选：\(source)。") }
        lines.append(contentsOf: limitations.map { "• " + $0 })
        return lines.joined(separator: "\n")
    }
}

struct HandoffReport: Decodable {
    let enabled: Bool
	let authorization_pending: Bool?
	var hasAuthorization: Bool { enabled || authorization_pending == true }
    let scope: String
    let session_id: String
    let threshold: Double
    let stage: String
    let message: String
    let authorization_id: String?
    let recovery_pending: Bool?
    let runner_ready: Bool?
}

struct Snapshot: Decodable {
    let version: String
    let captured_at: String
    let codex_home: String
    var accounts: [HelperAccount]
    let sessions: [HelperSession]
    let sessions_complete: Bool?
    let capabilities: [String: Capability]
    let warnings: [String]
    let sources: [SessionSource]?
    let warmup: WarmupReport?
    let resume: ResumeReport?
    let handoff: HandoffReport?

    func sessions(in scope: String) -> [HelperSession] { sessions.filter { $0.sessionScope == scope } }
    func complete(in scope: String) -> Bool {
        if let sources = sources { return sources.first(where: { $0.id == scope })?.complete ?? false }
        return scope == "desktop" && sessions_complete == true
    }
    func source(in scope: String) -> SessionSource? { sources?.first(where: { $0.id == scope }) }
    func warnings(in scope: String) -> [String] { warnings + (source(in: scope)?.warnings ?? []) }

    mutating func keepQuotaReadings(from previous: Snapshot?) {
        let previousAccounts = Dictionary((previous?.accounts ?? []).map { ($0.id,$0) }, uniquingKeysWith: { _,new in new })
        for index in accounts.indices where accounts[index].quota == nil {
            accounts[index].quota = previousAccounts[accounts[index].id]?.quota
            if let prior = previousAccounts[accounts[index].id], prior.quota != nil { accounts[index].quota_status = prior.quota_status }
        }
    }
}

// Observation is a separate concept from permission to switch accounts. The
// first iteration has no execution adapter and can never grant that permission.
struct SelectionState: Codable {
    var selectedKey: String?
    var observing = false
    var expandedProjects: [String] = []
    var projectLayoutInitialized = false
    var activeScope = "desktop"
    var selectedByScope: [String: String] = [:]
    var initializedScopes: [String] = []

    init() {}
    enum CodingKeys: String, CodingKey { case selectedKey, observing, expandedProjects, projectLayoutInitialized, activeScope, selectedByScope, initializedScopes }
    init(from decoder: Decoder) throws {
        let values = try decoder.container(keyedBy: CodingKeys.self)
        selectedKey = try values.decodeIfPresent(String.self, forKey: .selectedKey)
        observing = try values.decodeIfPresent(Bool.self, forKey: .observing) ?? false
        expandedProjects = try values.decodeIfPresent([String].self, forKey: .expandedProjects) ?? []
        projectLayoutInitialized = try values.decodeIfPresent(Bool.self, forKey: .projectLayoutInitialized) ?? false
        let scope = try values.decodeIfPresent(String.self, forKey: .activeScope) ?? "desktop"
        activeScope = scope == "cli" ? "cli" : "desktop"
        selectedByScope = try values.decodeIfPresent([String: String].self, forKey: .selectedByScope) ?? [:]
        initializedScopes = try values.decodeIfPresent([String].self, forKey: .initializedScopes) ?? []
    }

    mutating func select(_ key: String?) {
        guard key != selectedKey else { return }
        selectedKey = key
        selectedByScope[activeScope] = key
        observing = false
    }

    mutating func changeScope(_ scope: String) {
        guard ["desktop", "cli"].contains(scope), activeScope != scope else { return }
        selectedByScope[activeScope] = selectedKey
        activeScope = scope; selectedKey = selectedByScope[scope]; observing = false
    }

    func layoutInitialized(in scope: String) -> Bool {
        initializedScopes.contains(scope) || (scope == "desktop" && projectLayoutInitialized)
    }
    mutating func initializeLayout(in scope: String) {
        if !initializedScopes.contains(scope) { initializedScopes.append(scope) }
        if scope == "desktop" { projectLayoutInitialized = true }
    }

    mutating func reconcile(_ sessions: [HelperSession], complete: Bool = true) {
        guard let key = selectedKey else { observing = false; return }
        if let session = sessions.first(where: { $0.key == key }) {
            if session.archived || !complete { observing = false }
        } else if complete { select(nil) }
        else { observing = false }
    }

    mutating func toggleObservation() {
        guard selectedKey != nil else { observing = false; return }
        observing.toggle()
    }

    mutating func onLaunch() { observing = false }
    mutating func pauseObservation() { observing = false }
}

struct SessionProject {
    let key: String
    let path: String
    var title: String
    let sessions: [HelperSession]
    var latestActivity: Int64 { sessions.first?.updated_at ?? 0 }
}

func projectKey(_ path: String, scope: String = "desktop") -> String {
    let key = path.isEmpty ? "unknown-project" : "path:" + (path as NSString).standardizingPath
    return scope == "desktop" ? key : scope + "|" + key
}

func groupedSessions(_ sessions: [HelperSession]) -> [SessionProject] {
    let groups = Dictionary(grouping: sessions) { projectKey($0.project, scope: $0.sessionScope) }
    var projects = groups.map { key, sessions -> SessionProject in
        let path = sessions[0].project.isEmpty ? "" : (sessions[0].project as NSString).standardizingPath
        let title = path.isEmpty ? "未指定项目" : (path == "/" ? "根目录" : (path as NSString).lastPathComponent)
        let sorted = sessions.sorted { $0.updated_at == $1.updated_at ? $0.key < $1.key : $0.updated_at > $1.updated_at }
        return SessionProject(key: key, path: path, title: title, sessions: sorted)
    }
    let names = Dictionary(grouping: projects, by: \.title)
    for index in projects.indices where (names[projects[index].title]?.count ?? 0) > 1 {
        projects[index].title += " · " + (projects[index].path as NSString).deletingLastPathComponent
    }
    return projects.sorted {
        $0.latestActivity == $1.latestActivity ? $0.key < $1.key : $0.latestActivity > $1.latestActivity
    }
}

func filteredSessions(_ sessions: [HelperSession], search: String, includeArchived: Bool) -> [HelperSession] {
    let term = search.trimmingCharacters(in: .whitespacesAndNewlines)
    return sessions.filter {
        (includeArchived || !$0.archived) && (term.isEmpty || "\($0.title) \($0.project) \($0.id)".localizedStandardContains(term))
    }
}

struct StateStore {
    let directory: URL
    var file: URL { directory.appendingPathComponent("ui-state.json") }

    func load() -> SelectionState {
        var state = (try? JSONDecoder().decode(SelectionState.self, from: Data(contentsOf: file))) ?? SelectionState()
        state.onLaunch()
        return state
    }

    func save(_ state: SelectionState) throws {
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true,
                                               attributes: [.posixPermissions: 0o700])
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: directory.path)
        let data = try JSONEncoder().encode(state)
        try data.write(to: file, options: .atomic)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: file.path)
    }
}

struct ResumeReport: Decodable {
    let enabled: Bool
    let scope: String
    let id: String
    let key: String
    let status: String
    let message: String
}
