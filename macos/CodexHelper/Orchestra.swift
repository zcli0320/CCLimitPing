import Cocoa
import Darwin

struct OrchestraAgent: Decodable, Equatable {
    let name: String
    let label: String
    let available: Bool
    let reason: String?
}
struct OrchestraAutoSwitchStatus: Decodable {
    let stage: String; let source: String?; let target: String?; let started_at: String?; let review_required: Bool?
    var display: String {
        let names = ["pending":"切换已登记", "resuming":"继续任务中", "completed":"已完成", "uncertain":"结果待核查", "acknowledged":"已核查，等待新的额度触发"]
        var text = "自动切号：" + (names[stage] ?? "状态未知")
        if let source = source, let target = target { text += " · \(source) → \(target)" }
        if let started = started_at {
            text += " · 上次尝试 \(started)"
            let formatter = ISO8601DateFormatter(); formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
            let date = formatter.date(from: started) ?? ISO8601DateFormatter().date(from: started)
            if let date = date { let elapsed = Date().timeIntervalSince(date); text += elapsed < 0 ? " · 冷却时间待核查" : (elapsed < 1800 ? " · 冷却约 \(Int(ceil((1800-elapsed)/60))) 分钟" : " · 冷却已结束") }
            else { text += " · 冷却时间待核查" }
        }
        if review_required == true { text += " · 需要核查" }; return text
    }
}
struct OrchestraRunnerStatus: Decodable { let active: Bool; let dir: String; let paused: Bool?; let reason: String?; let remaining: Double?; let account: String?; let auto_switch: OrchestraAutoSwitchStatus? }
struct OrchestraSettings: Decodable { let account: String; let min_remaining: Double; let auto_switch: Bool?; let auto_start_runner: Bool?; let auto_rework_max: Int? }
struct OrchestraLoginStatus: Decodable { let logged_in: Bool; let state: String; let mode: String; let detail: String }
struct OrchestraLoginInfo: Decodable { let dir: String; let created: [String]; let login: OrchestraLoginStatus; let codex_bin: String }
struct OrchestraLoginActions {
    let canLogin: Bool
    let canLogout: Bool
    init(state: String, busy: Bool) {
        canLogin = !busy && state != "logged_in"
        canLogout = !busy && state == "logged_in"
    }
}
struct OrchestraRun: Decodable, Equatable { let receipt: String?; let error: String? }
struct OrchestraCheckEvidence: Decodable, Equatable { let command: String; let status: String; let exit: Int; let tail: String? }
struct OrchestraChangeEvidence: Decodable, Equatable { let status: String; let changed: [String]?; let out_of_scope: [String]?; let head_changed: Bool? }
struct OrchestraEvidence: Decodable, Equatable { let summary: String; let checks: [OrchestraCheckEvidence]?; let changes: OrchestraChangeEvidence?; let phase: String? }
struct OrchestraProject: Decodable, Equatable {
    let id: String; let name: String; let folder: String; let executor: String
    let lead: String?; let reviewer: String?; let checks: [String]?; let scope: [String]?; let archived: Bool?
}
struct OrchestraTask: Decodable, Equatable {
    let id: String
    let lead: String?
    let executor: String
    let folder: String
    let card: String
    let status: String
    let session: String?
    let updated: String
    let runs: [OrchestraRun]?
    let unconfirmed: Bool?
    let evidence: OrchestraEvidence?
    let project: String?
    let review_of: String?
    let review_task: String?
    let review_verdict: String?
    let review_text: String?
    var canCancel: Bool { status == "queued" || status == "running" || status == "reviewing" }
    var canAccept: Bool { status == "receipt" || status == "failed" || status == "reviewing" }
    var canRework: Bool { (status == "receipt" || status == "failed" || status == "quota_exhausted") && unconfirmed != true }
    var canResume: Bool { status == "quota_exhausted" && !(session ?? "").isEmpty && unconfirmed != true }
    var canConfirmStopped: Bool { unconfirmed == true }
    var lastResult: String { runs?.last?.receipt ?? runs?.last?.error ?? "暂无回执" }
    var cardHeadline: String {
        let first = card.components(separatedBy: .newlines).map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }.first { !$0.isEmpty } ?? ""
        return first.isEmpty ? "（无说明）" : first
    }
    var statusLabel: String {
        if unconfirmed == true { return "待确认已停止" }
        if evidence?.phase == "checking" { return "检查中" }
        return ["queued":"排队中","running":"执行中","reviewing":"审查中","receipt":"待验收","failed":"失败","quota_exhausted":"额度耗尽","cancelled":"已取消","accepted":"已验收"][status] ?? status
    }
    var reviewLabel: String {
        if let v = review_verdict, let label = ["pass":"通过","changes":"需修改","unavailable":"审查不可用"][v] { return label }
        if status == "reviewing" { return "审查中" }
        if !(review_of ?? "").isEmpty { return "审查任务" }
        return "—"
    }
}

enum OrchestraCLIError: LocalizedError {
    case failed(String)
    var errorDescription: String? { if case .failed(let message) = self { return message }; return nil }
}

func orchestraConnectionInstructions(executable: URL, stateDir: String?) -> String {
    func shellQuote(_ value: String) -> String { "'" + value.replacingOccurrences(of: "'", with: "'\\''") + "'" }
    func tomlQuote(_ value: String) -> String { "\"" + value.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\"", with: "\\\"") + "\"" }
    let override = stateDir.flatMap { $0.isEmpty ? nil : $0 }
    let shellEnv = "CODEX_HELPER_NATIVE_PREVIEW=1" + (override.map { " CODEX_HELPER_ORCHESTRA_DIR=" + shellQuote($0) } ?? "")
    let configEnv = "CODEX_HELPER_NATIVE_PREVIEW = \"1\"" + (override.map { "\nCODEX_HELPER_ORCHESTRA_DIR = " + tomlQuote($0) } ?? "")
    return "Claude Code:\nclaude mcp add codex-helper-orchestra -- /usr/bin/env \(shellEnv) \(shellQuote(executable.path)) orchestra mcp --lead claude\n\nCodex（~/.codex/config.toml）:\n[mcp_servers.codex-helper-orchestra]\ncommand = \(tomlQuote(executable.path))\nargs = [\"orchestra\", \"mcp\", \"--lead\", \"codex\"]\n[mcp_servers.codex-helper-orchestra.env]\n\(configEnv)"
}

struct OrchestraProjectGroup: Equatable {
    let key: String
    let projectID: String?
    let title: String
    let subtitle: String
    let folder: String
    let project: OrchestraProject?
    let tasks: [OrchestraTask]
    let isArchived: Bool
    let isUnassigned: Bool
}

private final class OrchestraGroupRowItem: NSObject {
    var group: OrchestraProjectGroup
    var children: [OrchestraTaskRowItem] = []
    init(_ group: OrchestraProjectGroup) { self.group = group }
}

private final class OrchestraTaskRowItem: NSObject {
    var task: OrchestraTask
    weak var parent: OrchestraGroupRowItem?
    init(_ task: OrchestraTask, parent: OrchestraGroupRowItem?) {
        self.task = task
        self.parent = parent
    }
}

private final class OrchestraProjectRowView: NSTableRowView {
    override func drawBackground(in dirtyRect: NSRect) {
        super.drawBackground(in: dirtyRect)
        guard !isSelected else { return }
        NSColor.controlAccentColor.withAlphaComponent(0.05).setFill()
        bounds.fill()
    }
}

func buildOrchestraProjectGroups(
    projects: [OrchestraProject],
    allProjects: [OrchestraProject] = [],
    tasks: [OrchestraTask],
    filterProjectID: String? = nil,
    unassignedOnly: Bool = false
) -> [OrchestraProjectGroup] {
    let sortedTasks = orderOrchestraTasksWithReviewChildren(tasks.sorted { $0.updated > $1.updated })
    let knownByID = Dictionary(uniqueKeysWithValues: (allProjects.isEmpty ? projects : allProjects).map { ($0.id, $0) })
    let activeIDs = Set(projects.map(\.id))

    func makeProjectGroup(_ p: OrchestraProject, groupTasks: [OrchestraTask]) -> OrchestraProjectGroup {
        let rev = (p.reviewer ?? "").isEmpty ? "无审查" : "审查 \(p.reviewer!)"
        let lead = (p.lead ?? "").isEmpty ? "" : "\(p.lead!) → "
        let sub = "\(lead)\(p.executor) · \(rev)" + (p.archived == true ? " · 已归档" : "")
        return OrchestraProjectGroup(
            key: "project:" + p.id,
            projectID: p.id,
            title: p.name,
            subtitle: sub,
            folder: p.folder,
            project: p,
            tasks: groupTasks,
            isArchived: p.archived == true,
            isUnassigned: false
        )
    }

    if unassignedOnly {
        let unassigned = sortedTasks.filter { ($0.project ?? "").isEmpty }
        return [OrchestraProjectGroup(
            key: "unassigned",
            projectID: nil,
            title: "未归入项目",
            subtitle: "独立派发的任务，可归入任意项目工作群",
            folder: "",
            project: nil,
            tasks: unassigned,
            isArchived: false,
            isUnassigned: true
        )]
    }

    if let pid = filterProjectID, let p = knownByID[pid] ?? projects.first(where: { $0.id == pid }) {
        let pTasks = sortedTasks.filter { $0.project == pid }
        return [makeProjectGroup(p, groupTasks: pTasks)]
    }

    var groups: [OrchestraProjectGroup] = []
    for p in projects {
        let pTasks = sortedTasks.filter { $0.project == p.id }
        groups.append(makeProjectGroup(p, groupTasks: pTasks))
    }

    var extraProjectIDs: [String] = []
    for t in sortedTasks {
        guard let pid = t.project, !pid.isEmpty, !activeIDs.contains(pid), !extraProjectIDs.contains(pid) else { continue }
        extraProjectIDs.append(pid)
    }
    for pid in extraProjectIDs {
        let pTasks = sortedTasks.filter { $0.project == pid }
        if let p = knownByID[pid] {
            groups.append(makeProjectGroup(p, groupTasks: pTasks))
        } else {
            groups.append(OrchestraProjectGroup(
                key: "project:" + pid,
                projectID: pid,
                title: pid,
                subtitle: "已归档或外部项目",
                folder: pTasks.first?.folder ?? "",
                project: nil,
                tasks: pTasks,
                isArchived: true,
                isUnassigned: false
            ))
        }
    }

    let unassigned = sortedTasks.filter { ($0.project ?? "").isEmpty }
    if !unassigned.isEmpty || groups.isEmpty {
        groups.append(OrchestraProjectGroup(
            key: "unassigned",
            projectID: nil,
            title: "未归入项目",
            subtitle: groups.isEmpty ? "当前尚未创建项目，建议先点击右上角「新建项目…」建立工作群" : "独立派发的任务，选中后可点击「归入项目…」收纳进项目",
            folder: "",
            project: nil,
            tasks: unassigned,
            isArchived: false,
            isUnassigned: true
        ))
    }
    return groups
}

private func orderOrchestraTasksWithReviewChildren(_ sorted: [OrchestraTask]) -> [OrchestraTask] {
    let byID = Set(sorted.map(\.id))
    var childrenByParent: [String: [OrchestraTask]] = [:]
    var roots: [OrchestraTask] = []
    for t in sorted {
        if let parentID = t.review_of, !parentID.isEmpty, byID.contains(parentID) {
            childrenByParent[parentID, default: []].append(t)
        } else {
            roots.append(t)
        }
    }
    var out: [OrchestraTask] = []
    for root in roots {
        out.append(root)
        if let kids = childrenByParent[root.id] {
            out.append(contentsOf: kids)
        }
    }
    return out
}

