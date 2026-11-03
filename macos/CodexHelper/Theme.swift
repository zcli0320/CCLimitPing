import Cocoa

final class FlippedDocument: NSView { override var isFlipped: Bool { true } }

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

    static func iconTextButton(_ title: String, symbol: String, primary: Bool = false, small: Bool = false,
                               target: AnyObject?, action: Selector?) -> NSButton {
        let btn = NSButton(title: title, target: target, action: action)
        button(btn, small: small, primary: primary)
        if let image = NSImage(systemSymbolName: symbol, accessibilityDescription: nil) {
            btn.image = image
            btn.imagePosition = .imageLeading
        }
        return btn
    }

    static func statusColor(for status: String) -> NSColor {
        switch status {
        case "accepted", "pass", "ready", "confirmed", "logged_in", "available":
            return .systemGreen
        case "running", "reviewing", "checking", "resuming", "watching", "window_running", "probe_verified":
            return .systemBlue
        case "queued", "receipt", "pending":
            return .systemTeal
        case "changes", "quota_exhausted", "uncertain", "unconfirmed", "needs_reapply":
            return .systemOrange
        case "failed", "error", "unavailable":
            return .systemRed
        default:
            return .secondaryLabelColor
        }
    }
}

/// Interactive summary card used in the top overview bar for quick navigation and live status.
final class QuickEntryCard: NSView {
    private let iconView = NSImageView()
    private let titleField = Theme.text("", size: 12, weight: .semibold, wraps: false)
    private let badge = StatusChip("", tint: .secondaryLabelColor)
    private let subtitleField = Theme.caption("")
    private var tracking: NSTrackingArea?
    private var hovered = false { didSet { needsDisplay = true } }
    var isActiveCard = false { didSet { needsDisplay = true } }
    var tint: NSColor { didSet { iconView.contentTintColor = tint; needsDisplay = true } }
    var onClick: (() -> Void)?

    init(title: String, symbol: String, tint: NSColor, subtitle: String, badgeText: String = "", onClick: (() -> Void)? = nil) {
        self.tint = tint
        self.onClick = onClick
        super.init(frame: .zero)
        iconView.image = NSImage(systemSymbolName: symbol, accessibilityDescription: title)
        iconView.symbolConfiguration = NSImage.SymbolConfiguration(pointSize: 13, weight: .semibold)
        iconView.contentTintColor = tint
        iconView.setContentHuggingPriority(.required, for: .horizontal)
        titleField.stringValue = title
        titleField.setContentCompressionResistancePriority(.defaultLow, for: .horizontal)
        badge.text = badgeText
        badge.tint = tint
        subtitleField.stringValue = subtitle
        subtitleField.maximumNumberOfLines = 1
        subtitleField.lineBreakMode = .byTruncatingTail
        let top = Theme.hstack([iconView, titleField, Theme.flexible(), badge], spacing: 6)
        let stack = Theme.vstack([top, subtitleField], spacing: 4)
        addSubview(stack); stack.translatesAutoresizingMaskIntoConstraints = false
        top.widthAnchor.constraint(equalTo: stack.widthAnchor).isActive = true
        subtitleField.widthAnchor.constraint(equalTo: stack.widthAnchor).isActive = true
        NSLayoutConstraint.activate([
            stack.topAnchor.constraint(equalTo: topAnchor, constant: 9),
            stack.bottomAnchor.constraint(equalTo: bottomAnchor, constant: -9),
            stack.leadingAnchor.constraint(equalTo: leadingAnchor, constant: 11),
            stack.trailingAnchor.constraint(equalTo: trailingAnchor, constant: -11)
        ])
        setAccessibilityElement(true)
        setAccessibilityRole(.button)
        setAccessibilityLabel(title)
    }
    required init?(coder: NSCoder) { fatalError("init(coder:) has not been implemented") }

    func update(subtitle: String, badgeText: String, tint: NSColor) {
        self.tint = tint
        subtitleField.stringValue = subtitle
        subtitleField.toolTip = subtitle
        badge.text = badgeText
        badge.tint = tint
    }

