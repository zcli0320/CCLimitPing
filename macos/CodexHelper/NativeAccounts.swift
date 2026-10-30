import Cocoa

struct AccountCenterError: Error { let message: String }

enum DesktopAccountOwner: String {
    case native, ccSwitch = "cc_switch", unknown

    init(_ value: Any?) { self = DesktopAccountOwner(rawValue: value as? String ?? "") ?? .unknown }
    var blockedMessage: String {
        self == .ccSwitch ? "Desktop 由 CC Switch 管理，请在 CC Switch 中切换账号。" : "Desktop 管理方无法确认，请检查 CC Switch 设置。"
    }
}

// A file observation is separate from the helper's permission to manage an
// account. In particular, CC Switch can change the file without creating a
// helper binding or proving which account an already-running client uses.
func accountScopePresentation(scope: String, title: String, bindings: [[String: String]], currentScopes: [String], expiredScopes: [String] = [], desktopOwner: DesktopAccountOwner = .native) -> (String, NSColor, String) {
    let state = bindings.first(where: { $0["scope"] == scope })?["state"]
    let current = currentScopes.contains(scope)
    if scope == "desktop" && desktopOwner == .ccSwitch {
        return (title + (current ? " · 当前（CC Switch）" : " · 由 CC Switch 管理"), .systemBlue,
                "Desktop 由 CC Switch 管理；当前账号根据登录文件只读识别，运行中的客户端身份需单独核验。")
    }
    if scope == "desktop" && desktopOwner == .unknown {
        return (title + " · 管理方待核查", .systemOrange, desktopOwner.blockedMessage)
    }
    if current && state == "needs_reapply" {
        return (title + " · 当前，需重新应用", .systemOrange, "登录文件属于此账号；重新登录后仍需重新应用。运行中的客户端身份需单独核验。")
    }
    if current && expiredScopes.contains(scope) {
        return (title + " · 当前，已过期", .systemOrange, "该端文件中的访问凭证已过期；运行中的客户端可能已经更新，请核查。")
    }
    if current {
        let managed = state == "bound"
        return (title + " · 当前", managed ? .systemGreen : .systemBlue,
                managed ? "该端登录文件匹配此账号，小助手绑定也一致；运行中的客户端身份需单独核验。" : "该端登录文件匹配此账号；这是只读识别，不会更改小助手绑定或 CC Switch。")
    }
    if state == "needs_reapply" { return (title + " · 需重新应用", .systemOrange, "重新登录后需重新应用此端。") }
    if state == "bound" { return (title + " · 已绑定", .systemGreen, "小助手保存的绑定；运行中的客户端身份需单独核验。") }
    if state != nil { return (title + " · 待核查", .systemOrange, "小助手绑定与该端文件不一致，请先核查。") }
    return (title + " · 未绑定", .secondaryLabelColor, "小助手未保存此账号的绑定。")
}

/// One private manager process owns all account mutations for this preview.
/// Neither its protocol output nor login URLs are written to logs.
final class AccountCenterClient {
    private let process = Process()
    private let input = Pipe()
    private let output = Pipe()
    private let bufferLock = NSLock()
    private var buffer = Data()
    private var nextID = 0
    private var callbacks: [Int: (Result<[String: Any], AccountCenterError>) -> Void] = [:]
    private(set) var failure: String?
    var isAvailable: Bool { failure == nil && process.isRunning }

    init(executable: URL) {
        process.executableURL = executable
        var environment = ProcessInfo.processInfo.environment
        environment["CODEX_HELPER_ACCOUNT_SERVICE"] = "1"
        process.environment = environment
        process.standardInput = input
        process.standardOutput = output
        process.standardError = FileHandle.nullDevice
        output.fileHandleForReading.readabilityHandler = { [weak self] handle in
            let data = handle.availableData
            if data.isEmpty { return }
            self?.consume(data)
        }
        process.terminationHandler = { [weak self] _ in
            DispatchQueue.main.async { self?.stopped() }
        }
        do { try process.run() }
        catch { failure = "账号管理器无法启动，请检查小助手安装。" }
    }

