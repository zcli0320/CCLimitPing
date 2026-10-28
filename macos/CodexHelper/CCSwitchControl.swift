import Cocoa
import ApplicationServices

// Narrow bridge to CC Switch's own button. Credentials never enter this
// process. Backend verification decides whether the requested switch succeeded.
enum CCSwitchControl {
    enum Failure: String, Error { case permissionRequired, unavailable, ambiguousTarget, targetMissing, switchUnavailable, timeout }
    static func attribute(_ element: AXUIElement, _ name: CFString) -> CFTypeRef? {
        var value: CFTypeRef?
        guard AXUIElementCopyAttributeValue(element, name, &value) == .success else { return nil }
        return value
    }
    static func label(_ element: AXUIElement) -> String {
        for key in [kAXTitleAttribute, kAXDescriptionAttribute, kAXValueAttribute] {
            if let value = attribute(element, key as CFString) as? String, !value.isEmpty { return value }
        }
        return ""
    }
    static func role(_ element: AXUIElement) -> String { attribute(element, kAXRoleAttribute as CFString) as? String ?? "" }
    static func flattened(_ root: AXUIElement, deadline: Date) throws -> [AXUIElement] {
        var output: [AXUIElement] = []
        func visit(_ element: AXUIElement, _ depth: Int) throws {
            guard Date() < deadline, output.count < 12000, depth < 40 else { throw Failure.timeout }
            output.append(element)
            if let children = attribute(element, kAXChildrenAttribute as CFString) as? [AXUIElement] {
                for child in children { try visit(child, depth + 1) }
            }
        }
        try visit(root, 0)
        return output
    }
    static func run(provider: String, inspectOnly: Bool) throws -> String {
        guard !provider.isEmpty, provider.count <= 256, !provider.contains("\n"), !provider.contains("\0") else { throw Failure.ambiguousTarget }
        guard AXIsProcessTrusted() else { throw Failure.permissionRequired }
        let appURL = URL(fileURLWithPath: "/Applications/CC Switch.app")
        let identifier = Bundle(url: appURL)?.bundleIdentifier
        guard let identifier = identifier else { throw Failure.unavailable }
        let apps = NSRunningApplication.runningApplications(withBundleIdentifier: identifier).filter { $0.bundleURL?.standardizedFileURL == appURL.standardizedFileURL }
        guard apps.count == 1 else { throw Failure.unavailable }
        let root = AXUIElementCreateApplication(apps[0].processIdentifier)
        AXUIElementSetMessagingTimeout(root, 1.0)
        let deadline = Date().addingTimeInterval(10)
        var elements = try flattened(root, deadline: deadline)
        let tabs = elements.filter { role($0) == kAXButtonRole && label($0) == "Codex" }
        guard tabs.count == 1 else { throw Failure.unavailable }
        guard AXUIElementPerformAction(tabs[0], kAXPressAction as CFString) == .success else { throw Failure.unavailable }
        repeat {
            RunLoop.current.run(until: Date().addingTimeInterval(0.15))
            elements = try flattened(root, deadline: deadline)
            let headings = elements.indices.filter { role(elements[$0]) == "AXHeading" && label(elements[$0]) == provider }
            if headings.count > 1 { throw Failure.ambiguousTarget }
            if let start = headings.first {
                let end = elements.indices.first { $0 > start && role(elements[$0]) == "AXHeading" } ?? elements.endIndex
                let buttons = elements[(start + 1)..<end].filter { role($0) == kAXButtonRole && label($0) == "切换" }
                guard buttons.count == 1 else { throw Failure.switchUnavailable }
                if inspectOnly { return "ready" }
                guard AXUIElementPerformAction(buttons[0], kAXPressAction as CFString) == .success else { throw Failure.switchUnavailable }
                return "requested"
            }
        } while Date() < deadline
        throw Failure.targetMissing
    }
    static func handle(arguments: [String]) -> Bool {
        guard let index = arguments.firstIndex(where: { $0 == "--ccswitch-inspect" || $0 == "--ccswitch-switch" }) else { return false }
        var result: [String: Any] = ["ok": false]
        do {
            guard index + 1 < arguments.count else { throw Failure.targetMissing }
            result = ["ok": true, "status": try run(provider: arguments[index + 1], inspectOnly: arguments[index] == "--ccswitch-inspect")]
        } catch let error as Failure { result["code"] = error.rawValue }
        catch { result["code"] = "unavailable" }
        if let data = try? JSONSerialization.data(withJSONObject: result), let text = String(data: data, encoding: .utf8) { print(text) }
        return true
    }
}
