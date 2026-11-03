import Cocoa

final class AccountActionReceiver: NSObject {
    var applied: [(String, String)] = []
    var authorizationOpened = 0
    @objc func apply(_ sender: AccountScopeButton) { applied.append((sender.accountID, sender.scope)) }
    @objc func detail(_ sender: NSButton) {}
    @objc func authorize(_ sender: NSButton) { authorizationOpened += 1 }
}

@main
enum AccountCardsTests {
    static func buttons(in view: NSView) -> [NSButton] {
        (view as? NSButton).map { [$0] } ?? view.subviews.flatMap { buttons(in: $0) }
    }
    static func texts(in view: NSView) -> [String] {
        var out: [String] = []
        if let field = view as? NSTextField { out.append(field.stringValue) }
        if let chip = view as? StatusChip { out.append(chip.text) }
        for sub in view.subviews { out.append(contentsOf: texts(in: sub)) }
        return out
    }
    static func hitTestable(_ button: NSButton, in root: NSView) -> Bool {
        var view: NSView = button
        var point = NSPoint(x: button.bounds.midX, y: button.bounds.midY)
        while let parent = view.superview {
            point = view.convert(point, to: parent)
            guard parent.bounds.contains(point) else { return false }
            if parent === root { return true }
            view = parent
        }
        return false
    }
    static func main() {
        _ = NSApplication.shared
        let receiver = AccountActionReceiver()
        let account = HelperAccount(id: "account-stable-id", name: "a***@example.test", provider_names: [],
                                    selected: false, default: false, quota_status: "unknown", quota: nil)
        let metadata: [String: Any] = ["managed": true, "sources": ["helper", "cc_switch"],
                                      "bindings": [], "current_scopes": ["desktop"]]
        for width: CGFloat in [500, 640] {
            let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: width, height: 480),
                                  styleMask: [.titled], backing: .buffered, defer: false)
            let root = window.contentView!
            let card = QuotaCard(account: account, index: 0, metadata: metadata, actionsEnabled: true, desktopOwner: .native,
                                 target: receiver, detailAction: #selector(AccountActionReceiver.detail(_:)),
                                 applyAction: #selector(AccountActionReceiver.apply(_:)),
                                 authorizeAction: #selector(AccountActionReceiver.authorize(_:)))
            root.addSubview(card); card.translatesAutoresizingMaskIntoConstraints = false
            NSLayoutConstraint.activate([card.leadingAnchor.constraint(equalTo: root.leadingAnchor, constant: 8),
                                         card.trailingAnchor.constraint(equalTo: root.trailingAnchor, constant: -8),
                                         card.topAnchor.constraint(equalTo: root.topAnchor, constant: 8)])
            root.layoutSubtreeIfNeeded()
            let switches = buttons(in: card).compactMap { $0 as? AccountScopeButton }
            precondition(switches.count == 2 && switches.allSatisfy { $0.isEnabled && hitTestable($0, in: root) },
                         "Both per-client switch buttons must be visible and hit-testable")
            precondition(Set(switches.map(\.scope)) == Set(["desktop", "cli"]))
            switches.first(where: { $0.scope == "cli" })!.performClick(nil)
            precondition(receiver.applied.last?.0 == account.id && receiver.applied.last?.1 == "cli",
                         "A switch must carry the stable account ID and selected client")
            window.close()
        }
        let blocked = QuotaCard(account: account, index: 0, metadata: metadata, actionsEnabled: false, desktopOwner: .native,
                                target: receiver, detailAction: #selector(AccountActionReceiver.detail(_:)),
                                applyAction: #selector(AccountActionReceiver.apply(_:)),
                                authorizeAction: #selector(AccountActionReceiver.authorize(_:)))
        precondition(buttons(in: blocked).compactMap { $0 as? AccountScopeButton }.allSatisfy { !$0.isEnabled },
                     "Unavailable or pending account state must disable both switches")
        for owner: DesktopAccountOwner in [.ccSwitch, .unknown] {
            let owned = QuotaCard(account: account, index: 0, metadata: metadata, actionsEnabled: true, desktopOwner: owner,
                                  target: receiver, detailAction: #selector(AccountActionReceiver.detail(_:)),
                                  applyAction: #selector(AccountActionReceiver.apply(_:)),
                                  authorizeAction: #selector(AccountActionReceiver.authorize(_:)))
            let switches = buttons(in: owned).compactMap { $0 as? AccountScopeButton }
            let desktop = switches.first(where: { $0.scope == "desktop" })!
            precondition(!desktop.isEnabled && desktop.toolTip == owner.blockedMessage,
                         "Unowned or unknown Desktop must be disabled with an explanation")
            precondition(switches.first(where: { $0.scope == "cli" })?.isEnabled == true,
                         "Desktop ownership must not disable CLI")
        }
        let external = QuotaCard(account: account, index: 0, metadata: ["managed": false, "sources": ["cc_switch"]],
                                 actionsEnabled: true, desktopOwner: .ccSwitch, target: receiver,
                                 detailAction: #selector(AccountActionReceiver.detail(_:)),
                                 applyAction: #selector(AccountActionReceiver.apply(_:)),
                                 authorizeAction: #selector(AccountActionReceiver.authorize(_:)))
        let externalButtons = buttons(in: external)
        precondition(externalButtons.compactMap { $0 as? AccountScopeButton }.isEmpty,
                     "CC Switch-only accounts must not acquire helper switch permissions")
        precondition(externalButtons.contains { $0.title == "前往授权管理" && $0.isEnabled })
        let ambiguous = QuotaCard(account: account, index: 0,
                                  metadata: ["managed": true, "sources": ["helper"], "bindings": [["scope": "desktop", "state": "needs_reapply"]], "current_scopes": ["desktop", "cli"], "expired_scopes": ["cli"]],
                                  actionsEnabled: true, desktopOwner: .unknown, target: receiver,
                                  detailAction: #selector(AccountActionReceiver.detail(_:)),
                                  applyAction: #selector(AccountActionReceiver.apply(_:)),
                                  authorizeAction: #selector(AccountActionReceiver.authorize(_:)))
        precondition(!texts(in: ambiguous).contains { $0.contains("当前生效") },
                     "File matching in current_scopes must never be presented as an active running identity")
        var cardClicks = 0
        let entryCard = QuickEntryCard(title: "账号额度", symbol: "person.2.fill", tint: .systemBlue, subtitle: "测试", badgeText: "1") {
            cardClicks += 1
        }
        let testWindow = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 300, height: 100), styleMask: [.titled], backing: .buffered, defer: false)
        testWindow.contentView?.addSubview(entryCard)
        precondition(entryCard.acceptsFirstResponder && testWindow.makeFirstResponder(entryCard),
                     "QuickEntryCard must accept keyboard focus")
        precondition(entryCard.accessibilityPerformPress() && cardClicks == 1,
                     "QuickEntryCard must support accessibilityPerformPress")
        if let spaceEvent = NSEvent.keyEvent(with: .keyDown, location: .zero, modifierFlags: [], timestamp: 0, windowNumber: 0, context: nil, characters: " ", charactersIgnoringModifiers: " ", isARepeat: false, keyCode: 49) {
            entryCard.keyDown(with: spaceEvent)
        }
        precondition(cardClicks == 2, "QuickEntryCard must activate on Space keyDown")
        testWindow.close()
        print("Main account card routing, guards, and layout passed")
    }
}