    private func consume(_ data: Data) {
        bufferLock.lock()
        buffer.append(data)
        if buffer.count > 1024 * 1024 {
            buffer.removeAll()
            bufferLock.unlock()
            DispatchQueue.main.async { [weak self] in self?.stopped() }
            return
        }
        var lines: [Data] = []
        while let index = buffer.firstIndex(of: 10) {
            lines.append(buffer.prefix(upTo: index))
            buffer.removeSubrange(...index)
        }
        bufferLock.unlock()
        for line in lines {
            guard let message = try? JSONSerialization.jsonObject(with: line) as? [String: Any],
                  let id = message["id"] as? Int else { continue }
            DispatchQueue.main.async { [weak self] in
                guard let self = self, let callback = self.callbacks.removeValue(forKey: id) else { return }
                if let error = message["error"] as? String { callback(.failure(AccountCenterError(message: error))) }
                else if let result = message["result"] as? [String: Any] { callback(.success(result)) }
                else { callback(.failure(AccountCenterError(message: "账号管理器返回无效状态。"))) }
            }
        }
    }

    private func stopped() {
        failure = "账号管理器已停止，请关闭授权管理或点刷新后重试。"
        let pending = callbacks.values
        callbacks.removeAll()
        pending.forEach { $0(.failure(AccountCenterError(message: failure!))) }
    }

    func send(_ method: String, fields: [String: String] = [:],
              completion: @escaping (Result<[String: Any], AccountCenterError>) -> Void) {
        guard failure == nil, process.isRunning else {
            completion(.failure(AccountCenterError(message: failure ?? "账号管理器不可用。"))); return
        }
        nextID += 1
        let id = nextID
        var request: [String: Any] = ["id": id, "method": method]
        fields.forEach { request[$0.key] = $0.value }
        guard var data = try? JSONSerialization.data(withJSONObject: request) else {
            completion(.failure(AccountCenterError(message: "账号操作无法编码。"))); return
        }
        data.append(10)
        callbacks[id] = completion
        do { try input.fileHandleForWriting.write(contentsOf: data) }
        catch { callbacks.removeValue(forKey: id); completion(.failure(AccountCenterError(message: "账号管理器连接已关闭。"))) }
    }

    func close() {
        output.fileHandleForReading.readabilityHandler = nil
        try? input.fileHandleForWriting.close()
        if process.isRunning { process.terminate() }
    }
}

final class AccountCenterController: NSObject, NSWindowDelegate {
    private let window: NSWindow
    private let client: AccountCenterClient
    private let ownsClient: Bool
    private let scroll = NSScrollView()
    private let document = FlippedDocument()
    private let contentStack = NSStackView()
    private let listBox = SectionBox(spacing: 12)
    private let listCount = Theme.caption()
    private let flowBanner = SectionBox(spacing: 8)
    private let status = NSTextField(wrappingLabelWithString: "正在读取账号…")
    private let flowStatus = NSTextField(wrappingLabelWithString: "")
    private var flowMessage: String {
        get { flowStatus.stringValue }
        set { flowStatus.stringValue = newValue; flowBanner.isHidden = newValue.isEmpty; layoutContent() }
    }
    private let browserButton = NSButton(title: "浏览器登录", target: nil, action: nil)
    private let deviceButton = NSButton(title: "设备码登录", target: nil, action: nil)
    private let cancelButton = NSButton(title: "取消登录", target: nil, action: nil)
    private let recoverButton = NSButton(title: "核查上次操作", target: nil, action: nil)
    private let clearDesktopButton = NSButton(title: "清除 Desktop 绑定", target: nil, action: nil)
    private var clearDesktopRow: NSStackView!
    private let reopenBrowserButton = NSButton(title: "重新打开浏览器", target: nil, action: nil)
    private let copyLoginButton = NSButton(title: "复制登录地址", target: nil, action: nil)
    private var browserActions: NSStackView!
    private var accountIDs: [String] = []
    private var accountRows: [[String: Any]] = []
    private var quotaText: [String: String] = [:]
    private var refreshGeneration = 0
    private var scopeObservations: [String: String] = [:]
    private var desktopOwner: DesktopAccountOwner = .unknown
    private var currentStateTimer: Timer?
    private var currentStateCheckInFlight = false
    private var currentListFingerprint: Data?
    private var flowID: String?
    private var loginURL: URL?
    private var loginCode = ""
    private var startingLogin = false
    private var polling = false
    private var timer: Timer?
    private let onChanged: () -> Void
    var isVisible: Bool { window.isVisible }