// CLI calls run on a serial background queue; stderr is returned to the user.
final class OrchestraWindowController: NSWindowController, NSWindowDelegate, NSOutlineViewDataSource, NSOutlineViewDelegate {
    private let executable: URL
    private let queue = DispatchQueue(label: "codex-helper.orchestra")
    private var timer: Timer?
    private var refreshing = false
    private var renderingOutline = false
    private var agents: [OrchestraAgent] = []
    private var renderedAgents: [OrchestraAgent] = []
    private var projects: [OrchestraProject] = []
    private var allProjects: [OrchestraProject] = []
    private var projectFilterBuilt = false
    private var allTasks: [OrchestraTask] = []
    private var tasks: [OrchestraTask] = []
    private var groupItems: [OrchestraGroupRowItem] = []
    private var groupItemsByKey: [String: OrchestraGroupRowItem] = [:]
    private var taskItemsByID: [String: OrchestraTaskRowItem] = [:]
    private var collapsedGroupKeys: Set<String> = []
    private var pendingSelectProjectID: String?
    private var pendingSelectTaskID: String?
    private var runner: Process?
    private var runnerActive = false
    private var terminating = false
    private var confirmedRunner = false
    private var actionBusy = false
    private var loginInfo: OrchestraLoginInfo?
    private var settings = OrchestraSettings(account: "desktop", min_remaining: 10, auto_switch: false, auto_start_runner: false, auto_rework_max: 1)
    private var runnerStatus: OrchestraRunnerStatus?
    private var loginProcess: Process?
    private var loginBusy = false
    private var loginCancelling = false
    private let outerScroll = NSScrollView()
    private let scrollDocument = FlippedDocument()
    private let contentStack = NSStackView()
    private let runnerStatusChip = StatusChip("未运行", tint: .secondaryLabelColor)
    private let taskCountChip = StatusChip("0 个任务", tint: .secondaryLabelColor)
    private let agentStack = NSStackView()
    private let agentLabel = NSTextField(wrappingLabelWithString: "正在读取代理…")
    private let runnerLabel = NSTextField(wrappingLabelWithString: "正在读取执行器状态…")
    private let loginLabel = NSTextField(wrappingLabelWithString: "正在读取执行账号…")
    private let loginPath = NSTextField(wrappingLabelWithString: "")
    private let loginMessage = NSTextField(wrappingLabelWithString: "")
    private let loginButton = NSButton(title: "登录", target: nil, action: nil)
    private let logoutButton = NSButton(title: "退出登录", target: nil, action: nil)
    private let loginCancel = NSButton(title: "取消登录", target: nil, action: nil)
    private let accountMode = NSPopUpButton()
    private let autoSwitch = NSButton(checkboxWithTitle: "Desktop 已关闭时自动切换账号", target: nil, action: nil)
    private let autoStartRunner = NSButton(checkboxWithTitle: "随小助手启动执行器", target: nil, action: nil)
    private let autoSwitchAck = NSButton(title: "已检查，解除自动切号暂停", target: nil, action: nil)
    private let reworkLimit = NSPopUpButton()
    private let showEvidence = NSButton(checkboxWithTitle: "显示检查详情", target: nil, action: nil)
    private let notice = NSTextField(wrappingLabelWithString: "")
    private let start = NSButton(title: "启动执行器", target: nil, action: nil)
    private let stop = NSButton(title: "停止执行器", target: nil, action: nil)
    private let projectFilter = NSPopUpButton()
    private let projectCaption = Theme.caption("按项目（工作群）收纳展示全部任务。")
    private let expandAllButton = NSButton(title: "全部展开", target: nil, action: nil)
    private let collapseAllButton = NSButton(title: "全部收起", target: nil, action: nil)
    private let newProject = NSButton(title: "新建项目…", target: nil, action: nil)
    private let archiveProject = NSButton(title: "归档项目", target: nil, action: nil)
    private let newTask = NSButton(title: "新任务…", target: nil, action: nil)
    private let assignTask = NSButton(title: "归入项目…", target: nil, action: nil)
    private let cancelTask = NSButton(title: "停止", target: nil, action: nil)
    private let acceptTask = NSButton(title: "验收", target: nil, action: nil)
    private let reworkTask = NSButton(title: "返工", target: nil, action: nil)
    private let resumeTask = NSButton(title: "继续", target: nil, action: nil)
    private let confirmStopped = NSButton(title: "确认已停止", target: nil, action: nil)
    private let refreshButton = NSButton(title: "刷新", target: nil, action: nil)
    private let copyMCPButton = NSButton(title: "复制配置", target: nil, action: nil)
    private let table = NSOutlineView()
    private let details = NSTextView()

