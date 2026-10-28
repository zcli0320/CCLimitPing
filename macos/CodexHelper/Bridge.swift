import Foundation
import Darwin

enum BridgeError: Error, Equatable { case timedOut, outputLimit, failed }

private final class TimeoutFlag {
    private let lock = NSLock()
    private var value = false
    func mark() { lock.lock(); value = true; lock.unlock() }
    var fired: Bool { lock.lock(); defer { lock.unlock() }; return value }
}

enum BridgeClient {
    // The watchdog belongs to our helper, never a user's Codex client.
    // A blocked pipe/file must not leave the GUI stuck in "refreshing".
    static func read(executable: URL, arguments: [String], timeout: TimeInterval = 20,
                     maximumBytes: Int = 16 << 20, onStart: ((Process) -> Void)? = nil) throws -> Data {
        let process = Process()
        process.executableURL = executable; process.arguments = arguments
        let output = Pipe(); process.standardOutput = output; process.standardError = FileHandle.nullDevice
        try process.run()
        onStart?(process)
        let flag = TimeoutFlag()
        let watchdog = DispatchSource.makeTimerSource(queue: DispatchQueue.global(qos: .utility))
        watchdog.schedule(deadline: .now() + timeout)
        watchdog.setEventHandler {
            guard process.isRunning else { return }
            flag.mark(); process.terminate()
            DispatchQueue.global().asyncAfter(deadline: .now() + 0.3) {
                if process.isRunning { kill(process.processIdentifier, SIGKILL) }
            }
        }
        watchdog.resume()
        defer {
            watchdog.cancel()
            if process.isRunning { kill(process.processIdentifier, SIGKILL) }
            process.waitUntilExit()
            try? output.fileHandleForReading.close()
        }
        var data = Data()
        while true {
            let chunk = output.fileHandleForReading.availableData
            if chunk.isEmpty { break }
            guard chunk.count <= maximumBytes - data.count else { throw BridgeError.outputLimit }
            data.append(chunk)
        }
        process.waitUntilExit()
        if flag.fired { throw BridgeError.timedOut }
        guard process.terminationStatus == 0 else { throw BridgeError.failed }
        return data
    }
}
