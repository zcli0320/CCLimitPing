import Cocoa
import Foundation

final class FlippedDocument: NSView { override var isFlipped: Bool { true } }

@main
enum AuthorizationFlowTests {
    static func wait(_ condition: () -> Bool) -> Bool {
        let end = Date().addingTimeInterval(8)
        while Date() < end {
            if condition() { return true }
            RunLoop.current.run(until: Date().addingTimeInterval(0.05))
        }
        return condition()
    }

    static func main() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent("cclimitping-authorization-ui-" + UUID().uuidString)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        defer { try? FileManager.default.removeItem(at: root) }
        let service = root.appendingPathComponent("fake-manager.py")
        let source = """
        #!/usr/bin/python3
        import json,sys
        for line in sys.stdin:
            request=json.loads(line)
            method=request.get('method')
            if method=='list':
                result={'accounts':[],'pending':False,'scope_observations':{},'cc_switch_status':'absent'}
            elif method=='login_start':
                result={'flow_id':'fixture','url':'https://auth.openai.com/fixture','user_code':'TEST-CODE' if request.get('mode')=='device' else '','interval':3}
            elif method=='login_poll':
                result={'state':'waiting'}
            elif method=='login_cancel':
                result={'state':'cancelled'}
            else:
                result={}
            print(json.dumps({'id':request['id'],'result':result}),flush=True)
        """
        try source.write(to: service, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: service.path)
        setenv("CODEX_HELPER_TEST_HOME", root.path, 1)
        _ = NSApplication.shared
        NSApp.setActivationPolicy(.accessory)
        let controller = AccountCenterController(executable: service, onChanged: {})
        defer { controller.close() }
        controller.testStartObserving()
        controller.testStartLogin("browser")
        precondition(wait { controller.testLoginActive && controller.testLoginMessage.contains("浏览器") },
                     "Browser authorization must reach the simulated login flow")
        controller.testCancelLogin()
        precondition(wait { !controller.testLoginActive && controller.testLoginMessage.contains("取消") },
                     "Browser authorization must cancel cleanly")
        controller.testStartLogin("device")
        precondition(wait { controller.testLoginActive && controller.testLoginMessage.contains("TEST-CODE") },
                     "Device authorization must display the simulated device code")
        controller.testCancelLogin()
        precondition(wait { !controller.testLoginActive && controller.testLoginMessage.contains("取消") },
                     "Device authorization must cancel cleanly")
        print("Authorization browser/device UI flows passed with a local fake manager")
    }
}