    init(executable: URL) {
        self.executable = executable
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 880, height: 800), styleMask: [.titled, .closable, .resizable, .miniaturizable], backing: .buffered, defer: false)
        window.title = "代理协作"; window.minSize = NSSize(width: 680, height: 600)
        window.titleVisibility = .hidden
        window.titlebarAppearsTransparent = true
        window.backgroundColor = .windowBackgroundColor
        super.init(window: window)
        window.delegate = self
        build()
        window.center()
    }
    required init?(coder: NSCoder) { fatalError("init(coder:) has not been implemented") }

    private func build() {
        guard let content = window?.contentView else { return }
        outerScroll.hasVerticalScroller = true; outerScroll.autohidesScrollers = true; outerScroll.drawsBackground = false
        outerScroll.documentView = scrollDocument
        scrollDocument.autoresizingMask = [.width]
        content.addSubview(outerScroll); outerScroll.translatesAutoresizingMaskIntoConstraints = false
        NSLayoutConstraint.activate([outerScroll.leadingAnchor.constraint(equalTo: content.leadingAnchor), outerScroll.trailingAnchor.constraint(equalTo: content.trailingAnchor), outerScroll.topAnchor.constraint(equalTo: content.topAnchor), outerScroll.bottomAnchor.constraint(equalTo: content.bottomAnchor)])
        let stack = contentStack; stack.orientation = .vertical; stack.alignment = .leading; stack.spacing = 12; stack.detachesHiddenViews = true
        scrollDocument.addSubview(stack); stack.translatesAutoresizingMaskIntoConstraints = false
        NSLayoutConstraint.activate([stack.leadingAnchor.constraint(equalTo: scrollDocument.leadingAnchor, constant: 20), stack.trailingAnchor.constraint(equalTo: scrollDocument.trailingAnchor, constant: -20), stack.topAnchor.constraint(equalTo: scrollDocument.topAnchor, constant: 16)])
        func add(_ view: NSView) { stack.addArrangedSubview(view); view.translatesAutoresizingMaskIntoConstraints = false; view.widthAnchor.constraint(equalTo: stack.widthAnchor).isActive = true }

        // Header bar
        let headerIcon = NSImageView(image: NSImage(systemSymbolName: "person.3.sequence.fill", accessibilityDescription: "代理协作")!)
        headerIcon.symbolConfiguration = NSImage.SymbolConfiguration(pointSize: 22, weight: .semibold)
        headerIcon.contentTintColor = .systemIndigo
        headerIcon.setContentHuggingPriority(.required, for: .horizontal)
        let title = Theme.text("多代理协作中心", size: 20, weight: .bold, wraps: false)
        let subtitle = Theme.caption("工作群分组派活、自动审查与无人值守队列执行。")
        let titleBlock = Theme.vstack([Theme.hstack([title, runnerStatusChip, taskCountChip], spacing: 8), subtitle], spacing: 2)
        refreshButton.target = self; refreshButton.action = #selector(refreshManual); Theme.button(refreshButton)
        refreshButton.image = NSImage(systemSymbolName: "arrow.clockwise", accessibilityDescription: nil); refreshButton.imagePosition = .imageLeading
        add(Theme.hstack([headerIcon, titleBlock, Theme.flexible(), refreshButton], spacing: 12))

        // Agents status section
        add(Theme.sectionTitle("代理状态"))
        let agentBox = SectionBox(spacing: 8)
        agentStack.orientation = .horizontal; agentStack.alignment = .centerY; agentStack.spacing = 10; agentStack.distribution = .fillEqually
        agentBox.add(agentStack)
        agentLabel.font = .systemFont(ofSize: 11.5); agentLabel.textColor = .secondaryLabelColor; agentLabel.maximumNumberOfLines = 0
        agentBox.add(agentLabel)
        add(agentBox)

        // Two-column dashboard: Runner (left) & Execution Account (right)
        let runnerBox = SectionBox(spacing: 8)
        runnerBox.add(Theme.hstack([
            NSImageView(image: NSImage(systemSymbolName: "gearshape.2.fill", accessibilityDescription: nil)!),
            Theme.text("执行器控制", size: 13, weight: .semibold, wraps: false),
            Theme.flexible()
        ], spacing: 6))
        runnerLabel.font = .systemFont(ofSize: 11.5); runnerLabel.maximumNumberOfLines = 0; runnerBox.add(runnerLabel)
        autoSwitch.target = self; autoSwitch.action = #selector(changeAutoSwitch)
        autoStartRunner.target = self; autoStartRunner.action = #selector(changeAutoStartRunner)
        runnerBox.add(Theme.hstack([autoSwitch, autoStartRunner, Theme.flexible()], spacing: 12))
        autoSwitchAck.target = self; autoSwitchAck.action = #selector(ackAutoSwitch); Theme.button(autoSwitchAck, small: true)
        autoSwitchAck.image = NSImage(systemSymbolName: "checkmark.shield", accessibilityDescription: nil); autoSwitchAck.imagePosition = .imageLeading
        runnerBox.add(autoSwitchAck)
        reworkLimit.addItems(withTitles:["自动返工：0 次", "自动返工：1 次", "自动返工：2 次"]); reworkLimit.target = self; reworkLimit.action = #selector(changeReworkLimit)
        runnerBox.add(reworkLimit)
        start.target = self; start.action = #selector(startRunner); Theme.button(start, primary: true)
        start.image = NSImage(systemSymbolName: "play.fill", accessibilityDescription: nil); start.imagePosition = .imageLeading
        stop.target = self; stop.action = #selector(stopRunnerAction); Theme.button(stop)
        stop.image = NSImage(systemSymbolName: "stop.fill", accessibilityDescription: nil); stop.imagePosition = .imageLeading
        runnerBox.add(Theme.hstack([start, stop, Theme.flexible()], spacing: 8))
        notice.font = .systemFont(ofSize: 11, weight: .medium); notice.textColor = .systemOrange; notice.maximumNumberOfLines = 0; runnerBox.add(notice)

        let loginBox = SectionBox(spacing: 8)
        loginBox.add(Theme.hstack([
            NSImageView(image: NSImage(systemSymbolName: "person.badge.key.fill", accessibilityDescription: nil)!),
            Theme.text("Codex 执行账号", size: 13, weight: .semibold, wraps: false),
            Theme.flexible()
        ], spacing: 6))
        accountMode.addItems(withTitles: ["使用 Desktop 账号", "使用独立账号"]); accountMode.target = self; accountMode.action = #selector(changeAccountMode); loginBox.add(accountMode)
        loginLabel.font = .systemFont(ofSize: 12, weight: .medium); loginBox.add(loginLabel)
        loginPath.font = .systemFont(ofSize: 11); loginPath.textColor = .secondaryLabelColor; loginPath.isSelectable = true; loginBox.add(loginPath)
        let accountCaption = Theme.caption("任务消耗所选账号的额度；使用独立账号可与日常客户端隔离。")
        accountCaption.maximumNumberOfLines = 0; loginBox.add(accountCaption)
        loginButton.target = self; loginButton.action = #selector(beginLogin); Theme.button(loginButton, small: true)
        loginButton.image = NSImage(systemSymbolName: "key.fill", accessibilityDescription: nil); loginButton.imagePosition = .imageLeading
        logoutButton.target = self; logoutButton.action = #selector(confirmLogout); Theme.button(logoutButton, small: true)
        loginCancel.target = self; loginCancel.action = #selector(cancelLogin); Theme.button(loginCancel, small: true)
        loginBox.add(Theme.hstack([loginButton, logoutButton, loginCancel, Theme.flexible()], spacing: 8))
        loginMessage.font = .systemFont(ofSize: 11); loginMessage.textColor = .secondaryLabelColor; loginMessage.maximumNumberOfLines = 0; loginBox.add(loginMessage)

        let controlGrid = NSStackView(views: [runnerBox, loginBox])
        controlGrid.orientation = .horizontal; controlGrid.distribution = .fillEqually; controlGrid.alignment = .top; controlGrid.spacing = 14
        runnerBox.bottomAnchor.constraint(equalTo: controlGrid.bottomAnchor).isActive = true
        loginBox.bottomAnchor.constraint(equalTo: controlGrid.bottomAnchor).isActive = true
        add(controlGrid)

        // Projects (Workgroups) & Tasks Workbench
        add(Theme.sectionTitle("项目（工作群）与任务队列"))
        let projectBox = SectionBox(spacing: 8)
        projectFilter.addItems(withTitles: ["全部项目与任务", "未归入项目"]); projectFilter.target = self; projectFilter.action = #selector(changeProjectFilter)
        projectFilter.widthAnchor.constraint(greaterThanOrEqualToConstant: 210).isActive = true
        expandAllButton.target = self; expandAllButton.action = #selector(expandAllGroups); Theme.button(expandAllButton, small: true)
        collapseAllButton.target = self; collapseAllButton.action = #selector(collapseAllGroups); Theme.button(collapseAllButton, small: true)
        newProject.target = self; newProject.action = #selector(createProject); Theme.button(newProject, small: true)
        newProject.image = NSImage(systemSymbolName: "folder.badge.plus", accessibilityDescription: nil); newProject.imagePosition = .imageLeading
        archiveProject.target = self; archiveProject.action = #selector(archiveSelectedProject); Theme.button(archiveProject, small: true)
        archiveProject.image = NSImage(systemSymbolName: "archivebox", accessibilityDescription: nil); archiveProject.imagePosition = .imageLeading
        let folderIcon = NSImageView(image: NSImage(systemSymbolName: "folder.fill.badge.person.crop", accessibilityDescription: nil)!)
        folderIcon.contentTintColor = .systemBlue
        projectBox.add(Theme.hstack([folderIcon, Theme.text("工作群视图：", size: 12, weight: .medium, wraps: false), projectFilter, expandAllButton, collapseAllButton, Theme.flexible(), newProject, archiveProject], spacing: 8))
        projectCaption.maximumNumberOfLines = 0; projectBox.add(projectCaption)
        add(projectBox)

        let taskBox = SectionBox(spacing: 10)
        for (identifier,title,width) in [("id","项目 / 任务",220.0),("status","状态",96.0),("executor","执行与审查",135.0),("review","审查结论",88.0),("summary","任务说明 / 证据摘要",290.0),("folder","工作目录",140.0),("updated","更新时间",145.0)] {
            let column = NSTableColumn(identifier: NSUserInterfaceItemIdentifier(identifier)); column.title = title; column.width = width; table.addTableColumn(column)
        }
        table.outlineTableColumn = table.tableColumns.first
        table.indentationPerLevel = 16
        table.delegate = self; table.dataSource = self; table.rowHeight = 28; table.usesAlternatingRowBackgroundColors = true
        table.intercellSpacing = NSSize(width: 8, height: 2)
        table.setAccessibilityLabel("协作任务列表")
        let tableScroll = NestedScrollView(); tableScroll.documentView = table; tableScroll.hasVerticalScroller = true; tableScroll.hasHorizontalScroller = true
        tableScroll.borderType = .bezelBorder
        tableScroll.heightAnchor.constraint(equalToConstant: 240).isActive = true; taskBox.add(tableScroll)
        newTask.target = self; newTask.action = #selector(createTask); Theme.button(newTask, small: true, primary: true)
        newTask.image = NSImage(systemSymbolName: "plus.circle.fill", accessibilityDescription: nil); newTask.imagePosition = .imageLeading
        assignTask.target = self; assignTask.action = #selector(assignSelectedTaskToProject); Theme.button(assignTask, small: true)
        assignTask.image = NSImage(systemSymbolName: "folder.badge.gearshape", accessibilityDescription: nil); assignTask.imagePosition = .imageLeading
        cancelTask.target = self; cancelTask.action = #selector(cancelSelected); Theme.button(cancelTask, small: true)
        cancelTask.image = NSImage(systemSymbolName: "stop.circle", accessibilityDescription: nil); cancelTask.imagePosition = .imageLeading
        acceptTask.target = self; acceptTask.action = #selector(acceptSelected); Theme.button(acceptTask, small: true)
        acceptTask.image = NSImage(systemSymbolName: "checkmark.circle", accessibilityDescription: nil); acceptTask.imagePosition = .imageLeading
        reworkTask.target = self; reworkTask.action = #selector(reworkSelected); Theme.button(reworkTask, small: true)
        reworkTask.image = NSImage(systemSymbolName: "arrow.uturn.backward.circle", accessibilityDescription: nil); reworkTask.imagePosition = .imageLeading
        resumeTask.target = self; resumeTask.action = #selector(resumeSelected); Theme.button(resumeTask, small: true)
        resumeTask.image = NSImage(systemSymbolName: "play.circle", accessibilityDescription: nil); resumeTask.imagePosition = .imageLeading
        confirmStopped.target = self; confirmStopped.action = #selector(confirmStoppedSelected); Theme.button(confirmStopped, small: true)
        confirmStopped.image = NSImage(systemSymbolName: "exclamationmark.triangle", accessibilityDescription: nil); confirmStopped.imagePosition = .imageLeading
        showEvidence.target = self; showEvidence.action = #selector(toggleEvidence)
        showEvidence.controlSize = .small; showEvidence.font = .systemFont(ofSize: 11)
        taskBox.add(Theme.hstack([newTask, assignTask, showEvidence, Theme.flexible(), acceptTask, reworkTask, resumeTask, cancelTask, confirmStopped], spacing: 8))
        taskBox.addDivider()
        details.isEditable = false; details.isSelectable = true; details.font = .systemFont(ofSize: 12); details.string = "选择项目查看配置，或选择任务查看任务卡与回执。"
        details.textContainerInset = NSSize(width: 8, height: 8)
        let detailScroll = NestedScrollView(); detailScroll.documentView = details; detailScroll.hasVerticalScroller = true; detailScroll.borderType = .bezelBorder
        details.isVerticallyResizable = true; details.autoresizingMask = [.width]; details.textContainer?.widthTracksTextView = true
        detailScroll.heightAnchor.constraint(equalToConstant: 150).isActive = true; taskBox.add(detailScroll); add(taskBox)

        // MCP Connection Footer
        copyMCPButton.target = self; copyMCPButton.action = #selector(copyMCPInstructions); Theme.button(copyMCPButton, small: true)
        copyMCPButton.image = NSImage(systemSymbolName: "doc.on.doc", accessibilityDescription: nil); copyMCPButton.imagePosition = .imageLeading
        add(Theme.sectionTitle("连接主控（MCP 配置）", accessories: [copyMCPButton]))
        let connectBox = SectionBox(); let instructions = NSTextView(); instructions.isEditable = false; instructions.isSelectable = true; instructions.font = .monospacedSystemFont(ofSize: 11, weight: .regular)
        instructions.textContainerInset = NSSize(width: 8, height: 8)
        instructions.string = orchestraConnectionInstructions(executable: executable, stateDir: ProcessInfo.processInfo.environment["CODEX_HELPER_ORCHESTRA_DIR"])
        let connectScroll = NestedScrollView(); connectScroll.documentView = instructions; connectScroll.hasVerticalScroller = true; connectScroll.borderType = .bezelBorder
        instructions.isVerticallyResizable = true; instructions.autoresizingMask = [.width]; instructions.textContainer?.widthTracksTextView = true
        connectScroll.heightAnchor.constraint(equalToConstant: 125).isActive = true; connectBox.add(connectScroll); add(connectBox)
        updateButtons()
        layoutContent()
    }

    func windowDidResize(_ notification: Notification) { layoutContent() }
    func layoutContent() {
        let clip = outerScroll.contentView
        let savedOrigin = clip.bounds.origin
        outerScroll.layoutSubtreeIfNeeded()
        let width = max(outerScroll.contentSize.width, 1)
        let initialHeight = max(scrollDocument.frame.height, outerScroll.contentSize.height, 1)
        if abs(scrollDocument.frame.width - width) > 0.5 || scrollDocument.frame.height < 1 {
            scrollDocument.setFrameSize(NSSize(width: width, height: initialHeight))
        }
        scrollDocument.layoutSubtreeIfNeeded()
        let stackHeight = max(contentStack.frame.maxY, ceil(contentStack.fittingSize.height) + 16)
        let height = max(outerScroll.contentSize.height, ceil(stackHeight) + 20)
        if abs(scrollDocument.frame.width - width) > 0.5 || abs(scrollDocument.frame.height - height) > 0.5 {
            scrollDocument.setFrameSize(NSSize(width: width, height: height))
        }
        let target = clip.constrainBoundsRect(NSRect(origin: savedOrigin, size: clip.bounds.size)).origin
        if abs(clip.bounds.origin.x - target.x) > 0.5 || abs(clip.bounds.origin.y - target.y) > 0.5 {
            clip.setBoundsOrigin(target)
            outerScroll.reflectScrolledClipView(clip)
        }
    }
    @objc private func refreshManual() { refresh() }
    @objc private func copyMCPInstructions() {
        let text = orchestraConnectionInstructions(executable: executable, stateDir: ProcessInfo.processInfo.environment["CODEX_HELPER_ORCHESTRA_DIR"])
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(text, forType: .string)
        notice.stringValue = "已复制 MCP 连接配置到剪贴板。"
    }

    override func showWindow(_ sender: Any?) {
        super.showWindow(sender); window?.makeKeyAndOrderFront(nil); NSApp.activate(ignoringOtherApps: true)
        layoutContent()
        refresh()
        if timer == nil { timer = Timer.scheduledTimer(withTimeInterval: 3, repeats: true) { [weak self] _ in self?.refresh() } }
    }
    func windowWillClose(_ notification: Notification) { timer?.invalidate(); timer = nil; cancelLogin() }

    private func command(_ arguments: [String], input: String? = nil) throws -> Data {
        let process = Process(); process.executableURL = executable; process.arguments = ["orchestra"] + arguments
        let stdout = Pipe(), stderr = Pipe(); process.standardOutput = stdout; process.standardError = stderr
        let stdin = Pipe(); process.standardInput = stdin
        try process.run()
        if let input = input { try stdin.fileHandleForWriting.write(contentsOf: Data(input.utf8)) }
        try stdin.fileHandleForWriting.close()
        let errData = NSMutableData(); let group = DispatchGroup(); group.enter()
        DispatchQueue.global(qos: .utility).async { errData.append(stderr.fileHandleForReading.readDataToEndOfFile()); group.leave() }
        let data = stdout.fileHandleForReading.readDataToEndOfFile(); process.waitUntilExit(); group.wait()
        guard process.terminationStatus == 0 else { throw OrchestraCLIError.failed(String(data: errData as Data, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines) ?? "命令执行失败") }
        return data
    }

    private func refresh() {
        guard window?.isVisible == true, !refreshing else { return }
        refreshing = true
        queue.async { [weak self] in
            guard let self = self else { return }
            do {
                let decoder = JSONDecoder()
                let agents = try decoder.decode([OrchestraAgent].self, from: self.command(["agents"]))
                let status = try decoder.decode(OrchestraRunnerStatus.self, from: self.command(["runner"]))
                let settings = try decoder.decode(OrchestraSettings.self, from: self.command(["settings"]))
                let login = try decoder.decode(OrchestraLoginInfo.self, from: self.command(["login-status"]))
                let projectData = try self.command(["project", "list", "--all"])
                let allProjects = try decoder.decode([OrchestraProject]?.self, from: projectData) ?? []
                let projects = allProjects.filter { $0.archived != true }
                let taskData = try self.command(["list"])
                let tasks = try decoder.decode([OrchestraTask]?.self, from: taskData) ?? []
                DispatchQueue.main.async {
                    let selectedTaskID = self.pendingSelectTaskID ?? self.selected?.id
                    let selectedGroupKey = self.pendingSelectProjectID.map { "project:" + $0 } ?? self.selectedGroup?.key
                    self.pendingSelectTaskID = nil
                    self.pendingSelectProjectID = nil
                    self.refreshing = false
                    self.agents = agents
                    self.runnerActive = status.active
                    self.runnerStatus = status
                    self.settings = settings
                    self.loginInfo = login
                    self.allProjects = allProjects
                    if !self.projectFilterBuilt || self.projects != projects {
                        let prevProjectID = self.selectedFilterProject?.id
                        let prevUnassigned = self.isUnassignedSelected
                        self.projects = projects
                        self.projectFilterBuilt = true
                        self.projectFilter.removeAllItems()
                        self.projectFilter.addItems(withTitles: ["全部项目与任务"] + projects.map { "\($0.name)（\($0.id)）" } + ["未归入项目"])
                        if let pid = prevProjectID, let idx = projects.firstIndex(where: { $0.id == pid }) {
                            self.projectFilter.selectItem(at: idx + 1)
                        } else if prevUnassigned {
                            self.projectFilter.selectItem(at: projects.count + 1)
                        } else {
                            self.projectFilter.selectItem(at: 0)
                        }
                    }
                    self.allTasks = tasks.sorted { $0.updated > $1.updated }
                    self.applyProjectFilter()
                    self.render(selectedTaskID: selectedTaskID, selectedGroupKey: selectedGroupKey)
                }
            } catch { DispatchQueue.main.async { self.refreshing = false; self.notice.stringValue = "刷新失败：\(error.localizedDescription)"; self.updateButtons() } }
        }
    }

    private var selectedFilterProject: OrchestraProject? {
        let idx = projectFilter.indexOfSelectedItem - 1
        return projects.indices.contains(idx) ? projects[idx] : nil
    }
    private var isUnassignedSelected: Bool {
        projectFilter.indexOfSelectedItem == projects.count + 1 && projectFilter.numberOfItems == projects.count + 2
    }
    private var selectedGroup: OrchestraProjectGroup? {
        let row = table.selectedRow
        guard row >= 0 else { return nil }
        if let g = table.item(atRow: row) as? OrchestraGroupRowItem { return g.group }
        if let t = table.item(atRow: row) as? OrchestraTaskRowItem { return t.parent?.group }
        return nil
    }
    private var selected: OrchestraTask? {
        let row = table.selectedRow
        guard row >= 0, let item = table.item(atRow: row) as? OrchestraTaskRowItem else { return nil }
        return item.task
    }
    private var activeProject: OrchestraProject? {
        if let rowGroup = selectedGroup, let p = rowGroup.project, p.archived != true { return p }
        if let t = selected, let pid = t.project, let p = projects.first(where: { $0.id == pid }) { return p }
        return selectedFilterProject
    }

    private func applyProjectFilter() {
        let groups = buildOrchestraProjectGroups(
            projects: projects,
            allProjects: allProjects,
            tasks: allTasks,
            filterProjectID: selectedFilterProject?.id,
            unassignedOnly: isUnassignedSelected
        )
        tasks = groups.flatMap(\.tasks)
        var nextGroupsByKey: [String: OrchestraGroupRowItem] = [:]
        var nextTasksByID: [String: OrchestraTaskRowItem] = [:]
        groupItems = groups.map { grp in
            let gItem = groupItemsByKey[grp.key] ?? OrchestraGroupRowItem(grp)
            gItem.group = grp
            gItem.children = grp.tasks.map { t in
                let tItem = taskItemsByID[t.id] ?? OrchestraTaskRowItem(t, parent: gItem)
                tItem.task = t
                tItem.parent = gItem
                nextTasksByID[t.id] = tItem
                return tItem
            }
            nextGroupsByKey[grp.key] = gItem
            return gItem
        }
        groupItemsByKey = nextGroupsByKey
        taskItemsByID = nextTasksByID
    }

    private func render(selectedTaskID: String?, selectedGroupKey: String?) {
        let previousOuterOrigin = outerScroll.contentView.bounds.origin
        let previousTableOrigin = table.enclosingScrollView?.contentView.bounds.origin
        let previousDetailOrigin = details.enclosingScrollView?.contentView.bounds.origin
        agentLabel.stringValue = agents.map { "\($0.label)：\($0.available ? "可用" : "不可用")" + (($0.available || $0.reason == nil) ? "" : " · \($0.reason!)") }.joined(separator: "\n")
        if renderedAgents != agents {
            renderedAgents = agents
            for view in agentStack.arrangedSubviews { agentStack.removeArrangedSubview(view); view.removeFromSuperview() }
            if !agents.isEmpty {
                for agent in agents {
                    let icon = NSImageView(image: NSImage(systemSymbolName: agent.available ? "checkmark.circle.fill" : "xmark.circle", accessibilityDescription: nil)!)
                    icon.contentTintColor = agent.available ? .systemGreen : .secondaryLabelColor
                    let name = Theme.text(agent.label, size: 12.5, weight: .semibold, wraps: false)
                    let chip = StatusChip(agent.available ? "可用" : "不可用", tint: agent.available ? .systemGreen : .secondaryLabelColor)
                    let top = Theme.hstack([icon, name, Theme.flexible(), chip], spacing: 6)
                    let sub = Theme.caption(agent.available ? "可执行派活与自动审查" : (agent.reason ?? "暂不可用"))
                    sub.maximumNumberOfLines = 1; sub.lineBreakMode = .byTruncatingTail; sub.toolTip = agent.reason
                    let card = SectionBox(spacing: 4, insets: NSEdgeInsets(top: 8, left: 10, bottom: 8, right: 10))
                    card.add(top); card.add(sub)
                    agentStack.addArrangedSubview(card)
                }
                agentStack.isHidden = false
                agentLabel.isHidden = true
            } else {
                agentStack.isHidden = true
                agentLabel.isHidden = false
            }
        }
        let paused = runnerStatus?.paused == true
        runnerStatusChip.text = runnerActive ? (paused ? "已暂停" : "执行器运行中") : "未运行"
        runnerStatusChip.tint = runnerActive ? (paused ? .systemOrange : .systemGreen) : .secondaryLabelColor
        let runningCount = allTasks.filter { $0.status == "running" || $0.status == "reviewing" || $0.status == "queued" }.count
        taskCountChip.text = runningCount > 0 ? "\(projects.count) 个项目 · \(tasks.count) 个任务（\(runningCount) 进行中）" : "\(projects.count) 个项目 · \(tasks.count) 个任务"
        taskCountChip.tint = runningCount > 0 ? .systemBlue : .secondaryLabelColor
        runnerLabel.stringValue = runnerActive ? ((runner?.isRunning == true) ? "执行器运行中（由本小助手启动）" : "检测到其他执行器正在运行") : "执行器未运行"
        if let status = runnerStatus, (settings.account == "desktop" && status.account == "Desktop") || (settings.account == "separate" && status.account == "独立账号") {
            runnerLabel.stringValue += "\n\(status.account ?? "账号") 五小时剩余：" + (status.remaining.map { String(format: "%.1f%%", $0) } ?? "未知") + (status.paused == true ? " · 已暂停：\(status.reason ?? "额度未知")" : "")
        }
        if settings.account == "separate" { runnerLabel.stringValue += "\n独立账号：开始前不检查额度" }
        if let status = runnerStatus?.auto_switch {
            runnerLabel.stringValue += "\n" + status.display
        }
        autoSwitch.state = settings.auto_switch == true ? .on : .off
        autoStartRunner.state = settings.auto_start_runner == true ? .on : .off
        reworkLimit.selectItem(at: max(0, min(2, settings.auto_rework_max ?? 1)))
        accountMode.selectItem(at: settings.account == "separate" ? 1 : 0)
        if let info = loginInfo {
            switch info.login.state {
            case "logged_in": loginLabel.stringValue = "Codex 执行账号：已登录" + (info.login.mode.isEmpty ? "" : "（\(info.login.mode)）")
            case "not_logged_in": loginLabel.stringValue = "Codex 执行账号：未登录"
            default: loginLabel.stringValue = "Codex 执行账号：无法读取"
            }
            loginPath.stringValue = "目录：" + info.dir
        }
        if let p = selectedFilterProject {
            let rev = (p.reviewer ?? "").isEmpty ? "无" : p.reviewer!
            projectCaption.stringValue = "仅显示项目「\(p.name)」· 执行者：\(p.executor) · 审查者：\(rev) · 目录：\(p.folder)"
        } else if isUnassignedSelected {
            projectCaption.stringValue = "仅显示尚未归入项目的独立任务；选中任务后可点击「归入项目…」收纳进任意工作群。"
        } else if projects.isEmpty {
            projectCaption.stringValue = "当前尚未建立项目（工作群）。点击右上角「新建项目…」创建工作群，任务将自动按项目收纳管理。"
        } else {
            projectCaption.stringValue = "按项目（工作群）层级收纳展示全部任务；点击项目行可查看配置或直接在该项目下派发新任务。"
        }

        renderingOutline = true
        table.reloadData()
        for item in groupItems {
            if collapsedGroupKeys.contains(item.group.key) && groupItems.count > 1 {
                table.collapseItem(item)
            } else {
                table.expandItem(item)
            }
        }
        if let tid = selectedTaskID, let tItem = taskItemsByID[tid] {
            if let parent = tItem.parent, !table.isItemExpanded(parent) {
                collapsedGroupKeys.remove(parent.group.key)
                table.expandItem(parent)
            }
            let row = table.row(forItem: tItem)
            if row >= 0 { table.selectRowIndexes(IndexSet(integer: row), byExtendingSelection: false) }
        } else if let gKey = selectedGroupKey, let gItem = groupItemsByKey[gKey] {
            let row = table.row(forItem: gItem)
            if row >= 0 { table.selectRowIndexes(IndexSet(integer: row), byExtendingSelection: false) }
        } else {
            table.deselectAll(nil)
        }
        renderingOutline = false

        if let origin = previousTableOrigin, let scroll = table.enclosingScrollView {
            let target = scroll.contentView.constrainBoundsRect(NSRect(origin: origin, size: scroll.contentView.bounds.size)).origin
            scroll.contentView.setBoundsOrigin(target)
            scroll.reflectScrolledClipView(scroll.contentView)
        }
        updateButtons()
        layoutContent()
        let outerClip = outerScroll.contentView
        let outerTarget = outerClip.constrainBoundsRect(NSRect(origin: previousOuterOrigin, size: outerClip.bounds.size)).origin
        if abs(outerClip.bounds.origin.x - outerTarget.x) > 0.5 || abs(outerClip.bounds.origin.y - outerTarget.y) > 0.5 {
            outerClip.setBoundsOrigin(outerTarget)
            outerScroll.reflectScrolledClipView(outerClip)
        }
        if let origin = previousDetailOrigin, let scroll = details.enclosingScrollView {
            let target = scroll.contentView.constrainBoundsRect(NSRect(origin: origin, size: scroll.contentView.bounds.size)).origin
            scroll.contentView.setBoundsOrigin(target)
            scroll.reflectScrolledClipView(scroll.contentView)
        }
    }

    private func updateButtons() {
        start.isEnabled = !runnerActive && !actionBusy && runner?.isRunning != true
        stop.isEnabled = runner?.isRunning == true && !actionBusy
        projectFilter.isEnabled = !actionBusy
        expandAllButton.isEnabled = !actionBusy && !groupItems.isEmpty
        collapseAllButton.isEnabled = !actionBusy && groupItems.count > 1
        newProject.isEnabled = !actionBusy && agents.contains(where: { $0.available })
        archiveProject.isEnabled = !actionBusy && activeProject != nil
        newTask.isEnabled = !actionBusy && (activeProject != nil || agents.contains(where: { $0.available }))
        assignTask.isEnabled = !actionBusy && selected != nil && !projects.isEmpty
        cancelTask.isEnabled = !actionBusy && selected?.canCancel == true
        acceptTask.isEnabled = !actionBusy && selected?.canAccept == true
        reworkTask.isEnabled = !actionBusy && selected?.canRework == true
        resumeTask.isEnabled = !actionBusy && selected?.canResume == true
        confirmStopped.isEnabled = !actionBusy && selected?.canConfirmStopped == true
        accountMode.isEnabled = !actionBusy && !loginBusy
        autoSwitch.isEnabled = !actionBusy && settings.account == "desktop"
        autoStartRunner.isEnabled = !actionBusy
        autoSwitchAck.isEnabled = !actionBusy && runnerStatus?.auto_switch?.stage == "uncertain" && runnerStatus?.auto_switch?.review_required == true
        reworkLimit.isEnabled = !actionBusy
        let separate = settings.account == "separate"
        loginLabel.isHidden = !separate; loginPath.isHidden = !separate; loginButton.isHidden = !separate; logoutButton.isHidden = !separate; loginMessage.isHidden = !separate
        let loginActions = OrchestraLoginActions(state: loginInfo?.login.state ?? "unknown", busy: loginBusy)
        loginButton.isEnabled = loginInfo != nil && loginActions.canLogin
        logoutButton.isEnabled = loginInfo != nil && loginActions.canLogout
        loginCancel.isEnabled = loginProcess?.isRunning == true && !loginCancelling
        loginCancel.isHidden = !loginBusy
        if let task = selected {
            let projLabel: String
            if let pid = task.project, !pid.isEmpty {
                if let p = allProjects.first(where: { $0.id == pid }) {
                    projLabel = "\(p.name)（\(p.id)）"
                } else {
                    projLabel = pid
                }
            } else {
                projLabel = "未归入项目"
            }
            var text = "所属项目：\(projLabel) · 执行代理：\(task.executor) · 目录：\(task.folder)\n"
            text += "任务卡：\n\(task.card)\n\n最近回执或错误：\n\(task.lastResult)"
            if !(task.review_verdict ?? "").isEmpty || !(task.review_text ?? "").isEmpty {
                text += "\n\n审查结论：\(task.reviewLabel)\n\(task.review_text ?? "")"
            }
            if task.status == "reviewing" {
                text += "\n\n审查中：\(task.review_task ?? "")"
            }
            if let evidence = task.evidence {
                text += "\n\n" + evidence.summary
                if showEvidence.state == .on {
                    for check in evidence.checks ?? [] {
                        text += "\n\n\(check.status)：\(check.command)（退出 \(check.exit)）\n\(check.tail ?? "")"
                    }
                    if let changes = evidence.changes {
                        text += "\n\n改动：\((changes.changed ?? []).joined(separator: ", "))\n超出范围：\((changes.out_of_scope ?? []).joined(separator: ", "))"
                    }
                }
            }
            if details.string != text {
                details.string = text
                if showEvidence.state == .on, task.evidence?.changes?.out_of_scope?.isEmpty == false,
                   let range = text.range(of: "超出范围：", options: .backwards) {
                    details.textStorage?.addAttribute(.foregroundColor, value: NSColor.systemRed, range: NSRange(range.lowerBound..<text.endIndex, in: text))
                }
            }
        } else if let group = selectedGroup {
            if let p = group.project {
                let leadText = (p.lead ?? "").isEmpty ? "不限（任意主控）" : p.lead!
                let revText = (p.reviewer ?? "").isEmpty ? "无自动审查" : p.reviewer!
                let checksText = (p.checks ?? []).isEmpty ? "（未配置检查命令）" : (p.checks ?? []).map { "  • \($0)" }.joined(separator: "\n")
                let scopeText = (p.scope ?? []).isEmpty ? "（未限制改动范围）" : (p.scope ?? []).map { "  • \($0)" }.joined(separator: "\n")
                let text = """
                项目（工作群）：\(p.name)（编号：\(p.id)）\(p.archived == true ? " · 已归档" : "")
                工作目录：\(p.folder)
                主控代理：\(leadText) · 执行代理：\(p.executor) · 审查代理：\(revText)
                收纳任务：共 \(group.tasks.count) 个任务（点击下方「新任务…」可直接向该项目派活）

                检查命令：
                \(checksText)

                允许改动范围：
                \(scopeText)
                """
                if details.string != text { details.string = text }
            } else {
                let text = "未归入项目的独立任务分组（共 \(group.tasks.count) 个任务）。\n\n提示：选中下方任意任务后点击「归入项目…」，即可将其收纳进指定项目（工作群）统一管理。"
                if details.string != text { details.string = text }
            }
        } else {
            let fallback = "选择项目查看工作群配置，或选择任务查看任务卡与执行回执。"
            if details.string != fallback { details.string = fallback }
        }
    }

    func outlineView(_ outlineView: NSOutlineView, numberOfChildrenOfItem item: Any?) -> Int {
        if item == nil { return groupItems.count }
        if let g = item as? OrchestraGroupRowItem { return g.children.count }
        return 0
    }
    func outlineView(_ outlineView: NSOutlineView, child index: Int, ofItem item: Any?) -> Any {
        if item == nil { return groupItems[index] }
        return (item as! OrchestraGroupRowItem).children[index]
    }
    func outlineView(_ outlineView: NSOutlineView, isItemExpandable item: Any) -> Bool {
        item is OrchestraGroupRowItem
    }
    func outlineView(_ outlineView: NSOutlineView, rowViewForItem item: Any) -> NSTableRowView? {
        if item is OrchestraGroupRowItem { return OrchestraProjectRowView() }
        return nil
    }
    func outlineViewSelectionDidChange(_ notification: Notification) {
        guard !renderingOutline else { return }
        updateButtons()
    }
    func outlineViewItemDidExpand(_ notification: Notification) {
        guard !renderingOutline, let g = notification.userInfo?["NSObject"] as? OrchestraGroupRowItem else { return }
        collapsedGroupKeys.remove(g.group.key)
    }
    func outlineViewItemDidCollapse(_ notification: Notification) {
        guard !renderingOutline, let g = notification.userInfo?["NSObject"] as? OrchestraGroupRowItem else { return }
        collapsedGroupKeys.insert(g.group.key)
    }
    func outlineView(_ outlineView: NSOutlineView, viewFor tableColumn: NSTableColumn?, item: Any) -> NSView? {
        guard let column = tableColumn else { return nil }
        let key = column.identifier.rawValue
        let cell = NSTableCellView()

        if let groupItem = item as? OrchestraGroupRowItem {
            let grp = groupItem.group
            if key == "id" {
                let symbol = grp.isUnassigned ? "tray.full.fill" : (grp.isArchived ? "archivebox.fill" : "folder.fill")
                let icon = NSImageView(image: NSImage(systemSymbolName: symbol, accessibilityDescription: nil)!)
                icon.contentTintColor = grp.isUnassigned ? .secondaryLabelColor : (grp.isArchived ? .tertiaryLabelColor : .systemBlue)
                icon.setContentHuggingPriority(.required, for: .horizontal)
                let titleField = Theme.text(grp.title, size: 12.5, weight: .semibold, wraps: false)
                titleField.setContentCompressionResistancePriority(.defaultLow, for: .horizontal)
                var rowViews: [NSView] = [icon, titleField]
                if let pid = grp.projectID, pid != grp.title {
                    let idChip = StatusChip(pid, tint: .secondaryLabelColor)
                    rowViews.append(idChip)
                }
                let row = Theme.hstack(rowViews, spacing: 6)
                cell.addSubview(row); row.translatesAutoresizingMaskIntoConstraints = false
                NSLayoutConstraint.activate([
                    row.leadingAnchor.constraint(equalTo: cell.leadingAnchor, constant: 2),
                    row.trailingAnchor.constraint(lessThanOrEqualTo: cell.trailingAnchor, constant: -4),
                    row.centerYAnchor.constraint(equalTo: cell.centerYAnchor)
                ])
                cell.toolTip = grp.projectID.map { "\(grp.title)（\($0)）" } ?? grp.title
                return cell
            }
            if key == "status" {
                let activeCount = grp.tasks.filter { $0.status == "running" || $0.status == "reviewing" || $0.status == "queued" }.count
                let receiptCount = grp.tasks.filter { $0.status == "receipt" }.count
                let label: String
                let tint: NSColor
                if activeCount > 0 {
                    label = "\(activeCount) 进行中"
                    tint = .systemBlue
                } else if receiptCount > 0 {
                    label = "\(receiptCount) 待验收"
                    tint = .systemTeal
                } else {
                    label = "\(grp.tasks.count) 个任务"
                    tint = .secondaryLabelColor
                }
                let chip = StatusChip(label, tint: tint)
                cell.addSubview(chip); chip.translatesAutoresizingMaskIntoConstraints = false
                NSLayoutConstraint.activate([
                    chip.leadingAnchor.constraint(equalTo: cell.leadingAnchor, constant: 2),
                    chip.centerYAnchor.constraint(equalTo: cell.centerYAnchor)
                ])
                return cell
            }
            let text: String
            switch key {
            case "executor":
                if let p = grp.project {
                    let lead = (p.lead ?? "").isEmpty ? "" : "\(p.lead!) → "
                    text = lead + p.executor
                } else {
                    text = "—"
                }
            case "review":
                if let p = grp.project {
                    text = (p.reviewer ?? "").isEmpty ? "无审查" : "审查 \(p.reviewer!)"
                } else {
                    text = "—"
                }
            case "summary":
                if let p = grp.project {
                    let checkCount = p.checks?.count ?? 0
                    let scopeCount = p.scope?.count ?? 0
                    text = "项目容器 · 检查命令 \(checkCount) 条 · 范围规则 \(scopeCount) 条"
                } else {
                    text = grp.subtitle
                }
            case "folder":
                text = grp.folder.isEmpty ? "—" : URL(fileURLWithPath: grp.folder).lastPathComponent
            default:
                text = grp.tasks.first.map { $0.updated.replacingOccurrences(of: "T", with: " ").prefix(19).description } ?? "—"
            }
            let field = NSTextField(labelWithString: text)
            field.lineBreakMode = .byTruncatingTail
            field.toolTip = key == "folder" && !grp.folder.isEmpty ? grp.folder : text
            field.font = .systemFont(ofSize: 11.5, weight: key == "executor" ? .medium : .regular)
            field.textColor = text == "—" ? .tertiaryLabelColor : .secondaryLabelColor
            cell.addSubview(field); field.translatesAutoresizingMaskIntoConstraints = false
            NSLayoutConstraint.activate([
                field.leadingAnchor.constraint(equalTo: cell.leadingAnchor, constant: 2),
                field.trailingAnchor.constraint(equalTo: cell.trailingAnchor, constant: -4),
                field.centerYAnchor.constraint(equalTo: cell.centerYAnchor)
            ])
            cell.textField = field
            return cell
        }

        guard let taskItem = item as? OrchestraTaskRowItem else { return nil }
        let task = taskItem.task
        if key == "status" {
            let colorKey = task.unconfirmed == true ? "unconfirmed" : (task.evidence?.phase == "checking" ? "checking" : task.status)
            let chip = StatusChip(task.statusLabel, tint: Theme.statusColor(for: colorKey))
            cell.addSubview(chip); chip.translatesAutoresizingMaskIntoConstraints = false
            NSLayoutConstraint.activate([
                chip.leadingAnchor.constraint(equalTo: cell.leadingAnchor, constant: 2),
                chip.centerYAnchor.constraint(equalTo: cell.centerYAnchor)
            ])
            return cell
        }
        if key == "review" && task.reviewLabel != "—" {
            let colorKey = task.review_verdict ?? (task.status == "reviewing" ? "reviewing" : "queued")
            let chip = StatusChip(task.reviewLabel, tint: Theme.statusColor(for: colorKey))
            cell.addSubview(chip); chip.translatesAutoresizingMaskIntoConstraints = false
            NSLayoutConstraint.activate([
                chip.leadingAnchor.constraint(equalTo: cell.leadingAnchor, constant: 2),
                chip.centerYAnchor.constraint(equalTo: cell.centerYAnchor)
            ])
            return cell
        }
        let isReviewChild = !(task.review_of ?? "").isEmpty
        let text: String
        switch key {
        case "id": text = isReviewChild ? "↳ \(task.id)" : task.id
        case "executor": text = (task.lead ?? "").isEmpty ? task.executor : "\(task.lead!) → \(task.executor)"
        case "status": text = task.statusLabel
        case "review": text = task.reviewLabel
        case "summary":
            if let ev = task.evidence?.summary, !ev.isEmpty {
                text = "\(task.cardHeadline) · \(ev)"
            } else {
                text = task.cardHeadline
            }
        case "folder": text = URL(fileURLWithPath: task.folder).lastPathComponent
        default: text = task.updated.replacingOccurrences(of: "T", with: " ").prefix(19).description
        }
        let field = NSTextField(labelWithString: text)
        field.lineBreakMode = .byTruncatingTail
        field.toolTip = key == "folder" ? task.folder : (key == "summary" ? task.card : text)
        if key == "id" || key == "updated" {
            field.font = .monospacedSystemFont(ofSize: 11.5, weight: key == "id" ? .medium : .regular)
            if key == "updated" || isReviewChild { field.textColor = .secondaryLabelColor }
        } else {
            field.font = .systemFont(ofSize: 12, weight: .regular)
            if text == "—" { field.textColor = .tertiaryLabelColor }
        }
        cell.addSubview(field); field.translatesAutoresizingMaskIntoConstraints = false
        NSLayoutConstraint.activate([
            field.leadingAnchor.constraint(equalTo: cell.leadingAnchor, constant: 2),
            field.trailingAnchor.constraint(equalTo: cell.trailingAnchor, constant: -4),
            field.centerYAnchor.constraint(equalTo: cell.centerYAnchor)
        ])
        cell.textField = field
        return cell
    }

    @objc private func startRunner() {
        guard !actionBusy, !runnerActive, runner?.isRunning != true, let window = window else { return }
        let begin = { [weak self] in self?.startAfterCheck() }
        if confirmedRunner && (settings.account == "desktop" || loginInfo?.login.state == "logged_in") { begin(); return }
        let alert = NSAlert(); alert.messageText = "启动无人值守执行器？"
        alert.informativeText = "执行器会立即处理队列中已有任务。代理将在无人值守时运行，并消耗所选代理的使用额度。Codex 使用工作区写入限制，请先核对任务和工作目录。" + (settings.account == "separate" && loginInfo?.login.state == "not_logged_in" ? "\n\n独立账号未登录，Codex 任务会失败。" : "")
        alert.addButton(withTitle: "启动"); alert.addButton(withTitle: "取消")
        alert.beginSheetModal(for: window) { [weak self] response in if response == .alertFirstButtonReturn { self?.confirmedRunner = true; begin() } }
    }
    private func startAfterCheck() {
        actionBusy = true; updateButtons()
        queue.async { [weak self] in
            guard let self = self else { return }
            do {
                let status = try JSONDecoder().decode(OrchestraRunnerStatus.self, from: self.command(["runner"]))
                DispatchQueue.main.async {
                    self.actionBusy = false
                    guard !status.active else { self.runnerActive = true; self.notice.stringValue = "已有执行器运行，未启动第二个。"; self.updateButtons(); return }
                    self.runnerActive = false // fresher than any earlier refresh
                    self.launchRunnerIfUnowned()
                }
            } catch { DispatchQueue.main.async { self.actionBusy = false; self.notice.stringValue = "无法确认执行器状态：\(error.localizedDescription)"; self.updateButtons() } }
        }
    }
    func startRunnerIfConfigured() {
        queue.async { [weak self] in
            guard let self = self else { return }
            do {
                let decoder = JSONDecoder()
                let settings = try decoder.decode(OrchestraSettings.self, from: self.command(["settings"]))
                guard settings.auto_start_runner == true else { return }
                if settings.account == "separate" {
                    let login = try decoder.decode(OrchestraLoginInfo.self, from: self.command(["login-status"]))
                    guard login.login.state == "logged_in" else {
                        DispatchQueue.main.async {
                            self.settings = settings
                            self.loginInfo = login
                            self.notice.stringValue = "独立账号未登录，未随小助手自动启动执行器。"
                            self.updateButtons()
                        }
                        return
                    }
                }
                let status = try decoder.decode(OrchestraRunnerStatus.self, from: self.command(["runner"]))
                guard !status.active else { return }
                DispatchQueue.main.async {
                    self.settings = settings
                    self.confirmedRunner = true
                    self.runnerActive = false // fresher than any earlier refresh
                    self.launchRunnerIfUnowned()
                }
            } catch {
                DispatchQueue.main.async {
                    guard !self.terminating else { return }
                    self.notice.stringValue = "自动启动执行器失败：\(error.localizedDescription)"
                    self.updateButtons()
                }
            }
        }
    }
    // Both start paths meet on the main queue, so a stale status response can
    // never overwrite the process handle of an already started runner.
    private func launchRunnerIfUnowned() {
        guard !terminating, !runnerActive, runner?.isRunning != true else { updateButtons(); return }
        let process = Process(); process.executableURL = executable; process.arguments = ["orchestra", "run"]
        process.standardOutput = FileHandle.nullDevice; process.standardError = FileHandle.nullDevice
        do { try process.run(); runner = process; notice.stringValue = "已请求启动执行器。" } catch { notice.stringValue = "启动失败：\(error.localizedDescription)" }
        updateButtons(); refresh()
    }
    @objc private func stopRunnerAction() { stopOwnedRunner(wait: false); notice.stringValue = "正在停止本小助手启动的执行器。"; refresh() }
    private func stopOwnedRunner(wait: Bool) {
        guard let process = runner, process.isRunning else { return }
        process.terminate()
        if wait {
            let deadline = Date().addingTimeInterval(10)
            while process.isRunning && Date() < deadline { usleep(100_000) }
            if process.isRunning { kill(process.processIdentifier, SIGKILL) }
        } else {
            DispatchQueue.global(qos: .utility).asyncAfter(deadline: .now() + 10) { if process.isRunning { kill(process.processIdentifier, SIGKILL) } }
        }
    }
    func stopRunnerForTermination() { terminating = true; timer?.invalidate(); cancelLogin(); stopOwnedRunner(wait: true) }

    @objc private func beginLogin() {
        guard !loginBusy, let info = loginInfo, !info.login.logged_in else { return }
        loginBusy = true; loginMessage.stringValue = "正在准备独立执行账号…"; updateButtons()
        queue.async { [weak self] in
            guard let self = self else { return }
            do {
                let setup = try JSONDecoder().decode(OrchestraLoginInfo.self, from: self.command(["setup"]))
                DispatchQueue.main.async {
                    guard self.loginBusy, self.window?.isVisible == true else { self.loginBusy = false; self.updateButtons(); return }
                    guard !setup.codex_bin.isEmpty else { self.loginBusy = false; self.loginMessage.stringValue = "找不到 Codex 命令行。"; self.updateButtons(); return }
                    let process = Process(); process.executableURL = URL(fileURLWithPath: setup.codex_bin); process.arguments = ["login"]
                    var environment = ProcessInfo.processInfo.environment; environment["CODEX_HOME"] = setup.dir; process.environment = environment
                    process.standardInput = FileHandle.nullDevice; process.standardOutput = FileHandle.nullDevice; process.standardError = FileHandle.nullDevice
                    process.terminationHandler = { [weak self] process in DispatchQueue.main.async {
                        guard let self = self, self.loginProcess === process else { return }
                        self.loginProcess = nil; self.loginBusy = false; self.loginCancelling = false
                        self.loginMessage.stringValue = process.terminationStatus == 0 ? "登录流程已结束，正在核对状态。" : "登录未完成或已取消，请重试。"
                        self.updateButtons(); self.refresh()
                    } }
                    do { try process.run(); self.loginProcess = process; self.loginMessage.stringValue = "请在浏览器中完成登录…" } catch { self.loginBusy = false; self.loginMessage.stringValue = "登录程序无法启动。" }
                    self.updateButtons()
                }
            } catch { DispatchQueue.main.async { self.loginBusy = false; self.loginMessage.stringValue = "无法准备执行账号：\(error.localizedDescription)"; self.updateButtons() } }
        }
    }
    @objc private func cancelLogin() {
        if let process = loginProcess, process.isRunning {
            guard !loginCancelling else { return }
            loginCancelling = true
            process.terminate()
            DispatchQueue.global(qos: .utility).asyncAfter(deadline: .now() + 2) { if process.isRunning { kill(process.processIdentifier, SIGKILL) } }
            loginMessage.stringValue = "正在取消登录。"
        } else { loginBusy = false; loginCancelling = false }
        updateButtons()
    }
    @objc private func confirmLogout() {
        guard !loginBusy, let info = loginInfo, info.login.logged_in, let window = window else { return }
        let alert = NSAlert(); alert.messageText = "退出 Codex 执行账号？"; alert.informativeText = "这会退出独立执行账号；之后 Codex 任务需要重新登录。"
        alert.addButton(withTitle: "退出登录"); alert.addButton(withTitle: "取消")
        alert.beginSheetModal(for: window) { [weak self] response in
            guard response == .alertFirstButtonReturn, let self = self, !self.loginBusy else { return }
            self.loginBusy = true; self.updateButtons()
            self.queue.async {
                let process = Process(); process.executableURL = URL(fileURLWithPath: info.codex_bin); process.arguments = ["logout"]
                var env = ProcessInfo.processInfo.environment; env["CODEX_HOME"] = info.dir; process.environment = env
                process.standardInput = FileHandle.nullDevice; process.standardOutput = FileHandle.nullDevice; process.standardError = FileHandle.nullDevice
                var success = false; do { try process.run(); process.waitUntilExit(); success = process.terminationStatus == 0 } catch {}
                DispatchQueue.main.async { self.loginBusy = false; self.loginMessage.stringValue = success ? "退出登录完成，正在核对状态。" : "退出登录未完成，请重试。"; self.updateButtons(); self.refresh() }
            }
        }
    }

    private func mutate(_ args: [String], input: String? = nil, successMessage: String = "操作已提交。", onSuccess: (() -> Void)? = nil, onFailure: ((String) -> Void)? = nil) {
        guard !actionBusy else { return }; actionBusy = true; updateButtons()
        queue.async { [weak self] in
            guard let self = self else { return }
            do {
                _ = try self.command(args, input: input)
                DispatchQueue.main.async {
                    self.actionBusy = false
                    self.notice.stringValue = successMessage
                    onSuccess?()
                    self.updateButtons()
                    self.refresh()
                }
            } catch {
                DispatchQueue.main.async {
                    let msg = error.localizedDescription
                    self.actionBusy = false
                    self.notice.stringValue = "操作失败：\(msg)"
                    self.updateButtons()
                    onFailure?(msg)
                }
            }
        }
    }
    @objc private func changeAccountMode() { mutate(["settings", "--account", accountMode.indexOfSelectedItem == 0 ? "desktop" : "separate"]) }
    @objc private func changeReworkLimit() { mutate(["settings", "--auto-rework-max", String(reworkLimit.indexOfSelectedItem)]) }
    @objc private func toggleEvidence() { updateButtons() }
    @objc private func ackAutoSwitch() {
        guard let window = window else { return }; let alert=NSAlert(); alert.messageText="解除自动切号暂停？"; alert.informativeText="请先核查账号及所有交接恢复记录。确认不会重试上次切号，仍会保留冷却时间。"; alert.addButton(withTitle:"已检查，解除暂停"); alert.addButton(withTitle:"取消")
        alert.beginSheetModal(for:window) { [weak self] response in if response == .alertFirstButtonReturn { self?.mutate(["auto-switch-ack"]) } }
    }
    @objc private func changeAutoSwitch() {
        guard let window = window else { return }
        if autoSwitch.state == .off { mutate(["settings", "--auto-switch=false"]); return }
        autoSwitch.state = .off
        let alert=NSAlert(); alert.messageText="启用自动切换账号？"; alert.informativeText="只有 Codex Desktop 已关闭时才会切换 ~/.codex 的账号。之后 Desktop 和执行器都会使用新账号，并消耗其他账号的额度。"; alert.addButton(withTitle:"启用"); alert.addButton(withTitle:"取消")
        alert.beginSheetModal(for:window) { [weak self] response in if response == .alertFirstButtonReturn { self?.mutate(["settings", "--auto-switch=true"]) } }
    }
    @objc private func changeAutoStartRunner() {
        guard let window = window else { return }
        if autoStartRunner.state == .off { mutate(["settings", "--auto-start-runner=false"]); return }
        autoStartRunner.state = .off
        let alert = NSAlert(); alert.messageText = "启动无人值守执行器？"
        alert.informativeText = "执行器会随小助手自动启动并立即处理队列中已有任务。代理将在无人值守时运行，并消耗所选代理的使用额度。Codex 使用工作区写入限制，请先核对任务和工作目录。" + (settings.account == "separate" && loginInfo?.login.state == "not_logged_in" ? "\n\n独立账号未登录，Codex 任务会失败。" : "")
        alert.addButton(withTitle: "启用"); alert.addButton(withTitle: "取消")
        alert.beginSheetModal(for: window) { [weak self] response in
            if response == .alertFirstButtonReturn {
                self?.confirmedRunner = true
                self?.mutate(["settings", "--auto-start-runner=true"])
            }
        }
    }
    @objc private func resumeSelected() { if let task = selected, task.canResume { mutate(["resume", task.id]) } }
    @objc private func confirmStoppedSelected() {
        guard let task = selected, task.canConfirmStopped, let window = window else { return }
        let alert = NSAlert(); alert.messageText = "确认原执行进程已停止？"; alert.informativeText = "请先检查原进程及其子进程均已停止。"; alert.addButton(withTitle: "确认已停止"); alert.addButton(withTitle: "取消")
        alert.beginSheetModal(for: window) { [weak self] response in if response == .alertFirstButtonReturn { self?.mutate(["confirm-stopped", task.id]) } }
    }
    @objc private func cancelSelected() { if let task = selected, task.canCancel { mutate(["cancel", task.id]) } }
    @objc private func acceptSelected() { if let task = selected, task.canAccept { mutate(["accept", task.id]) } }
    @objc private func reworkSelected() {
        guard let task = selected, task.canRework, let window = window else { return }
        let alert = NSAlert(); alert.messageText = "返工说明"; alert.informativeText = "仅描述需要执行者修改的内容。"
        let field = NSTextField(frame: NSRect(x: 0, y: 0, width: 400, height: 24)); alert.accessoryView = field
        alert.addButton(withTitle: "提交"); alert.addButton(withTitle: "取消")
        alert.beginSheetModal(for: window) { [weak self] response in if response == .alertFirstButtonReturn { self?.mutate(["rework", task.id], input: field.stringValue) } }
    }
    @objc private func expandAllGroups() {
        collapsedGroupKeys.removeAll()
        renderingOutline = true
        for item in groupItems { table.expandItem(item) }
        renderingOutline = false
    }
    @objc private func collapseAllGroups() {
        guard groupItems.count > 1 else { return }
        renderingOutline = true
        for item in groupItems {
            collapsedGroupKeys.insert(item.group.key)
            table.collapseItem(item)
        }
        renderingOutline = false
    }
    @objc private func changeProjectFilter() {
        let selectedTaskID = selected?.id
        let selectedGroupKey = selectedGroup?.key
        applyProjectFilter()
        render(selectedTaskID: selectedTaskID, selectedGroupKey: selectedGroupKey)
    }
    @objc private func archiveSelectedProject() {
        guard let p = activeProject, let window = window else { return }
        let alert = NSAlert(); alert.messageText = "归档项目“\(p.name)”？"; alert.informativeText = "归档后该项目不再显示在活跃项目列表中，已有任务记录仍会保留。"
        alert.addButton(withTitle: "归档"); alert.addButton(withTitle: "取消")
        alert.beginSheetModal(for: window) { [weak self] response in
            if response == .alertFirstButtonReturn {
                self?.mutate(["project", "archive", p.id], successMessage: "项目「\(p.name)」已归档。")
            }
        }
    }
    @objc private func assignSelectedTaskToProject() {
        guard let task = selected, !projects.isEmpty, let window = window else { return }
        let alert = NSAlert()
        alert.messageText = "归入项目（工作群）"
        alert.informativeText = "将任务「\(task.id)」收纳进指定项目，或移回未归入项目分组。"
        let popup = NSPopUpButton(frame: NSRect(x: 0, y: 0, width: 360, height: 26))
        popup.addItems(withTitles: projects.map { "\($0.name)（\($0.id)）" } + ["移出项目（设为未归入项目）"])
        if let pid = task.project, let idx = projects.firstIndex(where: { $0.id == pid }) {
            popup.selectItem(at: idx)
        } else {
            popup.selectItem(at: 0)
        }
        alert.accessoryView = popup
        alert.addButton(withTitle: "确认归入")
        alert.addButton(withTitle: "取消")
        alert.beginSheetModal(for: window) { [weak self] response in
            guard response == .alertFirstButtonReturn, let self = self else { return }
            let idx = popup.indexOfSelectedItem
            let targetProjectID = self.projects.indices.contains(idx) ? self.projects[idx].id : ""
            let targetName = self.projects.indices.contains(idx) ? "项目「\(self.projects[idx].name)」" : "未归入项目"
            self.pendingSelectTaskID = task.id
            self.mutate(["assign", "--id", task.id, "--project", targetProjectID], successMessage: "已将任务「\(task.id)」归入\(targetName)。")
        }
    }
    @objc private func createProject() {
        let defaultFolder = activeProject?.folder ?? selected?.folder ?? projects.first?.folder ?? allTasks.first?.folder ?? ""
        presentCreateProject(initialFolder: defaultFolder)
    }
    private func presentCreateProject(
        initialName: String = "",
        initialID: String = "",
        initialFolder: String = "",
        initialExecIndex: Int = 0,
        initialLeadIndex: Int = 0,
        initialRevIndex: Int = 0,
        initialChecks: String = "",
        initialScope: String = "",
        errorMessage: String? = nil
    ) {
        guard let window = window else { return }
        let available = agents.filter { $0.available }
        guard !available.isEmpty else { return }
        let alert = NSAlert()
        alert.messageText = "新建项目（工作群）"
        alert.informativeText = (errorMessage.map { "创建未完成：\($0)\n\n" } ?? "") + "项目是收纳任务的协作工作群：统一配置工作目录、主控/执行/审查代理、验收检查命令与改动范围。"
        let form = NSStackView(); form.orientation = .vertical; form.alignment = .leading; form.spacing = 8; form.frame = NSRect(x: 0, y: 0, width: 460, height: 380)
        func labeledRow(_ title: String, _ control: NSView) -> NSStackView {
            let lbl = Theme.text(title, size: 12, weight: .medium, wraps: false)
            lbl.widthAnchor.constraint(equalToConstant: 76).isActive = true
            return Theme.hstack([lbl, control], spacing: 8)
        }
        let name = NSTextField(string: initialName); name.placeholderString = "必填，例如：前端重构组 / 账号管理迭代"
        let id = NSTextField(string: initialID); id.placeholderString = "可选，留空根据项目名称或目录自动生成（如 fe-refactor）"
        let folder = NSTextField(string: initialFolder); folder.placeholderString = "输入路径或点击右侧选择工作目录"; folder.isEditable = true; folder.lineBreakMode = .byTruncatingMiddle
        let choose = NSButton(title: "选择文件夹…", target: nil, action: nil); Theme.button(choose, small: true)
        let target = FolderPickerTarget(field: folder, nameField: name, window: alert.window); choose.target = target; choose.action = #selector(FolderPickerTarget.choose)
        let folderRow = labeledRow("工作目录：", Theme.hstack([folder, choose], spacing: 6))
        let execPopup = NSPopUpButton(frame: NSRect(x: 0, y: 0, width: 376, height: 26)); execPopup.addItems(withTitles: available.map { $0.label })
        if available.indices.contains(initialExecIndex) { execPopup.selectItem(at: initialExecIndex) }
        let leadPopup = NSPopUpButton(frame: NSRect(x: 0, y: 0, width: 376, height: 26)); leadPopup.addItems(withTitles: ["不指定（任意主控）"] + agents.map { $0.label })
        if initialLeadIndex >= 0 && initialLeadIndex < leadPopup.numberOfItems { leadPopup.selectItem(at: initialLeadIndex) }
        let revPopup = NSPopUpButton(frame: NSRect(x: 0, y: 0, width: 376, height: 26)); revPopup.addItems(withTitles: ["无自动审查"] + agents.map { $0.label })
        if initialRevIndex >= 0 && initialRevIndex < revPopup.numberOfItems { revPopup.selectItem(at: initialRevIndex) }
        let execRow = labeledRow("执行代理：", execPopup)
        let leadRow = labeledRow("主控代理：", leadPopup)
        let revRow = labeledRow("审查代理：", revPopup)
        let checks = NSTextView(); checks.font = .monospacedSystemFont(ofSize: 11.5, weight: .regular); checks.string = initialChecks
        let checksScroll = NSScrollView(); checksScroll.documentView = checks; checksScroll.borderType = .bezelBorder; checksScroll.hasVerticalScroller = true; checksScroll.heightAnchor.constraint(equalToConstant: 52).isActive = true
        checks.isVerticallyResizable = true; checks.autoresizingMask = [.width]; checks.textContainer?.widthTracksTextView = true
        let scope = NSTextView(); scope.font = .monospacedSystemFont(ofSize: 11.5, weight: .regular); scope.string = initialScope
        let scopeScroll = NSScrollView(); scopeScroll.documentView = scope; scopeScroll.borderType = .bezelBorder; scopeScroll.hasVerticalScroller = true; scopeScroll.heightAnchor.constraint(equalToConstant: 52).isActive = true
        scope.isVerticallyResizable = true; scope.autoresizingMask = [.width]; scope.textContainer?.widthTracksTextView = true
        let items: [NSView] = [labeledRow("项目名称：", name), labeledRow("项目编号：", id), folderRow, execRow, revRow, leadRow, Theme.caption("检查命令（可选，每行一条，如 go test ./...）："), checksScroll, Theme.caption("允许改动范围（可选，每行一个相对路径前缀）："), scopeScroll]
        for item in items { form.addArrangedSubview(item); item.widthAnchor.constraint(equalToConstant: 460).isActive = true }
        alert.accessoryView = form; alert.addButton(withTitle: "创建项目"); alert.addButton(withTitle: "取消")
        alert.beginSheetModal(for: window) { [weak self] response in
            _ = target
            guard response == .alertFirstButtonReturn, let self = self, available.indices.contains(execPopup.indexOfSelectedItem) else { return }
            let draftName = name.stringValue.trimmingCharacters(in: .whitespacesAndNewlines)
            let rawID = id.stringValue.trimmingCharacters(in: .whitespacesAndNewlines)
            let draftFolder = (folder.stringValue.trimmingCharacters(in: .whitespacesAndNewlines) as NSString).expandingTildeInPath
            let execIdx = execPopup.indexOfSelectedItem
            let leadIdx = leadPopup.indexOfSelectedItem
            let revIdx = revPopup.indexOfSelectedItem
            let draftChecks = checks.string
            let draftScope = scope.string
            let reopen: (String) -> Void = { [weak self] msg in
                self?.presentCreateProject(
                    initialName: draftName,
                    initialID: rawID,
                    initialFolder: draftFolder,
                    initialExecIndex: execIdx,
                    initialLeadIndex: leadIdx,
                    initialRevIndex: revIdx,
                    initialChecks: draftChecks,
                    initialScope: draftScope,
                    errorMessage: msg
                )
            }
            if draftName.isEmpty { reopen("请填写项目名称。"); return }
            if draftFolder.isEmpty { reopen("请选择或填写工作目录。"); return }
            let existingProjectIDs = Set(self.allProjects.map(\.id))
            let resolvedID = resolveOrchestraProjectID(rawID, name: draftName, folder: draftFolder, existingIDs: existingProjectIDs)
            var args = ["project", "create", "--id", resolvedID, "--name", draftName, "--folder", draftFolder, "--to", available[execIdx].name]
            if self.agents.indices.contains(leadIdx - 1) { args += ["--lead", self.agents[leadIdx - 1].name] }
            if self.agents.indices.contains(revIdx - 1) { args += ["--reviewer", self.agents[revIdx - 1].name] }
            for line in draftChecks.components(separatedBy: .newlines).map({ $0.trimmingCharacters(in: .whitespacesAndNewlines) }) where !line.isEmpty { args += ["--check", line] }
            for line in draftScope.components(separatedBy: .newlines).map({ $0.trimmingCharacters(in: .whitespacesAndNewlines) }) where !line.isEmpty { args += ["--scope", line] }
            self.pendingSelectProjectID = resolvedID
            self.mutate(args, successMessage: "已创建项目「\(draftName)」（\(resolvedID)）。", onFailure: { msg in
                self.pendingSelectProjectID = nil
                reopen(msg)
            })
        }
    }
    @objc private func createTask() {
        let defaultProjectID = activeProject?.id
        let defaultFolder = activeProject?.folder ?? selected?.folder ?? projects.first?.folder ?? ""
        presentCreateTask(initialProjectID: defaultProjectID, initialFolder: defaultFolder)
    }
    private func presentCreateTask(initialProjectID: String? = nil, initialID: String = "", initialFolder: String = "", initialCard: String = "", initialAgentIndex: Int = 0, errorMessage: String? = nil) {
        guard let window = window else { return }
        let available = agents.filter { $0.available }
        guard !projects.isEmpty || !available.isEmpty else { return }
        let alert = NSAlert(); alert.messageText = "派发新任务"
        alert.informativeText = (errorMessage.map { "提交未完成：\($0)\n\n" } ?? "") + "建议选择所属项目（工作群），任务将直接收纳在项目内并继承其目录、执行者与审查配置。"
        let form = NSStackView(); form.orientation = .vertical; form.alignment = .leading; form.spacing = 8; form.frame = NSRect(x: 0, y: 0, width: 460, height: 290)
        form.detachesHiddenViews = true
        func labeledRow(_ title: String, _ control: NSView) -> NSStackView {
            let lbl = Theme.text(title, size: 12, weight: .medium, wraps: false)
            lbl.widthAnchor.constraint(equalToConstant: 76).isActive = true
            return Theme.hstack([lbl, control], spacing: 8)
        }
        let projectPopup = NSPopUpButton(frame: NSRect(x: 0, y: 0, width: 376, height: 26))
        projectPopup.addItems(withTitles: projects.map { "\($0.name)（\($0.id)）" } + ["未归入项目（独立派发）"])
        if let pid = initialProjectID, let idx = projects.firstIndex(where: { $0.id == pid }) {
            projectPopup.selectItem(at: idx)
        } else if !projects.isEmpty && !isUnassignedSelected {
            projectPopup.selectItem(at: 0)
        } else {
            projectPopup.selectItem(at: projects.count)
        }
        let projectInfo = Theme.caption("")
        projectInfo.maximumNumberOfLines = 2
        let popup = NSPopUpButton(frame: NSRect(x: 0, y: 0, width: 376, height: 26)); popup.addItems(withTitles: available.map { $0.label })
        if available.indices.contains(initialAgentIndex) { popup.selectItem(at: initialAgentIndex) }
        let folder = NSTextField(string: initialFolder); folder.placeholderString = "输入路径或点击右侧选择工作目录"; folder.isEditable = true; folder.lineBreakMode = .byTruncatingMiddle
        let choose = NSButton(title: "选择文件夹…", target: nil, action: nil); Theme.button(choose, small: true)
        let target = FolderPickerTarget(field: folder, nameField: nil, window: alert.window); choose.target = target; choose.action = #selector(FolderPickerTarget.choose)
        let execRow = labeledRow("执行代理：", popup)
        let folderRow = labeledRow("工作目录：", Theme.hstack([folder, choose], spacing: 6))
        let projSwitchTarget = ProjectSelectionTarget(projects: projects, popup: projectPopup, infoLabel: projectInfo, execRow: execRow, folderRow: folderRow)
        projectPopup.target = projSwitchTarget; projectPopup.action = #selector(ProjectSelectionTarget.changed)
        projSwitchTarget.changed()

        let id = NSTextField(string: initialID); id.placeholderString = "可选，留空自动按项目前缀生成编号"
        let card = NSTextView(); card.font = .systemFont(ofSize: 12); card.string = initialCard
        let cardScroll = NSScrollView(); cardScroll.documentView = card; cardScroll.borderType = .bezelBorder; cardScroll.hasVerticalScroller = true; cardScroll.heightAnchor.constraint(equalToConstant: 115).isActive = true
        card.isVerticallyResizable = true; card.autoresizingMask = [.width]; card.textContainer?.widthTracksTextView = true
        for item in [labeledRow("所属项目：", projectPopup), projectInfo, execRow, folderRow, labeledRow("任务编号：", id), Theme.caption("任务卡说明："), cardScroll] {
            form.addArrangedSubview(item); item.widthAnchor.constraint(equalToConstant: 460).isActive = true
        }
        alert.accessoryView = form; alert.addButton(withTitle: "加入队列"); alert.addButton(withTitle: "取消")
        alert.beginSheetModal(for: window) { [weak self] response in
            _ = target
            _ = projSwitchTarget
            guard response == .alertFirstButtonReturn, let self = self else { return }
            let projIdx = projectPopup.indexOfSelectedItem
            let chosenProject: OrchestraProject? = self.projects.indices.contains(projIdx) ? self.projects[projIdx] : nil
            let rawID = id.stringValue
            let resolvedID = resolveOrchestraTaskID(rawID, project: chosenProject?.id, existingIDs: Set(self.allTasks.map(\.id)))
            let draftFolder = (folder.stringValue.trimmingCharacters(in: .whitespacesAndNewlines) as NSString).expandingTildeInPath
            let draftCard = card.string
            let index = popup.indexOfSelectedItem
            let reopen: (String) -> Void = { [weak self] msg in
                self?.presentCreateTask(initialProjectID: chosenProject?.id, initialID: rawID, initialFolder: draftFolder, initialCard: draftCard, initialAgentIndex: index, errorMessage: msg)
            }
            if draftCard.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                reopen("请填写任务卡说明。"); return
            }
            self.pendingSelectTaskID = resolvedID
            if let p = chosenProject {
                self.mutate(["dispatch", "--project", p.id, "--id", resolvedID, "--lead", "user"], input: draftCard, successMessage: "已向项目「\(p.name)」派发任务 \(resolvedID)。", onFailure: { msg in
                    self.pendingSelectTaskID = nil
                    reopen(msg)
                })
            } else if available.indices.contains(index) {
                if draftFolder.isEmpty {
                    self.pendingSelectTaskID = nil
                    reopen("未归入项目时，请选择或填写工作目录。"); return
                }
                self.mutate(["dispatch", "--to", available[index].name, "--folder", draftFolder, "--id", resolvedID, "--lead", "user"], input: draftCard, successMessage: "已派发独立任务 \(resolvedID)。", onFailure: { msg in
                    self.pendingSelectTaskID = nil
                    reopen(msg)
                })
            }
        }
    }
}

