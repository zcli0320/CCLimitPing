import Cocoa
import Foundation

final class FlippedDocument: NSView { override var isFlipped: Bool { true } }

@main
enum AccountCenterSyncTests {
    static func jwt(_ payload: [String: Any]) throws -> String {
        func b64(_ data: Data) -> String { data.base64EncodedString().replacingOccurrences(of: "+", with: "-").replacingOccurrences(of: "/", with: "_").replacingOccurrences(of: "=", with: "") }
        return b64(Data("{\"alg\":\"none\"}".utf8)) + "." + b64(try JSONSerialization.data(withJSONObject: payload)) + "."
    }
    static func put(_ value: Any, at url: URL) throws {
        try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        try JSONSerialization.data(withJSONObject: value).write(to: url, options: .atomic)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
    }
    static func wait(_ condition: () -> Bool) -> Bool {
        let end = Date().addingTimeInterval(12)
        while Date() < end {
            if condition() { return true }
            RunLoop.current.run(until: Date().addingTimeInterval(0.1))
        }
        return condition()
    }
    static func main() throws {
        guard CommandLine.arguments.count == 2 else { fatalError("pass isolated account manager executable") }
        let root = FileManager.default.temporaryDirectory.appendingPathComponent("cclimitping-account-sync-test-" + UUID().uuidString)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        defer { try? FileManager.default.removeItem(at: root) }
        try Data().write(to: root.appendingPathComponent(".codex-helper-test-root"))
        setenv("CODEX_HELPER_TEST_HOME", root.path, 1)
        _ = NSApplication.shared
        NSApp.setActivationPolicy(.accessory)
        let exp = Int(Date().timeIntervalSince1970) + 3600
        let idA = try jwt(["sub":"alice"]), idB = try jwt(["sub":"bob"])
        let access = try jwt(["exp": exp])
        func entry(_ key: String, _ email: String, _ workspace: String, _ token: String) -> [String: Any] {
            ["account_id":key,"email":email,"chatgpt_account_id":workspace,"id_token":token,"refresh_token":"synthetic-refresh","authenticated_at":Int(Date().timeIntervalSince1970)]
        }
        func auth(_ workspace: String, _ token: String) -> [String: Any] {
            ["auth_mode":"chatgpt","tokens":["id_token":token,"access_token":access,"refresh_token":"synthetic-refresh","account_id":workspace],"last_refresh":ISO8601DateFormatter().string(from: Date())]
        }
        let helperStore = root.appendingPathComponent("Library/Application Support/CodexHelper-AccountPreview/accounts/codex_oauth_auth.json")
        let ccStore = root.appendingPathComponent(".cc-switch/codex_oauth_auth.json")
        let desktop = root.appendingPathComponent(".codex/auth.json")
        try put(["version":2,"accounts":["a":entry("a","alice@example.test","workspace-a",idA)]], at: helperStore)
        try put(["version":2,"accounts":["cc-a":entry("cc-a","alice@example.test","workspace-a",idA),"cc-b":entry("cc-b","bob@example.test","workspace-b",idB)]], at: ccStore)
        try put(auth("workspace-a",idA), at: desktop)
        for path in [".codex/config.toml", ".codex-cli/config.toml"] {
            let url = root.appendingPathComponent(path)
            try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            try Data("cli_auth_credentials_store = \"file\"\n".utf8).write(to: url)
            try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
        }
        let helperBefore = try Data(contentsOf: helperStore), ccBefore = try Data(contentsOf: ccStore)
        let controller = AccountCenterController(executable: URL(fileURLWithPath: CommandLine.arguments[1]), onChanged: {})
        defer { controller.close() }
        controller.testStartObserving()
        precondition(wait { controller.testAccountCount() == 2 && controller.testScopeLabel(name: "a***@example.test", scope: "desktop") == "Desktop · 当前" }, "Initial merged account/current state not displayed")
        try put(auth("workspace-b",idB), at: desktop)
        precondition(wait { controller.testScopeLabel(name: "b***@example.test", scope: "desktop") == "Desktop · 当前" && controller.testScopeLabel(name: "a***@example.test", scope: "desktop") == "Desktop · 未绑定" }, "External switch did not appear automatically")
        let helperAfter = try Data(contentsOf: helperStore), ccAfter = try Data(contentsOf: ccStore)
        precondition(helperAfter == helperBefore && ccAfter == ccBefore, "A read-only refresh changed an account store")
        precondition(!FileManager.default.fileExists(atPath: helperStore.deletingLastPathComponent().appendingPathComponent("bindings.json").path), "Observing an external switch created a helper binding")
        try put(["version":2,"accounts":["cc-a":entry("cc-a","alice@example.test","workspace-a",idA)]], at: ccStore)
        precondition(wait { controller.testAccountCount() == 1 && controller.testScopeLabel(name: "b***@example.test", scope: "desktop") == nil }, "External account removal did not update the window")
        let bindings = helperStore.deletingLastPathComponent().appendingPathComponent("bindings.json")
        try put(["version":1,"desktop":["account_id":"a","refresh_digest":"synthetic","needs_reapply":false],"cli":NSNull()], at: bindings)
        try put([:], at: root.appendingPathComponent(".cc-switch/settings.json"))
        precondition(wait { controller.testClearDesktopVisible && controller.testScopeLabel(name: "a***@example.test", scope: "desktop")?.contains("CC Switch") == true }, "Owned Desktop or clear-binding action was not displayed")
        let beforeClearStore = try Data(contentsOf: helperStore), beforeClearDesktop = try Data(contentsOf: desktop)
        controller.testClearDesktop()
        precondition(wait { !controller.testClearDesktopVisible && controller.testClearDesktopMessage.contains("绑定已清除") }, "Clear Desktop binding did not complete")
        let bindingValue = try JSONSerialization.jsonObject(with: Data(contentsOf: bindings)) as! [String: Any]
        let afterClearStore = try Data(contentsOf: helperStore), afterClearDesktop = try Data(contentsOf: desktop)
        precondition(bindingValue["desktop"] is NSNull && afterClearStore == beforeClearStore && afterClearDesktop == beforeClearDesktop, "Clear binding changed account or Desktop login")
        try put(["version":1,"desktop":["account_id":"a","refresh_digest":"synthetic","needs_reapply":true],"cli":NSNull()], at: bindings)
        let beforeRefusal = try Data(contentsOf: bindings)
        precondition(wait { controller.testClearDesktopVisible }, "Reapply binding action was not displayed")
        controller.testClearDesktop()
        precondition(wait { controller.testClearDesktopMessage.contains("暂不可安全清理") }, "Clear refusal reason was not displayed")
        let afterRefusal = try Data(contentsOf: bindings)
        precondition(beforeRefusal == afterRefusal, "Clear refusal changed the binding")
        try Data("{".utf8).write(to: root.appendingPathComponent(".cc-switch/settings.json"), options: .atomic)
        precondition(wait { controller.testClearDesktopVisible && !controller.testClearDesktopEnabled && controller.testScopeLabel(name: "a***@example.test", scope: "desktop")?.contains("待核查") == true }, "Unknown owner did not disable clearing and display a warning")
        print("Isolated real-manager UI synchronization passed")
    }
}
