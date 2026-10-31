import Cocoa
import Darwin

struct OrchestraAgent: Decodable {
    let name: String
    let label: String
    let available: Bool
    let reason: String?
}
struct OrchestraRunnerStatus: Decodable { let active: Bool; let dir: String }
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
struct OrchestraRun: Decodable { let receipt: String?; let error: String? }
struct OrchestraTask: Decodable {
    let id: String
    let executor: String
    let folder: String
    let card: String
    let status: String
    let updated: String
    let runs: [OrchestraRun]?
    var canCancel: Bool { status == "queued" || status == "running" }
    var canAccept: Bool { status == "receipt" || status == "failed" }
    var canRework: Bool { canAccept }
    var lastResult: String { runs?.last?.receipt ?? runs?.last?.error ?? "暂无回执" }
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

// CLI calls run on a serial background queue; stderr is returned to the user.
final class OrchestraWindowController: NSWindowController, NSWindowDelegate, NSTableViewDataSource, NSTableViewDelegate {
    private let executable: URL
    private let queue = DispatchQueue(label: "codex-helper.orchestra")
    private var timer: Timer?
    private var refreshing = false
    private var agents: [OrchestraAgent] = []
    private var tasks: [OrchestraTask] = []
    private var runner: Process?
    private var runnerActive = false
    private var confirmedRunner = false
    private var actionBusy = false
    private var loginInfo: OrchestraLoginInfo?
    private var loginProcess: Process?
    private var loginBusy = false
    private var loginCancelling = false
    private let agentLabel = NSTextField(wrappingLabelWithString: "正在读取代理…")
    private let runnerLabel = NSTextField(wrappingLabelWithString: "正在读取执行器状态…")
    private let loginLabel = NSTextField(wrappingLabelWithString: "正在读取执行账号…")
    private let loginPath = NSTextField(wrappingLabelWithString: "")
    private let loginMessage = NSTextField(wrappingLabelWithString: "")
    private let loginButton = NSButton(title: "登录", target: nil, action: nil)
    private let logoutButton = NSButton(title: "退出登录", target: nil, action: nil)
    private let loginCancel = NSButton(title: "取消登录", target: nil, action: nil)
    private let notice = NSTextField(wrappingLabelWithString: "")
    private let start = NSButton(title: "启动执行器", target: nil, action: nil)
    private let stop = NSButton(title: "停止执行器", target: nil, action: nil)
    private let newTask = NSButton(title: "新任务", target: nil, action: nil)
    private let cancelTask = NSButton(title: "停止", target: nil, action: nil)
    private let acceptTask = NSButton(title: "验收", target: nil, action: nil)
    private let reworkTask = NSButton(title: "返工", target: nil, action: nil)
    private let table = NSTableView()
    private let details = NSTextView()

