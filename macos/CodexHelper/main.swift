import Cocoa
import ServiceManagement

final class FlippedDocument: NSView { override var isFlipped: Bool { true } }
final class ProjectItem: NSObject {
    var group: SessionProject
    var children: [SessionItem] = []
    init(_ group: SessionProject) { self.group = group }
}
final class SessionItem: NSObject {
    var session: HelperSession
    init(_ session: HelperSession) { self.session = session }
}

final class Assistant: NSObject, NSApplicationDelegate, NSWindowDelegate, NSOutlineViewDataSource, NSOutlineViewDelegate, NSSearchFieldDelegate {
    var window: NSWindow!
    var snapshot: Snapshot?
    var visibleSessions: [HelperSession] = []
    var projects: [ProjectItem] = []
    var projectItems: [String: ProjectItem] = [:]
    var sessionItems: [String: SessionItem] = [:]
    var state: SelectionState
    let store: StateStore
    let fixture: URL?
    let status = NSTextField(wrappingLabelWithString: "正在读取 CC Switch 与会话记录…")
    let accountStack = NSStackView()
    let search = NSSearchField()
    let archived = NSButton(checkboxWithTitle: "包括归档", target: nil, action: nil)
    let sourceTabs = NSSegmentedControl(labels: ["Desktop", "CLI"], trackingMode: .selectOne, target: nil, action: nil)
    let sourceHint = NSTextField(wrappingLabelWithString: "")
    let table = NSOutlineView()
    let selection = NSTextField(wrappingLabelWithString: "选择一个会话后，可以开始观察其记录更新。")
    let detail = NSTextField(wrappingLabelWithString: "")
    let observe = NSButton(title: "开始观察", target: nil, action: nil)
    let feedback = NSTextField(wrappingLabelWithString: "")
    let refresh = NSButton(title: "刷新", target: nil, action: nil)
    let countLabel = NSTextField(labelWithString: "")
    let emptyLabel = NSTextField(wrappingLabelWithString: "")
    let selectionHint = NSTextField(wrappingLabelWithString: "")
    let clearSelection = NSButton(title: "取消选择", target: nil, action: nil)
    let outerScroll = NSScrollView()
    let document = FlippedDocument()
    let contentStack = NSStackView()
    let automationStack = NSStackView()
    let automationToggle = NSButton(title: "自动切号 · 待接入", target: nil, action: nil)
    var tableHeight: NSLayoutConstraint!
    var readError: String?
    var timer: Timer?
    var loading = false
    var lastQuotaRefresh: Date?
    var rendering = false
    var observedRevision: Int64?
    var observationMessage = ""
    var warmup: NSButton!
    var handoff: NSButton!
    let warmupCheck = NSButton(title: "检查一次", target: nil, action: nil)
    let warmupStatus = NSTextField(wrappingLabelWithString: "自动预热未开启")
    let scheduleLabel = NSTextField(labelWithString: "时段：全天允许 · UTC+8")
    let scheduleButton = NSButton(title: "自定义时段", target: nil, action: nil)
    var scheduleEditor: (NSButton, NSDatePicker, NSDatePicker, NSTextField, NSButton)?
    var warmupReport: WarmupReport?
    var warmupChecking = false
    var warmupControlBusy = false
    var warmupProcess: Process?
    var shuttingDown = false
    var warmupMenuItem: NSStatusItem?
    var warmupStopItem: NSMenuItem?
    var resumeStopItem: NSMenuItem?
    var handoffStopItem: NSMenuItem?
    var lastWarmupCheck = Date.distantPast
    var warmupControlRevision = 0
    let loginItem = NSButton(checkboxWithTitle: "登录后启动", target: nil, action: nil)
    let loginStatus = NSTextField(wrappingLabelWithString: "")
    let resumeToggle = NSButton(checkboxWithTitle: "为选中会话自动续跑（不切号）", target: nil, action: nil)
    let cliResume = NSButton(title: "在终端托管续跑", target: nil, action: nil)
    let resumeStatus = NSTextField(wrappingLabelWithString: "未开启指定会话续跑")
    let handoffThreshold = NSTextField(string: "10")
    let handoffPreflight = NSButton(title: "检查所选会话", target: nil, action: nil)
    let handoffStatus = NSTextField(wrappingLabelWithString: "选择会话后可以检查当前交接条件。")
    let handoffStart = NSButton(title: "托管并开启切号", target: nil, action: nil)
    let handoffStop = NSButton(title: "停止切号", target: nil, action: nil)
    let handoffAcknowledge = NSButton(title: "已检查，解除暂停", target: nil, action: nil)
    let cliHandoffAvailable = true
    let handoffRunStatus = NSTextField(wrappingLabelWithString: "")
    var handoffChecking = false
    var handoffCheckedKey: String?
    var handoffReport: HandoffReport?
    var handoffBusy = false
    var handoffRevision = 0
    var handoffPendingKey: String?
    var handoffLaunchDeadline: Date?
    var handoffCancelPending = false
    var desktopHandoffProcess: Process?
    var resumeReport: ResumeReport?
    var resumeBusy = false
    var resumeChecking = false
    var resumeRevision = 0
    var lastResumeCheck = Date.distantPast
    var resumeProcess: Process?
    var backgroundEnabled: Bool { warmupReport?.enabled == true || resumeReport?.enabled == true || handoffReport?.hasAuthorization == true || handoffPendingKey != nil }


    init(fixture: URL?, directory: URL) {
        self.fixture = fixture
        store = StateStore(directory: directory)
        state = store.load()
        super.init()
    }

    func label(_ text: String, size: CGFloat = 12, weight: NSFont.Weight = .regular,
               color: NSColor = .secondaryLabelColor) -> NSTextField {
        let field = NSTextField(wrappingLabelWithString: text)
        field.font = .systemFont(ofSize: size, weight: weight)
        field.textColor = color
        return field
    }

    func row(_ views: [NSView], spacing: CGFloat = 10) -> NSStackView {
        let view = NSStackView(views: views)
        view.orientation = .horizontal; view.alignment = .centerY; view.spacing = spacing
        return view
    }

    func spacer() -> NSView {
        let view = NSView(); view.setContentHuggingPriority(.defaultLow, for: .horizontal)
        return view
    }

    func add(_ view: NSView, to stack: NSStackView) {
        stack.addArrangedSubview(view)
        view.translatesAutoresizingMaskIntoConstraints = false
        view.widthAnchor.constraint(equalTo: stack.widthAnchor).isActive = true
    }

    func separator() -> NSBox { let box = NSBox(); box.boxType = .separator; return box }

