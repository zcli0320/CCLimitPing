import Cocoa

// The filled portion always represents remaining quota, matching the number.
final class RemainingQuotaBar: NSView {
    let percent: Double
    let tint: NSColor

    init(percent: Double, tint: NSColor) {
        self.percent = max(0, min(100, percent)); self.tint = tint
        super.init(frame: .zero)
        setAccessibilityElement(true)
        setAccessibilityLabel("剩余额度")
        setAccessibilityValue(String(format: "%.1f%%", self.percent))
    }
    required init?(coder: NSCoder) { fatalError("init(coder:) has not been implemented") }
    override func draw(_ dirtyRect: NSRect) {
        NSColor.quaternaryLabelColor.withAlphaComponent(0.2).setFill()
        NSBezierPath(roundedRect: bounds, xRadius: bounds.height / 2, yRadius: bounds.height / 2).fill()
        guard percent > 0 else { return }
        tint.setFill()
        let fill = NSRect(x: 0, y: 0, width: bounds.width * percent / 100, height: bounds.height)
        NSBezierPath(roundedRect: fill, xRadius: bounds.height / 2, yRadius: bounds.height / 2).fill()
    }
}

// Carry the stable account id with the button: list order may change as CC
// Switch updates its accounts, so a view index must not select a credential.
final class AccountScopeButton: NSButton {
    let accountID: String
    let scope: String

    init(title: String, accountID: String, scope: String, target: AnyObject, action: Selector) {
        self.accountID = accountID; self.scope = scope
        super.init(frame: .zero)
        self.title = title; self.target = target; self.action = action
    }
    required init?(coder: NSCoder) { fatalError("init(coder:) has not been implemented") }
}

final class QuotaCard: NSView {
    init(account: HelperAccount, index: Int, metadata: [String: Any]?, actionsEnabled: Bool, desktopOwner: DesktopAccountOwner,
         target: AnyObject, detailAction: Selector, applyAction: Selector, authorizeAction: Selector) {
        super.init(frame: .zero)
        let managed = metadata?["managed"] as? Bool == true
        let bindings = metadata?["bindings"] as? [[String: String]] ?? []
        let currentScopes = metadata?["current_scopes"] as? [String] ?? []
        let expiredScopes = metadata?["expired_scopes"] as? [String] ?? []
        let sources = metadata?["sources"] as? [String] ?? []
        let name = field(account.name, size: 13, weight: .semibold)
        name.lineBreakMode = .byTruncatingTail; name.maximumNumberOfLines = 1
        name.setContentCompressionResistancePriority(.defaultLow, for: .horizontal)
        name.toolTip = ([account.name] + account.provider_names).joined(separator: "\n")
        let source = metadata == nil ? "状态待核查" : (sources.contains("cc_switch") ? (managed ? "小助手 + CC Switch" : "CC Switch") : "小助手")
        let badge = StatusChip(source, tint: .secondaryLabelColor)
        badge.toolTip = metadata == nil ? "正在核查账号和两端状态。" : (managed ? "账号已添加到小助手。" : "账号仅来自 CC Switch，需在授权管理中登录后才能由小助手切换。")
        let more = NSButton(image: NSImage(systemSymbolName: "info.circle", accessibilityDescription: "账号额度详情")!,
                            target: target, action: detailAction)
        more.isBordered = false; more.bezelStyle = .inline; more.tag = index
        more.contentTintColor = .tertiaryLabelColor; more.toolTip = "额度查询时间、凭证来源与完整说明"
        more.setAccessibilityLabel("\(account.name) 额度详情")
        more.widthAnchor.constraint(equalToConstant: 18).isActive = true
        let header = Theme.hstack([InitialAvatar(name: account.name, size: 22), name, badge, Theme.flexible(), more], spacing: 8)
        let body: NSView
        if let quota = account.quota, quota.status == "ready", !quota.windows.isEmpty {
            let windows = quota.windows.sorted { $0.seconds < $1.seconds }
            let row = NSStackView(views: windows.map { metric($0) }); row.orientation = .horizontal
            row.distribution = .fillEqually; row.alignment = .top; row.spacing = 20
            body = row
        } else {
            let message = field(account.quota?.message ?? "正在读取剩余额度…", size: 11, color: .secondaryLabelColor)
            message.maximumNumberOfLines = 2; message.toolTip = message.stringValue
            body = message
        }
        func scopeChip(_ scope: String, _ title: String) -> StatusChip {
            guard metadata != nil else {
                let chip = StatusChip(title + " · 待核查", tint: .secondaryLabelColor)
                chip.toolTip = "账号状态暂不可读取。"
                return chip
            }
            let presentation = accountScopePresentation(scope: scope, title: title, bindings: bindings,
                                                        currentScopes: currentScopes, expiredScopes: expiredScopes, desktopOwner: desktopOwner)
            let chip = StatusChip(presentation.0, tint: presentation.1)
            chip.toolTip = presentation.2
            return chip
        }
        let scopes = Theme.hstack([scopeChip("desktop", "Desktop"), scopeChip("cli", "CLI"), Theme.flexible()], spacing: 6)
        let actions: NSView
        if managed {
            let desktop = AccountScopeButton(title: "应用到 Desktop", accountID: account.id, scope: "desktop", target: target, action: applyAction)
            let cli = AccountScopeButton(title: "应用到 CLI", accountID: account.id, scope: "cli", target: target, action: applyAction)
            for button in [desktop, cli] {
                Theme.button(button, small: true)
                button.isEnabled = actionsEnabled && (button.scope != "desktop" || desktopOwner == .native)
                button.setAccessibilityLabel("\(account.name) \(button.title)")
            }
            if desktopOwner != .native { desktop.toolTip = desktopOwner.blockedMessage }
            actions = Theme.hstack([desktop, cli, Theme.flexible()], spacing: 8)
        } else {
            let authorize = Theme.button(NSButton(title: "前往授权管理", target: target, action: authorizeAction), small: true)
            authorize.isEnabled = metadata != nil
            authorize.setAccessibilityLabel("\(account.name) 前往授权管理")
            actions = Theme.hstack([authorize, Theme.flexible()], spacing: 8)
        }
        let stack = NSStackView(views: [header, body, scopes, actions]); stack.orientation = .vertical
        stack.alignment = .leading; stack.spacing = 10
        addSubview(stack); stack.translatesAutoresizingMaskIntoConstraints = false
        for child in [header, body, scopes, actions] {
            child.translatesAutoresizingMaskIntoConstraints = false
            child.widthAnchor.constraint(equalTo: stack.widthAnchor).isActive = true
        }
        NSLayoutConstraint.activate([
            stack.leadingAnchor.constraint(equalTo: leadingAnchor, constant: 14),
            stack.trailingAnchor.constraint(equalTo: trailingAnchor, constant: -14),
            stack.topAnchor.constraint(equalTo: topAnchor, constant: 11),
            stack.bottomAnchor.constraint(equalTo: bottomAnchor, constant: -12)
        ])
    }
    required init?(coder: NSCoder) { fatalError("init(coder:) has not been implemented") }