    init(executable: URL) {
        self.executable = executable
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 760, height: 760), styleMask: [.titled, .closable, .resizable, .miniaturizable], backing: .buffered, defer: false)
        window.title = "代理协作"; window.minSize = NSSize(width: 600, height: 600)
        super.init(window: window)
        window.delegate = self
        build()
        window.center()
    }
    required init?(coder: NSCoder) { fatalError("init(coder:) has not been implemented") }

    private func build() {
        guard let content = window?.contentView else { return }
        let scroll = NSScrollView(); scroll.hasVerticalScroller = true; scroll.autohidesScrollers = true
        let document = NSView(); scroll.documentView = document
        content.addSubview(scroll); scroll.translatesAutoresizingMaskIntoConstraints = false
        NSLayoutConstraint.activate([scroll.leadingAnchor.constraint(equalTo: content.leadingAnchor), scroll.trailingAnchor.constraint(equalTo: content.trailingAnchor), scroll.topAnchor.constraint(equalTo: content.topAnchor), scroll.bottomAnchor.constraint(equalTo: content.bottomAnchor)])
        let stack = NSStackView(); stack.orientation = .vertical; stack.alignment = .leading; stack.spacing = 10
        document.addSubview(stack); stack.translatesAutoresizingMaskIntoConstraints = false
        document.translatesAutoresizingMaskIntoConstraints = false
        NSLayoutConstraint.activate([document.widthAnchor.constraint(equalTo: scroll.contentView.widthAnchor), stack.leadingAnchor.constraint(equalTo: document.leadingAnchor, constant: 20), stack.trailingAnchor.constraint(equalTo: document.trailingAnchor, constant: -20), stack.topAnchor.constraint(equalTo: document.topAnchor, constant: 20), stack.bottomAnchor.constraint(equalTo: document.bottomAnchor, constant: -20)])
        func add(_ view: NSView) { stack.addArrangedSubview(view); view.translatesAutoresizingMaskIntoConstraints = false; view.widthAnchor.constraint(equalTo: stack.widthAnchor).isActive = true }
        add(Theme.text("代理协作", size: 20, weight: .bold))
        add(Theme.caption("查看代理和任务；执行器启动后会处理队列中的任务。"))
        add(Theme.sectionTitle("代理"))
        let agentBox = SectionBox(); agentLabel.font = .systemFont(ofSize: 12); agentLabel.maximumNumberOfLines = 0; agentBox.add(agentLabel); add(agentBox)
        add(Theme.sectionTitle("执行账号"))
        let loginBox = SectionBox(); loginLabel.font = .systemFont(ofSize: 12, weight: .medium); loginBox.add(loginLabel)
        loginPath.font = .systemFont(ofSize: 11); loginPath.textColor = .secondaryLabelColor; loginPath.isSelectable = true; loginBox.add(loginPath)
        let accountCaption = Theme.caption("任务消耗登录账号的额度；执行账号独立于 Desktop（~/.codex）和 CLI（~/.codex-cli）。")
        accountCaption.maximumNumberOfLines = 0; loginBox.add(accountCaption)
        loginButton.target = self; loginButton.action = #selector(beginLogin); Theme.button(loginButton, small: true)
        logoutButton.target = self; logoutButton.action = #selector(confirmLogout); Theme.button(logoutButton, small: true)
        loginCancel.target = self; loginCancel.action = #selector(cancelLogin); Theme.button(loginCancel, small: true)
        loginBox.add(Theme.hstack([loginButton, logoutButton, loginCancel, Theme.flexible()]))
        loginMessage.font = .systemFont(ofSize: 11); loginMessage.textColor = .secondaryLabelColor; loginMessage.maximumNumberOfLines = 0; loginBox.add(loginMessage); add(loginBox)
        add(Theme.sectionTitle("执行器"))
        let runnerBox = SectionBox(); runnerLabel.font = .systemFont(ofSize: 12); runnerBox.add(runnerLabel)
        start.target = self; start.action = #selector(startRunner); Theme.button(start, primary: true)
        stop.target = self; stop.action = #selector(stopRunnerAction); Theme.button(stop)
        runnerBox.add(Theme.hstack([start, stop, Theme.flexible()]))
        notice.font = .systemFont(ofSize: 11); notice.textColor = .systemOrange; notice.maximumNumberOfLines = 0; runnerBox.add(notice); add(runnerBox)
        add(Theme.sectionTitle("任务"))
        let taskBox = SectionBox()
        for (identifier,title,width) in [("id","编号",100.0),("executor","执行者",90.0),("status","状态",100.0),("folder","文件夹",160.0),("updated","更新时间",170.0)] {
            let column = NSTableColumn(identifier: NSUserInterfaceItemIdentifier(identifier)); column.title = title; column.width = width; table.addTableColumn(column)
        }
        table.delegate = self; table.dataSource = self; table.rowHeight = 24; table.usesAlternatingRowBackgroundColors = true
        table.setAccessibilityLabel("协作任务列表")
        let tableScroll = NSScrollView(); tableScroll.documentView = table; tableScroll.hasVerticalScroller = true; tableScroll.hasHorizontalScroller = true
        tableScroll.heightAnchor.constraint(equalToConstant: 150).isActive = true; taskBox.add(tableScroll)
        newTask.target = self; newTask.action = #selector(createTask); Theme.button(newTask, small: true)
        cancelTask.target = self; cancelTask.action = #selector(cancelSelected); Theme.button(cancelTask, small: true)
        acceptTask.target = self; acceptTask.action = #selector(acceptSelected); Theme.button(acceptTask, small: true)
        reworkTask.target = self; reworkTask.action = #selector(reworkSelected); Theme.button(reworkTask, small: true)
        taskBox.add(Theme.hstack([newTask, Theme.flexible(), cancelTask, acceptTask, reworkTask]))
        details.isEditable = false; details.isSelectable = true; details.font = .systemFont(ofSize: 12); details.string = "选择任务查看任务卡和回执。"
        let detailScroll = NSScrollView(); detailScroll.documentView = details; detailScroll.hasVerticalScroller = true; detailScroll.borderType = .bezelBorder
        details.isVerticallyResizable = true; details.autoresizingMask = [.width]; details.textContainer?.widthTracksTextView = true
        detailScroll.heightAnchor.constraint(equalToConstant: 120).isActive = true; taskBox.add(detailScroll); add(taskBox)
        add(Theme.sectionTitle("连接主控"))
        let connectBox = SectionBox(); let instructions = NSTextView(); instructions.isEditable = false; instructions.isSelectable = true; instructions.font = .monospacedSystemFont(ofSize: 11, weight: .regular)
        instructions.string = orchestraConnectionInstructions(executable: executable, stateDir: ProcessInfo.processInfo.environment["CODEX_HELPER_ORCHESTRA_DIR"])
        let connectScroll = NSScrollView(); connectScroll.documentView = instructions; connectScroll.hasVerticalScroller = true; connectScroll.borderType = .bezelBorder
        instructions.isVerticallyResizable = true; instructions.autoresizingMask = [.width]; instructions.textContainer?.widthTracksTextView = true
        connectScroll.heightAnchor.constraint(equalToConstant: 135).isActive = true; connectBox.add(connectScroll); add(connectBox)
        updateButtons()
    }

    override func showWindow(_ sender: Any?) {
        super.showWindow(sender); window?.makeKeyAndOrderFront(nil); NSApp.activate(ignoringOtherApps: true)
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
                let login = try decoder.decode(OrchestraLoginInfo.self, from: self.command(["login-status"]))
                let taskData = try self.command(["list"])
                let tasks = try decoder.decode([OrchestraTask]?.self, from: taskData) ?? []
                DispatchQueue.main.async {
                    let selectedID = self.selected?.id
                    self.refreshing = false; self.agents = agents; self.runnerActive = status.active; self.loginInfo = login
                    self.tasks = tasks.sorted { $0.updated > $1.updated }; self.render(selectedID: selectedID)
                }
            } catch { DispatchQueue.main.async { self.refreshing = false; self.notice.stringValue = "刷新失败：\(error.localizedDescription)"; self.updateButtons() } }
        }
    }
    private var selected: OrchestraTask? { let row = table.selectedRow; return tasks.indices.contains(row) ? tasks[row] : nil }
    private func render(selectedID: String?) {
        agentLabel.stringValue = agents.map { "\($0.label)：\($0.available ? "可用" : "不可用")" + (($0.available || $0.reason == nil) ? "" : " · \($0.reason!)") }.joined(separator: "\n")
        runnerLabel.stringValue = runnerActive ? ((runner?.isRunning == true) ? "执行器运行中（由本小助手启动）" : "检测到其他执行器正在运行") : "执行器未运行"
        if let info = loginInfo {
            switch info.login.state {
            case "logged_in": loginLabel.stringValue = "Codex 执行账号：已登录" + (info.login.mode.isEmpty ? "" : "（\(info.login.mode)）")
            case "not_logged_in": loginLabel.stringValue = "Codex 执行账号：未登录"
            default: loginLabel.stringValue = "Codex 执行账号：无法读取"
            }
            loginPath.stringValue = "目录：" + info.dir
        }
        table.reloadData()
        if let id = selectedID, let index = tasks.firstIndex(where: { $0.id == id }) { table.selectRowIndexes(IndexSet(integer: index), byExtendingSelection: false) }
        updateButtons()
    }
    private func updateButtons() {
        start.isEnabled = !runnerActive && !actionBusy && runner?.isRunning != true
        stop.isEnabled = runner?.isRunning == true && !actionBusy
        newTask.isEnabled = !actionBusy && agents.contains(where: { $0.available })
        cancelTask.isEnabled = !actionBusy && selected?.canCancel == true
        acceptTask.isEnabled = !actionBusy && selected?.canAccept == true
        reworkTask.isEnabled = !actionBusy && selected?.canRework == true
        let loginActions = OrchestraLoginActions(state: loginInfo?.login.state ?? "unknown", busy: loginBusy)
        loginButton.isEnabled = loginInfo != nil && loginActions.canLogin
        logoutButton.isEnabled = loginInfo != nil && loginActions.canLogout
        loginCancel.isEnabled = loginProcess?.isRunning == true && !loginCancelling
        loginCancel.isHidden = !loginBusy
        if let task = selected { details.string = "任务卡：\n\(task.card)\n\n最近回执或错误：\n\(task.lastResult)" }
        else { details.string = "选择任务查看任务卡和回执。" }
    }
    func numberOfRows(in tableView: NSTableView) -> Int { tasks.count }
    func tableViewSelectionDidChange(_ notification: Notification) { updateButtons() }
    func tableView(_ tableView: NSTableView, viewFor column: NSTableColumn?, row: Int) -> NSView? {
        guard tasks.indices.contains(row), let column = column else { return nil }; let task = tasks[row]
        let text: String
        switch column.identifier.rawValue {
        case "id": text = task.id
        case "executor": text = task.executor
        case "status": text = task.status
        case "folder": text = URL(fileURLWithPath: task.folder).lastPathComponent
        default: text = task.updated.replacingOccurrences(of: "T", with: " ").prefix(19).description
        }
        return NSTextField(labelWithString: text)
    }

    @objc private func startRunner() {
        guard !actionBusy, !runnerActive, runner?.isRunning != true, let window = window else { return }
        let begin = { [weak self] in self?.startAfterCheck() }
        if confirmedRunner && loginInfo?.login.state == "logged_in" { begin(); return }
        let alert = NSAlert(); alert.messageText = "启动无人值守执行器？"
        alert.informativeText = "执行器会立即处理队列中已有任务。代理将在无人值守时运行，并消耗所选代理的使用额度。Codex 使用工作区写入限制，请先核对任务和工作目录。" + (loginInfo?.login.state == "not_logged_in" ? "\n\nCodex 执行账号未登录，Codex 任务会失败。" : (loginInfo?.login.state != "logged_in" ? "\n\nCodex 执行账号状态未知，任务可能失败。" : ""))
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
                    let process = Process(); process.executableURL = self.executable; process.arguments = ["orchestra", "run"]
                    process.standardOutput = FileHandle.nullDevice; process.standardError = FileHandle.nullDevice
                    do { try process.run(); self.runner = process; self.notice.stringValue = "已请求启动执行器。" } catch { self.notice.stringValue = "启动失败：\(error.localizedDescription)" }
                    self.updateButtons(); self.refresh()
                }
            } catch { DispatchQueue.main.async { self.actionBusy = false; self.notice.stringValue = "无法确认执行器状态：\(error.localizedDescription)"; self.updateButtons() } }
        }
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
    func stopRunnerForTermination() { timer?.invalidate(); cancelLogin(); stopOwnedRunner(wait: true) }

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

    private func mutate(_ args: [String], input: String? = nil) {
        guard !actionBusy else { return }; actionBusy = true; updateButtons()
        queue.async { [weak self] in guard let self = self else { return }; do { _ = try self.command(args, input: input); DispatchQueue.main.async { self.actionBusy = false; self.notice.stringValue = "操作已提交。"; self.updateButtons(); self.refresh() } } catch { DispatchQueue.main.async { self.actionBusy = false; self.notice.stringValue = "操作失败：\(error.localizedDescription)"; self.updateButtons() } } }
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
    @objc private func createTask() {
        guard let window = window else { return }; let available = agents.filter { $0.available }; guard !available.isEmpty else { return }
        let alert = NSAlert(); alert.messageText = "新任务"; alert.informativeText = "任务卡会进入队列；执行器运行时会自动执行。"
        let form = NSStackView(); form.orientation = .vertical; form.alignment = .leading; form.spacing = 8; form.frame = NSRect(x: 0, y: 0, width: 450, height: 230)
        let popup = NSPopUpButton(frame: NSRect(x: 0, y: 0, width: 450, height: 26)); popup.addItems(withTitles: available.map { $0.label })
        let folder = NSTextField(string: ""); folder.placeholderString = "选择工作目录"; folder.isEditable = false
        let choose = NSButton(title: "选择文件夹…", target: nil, action: nil)
        let target = FolderPickerTarget(field: folder, window: alert.window); choose.target = target; choose.action = #selector(FolderPickerTarget.choose)
        let folderRow = Theme.hstack([folder, choose]); let id = NSTextField(string: ""); id.placeholderString = "任务编号"
        let card = NSTextView(); card.font = .systemFont(ofSize: 12); let cardScroll = NSScrollView(); cardScroll.documentView = card; cardScroll.borderType = .bezelBorder; cardScroll.hasVerticalScroller = true; cardScroll.heightAnchor.constraint(equalToConstant: 115).isActive = true
        for item in [popup, folderRow, id, cardScroll] as [NSView] { form.addArrangedSubview(item); item.widthAnchor.constraint(equalToConstant: 450).isActive = true }
        alert.accessoryView = form; alert.addButton(withTitle: "加入队列"); alert.addButton(withTitle: "取消")
        alert.beginSheetModal(for: window) { [weak self] response in
            _ = target
            if response == .alertFirstButtonReturn { let index = popup.indexOfSelectedItem; if available.indices.contains(index) { self?.mutate(["dispatch", "--to", available[index].name, "--folder", folder.stringValue, "--id", id.stringValue, "--lead", "user"], input: card.string) } }
        }
    }
}

private final class FolderPickerTarget: NSObject {
    let field: NSTextField; weak var window: NSWindow?
    init(field: NSTextField, window: NSWindow) { self.field = field; self.window = window }
    @objc func choose() { guard let window = window else { return }; let panel = NSOpenPanel(); panel.canChooseDirectories = true; panel.canChooseFiles = false; panel.allowsMultipleSelection = false; panel.beginSheetModal(for: window) { [weak self] result in if result == .OK, let url = panel.url { self?.field.stringValue = url.path } } }
}
