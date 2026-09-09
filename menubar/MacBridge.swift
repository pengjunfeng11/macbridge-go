import AppKit

@main
final class MacBridge: NSObject, NSApplicationDelegate, NSMenuDelegate {
    static func main() {
        let app = NSApplication.shared
        let delegate = MacBridge()
        app.delegate = delegate
        app.setActivationPolicy(.accessory)
        withExtendedLifetime(delegate) { app.run() }
    }

    private let fm = FileManager.default
    private let env = ProcessInfo.processInfo.environment
    private var item: NSStatusItem!
    private var menu = NSMenu()
    private var status = NSMenuItem(title: "Stopped", action: nil, keyEquivalent: "")
    private var toggle = NSMenuItem(title: "Start", action: #selector(toggleBridge), keyEquivalent: "s")
    private var strict = NSMenuItem(title: "Strict approvals", action: #selector(toggleStrict), keyEquivalent: "")
    private var owner: Process?
    private var busy = false
    private var reachable = false
    private var terminating = false
    private var timer: Timer?
    private var data: URL { URL(fileURLWithPath: env["MAC_DEV_BRIDGE_DATA_DIR"] ?? NSHomeDirectory() + "/Library/Application Support/MacDeveloperBridge") }
    private var logs: URL { URL(fileURLWithPath: env["MAC_DEV_BRIDGE_LOG_DIR"] ?? NSHomeDirectory() + "/Library/Logs/MacDeveloperBridge") }
    private var binary: URL { Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/macbridge-core") }
    private var configuredPort: Int { Int(env["MAC_DEV_BRIDGE_HTTP_PORT"] ?? "") ?? (read("runtime.json")["httpPort"] as? Int) ?? 8787 }
    private var port: Int { (read("desktop-state.json")["httpPort"] as? String).flatMap(Int.init) ?? (read("desktop-state.json")["httpPort"] as? Int) ?? configuredPort }
    private var endpoint: String {
        if let active = read("desktop-state.json")["mcpURL"] as? String { return active }
        let configured = env["MAC_DEV_BRIDGE_PUBLIC_URL"] ?? (read("runtime.json")["publicURL"] as? String) ?? ""
        return configured.isEmpty ? "http://127.0.0.1:\(configuredPort)/mcp" : configured.trimmingCharacters(in: CharacterSet(charactersIn: "/")) + "/mcp"
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        item = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        item.button?.image = NSImage(systemSymbolName: "terminal", accessibilityDescription: "MacBridge")
        item.button?.toolTip = "MacBridge Go"
        menu.delegate = self
        menu.addItem(status)
        menu.addItem(.separator())
        menu.addItem(toggle)
        add("Copy MCP URL", #selector(copyURL))
        add("Copy OAuth Client ID", #selector(copyClient))
        add("Copy ChatGPT Setup", #selector(copySetup))
        add("Copy Bearer Token", #selector(copyToken))
        menu.addItem(.separator())
        menu.addItem(strict)
        add("Connection Settings…", #selector(settings))
        add("Rotate Token…", #selector(rotate))
        add("Install Chrome Native Host", #selector(installBrowser))
        add("Open Logs", #selector(openLogs))
        add("Reveal App in Finder", #selector(reveal))
        menu.addItem(.separator())
        add("Quit", #selector(quit), "q")
        for entry in menu.items { entry.target = self }
        item.menu = menu
        timer = Timer.scheduledTimer(withTimeInterval: 3, repeats: true) { [weak self] _ in self?.refresh() }
        refresh()
    }

    private func add(_ title: String, _ action: Selector, _ key: String = "") { menu.addItem(NSMenuItem(title: title, action: action, keyEquivalent: key)) }
    func menuNeedsUpdate(_ menu: NSMenu) { refresh() }
    private func read(_ name: String) -> [String: Any] {
        guard let bytes = try? Data(contentsOf: data.appendingPathComponent(name)), let value = try? JSONSerialization.jsonObject(with: bytes) as? [String: Any] else { return [:] }
        return value
    }
    private func save(_ name: String, _ value: [String: Any]) throws {
        try fm.createDirectory(at: data, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        let file = data.appendingPathComponent(name)
        try JSONSerialization.data(withJSONObject: value, options: [.prettyPrinted, .sortedKeys]).write(to: file, options: .atomic)
        try fm.setAttributes([.posixPermissions: 0o600], ofItemAtPath: file.path)
    }
    private func refresh() {
        strict.state = (read("settings.json")["strictApprovals"] as? Bool == true) ? .on : .off
        guard let url = URL(string: "http://127.0.0.1:\(port)/healthz") else { return }
        var request = URLRequest(url: url); request.timeoutInterval = 1
        URLSession.shared.dataTask(with: request) { [weak self] _, response, _ in
            DispatchQueue.main.async {
                guard let self else { return }
                self.reachable = (response as? HTTPURLResponse)?.statusCode == 200
                if !self.busy { self.status.title = self.reachable ? "Running · Go core" : (self.owner?.isRunning == true ? "Starting tunnel…" : "Stopped") }
                self.toggle.title = (self.reachable || self.owner?.isRunning == true) ? "Stop" : "Start"
                self.toggle.isEnabled = !self.busy
            }
        }.resume()
    }
    private func notify(_ title: String, _ text: String) {
        NSApp.activate(ignoringOtherApps: true)
        let alert = NSAlert(); alert.messageText = title; alert.informativeText = text; alert.runModal()
    }
    private func copy(_ text: String) { NSPasteboard.general.clearContents(); NSPasteboard.general.setString(text, forType: .string) }
    private func command(_ arguments: [String], completion: @escaping (Bool, String) -> Void) {
        busy = true; status.title = "Working…"; toggle.isEnabled = false
        let executable = binary
        DispatchQueue.global(qos: .userInitiated).async {
            let process = Process(); process.executableURL = executable; process.arguments = arguments
            let pipe = Pipe(); process.standardOutput = pipe; process.standardError = pipe
            var ok = false; var message = ""
            do { try process.run(); let output = pipe.fileHandleForReading.readDataToEndOfFile(); process.waitUntilExit(); ok = process.terminationStatus == 0; message = String(data: output, encoding: .utf8) ?? "" }
            catch { message = error.localizedDescription }
            DispatchQueue.main.async { self.busy = false; completion(ok, message); self.refresh() }
        }
    }
    private func start() {
        do {
            try fm.createDirectory(at: logs, withIntermediateDirectories: true)
            let log = logs.appendingPathComponent("menu.log"); if !fm.fileExists(atPath: log.path) { fm.createFile(atPath: log.path, contents: nil, attributes: [.posixPermissions: 0o600]) }
            let handle = try FileHandle(forWritingTo: log); try handle.seekToEnd()
            let process = Process(); process.executableURL = binary; process.arguments = ["desktop"]
            process.standardOutput = handle; process.standardError = handle
            process.terminationHandler = { [weak self] child in
                try? handle.close()
                DispatchQueue.main.async { if self?.owner === child { self?.owner = nil; self?.refresh(); if child.terminationStatus != 0 { self?.notify("MacBridge stopped", "See menu.log and bridge.log in Open Logs for the error.") } } }
            }
            try process.run(); owner = process; refresh()
        } catch { notify("Could not start", error.localizedDescription) }
    }
    @objc private func toggleBridge() {
        guard !busy && !terminating else { return }
        if reachable || owner?.isRunning == true { command(["stop"]) { ok, message in if !ok { self.notify("Stop incomplete", message) } } }
        else { start() }
    }
    @objc private func toggleStrict() {
        var value = read("settings.json"); value["strictApprovals"] = strict.state != .on
        do { try save("settings.json", value); refresh() } catch { notify("Could not save", error.localizedDescription) }
    }
    @objc private func copyURL() { copy(endpoint) }
    @objc private func copyClient() { if let id = read("oauth-go.json")["clientId"] as? String { copy(id) } else { notify("Start MacBridge first", "The OAuth client ID is created on first start.") } }
    @objc private func copyToken() {
        let file = env["MAC_DEV_BRIDGE_HTTP_TOKEN_FILE"].map { URL(fileURLWithPath: $0) } ?? data.appendingPathComponent("http-token")
        if env["MAC_DEV_BRIDGE_HTTP_TOKEN_FILE"] == nil, let token = env["MAC_DEV_BRIDGE_HTTP_TOKEN"], !token.isEmpty { copy(token) }
        else if let token = try? String(contentsOf: file, encoding: .utf8) { copy(token.trimmingCharacters(in: .whitespacesAndNewlines)) }
    }
    @objc private func copySetup() {
        guard let id = read("oauth-go.json")["clientId"] as? String else { notify("Start MacBridge first", "The connection details are created on first start."); return }
        copy("MCP URL: \(endpoint)\nAuthentication: OAuth\nRegistration: User-Defined OAuth Client\nClient ID: \(id)\nClient secret: leave blank\nToken endpoint auth: none\nDefault scopes: mcp\nOIDC: disabled\nAuthorize with the Bearer Token from the menu.")
    }
    @objc private func settings() {
        guard !busy && !terminating else { return }
        let value = read("runtime.json")
        let alert = NSAlert(); alert.messageText = "Connection settings"; alert.informativeText = "Applied on the next start. Auto uses a named Cloudflare tunnel when configured, otherwise a quick tunnel if cloudflared is installed."
        let view = NSView(frame: NSRect(x: 0, y: 0, width: 360, height: 116))
        let url = NSTextField(string: value["publicURL"] as? String ?? ""); url.placeholderString = "Public URL (optional)"; url.frame = NSRect(x: 0, y: 80, width: 360, height: 24); url.setAccessibilityLabel("Public URL")
        let portField = NSTextField(string: String(configuredPort)); portField.frame = NSRect(x: 0, y: 42, width: 100, height: 24); portField.setAccessibilityLabel("Local HTTP port")
        let mode = NSPopUpButton(frame: NSRect(x: 120, y: 40, width: 240, height: 28)); mode.addItems(withTitles: ["auto", "off", "named", "quick"]); mode.selectItem(withTitle: value["tunnelMode"] as? String ?? "auto"); mode.setAccessibilityLabel("Tunnel mode")
        view.addSubview(url); view.addSubview(portField); view.addSubview(mode); alert.accessoryView = view
        alert.addButton(withTitle: "Save"); alert.addButton(withTitle: "Cancel"); NSApp.activate(ignoringOtherApps: true)
        guard alert.runModal() == .alertFirstButtonReturn else { return }
        guard let portNumber = Int(portField.stringValue), (1...65535).contains(portNumber) else { notify("Invalid port", "Use a port between 1 and 65535."); return }
        let publicURL = url.stringValue.trimmingCharacters(in: .whitespacesAndNewlines).trimmingCharacters(in: CharacterSet(charactersIn: "/"))
        if !publicURL.isEmpty { guard let u = URL(string: publicURL), ["http", "https"].contains(u.scheme ?? ""), u.host?.isEmpty == false, u.user == nil, u.password == nil, u.query == nil, u.fragment == nil, u.path.isEmpty || u.path == "/" else { notify("Invalid URL", "Use an http or https origin without credentials, a path, query, or fragment."); return } }
        var updated = value; updated["publicURL"] = publicURL; updated["httpPort"] = portNumber; updated["tunnelMode"] = mode.titleOfSelectedItem
        do { try save("runtime.json", updated) } catch { notify("Could not save", error.localizedDescription) }
    }
    @objc private func rotate() {
        guard !busy && !terminating else { return }
        let alert = NSAlert(); alert.messageText = "Rotate the Bearer Token?"; alert.informativeText = "This stops the bridge and revokes existing OAuth tokens. Clients must reconnect."
        alert.addButton(withTitle: "Rotate"); alert.addButton(withTitle: "Cancel"); NSApp.activate(ignoringOtherApps: true)
        if alert.runModal() == .alertFirstButtonReturn { let restart = reachable || owner?.isRunning == true; command(["rotate-token"]) { ok, message in if ok && restart { self.start() } else if !ok { self.notify("Rotation failed", message) } } }
    }
    @objc private func installBrowser() { guard !busy && !terminating else { return }; command(["install-browser"]) { ok, message in self.notify(ok ? "Native host installed" : "Installation failed", message) } }
    @objc private func openLogs() { try? fm.createDirectory(at: logs, withIntermediateDirectories: true); NSWorkspace.shared.open(logs) }
    @objc private func reveal() { NSWorkspace.shared.activateFileViewerSelecting([Bundle.main.bundleURL]) }
    // App-menu, Dock and system termination follow the same cleanup as the menu item.
    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        if terminating { return .terminateLater }
        if busy { return .terminateCancel }
        terminating = true
        command(["stop"]) { ok, message in
            self.terminating = false
            if !ok { self.notify("Stop incomplete", message) }
            sender.reply(toApplicationShouldTerminate: ok)
        }
        return .terminateLater
    }
    @objc private func quit() { NSApp.terminate(nil) }
}