    override func draw(_ dirtyRect: NSRect) {
        let card = NSBezierPath(roundedRect: bounds.insetBy(dx: 0.5, dy: 0.5), xRadius: Theme.corner, yRadius: Theme.corner)
        NSColor.controlBackgroundColor.setFill(); card.fill()
        NSColor.separatorColor.withAlphaComponent(0.5).setStroke(); card.lineWidth = 0.5; card.stroke()
    }

    private func field(_ text: String, size: CGFloat, weight: NSFont.Weight = .regular,
                       color: NSColor = .labelColor) -> NSTextField {
        let field = NSTextField(wrappingLabelWithString: text)
        field.font = .systemFont(ofSize: size, weight: weight); field.textColor = color
        return field
    }

    private func metric(_ window: QuotaWindow) -> NSView {
        let percent = window.remainingPercent
        let tint: NSColor = percent <= 10 ? .systemRed : (percent <= 25 ? .systemOrange : (window.seconds == 18000 ? .systemTeal : .systemBlue))
        let title = field("\(window.title == "7 天" ? "周额度" : window.title)剩余", size: 11, color: .secondaryLabelColor)
        title.maximumNumberOfLines = 1
        let value = field(window.remainingText, size: 17, weight: .semibold)
        value.textColor = percent <= 25 ? tint : .labelColor
        value.font = .monospacedDigitSystemFont(ofSize: 17, weight: .semibold)
        value.alignment = .right; value.maximumNumberOfLines = 1
        value.setContentCompressionResistancePriority(.required, for: .horizontal)
        value.setAccessibilityLabel("\(window.title)剩余 \(window.remainingText)")
        let top = NSStackView(views: [title, Theme.flexible(), value]); top.orientation = .horizontal; top.alignment = .firstBaseline; top.spacing = 6
        let bar = RemainingQuotaBar(percent: percent, tint: tint)
        bar.heightAnchor.constraint(equalToConstant: 4).isActive = true
        let reset = field(window.resetCaption, size: 10.5, color: .tertiaryLabelColor)
        reset.maximumNumberOfLines = 1; reset.lineBreakMode = .byTruncatingTail
        reset.toolTip = window.resetCaption
        let column = NSStackView(views: [top, bar, reset]); column.orientation = .vertical
        column.alignment = .leading; column.spacing = 5
        for view in [top, bar, reset] {
            view.translatesAutoresizingMaskIntoConstraints = false
            view.widthAnchor.constraint(equalTo: column.widthAnchor).isActive = true
        }
        return column
    }
}
