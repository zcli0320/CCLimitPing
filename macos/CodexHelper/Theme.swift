import Cocoa

// Shared visual building blocks. Semantic colors only, so both appearances
// follow the system; drawing happens in draw(_:) and refreshes on change.
enum Theme {
    static let corner: CGFloat = 10
    static let pagePadding: CGFloat = 20
    static let sectionSpacing: CGFloat = 18

    static func text(_ value: String, size: CGFloat = 12, weight: NSFont.Weight = .regular,
                     color: NSColor = .labelColor, wraps: Bool = true) -> NSTextField {
        let field = wraps ? NSTextField(wrappingLabelWithString: value) : NSTextField(labelWithString: value)
        field.font = .systemFont(ofSize: size, weight: weight)
        field.textColor = color
        if !wraps { field.lineBreakMode = .byTruncatingTail }
        return field
    }

    static func caption(_ value: String = "") -> NSTextField {
        text(value, size: 11, color: .secondaryLabelColor)
    }

    /// A section title above a grouped box, with optional trailing accessories.
    static func sectionTitle(_ title: String, accessories: [NSView] = []) -> NSStackView {
        let label = text(title, size: 13, weight: .semibold, wraps: false)
        let row = hstack([label, flexible()] + accessories, spacing: 8)
        return row
    }

    static func hstack(_ views: [NSView], spacing: CGFloat = 8, alignment: NSLayoutConstraint.Attribute = .centerY) -> NSStackView {
        let stack = NSStackView(views: views)
        stack.orientation = .horizontal; stack.alignment = alignment; stack.spacing = spacing
        stack.detachesHiddenViews = true
        return stack
    }

    static func vstack(_ views: [NSView], spacing: CGFloat = 4) -> NSStackView {
        let stack = NSStackView(views: views)
        stack.orientation = .vertical; stack.alignment = .leading; stack.spacing = spacing
        stack.detachesHiddenViews = true
        return stack
    }

    static func flexible() -> NSView {
        let view = NSView()
        view.setContentHuggingPriority(.init(1), for: .horizontal)
        view.setContentCompressionResistancePriority(.init(1), for: .horizontal)
        return view
    }

    @discardableResult static func button(_ button: NSButton, small: Bool = false, primary: Bool = false) -> NSButton {
        button.bezelStyle = .rounded
        if small { button.controlSize = .small; button.font = .systemFont(ofSize: 11) }
        if primary { button.bezelColor = .controlAccentColor; button.keyEquivalent = "" }
        return button
    }

    static func symbolButton(_ symbol: String, label: String, target: AnyObject?, action: Selector?) -> NSButton {
        let image = NSImage(systemSymbolName: symbol, accessibilityDescription: label) ?? NSImage()
        let button = NSButton(image: image, target: target, action: action)
        button.bezelStyle = .rounded
        button.toolTip = label
        button.setAccessibilityLabel(label)
        return button
    }
}

/// A rounded grouped container (System Settings style). Rows are added to
/// `stack`; hidden rows collapse because the stack detaches hidden views.
final class SectionBox: NSView {
    let stack = NSStackView()
    var fill: NSColor = .controlBackgroundColor { didSet { needsDisplay = true } }
    var stroke: NSColor = .separatorColor { didSet { needsDisplay = true } }

    init(spacing: CGFloat = 10, insets: NSEdgeInsets = NSEdgeInsets(top: 12, left: 14, bottom: 12, right: 14)) {
        super.init(frame: .zero)
        stack.orientation = .vertical; stack.alignment = .leading; stack.spacing = spacing
        stack.detachesHiddenViews = true
        addSubview(stack); stack.translatesAutoresizingMaskIntoConstraints = false
        NSLayoutConstraint.activate([
            stack.topAnchor.constraint(equalTo: topAnchor, constant: insets.top),
            stack.bottomAnchor.constraint(equalTo: bottomAnchor, constant: -insets.bottom),
            stack.leadingAnchor.constraint(equalTo: leadingAnchor, constant: insets.left),
            stack.trailingAnchor.constraint(equalTo: trailingAnchor, constant: -insets.right)
        ])
    }
    required init?(coder: NSCoder) { fatalError("init(coder:) has not been implemented") }