    init(executable: URL, sharedClient: AccountCenterClient? = nil, onChanged: @escaping () -> Void) {
        self.onChanged = onChanged
        self.client = sharedClient ?? AccountCenterClient(executable: executable)
        self.ownsClient = sharedClient == nil
        self.window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 620, height: 540),
            styleMask: [.titled, .closable, .miniaturizable, .resizable], backing: .buffered, defer: false)
        super.init()
        window.title = "授权管理"
        window.minSize = NSSize(width: 540, height: 380)
        window.isReleasedWhenClosed = false
        window.delegate = self
        scroll.hasVerticalScroller = true
        scroll.autohidesScrollers = true
        scroll.drawsBackground = false
        window.contentView = scroll
        document.autoresizingMask = [.width]
        scroll.documentView = document
        let stack = contentStack
        stack.orientation = .vertical; stack.alignment = .leading; stack.spacing = 10; stack.detachesHiddenViews = true
        document.addSubview(stack); stack.translatesAutoresizingMaskIntoConstraints = false
        NSLayoutConstraint.activate([
            stack.topAnchor.constraint(equalTo: document.topAnchor, constant: 18),
            stack.leadingAnchor.constraint(equalTo: document.leadingAnchor, constant: 20),
            stack.trailingAnchor.constraint(equalTo: document.trailingAnchor, constant: -20)
        ])
        func add(_ view: NSView) { stack.addArrangedSubview(view); view.widthAnchor.constraint(equalTo: stack.widthAnchor).isActive = true }

        let title = Theme.text("授权管理", size: 18, weight: .bold, wraps: false)
        let subtitle = Theme.caption("添加、重新登录或移除小助手的账号授权；Desktop 由 CC Switch 管理时请在 CC Switch 中切换。")
        add(Theme.vstack([title, subtitle], spacing: 3))

        browserButton.target = self; browserButton.action = #selector(startBrowser)
        Theme.button(browserButton, primary: true)
        browserButton.image = NSImage(systemSymbolName: "globe", accessibilityDescription: nil); browserButton.imagePosition = .imageLeading
        deviceButton.target = self; deviceButton.action = #selector(startDevice)
        Theme.button(deviceButton)
        deviceButton.image = NSImage(systemSymbolName: "number.square", accessibilityDescription: nil); deviceButton.imagePosition = .imageLeading
        cancelButton.target = self; cancelButton.action = #selector(cancelLogin); cancelButton.isEnabled = false; cancelButton.isHidden = true
        Theme.button(cancelButton)
        recoverButton.target = self; recoverButton.action = #selector(resolvePending); recoverButton.isHidden = true
        Theme.button(recoverButton)
        recoverButton.image = NSImage(systemSymbolName: "exclamationmark.triangle", accessibilityDescription: nil); recoverButton.imagePosition = .imageLeading
        recoverButton.contentTintColor = .systemOrange
        let refresh = Theme.symbolButton("arrow.clockwise", label: "刷新", target: self, action: #selector(refresh))
        let buttons = Theme.hstack([browserButton, deviceButton, cancelButton, Theme.flexible(), recoverButton, refresh], spacing: 8)
        add(buttons)
        stack.setCustomSpacing(8, after: buttons)
        status.textColor = .secondaryLabelColor; status.font = .systemFont(ofSize: 11)
        add(status)
        clearDesktopButton.target = self; clearDesktopButton.action = #selector(clearDesktopBinding)
        Theme.button(clearDesktopButton, small: true)
        clearDesktopButton.toolTip = "只清除小助手保存的 Desktop 绑定，不修改账号或两端登录文件。"
        clearDesktopRow = Theme.hstack([clearDesktopButton, Theme.flexible()], spacing: 8)
        clearDesktopRow.isHidden = true
        add(clearDesktopRow)
        stack.setCustomSpacing(14, after: clearDesktopRow)

        flowBanner.fill = NSColor.systemBlue.withAlphaComponent(0.08)
        flowBanner.stroke = NSColor.systemBlue.withAlphaComponent(0.45)
        flowStatus.textColor = .labelColor; flowStatus.font = .systemFont(ofSize: 12)
        flowStatus.isSelectable = true
        let info = NSImageView(image: NSImage(systemSymbolName: "info.circle.fill", accessibilityDescription: nil)!)
        info.contentTintColor = .systemBlue
        info.setContentHuggingPriority(.required, for: .horizontal)
        flowStatus.setContentCompressionResistancePriority(.defaultLow, for: .horizontal)
        flowBanner.add(Theme.hstack([info, flowStatus], spacing: 8, alignment: .top))
        reopenBrowserButton.target = self; reopenBrowserButton.action = #selector(reopenBrowser)
        copyLoginButton.target = self; copyLoginButton.action = #selector(copyLoginAddress)
        Theme.button(reopenBrowserButton, small: true); Theme.button(copyLoginButton, small: true)
        browserActions = Theme.hstack([reopenBrowserButton, copyLoginButton, Theme.flexible()], spacing: 8)
        browserActions.isHidden = true
        flowBanner.add(browserActions)
        flowBanner.isHidden = true
        add(flowBanner)
        stack.setCustomSpacing(16, after: flowBanner)

        add(Theme.sectionTitle("账号授权", accessories: [listCount]))
        add(listBox)
    }

    func close() { window.close() }

    private var shownOnce = false
    func show() {
        window.center()
        window.makeKeyAndOrderFront(nil)
        // Ordering front can widen the window to the screen's preferred size;
        // keep the designed size the first time it opens.
        if !shownOnce {
            shownOnce = true
            window.setContentSize(NSSize(width: 620, height: 540))
            window.center()
        }
        NSApp.activate(ignoringOtherApps: true)
        layoutContent()
        refresh()
        startCurrentStateTimer()
    }
    private func startCurrentStateTimer() {
        if currentStateTimer == nil {
            currentStateTimer = Timer.scheduledTimer(withTimeInterval: 5, repeats: true) { [weak self] _ in self?.refreshCurrentState() }
        }
    }
    func windowDidResize(_ notification: Notification) { layoutContent() }
    private func layoutContent() {
        scroll.layoutSubtreeIfNeeded()
        let width = max(scroll.contentSize.width, 1)
        document.setFrameSize(NSSize(width: width, height: max(scroll.contentSize.height, 1)))
        document.layoutSubtreeIfNeeded()
        let height = max(scroll.contentSize.height, contentStack.fittingSize.height + 40)
        document.setFrameSize(NSSize(width: width, height: height))
    }
    #if ACCOUNT_UI_TEST
    func testLoginButtonBounds() -> Bool {
        layoutContent()
        for button in [browserButton, deviceButton] {
            var view: NSView = button
            var point = NSPoint(x: button.bounds.midX, y: button.bounds.midY)
            while let parent = view.superview {
                point = view.convert(point, to: parent)
                guard parent.bounds.contains(point) else { return false }
                if parent === scroll { break }
                view = parent
            }
        }
        return true
    }
    func testResize(_ size: NSSize) {
        window.setContentSize(size)
        layoutContent()
    }
    func testStartObserving() {
        window.orderFront(nil)
        refresh()
        startCurrentStateTimer()
    }
    func testScopeLabel(name: String, scope: String) -> String? {
        guard let row = accountRows.first(where: { ($0["name"] as? String) == name }) else { return nil }
        let title = scope == "desktop" ? "Desktop" : "CLI"
        return accountScopePresentation(scope: scope, title: title,
            bindings: row["bindings"] as? [[String: String]] ?? [],
            currentScopes: row["current_scopes"] as? [String] ?? [],
            expiredScopes: row["expired_scopes"] as? [String] ?? [], desktopOwner: desktopOwner).0
    }
    func testAccountCount() -> Int { accountRows.count }
    var testClearDesktopVisible: Bool { !clearDesktopRow.isHidden }
    var testClearDesktopEnabled: Bool { clearDesktopButton.isEnabled }
    func testClearDesktop() { clearDesktopButton.performClick(nil) }
    var testClearDesktopMessage: String { flowMessage }
    func testStartLogin(_ mode: String) { (mode == "device" ? deviceButton : browserButton).performClick(nil) }
    func testCancelLogin() { cancelButton.performClick(nil) }
    var testLoginMessage: String { flowMessage }
    var testLoginActive: Bool { flowID != nil }
    #endif
    func windowWillClose(_ notification: Notification) {
        timer?.invalidate()
        currentStateTimer?.invalidate(); currentStateTimer = nil
        if let flow = flowID {
            client.send("login_cancel", fields: ["flow_id": flow]) { [weak self, client, ownsClient] _ in
                self?.finishFlow("登录已取消。")
                if ownsClient { client.close() }
            }
        } else if ownsClient { client.close() }
    }
    @objc private func refresh() {
        refreshGeneration += 1
        let generation = refreshGeneration
        client.send("list") { [weak self] result in
            guard let self = self else { return }
            guard self.refreshGeneration == generation else { return }
            switch result {
            case .failure(let error): self.showUnavailableState(error.message)
            case .success(let data):
                guard let rows = data["accounts"] as? [[String: Any]] else { self.status.stringValue = "账号列表不可读取。"; return }
                self.accountRows = rows
                self.updateCurrentScopeStatus(data)
                self.render(rows)
                let pending = data["pending"] as? Bool == true
                self.recoverButton.isHidden = !pending
                self.onChanged()
                if !pending && ProcessInfo.processInfo.environment["CODEX_HELPER_TEST_HOME"] == nil {
                    for row in rows {
                        guard let id = row["id"] as? String else { continue }
                        if row["managed"] as? Bool == false {
                            self.quotaText[id] = "由 CC Switch 管理；登录有效性和额度尚未核验"
                            continue
                        }
                        self.client.send("quota", fields: ["account_id": id]) { [weak self] result in
                            guard let self = self, self.refreshGeneration == generation else { return }
                            switch result {
                            case .failure(let error): self.quotaText[id] = "额度：" + error.message
                            case .success(let data): self.quotaText[id] = self.quotaSummary(data)
                            }
                            self.render(self.accountRows)
                        }
                    }
                }
            }
        }
    }

    private func updateCurrentScopeStatus(_ data: [String: Any]) {
        scopeObservations = data["scope_observations"] as? [String: String] ?? [:]
        desktopOwner = DesktopAccountOwner(data["desktop_owner"])
        currentListFingerprint = try? JSONSerialization.data(withJSONObject: data, options: [.sortedKeys])
        let pending = data["pending"] as? Bool == true
        clearDesktopRow.isHidden = data["desktop_binding_present"] as? Bool != true
        clearDesktopButton.isEnabled = !pending && desktopOwner != .unknown && flowID == nil && !startingLogin
        clearDesktopButton.toolTip = desktopOwner == .unknown ? desktopOwner.blockedMessage : "只清除小助手保存的 Desktop 绑定，不修改账号或两端登录文件。"
        recoverButton.isHidden = !pending
        if pending {
            status.stringValue = "上次账号操作待核查，请先关闭相关客户端再核查。"
            return
        }
        var warnings: [String] = []
        if desktopOwner == .ccSwitch { warnings.append("Desktop 由 CC Switch 管理") }
        if desktopOwner == .unknown { warnings.append("Desktop 管理方无法确认，请检查 CC Switch 设置") }
        if data["cc_switch_status"] as? String == "unavailable" {
            warnings.append("CC Switch 账号库暂时无法读取，列表可能不完整")
        }
        for (scope, name) in [("desktop", "Desktop"), ("cli", "CLI")] {
            switch scopeObservations[scope] {
            case "unmatched": warnings.append("\(name) 当前文件未匹配到已同步账号")
            case "ambiguous": warnings.append("\(name) 当前文件匹配多个账号，暂无法确认")
            case "unavailable": warnings.append("\(name) 当前登录文件无法识别")
            case "expired": warnings.append("\(name) 当前文件的访问凭证已过期")
            default: break
            }
        }
        status.stringValue = warnings.isEmpty ? "当前账号按各端登录文件识别；运行中的客户端身份需单独核验。" : warnings.joined(separator: "；") + "。"
    }

    private func showUnavailableState(_ message: String) {
        currentListFingerprint = nil
        desktopOwner = .unknown
        clearDesktopButton.isEnabled = false
        accountRows = accountRows.map { row in
            var copy = row
            copy["current_scopes"] = [String]()
            if var bindings = row["bindings"] as? [[String: String]] {
                for index in bindings.indices where bindings[index]["state"] == "bound" { bindings[index]["state"] = "unknown" }
                copy["bindings"] = bindings
            }
            return copy
        }
        status.stringValue = message + " 当前账号状态暂不可确认。"
        render(accountRows)
    }

    // Only the local account list is polled; quota and model requests remain
    // on their existing schedules. Changes made in CC Switch appear while the
    // account window is open without adopting credentials or bindings.
    private func refreshCurrentState() {
        guard window.isVisible, !currentStateCheckInFlight, !startingLogin, flowID == nil else { return }
        currentStateCheckInFlight = true
        let generation = refreshGeneration
        client.send("list") { [weak self] result in
            guard let self = self else { return }
            self.currentStateCheckInFlight = false
            guard self.window.isVisible, self.refreshGeneration == generation else { return }
            switch result {
            case .failure(let error): self.showUnavailableState(error.message)
            case .success(let data):
                guard let rows = data["accounts"] as? [[String: Any]] else { self.status.stringValue = "账号列表不可读取。"; return }
                let fingerprint = try? JSONSerialization.data(withJSONObject: data, options: [.sortedKeys])
                guard fingerprint != self.currentListFingerprint else { return }
                self.accountRows = rows
                self.updateCurrentScopeStatus(data)
                self.render(rows)
                self.onChanged()
            }
        }
    }
    private func quotaSummary(_ data: [String: Any]) -> String {
        guard let windows = data["windows"] as? [[String: Any]] else { return "额度暂不可用" }
        let values = windows.compactMap { item -> String? in
            guard let seconds = item["seconds"] as? Int, let remaining = item["remaining"] as? Double else { return nil }
            let name = seconds == 18000 ? "5 小时" : (seconds == 604800 ? "周额度" : "\(seconds / 3600) 小时")
            return "\(name)剩余 \(String(format: "%.1f", remaining))%"
        }
        return values.isEmpty ? "额度暂不可用" : values.joined(separator: "  ·  ")
    }
    private func render(_ rows: [[String: Any]]) {
        for view in listBox.stack.arrangedSubviews { listBox.stack.removeArrangedSubview(view); view.removeFromSuperview() }
        accountIDs = []
        listCount.stringValue = rows.isEmpty ? "" : "\(rows.count) 个"
        if rows.isEmpty {
            let icon = NSImageView(image: NSImage(systemSymbolName: "person.crop.circle.badge.plus", accessibilityDescription: nil)!)
            icon.contentTintColor = .tertiaryLabelColor
            icon.symbolConfiguration = NSImage.SymbolConfiguration(pointSize: 22, weight: .regular)
            let empty = Theme.caption("尚未添加账号。使用上方的浏览器登录或设备码登录。")
            empty.alignment = .center
            let column = Theme.vstack([icon, empty], spacing: 6); column.alignment = .centerX
            column.edgeInsets = NSEdgeInsets(top: 12, left: 0, bottom: 12, right: 0)
            listBox.add(column)
        }
        for row in rows {
            guard let id = row["id"] as? String, let name = row["name"] as? String else { continue }
            let index = accountIDs.count; accountIDs.append(id)
            if index > 0 { listBox.addDivider() }
            let bindings = row["bindings"] as? [[String: String]] ?? []
            let currentScopes = row["current_scopes"] as? [String] ?? []
            let expiredScopes = row["expired_scopes"] as? [String] ?? []
            let managed = row["managed"] as? Bool ?? true
            let sources = row["sources"] as? [String] ?? ["helper"]
            func binding(_ scope: String, _ title: String) -> StatusChip {
                let presentation = accountScopePresentation(scope: scope, title: title, bindings: bindings, currentScopes: currentScopes, expiredScopes: expiredScopes, desktopOwner: desktopOwner)
                let chip = StatusChip(presentation.0, tint: presentation.1)
                chip.toolTip = presentation.2
                return chip
            }
            let title = Theme.text(name, size: 13, weight: .semibold, wraps: false)
            title.setContentCompressionResistancePriority(.defaultLow, for: .horizontal)
            let sourceText = sources.contains("cc_switch") ? (managed ? "小助手 + CC Switch" : "CC Switch") : "小助手"
            let source = StatusChip(sourceText, tint: .secondaryLabelColor)
            source.toolTip = managed ? "该账号已添加到小助手。" : "该账号只存在 CC Switch；请在 CC Switch 管理或另行登录小助手。已保存不代表登录仍有效。"
            let heading = Theme.hstack([title, source], spacing: 6)
            let chips = Theme.hstack([binding("desktop", "Desktop"), binding("cli", "CLI")], spacing: 6)
            let quota = Theme.caption(quotaText[id] ?? (managed ? "额度等待查询" : "由 CC Switch 管理；登录有效性和额度尚未核验"))
            quota.maximumNumberOfLines = 2
            let details = Theme.vstack([heading, chips, quota], spacing: 5)
            details.setContentCompressionResistancePriority(.defaultLow, for: .horizontal)
            let more = NSPopUpButton(frame: .zero, pullsDown: true)
            more.isBordered = false
            (more.cell as? NSPopUpButtonCell)?.arrowPosition = .noArrow
            more.addItem(withTitle: "")
            more.item(at: 0)?.image = NSImage(systemSymbolName: "ellipsis.circle", accessibilityDescription: "更多操作")
            let reauth = NSMenuItem(title: "重新授权…", action: #selector(reauthenticate(_:)), keyEquivalent: ""); reauth.target = self; reauth.tag = index
            let remove = NSMenuItem(title: "从小助手移除…", action: #selector(removeAccount(_:)), keyEquivalent: ""); remove.target = self; remove.tag = index
            more.menu?.addItem(reauth); more.menu?.addItem(.separator()); more.menu?.addItem(remove)
            more.toolTip = "重新授权或移除"; more.setAccessibilityLabel("\(name) 授权操作")
            let openSwitch = Theme.button(NSButton(title: "打开 CC Switch", target: self, action: #selector(openCCSwitch)), small: true)
            let actions = Theme.hstack(managed ? [] : [openSwitch], spacing: 6)
            more.isHidden = !managed
            let avatar = InitialAvatar(name: name, size: 32)
            let line = Theme.hstack([avatar, details, Theme.flexible(), actions, more], spacing: 12, alignment: .centerY)
            listBox.add(line)
        }
        layoutContent()
    }
    @objc private func openCCSwitch() {
        let url = URL(fileURLWithPath: "/Applications/CC Switch.app")
        if !NSWorkspace.shared.open(url) { flowMessage = "无法打开 CC Switch，请检查应用是否已安装。" }
    }
    @objc private func clearDesktopBinding() {
        guard !clearDesktopRow.isHidden, clearDesktopButton.isEnabled, flowID == nil, !startingLogin else { return }
        clearDesktopButton.isEnabled = false
        client.send("clear_desktop_binding") { [weak self] result in
            guard let self = self else { return }
            switch result {
            case .failure(let error):
                self.flowMessage = error.message
                self.clearDesktopButton.isEnabled = self.desktopOwner != .unknown && self.recoverButton.isHidden && self.flowID == nil && !self.startingLogin
            case .success:
                self.flowMessage = "Desktop 绑定已清除；账号和两端现有登录保留。"
                self.refresh()
            }
        }
    }
    @objc private func startBrowser() { startLogin(mode: "browser", account: nil) }
    @objc private func startDevice() { startLogin(mode: "device", account: nil) }
    private func tag(of sender: Any) -> Int { (sender as? NSMenuItem)?.tag ?? (sender as? NSControl)?.tag ?? -1 }
    @objc private func reauthenticate(_ sender: Any) {
        guard accountIDs.indices.contains(tag(of: sender)) else { return }
        startLogin(mode: "browser", account: accountIDs[tag(of: sender)])
    }
    @objc private func removeAccount(_ sender: Any) {
        guard accountIDs.indices.contains(tag(of: sender)) else { return }
        let account = accountIDs[tag(of: sender)]
        let alert = NSAlert(); alert.messageText = "移除小助手中的账号？"
        alert.informativeText = "将从小助手账号列表移除该账号及绑定记录。两端当前登录文件和用于恢复的历史备份会保留。"
        alert.addButton(withTitle: "移除"); alert.addButton(withTitle: "取消")
        guard alert.runModal() == .alertFirstButtonReturn else { return }
        client.send("remove", fields: ["account_id": account]) { [weak self] result in
            guard let self = self else { return }
            switch result {
            case .failure(let error): self.flowMessage = error.message
            case .success: self.flowMessage = "账号已从小助手移除；两端现有登录保留。"; self.refresh()
            }
        }
    }
    private func startLogin(mode: String, account: String?) {
        guard flowID == nil, !startingLogin else { flowMessage = "已有登录正在进行。"; return }
        var fields = ["mode": mode]
        if let account = account { fields["account_id"] = account }
        startingLogin = true
        flowMessage = "正在准备登录…"
        client.send("login_start", fields: fields) { [weak self] result in
            guard let self = self else { return }
            self.startingLogin = false
            switch result {
            case .failure(let error): self.flowMessage = error.message
            case .success(let data):
                guard let flow = data["flow_id"] as? String else { self.flowMessage = "登录状态不可确认。"; return }
                guard self.window.isVisible else {
                    self.client.send("login_cancel", fields: ["flow_id": flow]) { _ in }
                    self.flowMessage = "登录已取消。"; return
                }
                guard let value = data["url"] as? String, let url = URL(string: value),
                      url.scheme == "https", url.host == "auth.openai.com" else {
                    self.client.send("login_cancel", fields: ["flow_id": flow]) { _ in }
                    self.flowMessage = "登录地址不可确认，已停止。"; return
                }
                self.flowID = flow; self.cancelButton.isEnabled = true; self.cancelButton.isHidden = false
                self.loginURL = url
                self.loginCode = data["user_code"] as? String ?? ""
                self.browserActions.isHidden = false
                self.layoutContent()
                self.flowMessage = "正在打开浏览器…"
                self.openBrowser(url, for: flow)
                let interval = max(3, min(15, data["interval"] as? Double ?? 3))
                self.timer = Timer.scheduledTimer(withTimeInterval: interval, repeats: true) { [weak self] _ in self?.pollLogin() }
                self.pollLogin()
            }
        }
    }
    private func openBrowser(_ url: URL, for flow: String) {
        #if ACCOUNT_UI_TEST
        let opened = ProcessInfo.processInfo.environment["CODEX_HELPER_TEST_OPEN_FAIL"] != "1"
        showBrowserResult(opened: opened, for: flow)
        #else
        guard let application = NSWorkspace.shared.urlForApplication(toOpen: url) else {
            showBrowserResult(opened: false, for: flow); return
        }
        let configuration = NSWorkspace.OpenConfiguration()
        configuration.activates = true
        NSWorkspace.shared.open([url], withApplicationAt: application, configuration: configuration) { [weak self] app, error in
            DispatchQueue.main.async {
                self?.showBrowserResult(opened: error == nil && app != nil, for: flow)
            }
        }
        #endif
    }
    private func showBrowserResult(opened: Bool, for flow: String) {
        guard flowID == flow else { return }
        let instruction = loginCode.isEmpty ? "请在浏览器完成账号登录。" : "请在浏览器输入设备码：\(loginCode)"
        flowMessage = opened ? instruction : instruction + "\n浏览器未能打开，可重新打开或复制登录地址。"
    }
    @objc private func reopenBrowser() {
        guard let url = loginURL, let flow = flowID else { return }
        flowMessage = "正在打开浏览器…"
        openBrowser(url, for: flow)
    }
    @objc private func copyLoginAddress() {
        guard let url = loginURL else { return }
        NSPasteboard.general.clearContents()
        guard NSPasteboard.general.setString(url.absoluteString, forType: .string) else {
            flowMessage = "无法复制登录地址，请重新打开浏览器。"; return
        }
        let code = loginCode.isEmpty ? "" : " 设备码：\(loginCode)"
        flowMessage = "登录地址已复制，请粘贴到浏览器。\(code)"
    }
    private func pollLogin() {
        guard let flow = flowID, !polling else { return }
        polling = true
        client.send("login_poll", fields: ["flow_id": flow]) { [weak self] result in
            guard let self = self else { return }
            self.polling = false
            switch result {
            case .failure(let error): self.finishFlow(error.message)
            case .success(let value):
                if value["state"] as? String == "complete" {
                    let next = self.desktopOwner == .ccSwitch ? "Desktop 请在 CC Switch 中切换。" : (self.desktopOwner == .native ? "可在主界面选择使用端。" : "请先核查 Desktop 管理方。")
                    self.finishFlow("账号已添加到小助手。" + next)
                    self.refresh()
                }
            }
        }
    }
    private func finishFlow(_ message: String) {
        timer?.invalidate(); timer = nil; flowID = nil; cancelButton.isEnabled = false; cancelButton.isHidden = true
        loginURL = nil; loginCode = ""; browserActions.isHidden = true
        layoutContent()
        flowMessage = message
    }
    @objc private func cancelLogin() {
        guard let flow = flowID else { return }
        timer?.invalidate(); timer = nil
        client.send("login_cancel", fields: ["flow_id": flow]) { [weak self] _ in self?.finishFlow("登录已取消。") }
    }
    @objc private func resolvePending() {
        flowMessage = "正在核查上次操作…"
        client.send("resolve_pending") { [weak self] result in
            guard let self = self else { return }
            switch result {
            case .failure(let error): self.flowMessage = error.message
            case .success(let value):
                self.flowMessage = value["state"] as? String == "completed"
                    ? "已核对已写入的认证；启动客户端后请确认实际账号。"
                    : "已核对未完成的操作，现有登录文件保留。"
                self.refresh()
            }
        }
    }
}