    override func updateTrackingAreas() {
        super.updateTrackingAreas()
        if let tracking = tracking { removeTrackingArea(tracking) }
        let area = NSTrackingArea(rect: bounds, options: [.mouseEnteredAndExited, .activeInKeyWindow, .inVisibleRect], owner: self, userInfo: nil)
        addTrackingArea(area)
        tracking = area
    }
    override var acceptsFirstResponder: Bool { true }
    override var canBecomeKeyView: Bool { true }
    override func becomeFirstResponder() -> Bool {
        let ok = super.becomeFirstResponder()
        needsDisplay = true
        return ok
    }
    override func resignFirstResponder() -> Bool {
        let ok = super.resignFirstResponder()
        needsDisplay = true
        return ok
    }
    override var focusRingMaskBounds: NSRect { bounds }
    override func drawFocusRingMask() {
        NSBezierPath(roundedRect: bounds.insetBy(dx: 0.5, dy: 0.5), xRadius: 8, yRadius: 8).fill()
    }
    override func mouseEntered(with event: NSEvent) { hovered = true }
    override func mouseExited(with event: NSEvent) { hovered = false }
    override func mouseDown(with event: NSEvent) {
        window?.makeFirstResponder(self)
        onClick?()
    }
    override func keyDown(with event: NSEvent) {
        if event.keyCode == 49 || event.keyCode == 36 || event.keyCode == 76 || event.charactersIgnoringModifiers == " " || event.charactersIgnoringModifiers == "\r" || event.charactersIgnoringModifiers == "\u{03}" {
            onClick?()
        } else {
            super.keyDown(with: event)
        }
    }
    override func accessibilityPerformPress() -> Bool {
        guard let onClick = onClick else { return false }
        onClick()
        return true
    }

    override func draw(_ dirtyRect: NSRect) {
        let path = NSBezierPath(roundedRect: bounds.insetBy(dx: 0.5, dy: 0.5), xRadius: 8, yRadius: 8)
        let bg: NSColor = isActiveCard
            ? tint.withAlphaComponent(0.11)
            : (hovered ? NSColor.controlBackgroundColor.blended(withFraction: 0.06, of: tint) ?? .controlBackgroundColor : .controlBackgroundColor)
        bg.setFill(); path.fill()
        let border: NSColor = isActiveCard ? tint.withAlphaComponent(0.65) : (hovered ? tint.withAlphaComponent(0.4) : NSColor.separatorColor.withAlphaComponent(0.5))
        border.setStroke()
        path.lineWidth = isActiveCard ? 1.0 : 0.5
        path.stroke()
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

/// Scroll view that forwards vertical wheel events to its enclosing scroll view
/// when it has no vertical overflow, has reached its scroll boundary in that direction,
/// or slides under the cursor during an in-flight outer scroll gesture.
final class NestedScrollView: NSScrollView {
    private var ownsCurrentGesture = false
    private var forwardingGesture = false

    private var parentScrollView: NSScrollView? {
        superview?.enclosingScrollView
    }

    private func forwardToParent(_ event: NSEvent) {
        if let parent = parentScrollView {
            parent.scrollWheel(with: event)
        } else {
            nextResponder?.scrollWheel(with: event)
        }
    }

    override func scrollWheel(with event: NSEvent) {
        guard let doc = documentView else {
            super.scrollWheel(with: event)
            return
        }
        let clipBounds = contentView.bounds
        let maxOffset = max(0, doc.frame.height - clipBounds.height)
        if maxOffset <= 0.5 {
            ownsCurrentGesture = false
            forwardingGesture = true
            forwardToParent(event)
            return
        }

        let phase = event.phase
        let momentum = event.momentumPhase
        if phase == .mayBegin || phase == .began {
            ownsCurrentGesture = true
            forwardingGesture = false
        } else if (phase != [] || momentum != []) && !ownsCurrentGesture {
            // Gesture began on the outer scroll view and this view slid under the cursor.
            forwardToParent(event)
            return
        }

        if forwardingGesture && (phase != [] || momentum != []) {
            forwardToParent(event)
            if phase == .ended || phase == .cancelled || momentum == .ended {
                ownsCurrentGesture = false
                forwardingGesture = false
            }
            return
        }

        let dy = event.scrollingDeltaY
        let dx = event.scrollingDeltaX
        if abs(dy) >= abs(dx) && abs(dy) > 0.01 {
            let y = clipBounds.origin.y
            let atTop = doc.isFlipped ? (y <= 0.5) : (y >= maxOffset - 0.5)
            let atBottom = doc.isFlipped ? (y >= maxOffset - 0.5) : (y <= 0.5)
            if (dy > 0 && atTop) || (dy < 0 && atBottom) {
                forwardingGesture = true
                forwardToParent(event)
                return
            }
        }

        super.scrollWheel(with: event)
        if phase == .ended || phase == .cancelled || momentum == .ended {
            ownsCurrentGesture = false
            forwardingGesture = false
        }
    }
}