    @discardableResult func add(_ view: NSView) -> NSView {
        stack.addArrangedSubview(view)
        view.translatesAutoresizingMaskIntoConstraints = false
        view.widthAnchor.constraint(equalTo: stack.widthAnchor).isActive = true
        return view
    }

    func addDivider() {
        let line = HairlineView()
        stack.addArrangedSubview(line)
        line.translatesAutoresizingMaskIntoConstraints = false
        line.widthAnchor.constraint(equalTo: stack.widthAnchor).isActive = true
    }

    override func draw(_ dirtyRect: NSRect) {
        let path = NSBezierPath(roundedRect: bounds.insetBy(dx: 0.5, dy: 0.5), xRadius: Theme.corner, yRadius: Theme.corner)
        fill.setFill(); path.fill()
        stroke.withAlphaComponent(0.5).setStroke(); path.lineWidth = 0.5; path.stroke()
    }
}

final class HairlineView: NSView {
    override var intrinsicContentSize: NSSize { NSSize(width: NSView.noIntrinsicMetric, height: 1) }
    override func draw(_ dirtyRect: NSRect) {
        NSColor.separatorColor.withAlphaComponent(0.6).setFill()
        NSRect(x: 0, y: (bounds.height - 0.5) / 2, width: bounds.width, height: 0.5).fill()
    }
}

/// Small capsule label for states such as "已绑定" or "CC Switch 当前".
final class StatusChip: NSView {
    private let label = NSTextField(labelWithString: "")
    var tint: NSColor { didSet { label.textColor = tint; needsDisplay = true } }
    var text: String {
        get { label.stringValue }
        set { label.stringValue = newValue; isHidden = newValue.isEmpty; invalidateIntrinsicContentSize() }
    }

    init(_ text: String, tint: NSColor) {
        self.tint = tint
        super.init(frame: .zero)
        label.font = .systemFont(ofSize: 10.5, weight: .medium)
        label.textColor = tint
        label.stringValue = text
        isHidden = text.isEmpty
        addSubview(label); label.translatesAutoresizingMaskIntoConstraints = false
        NSLayoutConstraint.activate([
            label.leadingAnchor.constraint(equalTo: leadingAnchor, constant: 7),
            label.trailingAnchor.constraint(equalTo: trailingAnchor, constant: -7),
            label.topAnchor.constraint(equalTo: topAnchor, constant: 2),
            label.bottomAnchor.constraint(equalTo: bottomAnchor, constant: -2)
        ])
        setContentHuggingPriority(.required, for: .horizontal)
        setContentCompressionResistancePriority(.required, for: .horizontal)
    }
    required init?(coder: NSCoder) { fatalError("init(coder:) has not been implemented") }

    override func draw(_ dirtyRect: NSRect) {
        let path = NSBezierPath(roundedRect: bounds, xRadius: bounds.height / 2, yRadius: bounds.height / 2)
        tint.withAlphaComponent(0.14).setFill(); path.fill()
    }
}

/// Circular initial badge used to make account rows easy to tell apart.
final class InitialAvatar: NSView {
    private let tint: NSColor
    private let label = NSTextField(labelWithString: "")

    init(name: String, size: CGFloat = 30) {
        let palette: [NSColor] = [.systemBlue, .systemTeal, .systemIndigo, .systemPurple, .systemPink, .systemOrange, .systemGreen]
        let hash = name.unicodeScalars.reduce(0) { ($0 &* 31 &+ Int($1.value)) & 0x7fffffff }
        tint = palette[hash % palette.count]
        super.init(frame: .zero)
        label.stringValue = name.first.map { String($0).uppercased() } ?? "?"
        label.font = .systemFont(ofSize: size * 0.45, weight: .semibold)
        label.textColor = tint
        label.alignment = .center
        addSubview(label); label.translatesAutoresizingMaskIntoConstraints = false
        NSLayoutConstraint.activate([
            widthAnchor.constraint(equalToConstant: size), heightAnchor.constraint(equalToConstant: size),
            label.centerXAnchor.constraint(equalTo: centerXAnchor), label.centerYAnchor.constraint(equalTo: centerYAnchor)
        ])
        setAccessibilityElement(false)
    }
    required init?(coder: NSCoder) { fatalError("init(coder:) has not been implemented") }

    override func draw(_ dirtyRect: NSRect) {
        tint.withAlphaComponent(0.16).setFill()
        NSBezierPath(ovalIn: bounds).fill()
    }
}
