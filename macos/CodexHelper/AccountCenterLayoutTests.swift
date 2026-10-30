import Cocoa

final class FlippedDocument: NSView { override var isFlipped: Bool { true } }

@main
enum AccountCenterLayoutTests {
    static func main() {
        _ = NSApplication.shared
        let controller = AccountCenterController(executable: URL(fileURLWithPath: "/usr/bin/true"), onChanged: {})
        for size in [NSSize(width: 600, height: 480), NSSize(width: 540, height: 380),
                     NSSize(width: 800, height: 620)] {
            controller.testResize(size)
            precondition(controller.testLoginButtonBounds(), "Login buttons must be inside every ancestor's hit-test bounds")
        }
        let externallyCurrent = accountScopePresentation(scope: "desktop", title: "Desktop", bindings: [], currentScopes: ["desktop"])
        precondition(externallyCurrent.0 == "Desktop · 当前" && externallyCurrent.2.contains("只读"), "An external switch must be shown as current without claiming a helper binding")
        let staleBinding = accountScopePresentation(scope: "desktop", title: "Desktop", bindings: [["scope": "desktop", "state": "unknown"]], currentScopes: [])
        precondition(staleBinding.0.contains("待核查"), "An externally changed helper binding must remain visibly unresolved")
        let reapply = accountScopePresentation(scope: "cli", title: "CLI", bindings: [["scope": "cli", "state": "needs_reapply"]], currentScopes: ["cli"])
        precondition(reapply.0.contains("需重新应用"), "Current file detection must not erase a required reapplication")
        let expired = accountScopePresentation(scope: "cli", title: "CLI", bindings: [], currentScopes: ["cli"], expiredScopes: ["cli"])
        precondition(expired.0.contains("已过期"), "An expired local credential must not be presented as an active login")
        let owned = accountScopePresentation(scope: "desktop", title: "Desktop", bindings: [["scope": "desktop", "state": "bound"]], currentScopes: ["desktop"], desktopOwner: .ccSwitch)
        precondition(owned.0.contains("CC Switch") && !owned.2.contains("绑定也一致"), "CC Switch ownership must override stale binding presentation")
        let unknown = accountScopePresentation(scope: "desktop", title: "Desktop", bindings: [], currentScopes: ["desktop"], desktopOwner: .unknown)
        precondition(unknown.0.contains("待核查"), "Unknown Desktop ownership must not appear managed")
        print("Account window button geometry checks passed")
    }
}