func resolveOrchestraProjectID(_ raw: String, name: String, folder: String, existingIDs: Set<String> = [], now: Date = Date()) -> String {
    let trimmed = raw.trimmingCharacters(in: .whitespacesAndNewlines)
    if !trimmed.isEmpty { return trimmed }
    func slugify(_ input: String) -> String {
        var out = ""
        var prevSep = false
        for scalar in input.lowercased().unicodeScalars {
            let v = scalar.value
            let isASCIIAlphaNum = (v >= 97 && v <= 122) || (v >= 48 && v <= 57)
            if isASCIIAlphaNum {
                out.append(Character(scalar))
                prevSep = false
            } else if (v == 46 || v == 95) && !out.isEmpty && !prevSep {
                out.append(Character(scalar))
                prevSep = true
            } else if !out.isEmpty && !prevSep {
                out.append("-")
                prevSep = true
            }
        }
        while out.hasSuffix("-") || out.hasSuffix(".") || out.hasSuffix("_") { out.removeLast() }
        return String(out.prefix(32))
    }
    var base = slugify(name)
    if base.isEmpty {
        base = slugify(URL(fileURLWithPath: folder).lastPathComponent)
    }
    if base.isEmpty {
        let formatter = DateFormatter()
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.dateFormat = "MMdd-HHmmss"
        base = "proj-\(formatter.string(from: now))"
    }
    if !existingIDs.contains(base) { return base }
    var suffix = 2
    while existingIDs.contains("\(base)-\(suffix)") { suffix += 1 }
    return "\(base)-\(suffix)"
}

