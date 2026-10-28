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
        NSColor.quaternaryLabelColor.withAlphaComponent(0.14).setFill()
        NSBezierPath(roundedRect: bounds, xRadius: 3, yRadius: 3).fill()
        guard percent > 0 else { return }
        tint.setFill()
        let fill = NSRect(x: 0, y: 0, width: bounds.width * percent / 100, height: bounds.height)
        NSBezierPath(roundedRect: fill, xRadius: 3, yRadius: 3).fill()
    }
}

final class QuotaCard: NSView {
    init(account: HelperAccount, index: Int, target: AnyObject, action: Selector) {
        super.init(frame: .zero)
        let name = field(account.name, size: 13, weight: .semibold)
        name.lineBreakMode = .byTruncatingTail; name.maximumNumberOfLines = 1
        name.setContentCompressionResistancePriority(.defaultLow, for: .horizontal)
        name.toolTip = ([account.name] + account.provider_names).joined(separator: "\n")
        let badge = field(account.selected ? "CC Switch 当前" : (account.default ? "默认账号" : ""), size: 10,
                          color: account.selected ? .systemTeal : .secondaryLabelColor)
        badge.setContentCompressionResistancePriority(.required, for: .horizontal)
        let more = NSButton(image: NSImage(systemSymbolName: "info.circle", accessibilityDescription: "账号额度详情")!,
                            target: target, action: action)
        more.isBordered = false; more.bezelStyle = .inline; more.tag = index
        more.contentTintColor = .secondaryLabelColor; more.toolTip = "额度查询时间、凭证来源与完整说明"
        more.setAccessibilityLabel("\(account.name) 额度详情")
        more.widthAnchor.constraint(equalToConstant: 20).isActive = true
        let header = NSView()
        for view in [name, badge, more] { header.addSubview(view); view.translatesAutoresizingMaskIntoConstraints = false }
        badge.alignment = .right
        NSLayoutConstraint.activate([
            header.heightAnchor.constraint(equalToConstant: 20),
            name.leadingAnchor.constraint(equalTo: header.leadingAnchor),
            name.trailingAnchor.constraint(equalTo: badge.leadingAnchor, constant: -12),
            name.centerYAnchor.constraint(equalTo: header.centerYAnchor),
            badge.trailingAnchor.constraint(equalTo: more.leadingAnchor, constant: -8),
            badge.widthAnchor.constraint(equalToConstant: 100),
            badge.centerYAnchor.constraint(equalTo: header.centerYAnchor),
            more.trailingAnchor.constraint(equalTo: header.trailingAnchor),
            more.centerYAnchor.constraint(equalTo: header.centerYAnchor)
        ])
        let body: NSView
        if let quota = account.quota, quota.status == "ready" {
            let windows = quota.windows.sorted { $0.seconds < $1.seconds }
            let metrics = windows.map { metric($0) }
            let row = NSStackView(views: metrics); row.orientation = .horizontal
            row.distribution = .fillEqually; row.alignment = .top; row.spacing = 24
            body = row
        } else {
            let message = field(account.quota?.message ?? "正在读取剩余额度…", size: 12, color: .secondaryLabelColor)
            message.maximumNumberOfLines = 2; message.toolTip = message.stringValue
            body = message
        }
        let stack = NSStackView(views: [header, body]); stack.orientation = .vertical
        stack.alignment = .leading; stack.spacing = 12
        addSubview(stack); stack.translatesAutoresizingMaskIntoConstraints = false
        for child in [header, body] {
            child.translatesAutoresizingMaskIntoConstraints = false
            child.widthAnchor.constraint(equalTo: stack.widthAnchor).isActive = true
        }
        NSLayoutConstraint.activate([
            stack.leadingAnchor.constraint(equalTo: leadingAnchor, constant: 14),
            stack.trailingAnchor.constraint(equalTo: trailingAnchor, constant: -14),
            stack.topAnchor.constraint(equalTo: topAnchor, constant: 12),
            stack.bottomAnchor.constraint(equalTo: bottomAnchor, constant: -12)
        ])
    }
    required init?(coder: NSCoder) { fatalError("init(coder:) has not been implemented") }

    override func draw(_ dirtyRect: NSRect) {
        let card = NSBezierPath(roundedRect: bounds.insetBy(dx: 0.5, dy: 0.5), xRadius: 12, yRadius: 12)
        NSColor.controlBackgroundColor.setFill(); card.fill()
        NSColor.separatorColor.withAlphaComponent(0.45).setStroke(); card.lineWidth = 0.5; card.stroke()
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
        let value = field(window.remainingText, size: 24, weight: .semibold)
        value.textColor = percent <= 25 ? tint : .labelColor
        value.font = .monospacedDigitSystemFont(ofSize: 24, weight: .semibold)
        value.alignment = .right; value.maximumNumberOfLines = 1
        value.setAccessibilityLabel("\(window.title)剩余 \(window.remainingText)")
        let top = NSStackView(views: [title, value]); top.orientation = .horizontal; top.alignment = .firstBaseline; top.spacing = 8
        title.setContentHuggingPriority(.defaultLow, for: .horizontal)
        let bar = RemainingQuotaBar(percent: percent, tint: tint)
        bar.heightAnchor.constraint(equalToConstant: 5).isActive = true
        let reset = field(window.resetCaption, size: 10, color: .secondaryLabelColor)
        reset.maximumNumberOfLines = 1; reset.lineBreakMode = .byTruncatingTail
        reset.toolTip = window.resetCaption
        let column = NSStackView(views: [top, bar, reset]); column.orientation = .vertical
        column.alignment = .leading; column.spacing = 6
        for view in [top, bar, reset] {
            view.translatesAutoresizingMaskIntoConstraints = false
            view.widthAnchor.constraint(equalTo: column.widthAnchor).isActive = true
        }
        return column
    }
}