    func pairedLabels(_ leading: NSTextField, _ trailing: NSTextField) -> NSView {
        let view = NSView(); trailing.alignment = .right
        view.addSubview(leading); view.addSubview(trailing)
        leading.translatesAutoresizingMaskIntoConstraints = false; trailing.translatesAutoresizingMaskIntoConstraints = false
        NSLayoutConstraint.activate([
            view.heightAnchor.constraint(equalToConstant: 18),
            leading.leadingAnchor.constraint(equalTo: view.leadingAnchor),
            leading.trailingAnchor.constraint(equalTo: trailing.leadingAnchor, constant: -12),
            leading.centerYAnchor.constraint(equalTo: view.centerYAnchor),
            trailing.trailingAnchor.constraint(equalTo: view.trailingAnchor),
            trailing.widthAnchor.constraint(equalToConstant: 160),
            trailing.centerYAnchor.constraint(equalTo: view.centerYAnchor)
        ])
        return view
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        let mainMenu = NSMenu(), appMenu = NSMenu(), item = NSMenuItem()
        appMenu.addItem(withTitle: "退出 Codex 小助手", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        item.submenu = appMenu; mainMenu.addItem(item)
        let editItem = NSMenuItem(), editMenu = NSMenu(title: "编辑")
        editMenu.addItem(withTitle: "拷贝", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        editMenu.addItem(withTitle: "粘贴", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        editMenu.addItem(withTitle: "全选", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        editItem.submenu = editMenu; mainMenu.addItem(editItem); NSApp.mainMenu = mainMenu

        window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 660, height: 700),
                          styleMask: [.titled, .closable, .miniaturizable, .resizable], backing: .buffered, defer: false)
        window.title = "Codex 小助手"
        window.minSize = NSSize(width: 560, height: 500)
        window.delegate = self
        window.isReleasedWhenClosed = false
        window.titlebarAppearsTransparent = true
        let content = NSView(); window.contentView = content
        outerScroll.hasVerticalScroller = true; outerScroll.autohidesScrollers = true
        outerScroll.drawsBackground = false
        content.addSubview(outerScroll); outerScroll.translatesAutoresizingMaskIntoConstraints = false
        NSLayoutConstraint.activate([
            outerScroll.topAnchor.constraint(equalTo: content.topAnchor), outerScroll.bottomAnchor.constraint(equalTo: content.bottomAnchor),
            outerScroll.leadingAnchor.constraint(equalTo: content.leadingAnchor), outerScroll.trailingAnchor.constraint(equalTo: content.trailingAnchor)
        ])
        outerScroll.documentView = document
        document.autoresizingMask = [.width]
        let stack = contentStack; stack.orientation = .vertical; stack.alignment = .leading; stack.spacing = 10; stack.detachesHiddenViews = true
        document.addSubview(stack); stack.translatesAutoresizingMaskIntoConstraints = false
        NSLayoutConstraint.activate([stack.topAnchor.constraint(equalTo: document.topAnchor, constant: 18),
                                     stack.leadingAnchor.constraint(equalTo: document.leadingAnchor, constant: 22),
                                     stack.trailingAnchor.constraint(equalTo: document.trailingAnchor, constant: -22)])
        let appIcon = Bundle.main.url(forResource: "CodexHelper", withExtension: "icns").flatMap { NSImage(contentsOf: $0) }
            ?? NSImage(systemSymbolName: "person.2.badge.gearshape", accessibilityDescription: nil)!
        let icon = NSImageView(image: appIcon)
        icon.imageScaling = .scaleProportionallyUpOrDown
        icon.setAccessibilityLabel("Codex 小助手")
        icon.widthAnchor.constraint(equalToConstant: 32).isActive = true
        icon.heightAnchor.constraint(equalToConstant: 32).isActive = true
        let version = Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "开发版"
        let versionLabel = cliHandoffAvailable ? version : version + " · 验收中"
        add(row([icon, label("账号与会话", size: 20, weight: .semibold, color: .labelColor), spacer(), label(versionLabel)]), to: stack)
        status.font = .systemFont(ofSize: 12); status.textColor = .secondaryLabelColor
        status.maximumNumberOfLines = 2
        add(status, to: stack)
        let openSwitch = NSButton(title: "打开 CC Switch", target: self, action: #selector(openCCSwitch))
        openSwitch.bezelStyle = .rounded
        refresh.target = self; refresh.action = #selector(refreshNow); refresh.bezelStyle = .rounded
        add(row([label("CC Switch 账号", size: 13, weight: .semibold, color: .labelColor), spacer(), openSwitch, refresh]), to: stack)
        accountStack.orientation = .vertical; accountStack.alignment = .leading; accountStack.spacing = 8
        add(accountStack, to: stack)
        warmup = NSButton(checkboxWithTitle: "自动预热五小时窗口", target: self, action: #selector(toggleWarmup))
        warmup.isEnabled = false
        warmupCheck.target = self; warmupCheck.action = #selector(checkWarmupNow); warmupCheck.bezelStyle = .rounded
        warmupCheck.isEnabled = false
        add(row([warmup, spacer(), warmupCheck]), to: stack)
        scheduleLabel.font = .systemFont(ofSize: 11); scheduleLabel.textColor = .secondaryLabelColor
        scheduleLabel.lineBreakMode = .byTruncatingTail
        scheduleButton.target = self; scheduleButton.action = #selector(editWarmupSchedule); scheduleButton.bezelStyle = .rounded
        add(row([scheduleLabel, spacer(), scheduleButton]), to: stack)
        warmupStatus.font = .systemFont(ofSize: 11); warmupStatus.textColor = .secondaryLabelColor
        warmupStatus.maximumNumberOfLines = 3
        add(warmupStatus, to: stack)
        add(separator(), to: stack)
        add(row([label("会话 · 按项目收纳", size: 13, weight: .semibold, color: .labelColor), spacer(), countLabel]), to: stack)
        sourceTabs.target = self; sourceTabs.action = #selector(changeSource)
        sourceTabs.segmentDistribution = .fillEqually
        sourceTabs.selectedSegment = state.activeScope == "cli" ? 1 : 0
        add(sourceTabs, to: stack)
        sourceHint.font = .systemFont(ofSize: 11); sourceHint.textColor = .secondaryLabelColor
        sourceHint.maximumNumberOfLines = 2
        add(sourceHint, to: stack)
        search.placeholderString = "搜索标题、项目或会话 ID"
        search.delegate = self; search.sendsSearchStringImmediately = true
        archived.target = self; archived.action = #selector(filterChanged)
        add(row([search, archived]), to: stack)
        search.setContentHuggingPriority(.defaultLow, for: .horizontal)
        let titleColumn = NSTableColumn(identifier: NSUserInterfaceItemIdentifier("title")); titleColumn.title = "项目 / 会话"; titleColumn.width = 460; titleColumn.minWidth = 240
        let timeColumn = NSTableColumn(identifier: NSUserInterfaceItemIdentifier("time")); timeColumn.title = "最近活动"; timeColumn.width = 100; timeColumn.minWidth = 90
        table.addTableColumn(titleColumn); table.addTableColumn(timeColumn)
        table.outlineTableColumn = titleColumn; table.indentationPerLevel = 16
        table.autoresizesOutlineColumn = false
        table.delegate = self; table.dataSource = self; table.rowHeight = 38
        table.usesAlternatingRowBackgroundColors = true; table.allowsEmptySelection = true
        table.columnAutoresizingStyle = .firstColumnOnlyAutoresizingStyle
        table.setAccessibilityLabel("按项目分组的 Codex 会话")
        let scroll = NSScrollView(); scroll.documentView = table; scroll.hasVerticalScroller = true; scroll.borderType = .bezelBorder
        scroll.autohidesScrollers = true
        tableHeight = scroll.heightAnchor.constraint(equalToConstant: 220); tableHeight.isActive = true
        scroll.setContentHuggingPriority(.defaultLow, for: .vertical)
        add(scroll, to: stack)
        emptyLabel.font = .systemFont(ofSize: 12); emptyLabel.textColor = .secondaryLabelColor
        add(emptyLabel, to: stack); emptyLabel.isHidden = true
        selection.font = .systemFont(ofSize: 13, weight: .medium); selection.maximumNumberOfLines = 2
        detail.font = .systemFont(ofSize: 11); detail.textColor = .secondaryLabelColor; detail.maximumNumberOfLines = 3
        add(selection, to: stack); add(detail, to: stack)
        selection.isSelectable = true; detail.isSelectable = true
        selectionHint.font = .systemFont(ofSize: 11); selectionHint.textColor = .systemOrange
        add(selectionHint, to: stack); selectionHint.isHidden = true
        observe.target = self; observe.action = #selector(toggleObservation); observe.bezelStyle = .rounded
        observe.isEnabled = false
        clearSelection.target = self; clearSelection.action = #selector(clearTarget); clearSelection.bezelStyle = .rounded
        clearSelection.isEnabled = false
        add(row([label("仅观察本机记录"), spacer(), clearSelection, observe]), to: stack)
        resumeToggle.target = self; resumeToggle.action = #selector(toggleResume)
        cliResume.target = self; cliResume.action = #selector(openManagedCLI); cliResume.bezelStyle = .rounded
        add(row([resumeToggle, spacer(), cliResume]), to: stack)
        resumeStatus.font = .systemFont(ofSize: 11); resumeStatus.textColor = .secondaryLabelColor
        add(resumeStatus, to: stack)
        add(separator(), to: stack)
        handoffThreshold.alignment = .right
        handoffThreshold.widthAnchor.constraint(equalToConstant: 44).isActive = true
        handoffPreflight.target = self; handoffPreflight.action = #selector(checkHandoffPreflight)
        handoffPreflight.bezelStyle = .rounded
        handoffStart.target = self; handoffStart.action = #selector(startCLIHandoff); handoffStart.bezelStyle = .rounded
        handoffStop.target = self; handoffStop.action = #selector(stopHandoff); handoffStop.bezelStyle = .rounded
        handoffAcknowledge.target = self; handoffAcknowledge.action = #selector(acknowledgeHandoff); handoffAcknowledge.bezelStyle = .rounded
        add(row([label("自动切号", weight: .medium), spacer(), label("剩余低于"), handoffThreshold, label("%"), handoffPreflight]), to: stack)
        add(row([handoffStart, handoffStop, handoffAcknowledge, spacer()]), to: stack)
        handoffRunStatus.font = .systemFont(ofSize: 11); handoffRunStatus.textColor = .secondaryLabelColor
        add(handoffRunStatus, to: stack)
        handoffStatus.font = .systemFont(ofSize: 11); handoffStatus.textColor = .secondaryLabelColor
        add(handoffStatus, to: stack)
        add(separator(), to: stack)
        let help = NSButton(title: "接入说明", target: self, action: #selector(showDetails)); help.bezelStyle = .rounded
        automationToggle.target = self; automationToggle.action = #selector(toggleAutomationDetails)
        automationToggle.bezelStyle = .inline; automationToggle.isBordered = false
        automationToggle.font = .systemFont(ofSize: 12, weight: .medium)
        automationToggle.image = NSImage(systemSymbolName: "chevron.right", accessibilityDescription: nil)
        automationToggle.imagePosition = .imageLeading
        add(row([spacer(), help]), to: stack)
        automationStack.orientation = .vertical; automationStack.alignment = .leading; automationStack.spacing = 8
        automationStack.isHidden = true
        handoff = NSButton(checkboxWithTitle: "为选中会话自动切号续跑", target: nil, action: nil)
        handoff.isEnabled = false
        add(row([handoff, spacer(), label("待验证", color: .systemOrange)]), to: automationStack)
        feedback.font = .systemFont(ofSize: 11); feedback.textColor = .secondaryLabelColor
        feedback.maximumNumberOfLines = 2
        add(feedback, to: stack)
        if let visible = NSScreen.main?.visibleFrame {
            window.setContentSize(NSSize(width: min(660, visible.width - 40), height: min(700, visible.height - 60)))
        }
        loginItem.target = self; loginItem.action = #selector(toggleLoginItem)
        loginStatus.font = .systemFont(ofSize: 11); loginStatus.textColor = .secondaryLabelColor
        add(row([loginItem, spacer(), loginStatus]), to: stack)
        updateLoginItem()
        window.center(); window.makeKeyAndOrderFront(nil); layoutDocument(); NSApp.activate(ignoringOtherApps: true)
        refreshNow()
        timer = Timer.scheduledTimer(withTimeInterval: 30, repeats: true) { [weak self] _ in
            self?.refreshData(forceQuota: false)
            self?.runWarmupCheckIfDue()
            self?.runResumeCheckIfDue()
            self?.updateLoginItem()
        }
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { !backgroundEnabled }
    func applicationWillTerminate(_ notification: Notification) {
        shuttingDown = true; timer?.invalidate()
        if let process = warmupProcess, process.isRunning { process.terminate() }
        if let process = resumeProcess, process.isRunning { process.terminate() }
        if fixture == nil && resumeReport?.enabled == true {
            let bridge = Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/codex-helper")
            _ = try? BridgeClient.read(executable: bridge, arguments: ["resume","disable"], timeout: 3)
        }
        if fixture == nil && (handoffReport?.hasAuthorization == true || handoffPendingKey != nil) {
            let bridge = Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/codex-helper")
            _ = try? BridgeClient.read(executable: bridge, arguments: ["handoff","disable"], timeout: 3)
        }
    }
    func windowShouldClose(_ sender: NSWindow) -> Bool {
        if backgroundEnabled { sender.orderOut(nil); return false }
        return true
    }
    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        showHelperWindow(); return true
    }
    @objc func showHelperWindow() { window.makeKeyAndOrderFront(nil); NSApp.activate(ignoringOtherApps: true) }
    func windowDidResize(_ notification: Notification) { layoutDocument() }

    func layoutDocument() {
        guard tableHeight != nil else { return }
        outerScroll.superview?.layoutSubtreeIfNeeded()
        document.setFrameSize(NSSize(width: outerScroll.contentSize.width, height: max(document.frame.height, 1)))
        tableHeight.constant = max(180, min(360, outerScroll.contentSize.height * 0.32))
        document.layoutSubtreeIfNeeded()
        let height = max(outerScroll.contentSize.height, contentStack.fittingSize.height + 36)
        document.setFrameSize(NSSize(width: outerScroll.contentSize.width, height: height))
    }

    @objc func toggleAutomationDetails() {
        automationStack.isHidden.toggle()
        automationToggle.image = NSImage(systemSymbolName: automationStack.isHidden ? "chevron.right" : "chevron.down", accessibilityDescription: nil)
        layoutDocument()
    }

    @objc func showDetails() {
        let alert = NSAlert(); alert.messageText = "接入说明"
        alert.informativeText = "账号与额度沿用 CC Switch。预热在独立临时进程执行；CLI 可为所选会话启动托管切号，Desktop 切号仍在开发。"
        var lines = ["观察记录不能判断任务是否运行、实际使用账号或是否触及额度。"]
        if let snapshot = snapshot {
            for source in snapshot.sources ?? [] {
                lines += ["", "\(source.label) 会话目录：\(source.home)"]
                lines += source.warnings.map { "• \($0)" }
            }
            lines += ["", "CC Switch 配置目录：\(snapshot.codex_home)", "两端仅按目录分隔历史；账号卡片来自 CC Switch，不证明运行实例身份。"]
            lines += ["", "额度每 2 分钟自动查询，点刷新可立即查询。只读使用已签发的有效访问凭证；凭证过期需在 CC Switch 刷新，不复制或轮换刷新凭证。"]
            if let report = warmupReport {
                lines += ["", "预热：" + report.summary, "开启时每分钟检查；关闭窗口后仍在菜单栏运行。退出应用停止后续检查。"]
                for account in report.accounts {
                    if let execution = account.execution {
                        lines += ["\(account.name)：登记请求 \(account.attempts) 次，模型 \(execution.model ?? "未知")"]
                        if let usage = execution.usage { lines += ["输入 \(usage.input_tokens)，输出 \(usage.output_tokens)，合计 \(usage.total_tokens) tokens"] }
                    }
                }
            }
            lines += snapshot.warnings.isEmpty ? ["账号配置读取：无异常提示。"] : ["账号配置提示："] + snapshot.warnings.map { "• \($0)" }
            for (key,title) in [("account_warmup","账号预热"),("cli_handoff","CLI 切号交接"),("desktop_resume","Desktop 原会话续跑")] {
                if let capability = snapshot.capabilities[key] { lines += ["", "\(title)：\(capability.reason)"] }
            }
        } else { lines += ["", readError ?? "数据尚未读取。"] }
        let text = NSTextView(frame: NSRect(x: 0,y: 0,width: 440,height: 220))
        text.isEditable = false; text.isSelectable = true; text.font = .systemFont(ofSize: 12)
        text.string = lines.joined(separator: "\n"); text.textContainerInset = NSSize(width: 8,height: 8)
        let scroll = NSScrollView(frame: text.frame); scroll.hasVerticalScroller = true; scroll.documentView = text
        text.autoresizingMask = [.width]; text.textContainer?.widthTracksTextView = true
        alert.accessoryView = scroll; alert.addButton(withTitle: "知道了")
        alert.beginSheetModal(for: window)
    }

    @objc func openCCSwitch() {
        let candidates = [NSWorkspace.shared.urlForApplication(withBundleIdentifier: "com.ccswitch.desktop"),
                          URL(fileURLWithPath: "/Applications/CC Switch.app"),
                          FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Applications/CC Switch.app")]
        guard let url = candidates.compactMap({ $0 }).first(where: { FileManager.default.fileExists(atPath: $0.path) }) else {
            feedback.stringValue = "未找到 CC Switch，请先确认应用安装位置。"; return
        }
        NSWorkspace.shared.openApplication(at: url, configuration: NSWorkspace.OpenConfiguration()) { [weak self] _, error in
            if error != nil { DispatchQueue.main.async { self?.feedback.stringValue = "CC Switch 打开失败。" } }
        }
    }

    @objc func refreshNow() {
        refreshData(forceQuota: true)
    }

    func refreshData(forceQuota: Bool) {
        guard !loading else { return }; loading = true; refresh.isEnabled = false
        refresh.title = "刷新中…"
        let queryQuota = forceQuota || lastQuotaRefresh == nil || Date().timeIntervalSince(lastQuotaRefresh!) >= 120
        let warmupRevision = warmupControlRevision
        let resumeRevision = self.resumeRevision
        let handoffRevision = self.handoffRevision
        let fixture = self.fixture
        let bridge = Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/codex-helper")
        DispatchQueue.global(qos: .utility).async { [weak self] in
            let result: Result<Snapshot, Error>
            do {
                let data: Data
                if let fixture = fixture { data = try Data(contentsOf: fixture) }
                else {
                    data = try BridgeClient.read(executable: bridge, arguments: queryQuota ? ["snapshot","--quota"] : ["snapshot"])
                }
                result = .success(try JSONDecoder().decode(Snapshot.self, from: data))
            } catch { result = .failure(error) }
            DispatchQueue.main.async {
                guard let self = self else { return }
                self.loading = false; self.refresh.isEnabled = true; self.refresh.title = "刷新"
                switch result {
                case .success(var snapshot):
                    if queryQuota { self.lastQuotaRefresh = Date() }
                    else { snapshot.keepQuotaReadings(from: self.snapshot) }
                    self.readError = nil; self.snapshot = snapshot
                    if !self.warmupControlBusy && self.warmupControlRevision == warmupRevision { self.warmupReport = snapshot.warmup; self.updateWarmupUI() }
                    self.state.reconcile(snapshot.sessions(in: self.state.activeScope), complete: snapshot.complete(in: self.state.activeScope))
                    if !self.resumeBusy && self.resumeRevision == resumeRevision { self.resumeReport = snapshot.resume; self.updateResumeUI() }
                    if !self.handoffBusy && self.handoffRevision == handoffRevision {
                        self.handoffReport = snapshot.handoff
                        if snapshot.handoff?.enabled == true && snapshot.handoff?.session_id == self.selected?.id && snapshot.handoff?.runner_ready == true {
                            self.handoffPendingKey = nil; self.handoffLaunchDeadline = nil
                        } else if self.handoffPendingKey != nil, let deadline = self.handoffLaunchDeadline, Date() >= deadline {
                            self.stopHandoff()
                            self.handoffRunStatus.stringValue = self.state.activeScope == "desktop" ? "Desktop 监控未就绪，正在取消本次授权。" : "托管终端未就绪，正在取消本次授权。请检查终端提示后重试。"
                        }
                        self.updateHandoffUI()
                    }
                    if self.resumeReport?.enabled == true && (self.resumeReport?.key != self.state.selectedKey || self.selected?.archived != false || !snapshot.complete(in: self.state.activeScope)) { self.stopResumeForTargetChange() }
                    let switchingDesktop = self.handoffReport?.scope == "desktop" && ["pending", "waiting_exit", "ready_switch", "waiting_switch", "ready_restart", "waiting_restart", "ready_continue", "waiting_continue"].contains(self.handoffReport?.stage ?? "")
                    let selectionChanged = self.handoffReport?.scope != self.state.activeScope || (snapshot.complete(in: self.state.activeScope) && (self.handoffReport?.session_id != self.selected?.id || self.selected?.archived != false))
                    if self.handoffReport?.hasAuthorization == true && (selectionChanged || (!switchingDesktop && !snapshot.complete(in: self.state.activeScope))) { self.stopHandoffForTargetChange() }
                    self.updateObservation(); self.render(); self.save()
                case .failure(let error):
                    // Fail closed: an old list cannot silently remain actionable.
                    self.readError = (error as? BridgeError) == .timedOut ? "读取超时，请点刷新重试。" : "读取失败，请检查本机数据后点刷新重试。"
                    self.stopResumeForTargetChange()
                    let switchingDesktop = self.handoffReport?.scope == "desktop" && ["pending", "waiting_exit", "ready_switch", "waiting_switch", "ready_restart", "waiting_restart", "ready_continue", "waiting_continue"].contains(self.handoffReport?.stage ?? "")
                    if !switchingDesktop { self.stopHandoffForTargetChange() }
                    self.snapshot = nil; self.state.pauseObservation(); self.updateObservation(); self.render(); self.save()
                }
            }
        }
    }

    func render(preserveScroll: Bool = true) {
        for view in accountStack.arrangedSubviews { accountStack.removeArrangedSubview(view); view.removeFromSuperview() }
        guard let snapshot = snapshot else {
            sourceHint.stringValue = "\(state.activeScope == "cli" ? "CLI · ~/.codex-cli" : "Desktop · ~/.codex") · 数据暂不可用"
            status.textColor = .systemOrange
            status.stringValue = readError ?? "正在读取本机数据…"
            feedback.stringValue = "观察已暂停；恢复后需要手动开启。"
            filterChanged(); return
        }
        let prefix = fixture != nil || ProcessInfo.processInfo.environment["CODEX_HELPER_TEST_HOME"] != nil ? "隔离测试数据" : "本机数据与账号额度"
        let warnings = snapshot.warnings(in: state.activeScope)
        sourceTabs.selectedSegment = state.activeScope == "cli" ? 1 : 0
        sourceTabs.setLabel("Desktop（\(snapshot.sessions(in: "desktop").count)）", forSegment: 0)
        sourceTabs.setLabel("CLI（\(snapshot.sessions(in: "cli").count)）", forSegment: 1)
        if let source = snapshot.source(in: state.activeScope) {
            sourceHint.stringValue = "\(source.label) 目录：\((source.home as NSString).abbreviatingWithTildeInPath) · 按存放目录归类历史"
            sourceHint.toolTip = source.home
        } else { sourceHint.stringValue = "按目录分隔会话；旧快照仅支持 Desktop 数据。" }
        status.textColor = warnings.isEmpty ? .secondaryLabelColor : .systemOrange
        let iso = ISO8601DateFormatter(); iso.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        let captured = iso.date(from: snapshot.captured_at) ?? ISO8601DateFormatter().date(from: snapshot.captured_at)
        status.stringValue = "\(prefix) · \(captured.map { DateFormatter.localizedString(from: $0, dateStyle: .none, timeStyle: .short) } ?? "刚刚") 更新\(warnings.first.map { "\n" + $0 } ?? " · 额度每 2 分钟自动更新")"
        for (index, account) in snapshot.accounts.enumerated() {
            add(QuotaCard(account: account, index: index, target: self, action: #selector(showAccountQuotaDetails(_:))), to: accountStack)
        }
        if snapshot.accounts.isEmpty { add(label("未读取到 CC Switch Codex 账号。"), to: accountStack) }
        warmup.toolTip = snapshot.capabilities["account_warmup"]?.reason
        handoff.toolTip = snapshot.capabilities["desktop_handoff"]?.reason
        applyFilter(preserveScroll: preserveScroll)
        feedback.stringValue = warnings.isEmpty ? (backgroundEnabled ? "自动任务开启 · 关闭窗口后继续运行" : "自动任务关闭 · 关闭窗口即退出") : "\(warnings.count) 条读取提示，可在「接入说明」中查看。"
        layoutDocument()
    }

    func updateWarmupUI() {
        let enabled = warmupReport?.enabled == true
        warmup.state = enabled ? .on : .off
        warmup.isEnabled = !warmupControlBusy && (enabled || warmupReport?.available == true) && fixture == nil
        warmupCheck.isEnabled = enabled && !warmupChecking && !warmupControlBusy && fixture == nil
        warmupCheck.title = warmupChecking ? "检查中…" : "检查一次"
        scheduleLabel.stringValue = "时段：" + (warmupReport?.schedule?.caption ?? "全天允许 · UTC+8")
        scheduleLabel.toolTip = scheduleLabel.stringValue
        scheduleButton.isEnabled = !warmupControlBusy && fixture == nil
        warmupStatus.stringValue = (warmupReport?.summary ?? "预热状态不可用") + (enabled ? "\n每分钟检查 · 关闭窗口后继续运行" : "\n开启后会消耗少量额度，仅在需要时发出请求。")
        warmupStatus.toolTip = warmupReport?.summary
        if !warmupControlBusy { feedback.stringValue = backgroundEnabled ? "自动任务开启 · 关闭窗口后继续运行" : "自动任务关闭 · 关闭窗口即退出" }
        if backgroundEnabled && warmupMenuItem == nil {
            let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
            item.button?.image = NSImage(systemSymbolName: "sun.max", accessibilityDescription: "Codex 小助手后台运行")
            let menu = NSMenu(); menu.autoenablesItems = false
            menu.addItem(withTitle: "打开 Codex 小助手", action: #selector(showHelperWindow), keyEquivalent: "").target = self
            let warmupStop = menu.addItem(withTitle: "关闭自动预热", action: #selector(toggleWarmup), keyEquivalent: ""); warmupStop.target = self; warmupStopItem = warmupStop
            let resumeStop = menu.addItem(withTitle: "关闭会话续跑", action: #selector(stopResumeFromMenu), keyEquivalent: ""); resumeStop.target = self; resumeStopItem = resumeStop
            let handoffStop = menu.addItem(withTitle: "关闭自动切号", action: #selector(stopHandoff), keyEquivalent: ""); handoffStop.target = self; handoffStopItem = handoffStop
            menu.addItem(NSMenuItem.separator())
            menu.addItem(withTitle: "退出小助手", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
            item.menu = menu; warmupMenuItem = item
        } else if !backgroundEnabled, let item = warmupMenuItem {
            NSStatusBar.system.removeStatusItem(item); warmupMenuItem = nil; warmupStopItem = nil; resumeStopItem = nil; handoffStopItem = nil
        }
        warmupStopItem?.isEnabled = enabled && !warmupControlBusy
        resumeStopItem?.isEnabled = resumeReport?.enabled == true && !resumeBusy
        handoffStopItem?.isEnabled = (handoffReport?.hasAuthorization == true || handoffPendingKey != nil) && !handoffBusy
        layoutDocument()
    }

    func scheduleTime(_ picker: NSDatePicker) -> String {
        let formatter = DateFormatter(); formatter.timeZone = TimeZone(identifier: "Asia/Singapore")
        formatter.locale = Locale(identifier: "en_US_POSIX"); formatter.dateFormat = "HH:mm"
        return formatter.string(from: picker.dateValue)
    }

    @objc func scheduleEditorChanged() {
        guard let (limit, start, end, hint, save) = scheduleEditor else { return }
        let enabled = limit.state == .on
        start.isEnabled = enabled; end.isEnabled = enabled
        let a = scheduleTime(start), b = scheduleTime(end)
        save.isEnabled = !enabled || a != b
        if enabled && a == b { hint.stringValue = "开始和结束不能相同；全天允许请取消勾选。" }
        else if enabled && b < a { hint.stringValue = "跨午夜：每天 \(a) 至次日 \(b)（UTC+8）。" }
        else { hint.stringValue = "仅限制新预热请求；已经启动的五小时窗口正常倒计时。" }
    }

    @objc func editWarmupSchedule() {
        guard !warmupControlBusy, fixture == nil else { return }
        let alert = NSAlert(); alert.messageText = "自定义预热时段"
        alert.informativeText = "按 Asia/Singapore（UTC+8）每天重复。时段外不发送新预热请求，进入时段后重新检查，不补发。"
        alert.addButton(withTitle: "保存"); alert.addButton(withTitle: "取消")
        let limit = NSButton(checkboxWithTitle: "仅在指定时段允许预热", target: self, action: #selector(scheduleEditorChanged))
        limit.state = warmupReport?.schedule?.enabled == true ? .on : .off
        let formatter = DateFormatter(); formatter.timeZone = TimeZone(identifier: "Asia/Singapore")
        formatter.locale = Locale(identifier: "en_US_POSIX"); formatter.dateFormat = "yyyy-MM-dd HH:mm"
        func picker(_ text: String) -> NSDatePicker {
            let picker = NSDatePicker(); picker.datePickerStyle = .textFieldAndStepper
            picker.datePickerElements = [.hourMinute]; picker.locale = Locale(identifier: "zh_CN")
            picker.timeZone = TimeZone(identifier: "Asia/Singapore"); picker.dateValue = formatter.date(from: "2026-01-01 " + text) ?? Date()
            picker.target = self; picker.action = #selector(scheduleEditorChanged)
            picker.widthAnchor.constraint(equalToConstant: 112).isActive = true
            return picker
        }
        let start = picker(warmupReport?.schedule?.start ?? "08:00"), end = picker(warmupReport?.schedule?.end ?? "23:00")
        start.setAccessibilityLabel("预热开始时间"); end.setAccessibilityLabel("预热结束时间")
        let hint = label("", size: 11)
        let stack = NSStackView(views: [limit, row([label("开始", color: .labelColor), start, label("结束", color: .labelColor), end]), hint])
        stack.orientation = .vertical; stack.alignment = .leading; stack.spacing = 14
        stack.setFrameSize(NSSize(width: 420, height: 114)); hint.preferredMaxLayoutWidth = 420
        alert.accessoryView = stack
        scheduleEditor = (limit,start,end,hint,alert.buttons[0]); scheduleEditorChanged()
        alert.beginSheetModal(for: window) { [weak self] response in
            guard let self = self else { return }
            self.scheduleEditor = nil
            if response == .alertFirstButtonReturn { self.saveWarmupSchedule(enabled: limit.state == .on, start: self.scheduleTime(start), end: self.scheduleTime(end)) }
        }
    }

    func saveWarmupSchedule(enabled: Bool, start: String, end: String) {
        warmupControlBusy = true; warmupControlRevision += 1; updateWarmupUI()
        let bridge = Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/codex-helper")
        DispatchQueue.global(qos: .utility).async { [weak self] in
            let report = try? JSONDecoder().decode(WarmupReport.self, from: BridgeClient.read(executable: bridge, arguments: ["warmup","schedule",enabled ? "on" : "off",start,end]))
            DispatchQueue.main.async {
                guard let self = self else { return }
                self.warmupControlBusy = false; self.warmupControlRevision += 1
                if let report = report { self.warmupReport = report }
                self.updateWarmupUI()
                if report == nil || report?.error != nil {
                    let alert = NSAlert(); alert.messageText = "时段未保存"
                    alert.informativeText = report?.error ?? "保存失败，请重试。"
                    alert.beginSheetModal(for: self.window)
                } else {
                    self.lastWarmupCheck = .distantPast; self.runWarmupCheckIfDue()
                }
            }
        }
    }

    @objc func toggleWarmup() {
        guard !warmupControlBusy, fixture == nil else { return }
        let command = warmupReport?.enabled == true ? "disable" : "enable"
        warmupControlBusy = true; warmupControlRevision += 1; updateWarmupUI()
        let bridge = Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/codex-helper")
        DispatchQueue.global(qos: .utility).async { [weak self] in
            let report = try? JSONDecoder().decode(WarmupReport.self, from: BridgeClient.read(executable: bridge, arguments: ["warmup",command]))
            DispatchQueue.main.async {
                guard let self = self else { return }
                self.warmupControlBusy = false; self.warmupControlRevision += 1
                if let report = report { self.warmupReport = report }
                else { self.feedback.stringValue = "预热设置失败，请重试。" }
                self.updateWarmupUI()
                if command == "enable", report?.enabled == true { self.lastWarmupCheck = .distantPast; self.runWarmupCheckIfDue() }
            }
        }
    }

    @objc func checkWarmupNow() { lastWarmupCheck = .distantPast; runWarmupCheckIfDue() }
    func runWarmupCheckIfDue() {
        guard fixture == nil, warmupReport?.enabled == true, !warmupChecking, !shuttingDown,
              Date().timeIntervalSince(lastWarmupCheck) >= 60 else { return }
        warmupChecking = true; lastWarmupCheck = Date(); updateWarmupUI()
        let revision = warmupControlRevision
        let bridge = Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/codex-helper")
        DispatchQueue.global(qos: .utility).async { [weak self] in
            let report = try? JSONDecoder().decode(WarmupReport.self, from: BridgeClient.read(executable: bridge, arguments: ["warmup","check"], timeout: 220, maximumBytes: 1 << 20, onStart: { process in
                DispatchQueue.main.sync {
                    if self?.shuttingDown == true { process.terminate() }
                    else { self?.warmupProcess = process }
                }
            }))
            DispatchQueue.main.async {
                guard let self = self else { return }
                self.warmupProcess = nil; self.warmupChecking = false
                if let report = report, self.warmupControlRevision == revision, !self.warmupControlBusy { self.warmupReport = report }
                else { self.feedback.stringValue = "预热检查未完成，已登记的请求不会盲目重试。" }
                self.updateWarmupUI()
            }
        }
    }

    @objc func filterChanged() { applyFilter(preserveScroll: false) }

    func quotaTime(_ iso: String) -> String {
        let parser = ISO8601DateFormatter(); parser.formatOptions = [.withInternetDateTime,.withFractionalSeconds]
        guard let date = parser.date(from: iso) ?? ISO8601DateFormatter().date(from: iso) else { return "时间未知" }
        return DateFormatter.localizedString(from: date, dateStyle: .none, timeStyle: .short)
    }

    @objc func showAccountQuotaDetails(_ sender: NSButton) {
        guard let snapshot = snapshot, snapshot.accounts.indices.contains(sender.tag) else { return }
        let account = snapshot.accounts[sender.tag]
        let alert = NSAlert(); alert.messageText = "账号额度详情"
        var lines = [account.name]
        if !account.provider_names.isEmpty { lines += ["供应商：" + account.provider_names.joined(separator: "、")] }
        if let quota = account.quota {
            lines += ["", quota.summary, "", "查询时间：" + quotaTime(quota.queried_at)]
            if !quota.credential_sources.isEmpty { lines += ["凭证来源：" + quota.credential_sources.joined(separator: " / ")] }
        } else { lines += ["", "尚未查询额度，点刷新读取。"] }
        lines += ["", "账号选择来自 CC Switch 配置。磁盘凭证归属不代表运行实例身份。"]
        alert.informativeText = lines.joined(separator: "\n")
        alert.addButton(withTitle: "知道了"); alert.beginSheetModal(for: window)
    }

    @objc func changeSource() {
        let scope = sourceTabs.selectedSegment == 1 ? "cli" : "desktop"
        guard scope != state.activeScope else { return }
        stopResumeForTargetChange()
        stopHandoffForTargetChange()
        state.changeScope(scope); observedRevision = nil; observationMessage = ""
        if let snapshot = snapshot { state.reconcile(snapshot.sessions(in: scope), complete: snapshot.complete(in: scope)) }
        render(preserveScroll: false); save()
    }

    func rowKey(_ row: Int) -> String? {
        guard row >= 0, row < table.numberOfRows else { return nil }
        if let item = table.item(atRow: row) as? ProjectItem { return "project:" + item.group.key }
        if let item = table.item(atRow: row) as? SessionItem { return "session:" + item.session.key }
        return nil
    }

    func rowForKey(_ key: String) -> Int? {
        if key.hasPrefix("project:"), let item = projectItems[String(key.dropFirst(8))] {
            let row = table.row(forItem: item); return row >= 0 ? row : nil
        }
        if key.hasPrefix("session:"), let item = sessionItems[String(key.dropFirst(8))] {
            let row = table.row(forItem: item); return row >= 0 ? row : nil
        }
        return nil
    }

    func applyFilter(preserveScroll: Bool) {
        let oldOrigin = table.enclosingScrollView?.contentView.bounds.origin ?? .zero
        let firstRow = table.row(at: NSPoint(x: 0, y: max(0,oldOrigin.y)))
        let anchor = preserveScroll ? rowKey(firstRow) : nil
        let offset = anchor == nil ? 0 : oldOrigin.y - table.rect(ofRow: firstRow).minY
        visibleSessions = filteredSessions(snapshot?.sessions(in: state.activeScope) ?? [], search: search.stringValue, includeArchived: archived.state == .on)
        let groups = groupedSessions(visibleSessions)
        let searching = !search.stringValue.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
        if !state.layoutInitialized(in: state.activeScope) && !groups.isEmpty && !searching {
            let initial = groups.first(where: { $0.sessions.contains(where: { $0.key == state.selectedKey }) }) ?? groups[0]
            if !state.expandedProjects.contains(initial.key) { state.expandedProjects.append(initial.key) }
            state.initializeLayout(in: state.activeScope)
        }
        rendering = true
        var nextProjects: [String: ProjectItem] = [:]
        var nextSessions: [String: SessionItem] = [:]
        projects = groups.map { group in
            let item = projectItems[group.key] ?? ProjectItem(group); item.group = group
            item.children = group.sessions.map { session in
                let child = sessionItems[session.key] ?? SessionItem(session); child.session = session
                nextSessions[session.key] = child; return child
            }
            nextProjects[group.key] = item; return item
        }
        projectItems = nextProjects; sessionItems = nextSessions
        table.reloadData()
        for item in projects {
            if searching || state.expandedProjects.contains(item.group.key) { table.expandItem(item) }
            else { table.collapseItem(item) }
        }
        if let key = state.selectedKey, let item = sessionItems[key], table.row(forItem: item) >= 0 {
            table.selectRowIndexes(IndexSet(integer: table.row(forItem: item)), byExtendingSelection: false)
        } else { table.deselectAll(nil) }
        rendering = false
        if let clip = table.enclosingScrollView?.contentView {
            let index = anchor.flatMap { rowForKey($0) }
            let y = index.map { table.rect(ofRow: $0).minY + offset } ?? (preserveScroll ? oldOrigin.y : -1000000)
            let target = clip.constrainBoundsRect(NSRect(origin: NSPoint(x: oldOrigin.x,y:y), size: clip.bounds.size)).origin
            clip.scroll(to: target); table.enclosingScrollView?.reflectScrolledClipView(clip)
        }
        let total = snapshot?.sessions(in: state.activeScope).count ?? 0
        countLabel.stringValue = "\(groups.count) 个项目 · \(visibleSessions.count) / \(total) 个会话"
        emptyLabel.isHidden = !visibleSessions.isEmpty
        if snapshot == nil { emptyLabel.stringValue = readError ?? "正在读取会话…" }
        else if total == 0 { emptyLabel.stringValue = "没有读取到会话，可检查 Codex 数据目录后刷新。" }
        else { emptyLabel.stringValue = "没有匹配的会话，可清除搜索或勾选「包括归档」。" }
        updateSelection(); layoutDocument()
    }

    func controlTextDidChange(_ obj: Notification) { filterChanged() }
    func outlineView(_ outlineView: NSOutlineView, numberOfChildrenOfItem item: Any?) -> Int {
        (item as? ProjectItem)?.children.count ?? (item == nil ? projects.count : 0)
    }
    func outlineView(_ outlineView: NSOutlineView, child index: Int, ofItem item: Any?) -> Any {
        if let project = item as? ProjectItem { return project.children[index] }
        return projects[index]
    }
    func outlineView(_ outlineView: NSOutlineView, isItemExpandable item: Any) -> Bool { item is ProjectItem }
    func outlineView(_ outlineView: NSOutlineView, shouldSelectItem item: Any) -> Bool { item is SessionItem }
    func outlineView(_ outlineView: NSOutlineView, isGroupItem item: Any) -> Bool { item is ProjectItem }

    func outlineView(_ outlineView: NSOutlineView, viewFor tableColumn: NSTableColumn?, item: Any) -> NSView? {
        let text: String
        let tooltip: String
        let isProject = item is ProjectItem
        if let project = item as? ProjectItem {
            text = tableColumn?.identifier.rawValue == "time" ? activityTime(project.group.latestActivity) : "\(project.group.title)（\(project.children.count)）"
            tooltip = project.group.path.isEmpty ? "没有项目目录的会话" : project.group.path
        } else if let child = item as? SessionItem {
            let session = child.session
            text = tableColumn?.identifier.rawValue == "time" ? activityTime(session.updated_at) : (session.archived ? "[归档] " : "") + session.title
            tooltip = "\(session.title)\n\(session.project)\n\(session.id)"
        } else { return nil }
        let field = NSTextField(labelWithString: text)
        field.font = .systemFont(ofSize: 12, weight: isProject ? .semibold : .regular)
        field.lineBreakMode = .byTruncatingTail; field.maximumNumberOfLines = 1
        let cell = NSTableCellView(); cell.addSubview(field); field.translatesAutoresizingMaskIntoConstraints = false
        NSLayoutConstraint.activate([field.leadingAnchor.constraint(equalTo: cell.leadingAnchor, constant: 5),
                                     field.trailingAnchor.constraint(equalTo: cell.trailingAnchor, constant: -5),
                                     field.centerYAnchor.constraint(equalTo: cell.centerYAnchor)])
        cell.textField = field; cell.toolTip = tooltip
        return cell
    }

    func outlineViewSelectionDidChange(_ notification: Notification) {
        guard !rendering else { return }
        let index = table.selectedRow
        if index >= 0, let item = table.item(atRow: index) as? SessionItem {
            let session = item.session
            if session.key != state.selectedKey { stopResumeForTargetChange(); stopHandoffForTargetChange(); observedRevision = nil; observationMessage = "" }
            state.select(session.key)
            let key = projectKey(session.project, scope: session.sessionScope)
            if !state.expandedProjects.contains(key) { state.expandedProjects.append(key) }
        }
        // A disclosure collapse may deselect a child. Only the explicit clear
        // button removes the managed binding; folding never pauses observation.
        updateSelection(); layoutDocument(); save()
    }

    func outlineViewItemDidExpand(_ notification: Notification) { projectDisclosure(notification, expanded: true) }
    func outlineViewItemDidCollapse(_ notification: Notification) { projectDisclosure(notification, expanded: false) }
    func projectDisclosure(_ notification: Notification, expanded: Bool) {
        guard !rendering, let item = notification.userInfo?["NSObject"] as? ProjectItem else { return }
        var keys = Set(state.expandedProjects)
        if expanded { keys.insert(item.group.key) } else { keys.remove(item.group.key) }
        state.expandedProjects = keys.sorted()
        updateSelection(); layoutDocument(); save()
    }

    var selected: HelperSession? { snapshot?.sessions(in: state.activeScope).first(where: { $0.key == state.selectedKey }) }

    func updateSelection() {
        updateResumeUI()
        updateHandoffUI()
        clearSelection.isEnabled = state.selectedKey != nil
        selectionHint.isHidden = true
        if let session = selected {
            selection.stringValue = session.title
            let observation = state.observing ? "观察中 · \(observationMessage)" : "未观察"
            detail.stringValue = "\(session.source) · ID \(session.id.prefix(8)) · \(observation)\n最近记录 \(activityTime(session.updated_at)) · \(session.project)"
            observe.isEnabled = !session.archived && snapshot?.complete(in: state.activeScope) == true
            if session.archived {
                selectionHint.stringValue = "归档会话仅供查看，不能开启观察。"; selectionHint.isHidden = false
            } else if snapshot?.complete(in: state.activeScope) != true {
                selectionHint.stringValue = "当前读取结果不完整，观察已暂停。"; selectionHint.isHidden = false
            } else if !visibleSessions.contains(where: { $0.key == session.key }) {
                selectionHint.stringValue = "当前选择不在筛选结果中；观察对象未改变。"; selectionHint.isHidden = false
            } else if let item = sessionItems[session.key], table.row(forItem: item) < 0 {
                selectionHint.stringValue = "当前会话所在项目已收起；观察对象未改变。"; selectionHint.isHidden = false
            }
        } else {
            selection.stringValue = state.selectedKey != nil ? "已保留该端上次会话选择" : "选择一个会话，观察其记录更新。"
            detail.stringValue = snapshot == nil || state.selectedKey != nil ? "该端数据恢复后可重新开启观察。" : "选择会话不会开启自动切号。"
            observe.isEnabled = false
        }
        observe.title = state.observing ? "停止观察" : "开始观察"
        observe.toolTip = "只刷新本机记录；不会暂停、切换账号或发送继续消息。"
    }

    @objc func toggleObservation() {
        guard let session = selected, !session.archived, snapshot?.complete(in: state.activeScope) == true else { return }
        state.toggleObservation(); save()
        observedRevision = state.observing ? session.updated_at : nil
        observationMessage = state.observing ? "等待新记录" : ""
        updateSelection(); layoutDocument()
    }

    @objc func clearTarget() {
        stopResumeForTargetChange()
        stopHandoffForTargetChange()
        state.select(nil); observedRevision = nil; observationMessage = ""
        rendering = true; table.deselectAll(nil); rendering = false
        updateSelection(); layoutDocument(); save()
    }

    func activityTime(_ unix: Int64) -> String {
        let formatter = DateFormatter(); formatter.dateFormat = "MM/dd HH:mm"
        return formatter.string(from: Date(timeIntervalSince1970: Double(unix)))
    }

    func updateObservation() {
        guard state.observing, let session = selected else {
            observedRevision = nil; observationMessage = ""; return
        }
        if let previous = observedRevision, session.updated_at != previous {
            observationMessage = "发现记录更新 \(activityTime(session.updated_at))"
        } else if observationMessage.isEmpty { observationMessage = "等待新记录" }
        observedRevision = session.updated_at
    }

    func save() {
        do { try store.save(state) }
        catch { feedback.stringValue = "选择状态保存失败；本次窗口内仍可使用。" }
    }
}

extension Assistant {
    func updateLoginItem() {
        guard fixture == nil, ProcessInfo.processInfo.environment["CODEX_HELPER_TEST_HOME"] == nil else {
            loginItem.isEnabled = false; loginStatus.stringValue = "隔离测试不修改登录项"; return
        }
        if #available(macOS 13.0, *) {
            switch SMAppService.mainApp.status {
            case .enabled: loginItem.state = .on; loginStatus.stringValue = "已启用"
            case .requiresApproval: loginItem.state = .mixed; loginStatus.stringValue = "请在系统登录项中允许"
            default: loginItem.state = .off; loginStatus.stringValue = ""
            }
        } else { loginItem.isEnabled = false; loginStatus.stringValue = "需要 macOS 13 或更新版本" }
    }
    @objc func toggleLoginItem() {
        guard fixture == nil, ProcessInfo.processInfo.environment["CODEX_HELPER_TEST_HOME"] == nil else { return }
        if #available(macOS 13.0, *) {
            do {
                if SMAppService.mainApp.status == .enabled || SMAppService.mainApp.status == .requiresApproval {
                    try SMAppService.mainApp.unregister()
                } else { try SMAppService.mainApp.register() }
                updateLoginItem()
                if SMAppService.mainApp.status == .requiresApproval { SMAppService.openSystemSettingsLoginItems() }
            } catch { updateLoginItem(); loginStatus.stringValue = "设置失败，请在系统登录项中检查" }
        }
    }
    func updateResumeUI() {
        let matching = resumeReport?.enabled == true && resumeReport?.key == state.selectedKey
        let handoffActive = handoffReport?.hasAuthorization == true || handoffPendingKey != nil || handoffBusy
        resumeToggle.state = matching ? .on : .off
        resumeToggle.isEnabled = !handoffActive && !resumeBusy && fixture == nil && selected?.archived == false && snapshot?.complete(in: state.activeScope) == true
        cliResume.isHidden = state.activeScope != "cli"
        cliResume.isEnabled = !handoffActive && fixture == nil && !resumeBusy && selected?.archived == false && snapshot?.complete(in: "cli") == true
        resumeStatus.stringValue = state.activeScope == "cli" && !matching ? "首次请点“在终端托管续跑”；若该会话已在别处运行，请先退出原终端中的会话。" : (resumeReport?.message ?? "手动开启后，只在明确触限且额度恢复时继续原会话。")
        resumeStatus.toolTip = resumeStatus.stringValue
        handoffPreflight.isEnabled = !handoffChecking && fixture == nil && selected?.archived == false && snapshot?.complete(in: state.activeScope) == true
        if handoffCheckedKey != nil && handoffCheckedKey != state.selectedKey {
            handoffCheckedKey = nil
            handoffStatus.stringValue = "选择会话后可以检查当前交接条件。"
        }
    }
    @objc func checkHandoffPreflight() {
        guard !handoffChecking, let target = selected, !target.archived, fixture == nil else { return }
        guard let threshold = Double(handoffThreshold.stringValue.trimmingCharacters(in: .whitespacesAndNewlines)), threshold.isFinite, threshold > 0, threshold < 100 else {
            handoffStatus.stringValue = "请输入大于 0、小于 100 的剩余额度阈值。"; return
        }
        handoffChecking = true; handoffCheckedKey = target.key; updateResumeUI()
        handoffStatus.stringValue = "正在只读检查账号、额度及会话…"
        let bridge = Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/codex-helper")
        DispatchQueue.global(qos: .utility).async { [weak self] in
            let report = try? JSONDecoder().decode(HandoffPreflightReport.self, from: BridgeClient.read(executable: bridge, arguments: ["handoff-preflight",target.sessionScope,target.id,String(threshold)], timeout: 25))
            DispatchQueue.main.async {
                guard let self = self else { return }
                self.handoffChecking = false
                if self.state.selectedKey == target.key {
                    self.handoffStatus.stringValue = report?.summary ?? "检查未完成；自动切号仍不可用。"
                }
                self.updateResumeUI(); self.layoutDocument()
            }
        }
    }
    func updateHandoffUI() {
        let matches = handoffReport?.hasAuthorization == true && handoffReport?.scope == state.activeScope && handoffReport?.session_id == selected?.id
        let active = handoffReport?.hasAuthorization == true || handoffPendingKey != nil
        let needsReview = handoffReport?.stage == "uncertain" || (handoffReport?.stage == "completed" && handoffReport?.recovery_pending == true)
        handoffStart.isHidden = false
        handoffStart.title = state.activeScope == "desktop" ? "为选中会话开启切号" : "托管并开启切号"
        handoffStart.toolTip = state.activeScope == "desktop" ? "触发时会退出整个 Desktop；其他会话和未提交的输入可能受影响。" : "在托管终端中使用所选会话。"
        handoffStop.isHidden = !active
        handoffAcknowledge.isHidden = !needsReview || active
        handoffAcknowledge.title = handoffReport?.recovery_pending == true ? "保留当前账号并解除暂停" : "已检查，解除暂停"
        handoffAcknowledge.toolTip = handoffReport?.recovery_pending == true ? (handoffReport?.scope == "desktop" ? "先停止交接并核查 Desktop 会话及当前账号；通过认证检查后保留当前认证并清理旧恢复副本。" : "先关闭托管终端并核查会话。通过认证检查后保留当前认证并清理旧恢复副本。") : "请先核查原会话状态。"
        let available = state.activeScope == "desktop" ? snapshot?.capabilities["desktop_handoff"]?.available == true : cliHandoffAvailable
        handoffStart.isEnabled = available && fixture == nil && !needsReview && !active && !handoffBusy && !resumeBusy && resumeReport?.enabled != true && selected?.archived == false && snapshot?.complete(in: state.activeScope) == true
        handoffStop.isEnabled = !handoffBusy
        handoffAcknowledge.isEnabled = !handoffBusy && fixture == nil
        handoffThreshold.isEnabled = !active && !handoffBusy
        if handoffPendingKey != nil { handoffRunStatus.stringValue = state.activeScope == "desktop" ? "正在启动 Desktop 会话监控。" : "正在打开托管终端；请在该终端使用所选会话。" }
        else if matches { handoffRunStatus.stringValue = handoffReport?.message ?? "正在监控所选会话。" }
        else if needsReview || handoffReport?.stage == "error" { handoffRunStatus.stringValue = handoffReport?.recovery_pending == true ? (handoffReport?.scope == "desktop" ? "请停止交接并核查 Desktop 会话与账号，再确认保留当前认证。" : "请关闭托管终端并核查账号。确认后可保留当前认证并解除暂停。") : (handoffReport?.message ?? "请检查原会话状态。") }
        else if state.activeScope == "desktop" { handoffRunStatus.stringValue = available ? "仅监控选中会话；触发时会退出整个 Desktop，其他会话和未提交输入可能受影响。" : "Desktop 切号代码已接入，完整实机交接尚待验收。" }
        else if resumeReport?.enabled == true { handoffRunStatus.stringValue = "请先关闭“不切号续跑”，再开启托管切号。" }
        else if !cliHandoffAvailable { handoffRunStatus.stringValue = "CLI 切号的真实服务验收尚未通过，暂不可开启。可先检查账号和额度。" }
        else { handoffRunStatus.stringValue = "先退出其他终端中的同一会话，再托管并开启切号。" }
        handoffRunStatus.toolTip = handoffRunStatus.stringValue
        updateResumeUI()
    }
    @objc func startCLIHandoff() {
        if state.activeScope == "desktop" { startDesktopHandoff(); return }
        guard cliHandoffAvailable, !handoffBusy, !resumeBusy, resumeReport?.enabled != true, handoffPendingKey == nil, handoffReport?.hasAuthorization != true, let target = selected, target.sessionScope == "cli", !target.archived, fixture == nil else { return }
        guard let threshold = Double(handoffThreshold.stringValue.trimmingCharacters(in: .whitespacesAndNewlines)), threshold.isFinite, threshold > 0, threshold < 100 else {
            handoffRunStatus.stringValue = "请输入大于 0、小于 100 的剩余额度阈值。"; return
        }
        handoffBusy = true; handoffRevision += 1; handoffPendingKey = target.key; handoffCancelPending = false; updateHandoffUI(); updateWarmupUI()
        let bridge = Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/codex-helper")
        DispatchQueue.global(qos: .utility).async { [weak self] in
            let report = try? JSONDecoder().decode(HandoffReport.self, from: BridgeClient.read(executable: bridge, arguments: ["handoff","begin",target.id,String(threshold)], timeout: 25))
            DispatchQueue.main.async {
                guard let self = self else { return }
                self.handoffBusy = false; self.handoffRevision += 1
                guard let report = report, report.enabled, let authorization = report.authorization_id, !authorization.isEmpty else {
                    self.handoffPendingKey = nil; self.handoffLaunchDeadline = nil; self.updateHandoffUI(); self.updateWarmupUI()
                    self.handoffRunStatus.stringValue = report?.message ?? "未能准备自动切号，请重试。"; return
                }
                self.handoffReport = report
                if self.handoffCancelPending || self.state.selectedKey != target.key || self.state.activeScope != "cli" {
                    self.stopHandoff(); return
                }
                func quote(_ value: String) -> String { "'" + value.replacingOccurrences(of: "'", with: "'\\''") + "'" }
                do {
                    let directory = FileManager.default.temporaryDirectory.appendingPathComponent("codex-helper-launch-" + UUID().uuidString)
                    try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
                    let file = directory.appendingPathComponent("Codex 自动切号.command")
                    let script = "#!/bin/zsh\n" + quote(bridge.path) + " handoff cli " + quote(target.id) + " " + quote(String(threshold)) + " " + quote(authorization) + "\n"
                    try script.write(to: file, atomically: true, encoding: .utf8)
                    try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: file.path)
                    if !NSWorkspace.shared.open(file) { self.stopHandoff(); self.handoffRunStatus.stringValue = "终端未打开，请重试。" }
                    else {
                        self.handoffLaunchDeadline = Date().addingTimeInterval(20)
                        self.updateHandoffUI(); self.updateWarmupUI()
                        DispatchQueue.main.asyncAfter(deadline: .now() + 3) { self.refreshNow() }
                        DispatchQueue.main.asyncAfter(deadline: .now() + 21) { self.refreshData(forceQuota: false) }
                    }
                } catch { self.stopHandoff(); self.handoffRunStatus.stringValue = "无法创建托管终端入口。" }
            }
        }
    }
    func startDesktopHandoff() {
        guard !handoffBusy, !resumeBusy, resumeReport?.enabled != true, handoffPendingKey == nil, handoffReport?.hasAuthorization != true, let target = selected, target.sessionScope == "desktop", !target.archived, fixture == nil else { return }
        guard let threshold = Double(handoffThreshold.stringValue.trimmingCharacters(in: .whitespacesAndNewlines)), threshold.isFinite, threshold > 0, threshold < 100 else {
            handoffRunStatus.stringValue = "请输入大于 0、小于 100 的剩余额度阈值。"; return
        }
        handoffBusy = true; handoffRevision += 1; handoffPendingKey = target.key; handoffCancelPending = false; updateHandoffUI(); updateWarmupUI()
        let bridge = Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/codex-helper")
        DispatchQueue.global(qos: .utility).async { [weak self] in
            let report = try? JSONDecoder().decode(HandoffReport.self, from: BridgeClient.read(executable: bridge, arguments: ["handoff","desktop-begin",target.id,String(threshold)], timeout: 25))
            DispatchQueue.main.async {
                guard let self = self else { return }
                self.handoffBusy = false; self.handoffRevision += 1
                guard let report = report, report.enabled, let authorization = report.authorization_id, !authorization.isEmpty else {
                    self.handoffPendingKey = nil; self.handoffLaunchDeadline = nil; self.updateHandoffUI(); self.updateWarmupUI()
                    self.handoffRunStatus.stringValue = report?.message ?? "未能准备 Desktop 自动切号。"; return
                }
                self.handoffReport = report
                if self.handoffCancelPending || self.state.selectedKey != target.key || self.state.activeScope != "desktop" { self.stopHandoff(); return }
                let process = Process(); process.executableURL = bridge
                process.arguments = ["handoff","desktop-run",target.id,String(threshold),authorization]
                process.standardInput = FileHandle.nullDevice; process.standardOutput = FileHandle.nullDevice; process.standardError = FileHandle.nullDevice
                process.terminationHandler = { [weak self] _ in DispatchQueue.main.async {
                    guard let self = self else { return }
                    self.desktopHandoffProcess = nil; self.handoffPendingKey = nil; self.handoffLaunchDeadline = nil
                    self.refreshData(forceQuota: false)
                } }
                do {
                    try process.run(); self.desktopHandoffProcess = process
                    self.handoffLaunchDeadline = Date().addingTimeInterval(20); self.updateHandoffUI(); self.updateWarmupUI()
                    DispatchQueue.main.asyncAfter(deadline: .now() + 3) { self.refreshNow() }
                    DispatchQueue.main.asyncAfter(deadline: .now() + 21) { self.refreshData(forceQuota: false) }
                } catch { self.stopHandoff(); self.handoffRunStatus.stringValue = "无法启动 Desktop 监控。" }
            }
        }
    }
    func stopHandoffForTargetChange() {
        guard handoffReport?.hasAuthorization == true || handoffPendingKey != nil else { return }
        if handoffBusy { handoffCancelPending = true; return }
        stopHandoff()
    }
    @objc func stopHandoff() {
        if handoffBusy { handoffCancelPending = true; return }
        handoffBusy = true; handoffRevision += 1; updateHandoffUI()
        let bridge = Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/codex-helper")
        DispatchQueue.global(qos: .utility).async { [weak self] in
            let report = try? JSONDecoder().decode(HandoffReport.self, from: BridgeClient.read(executable: bridge, arguments: ["handoff","disable"], timeout: 5))
            DispatchQueue.main.async {
                guard let self = self else { return }
                self.handoffBusy = false; self.handoffRevision += 1; self.handoffPendingKey = nil; self.handoffLaunchDeadline = nil; self.handoffCancelPending = false
                if let report = report { self.handoffReport = report }
                self.updateHandoffUI(); self.updateWarmupUI()
                if report == nil { self.handoffRunStatus.stringValue = "停止请求未确认，请重试。" }
                else if report?.enabled == true { self.handoffRunStatus.stringValue = report?.message ?? "停止请求已登记。" }
            }
        }
    }
    @objc func acknowledgeHandoff() {
        guard !handoffBusy, fixture == nil, handoffReport?.stage == "uncertain" || (handoffReport?.stage == "completed" && handoffReport?.recovery_pending == true) else { return }
        handoffBusy = true; handoffRevision += 1; updateHandoffUI()
        let bridge = Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/codex-helper")
        DispatchQueue.global(qos: .utility).async { [weak self] in
            let report = try? JSONDecoder().decode(HandoffReport.self, from: BridgeClient.read(executable: bridge, arguments: ["handoff","acknowledge"], timeout: 20))
            DispatchQueue.main.async {
                guard let self = self else { return }
                self.handoffBusy = false; self.handoffRevision += 1
                if let report = report { self.handoffReport = report }
                self.updateHandoffUI(); self.updateWarmupUI()
                self.handoffRunStatus.stringValue = report?.message ?? "状态未确认，请重试。"
            }
        }
    }
    @objc func openManagedCLI() {
        guard handoffReport?.hasAuthorization != true, handoffPendingKey == nil, !handoffBusy, let target = selected, target.sessionScope == "cli", !target.archived, fixture == nil else { return }
        func quote(_ value: String) -> String { "'" + value.replacingOccurrences(of: "'", with: "'\\''") + "'" }
        do {
            let directory = FileManager.default.temporaryDirectory.appendingPathComponent("codex-helper-launch-" + UUID().uuidString)
            try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            let file = directory.appendingPathComponent("Codex 会话续跑.command")
            let bridge = Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/codex-helper").path
            let script = "#!/bin/zsh\n" + quote(bridge) + " resume cli " + quote(target.id) + "\n"
            try script.write(to: file, atomically: true, encoding: .utf8)
            try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: file.path)
            if NSWorkspace.shared.open(file) { resumeStatus.stringValue = "正在打开原 CLI 会话的托管终端…" }
            else { resumeStatus.stringValue = "终端未打开，请重试。" }
        } catch { resumeStatus.stringValue = "无法创建托管终端入口，请重试。" }
    }
    @objc func toggleResume() {
        guard handoffReport?.hasAuthorization != true, handoffPendingKey == nil, !handoffBusy, !resumeBusy, let target = selected, fixture == nil else { return }
        let args = resumeReport?.enabled == true && resumeReport?.key == target.key ? ["resume","disable"] : ["resume","enable",target.sessionScope,target.id]
        resumeControl(args)
    }
    @objc func stopResumeFromMenu() { resumeControl(["resume","disable"]) }
    func stopResumeForTargetChange() {
        guard !resumeBusy, resumeReport?.enabled == true else { return }
        resumeControl(["resume","disable"])
    }
    func resumeControl(_ arguments: [String]) {
        resumeBusy = true; resumeRevision += 1; updateHandoffUI()
        // The policy command is short and independent of a running quota check.
        let bridge = Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/codex-helper")
        DispatchQueue.global(qos: .utility).async { [weak self] in
            let report = try? JSONDecoder().decode(ResumeReport.self, from: BridgeClient.read(executable: bridge, arguments: arguments, timeout: 25))
            DispatchQueue.main.async {
                guard let self = self else { return }
                self.resumeBusy = false; self.resumeRevision += 1
                if let report = report { self.resumeReport = report }
                self.updateHandoffUI(); self.updateWarmupUI()
                if report == nil { self.resumeStatus.stringValue = "续跑设置未确认，请重试。" }
                else if report?.enabled == true && report?.status != "error" { self.lastResumeCheck = .distantPast; self.runResumeCheckIfDue() }
            }
        }
    }
    func runResumeCheckIfDue() {
        guard fixture == nil, !resumeBusy, !resumeChecking, !shuttingDown, resumeReport?.enabled == true,
              Date().timeIntervalSince(lastResumeCheck) >= 30 else { return }
        guard resumeReport?.key == state.selectedKey, selected?.archived == false, snapshot?.complete(in: state.activeScope) == true else {
            stopResumeForTargetChange(); return
        }
        resumeChecking = true; lastResumeCheck = Date()
        let revision = resumeRevision
        let bridge = Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/codex-helper")
        DispatchQueue.global(qos: .utility).async { [weak self] in
            let report = try? JSONDecoder().decode(ResumeReport.self, from: BridgeClient.read(executable: bridge, arguments: ["resume","check"], timeout: 40, onStart: { process in
                DispatchQueue.main.sync {
                    if self?.shuttingDown == true { process.terminate() }
                    else { self?.resumeProcess = process }
                }
            }))
            DispatchQueue.main.async {
                guard let self = self else { return }
                self.resumeProcess = nil; self.resumeChecking = false
                if self.resumeRevision == revision && !self.resumeBusy {
                    if let report = report { self.resumeReport = report }
                    self.updateResumeUI(); self.updateWarmupUI()
                    if report == nil { self.resumeStatus.stringValue = "续跑检查未完成；不会盲目重发，请查看原会话。" }
                }
            }
        }
    }
}

let args = CommandLine.arguments
if CCSwitchControl.handle(arguments: args) { exit(0) }
func argument(_ name: String) -> String? {
    guard let index = args.firstIndex(of: name), index + 1 < args.count else { return nil }
    return args[index + 1]
}
let fixture = (argument("--fixture") ?? ProcessInfo.processInfo.environment["CODEX_HELPER_FIXTURE"]).map { URL(fileURLWithPath: $0) }
let directory = (argument("--state-dir") ?? ProcessInfo.processInfo.environment["CODEX_HELPER_STATE_DIR"]).map { URL(fileURLWithPath: $0) }
    ?? FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Library/Application Support/CodexHelper")
let app = NSApplication.shared
if fixture != nil, ProcessInfo.processInfo.environment["CODEX_HELPER_TEST_APPEARANCE"] == "dark" { app.appearance = NSAppearance(named: .darkAqua) }
app.setActivationPolicy(.regular)
let delegate = Assistant(fixture: fixture, directory: directory)
app.delegate = delegate
app.run()