func resolveOrchestraTaskID(_ raw: String, project: String?, existingIDs: Set<String> = [], now: Date = Date()) -> String {
    let trimmed = raw.trimmingCharacters(in: .whitespacesAndNewlines)
    if !trimmed.isEmpty { return trimmed }
    let formatter = DateFormatter()
    formatter.locale = Locale(identifier: "en_US_POSIX")
    formatter.dateFormat = "MMdd-HHmmss"
    let rawPrefix = (project ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
    let prefix = rawPrefix.isEmpty ? "task" : String(rawPrefix.prefix(32))
    let base = "\(prefix)-\(formatter.string(from: now))"
    if !existingIDs.contains(base) { return base }
    var suffix = 2
    while existingIDs.contains("\(base)-\(suffix)") { suffix += 1 }
    return "\(base)-\(suffix)"
}

private final class ProjectSelectionTarget: NSObject {
    let projects: [OrchestraProject]
    let popup: NSPopUpButton
    let infoLabel: NSTextField
    let execRow: NSView
    let folderRow: NSView
    init(projects: [OrchestraProject], popup: NSPopUpButton, infoLabel: NSTextField, execRow: NSView, folderRow: NSView) {
        self.projects = projects
        self.popup = popup
        self.infoLabel = infoLabel
        self.execRow = execRow
        self.folderRow = folderRow
    }
    @objc func changed() {
        let idx = popup.indexOfSelectedItem
        if projects.indices.contains(idx) {
            let p = projects[idx]
            let rev = (p.reviewer ?? "").isEmpty ? "无审查" : "审查 \(p.reviewer!)"
            infoLabel.stringValue = "将归入项目「\(p.name)」· 执行者：\(p.executor) · \(rev) · 目录：\(p.folder)"
            infoLabel.isHidden = false
            execRow.isHidden = true
            folderRow.isHidden = true
        } else {
            infoLabel.stringValue = "未归入项目：请手动指定本次任务的执行代理与工作目录。"
            infoLabel.isHidden = false
            execRow.isHidden = false
            folderRow.isHidden = false
        }
    }
}

private final class FolderPickerTarget: NSObject {
    let field: NSTextField
    let nameField: NSTextField?
    weak var window: NSWindow?
    init(field: NSTextField, nameField: NSTextField?, window: NSWindow) {
        self.field = field
        self.nameField = nameField
        self.window = window
    }
    @objc func choose() {
        guard let window = window else { return }
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.allowsMultipleSelection = false
        panel.canCreateDirectories = true
        let current = (field.stringValue.trimmingCharacters(in: .whitespacesAndNewlines) as NSString).expandingTildeInPath
        if !current.isEmpty { panel.directoryURL = URL(fileURLWithPath: current) }
        panel.beginSheetModal(for: window) { [weak self] result in
            guard result == .OK, let url = panel.url, let self = self else { return }
            self.field.stringValue = url.path
            if let nf = self.nameField, nf.stringValue.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                nf.stringValue = url.lastPathComponent
            }
        }
    }
}
