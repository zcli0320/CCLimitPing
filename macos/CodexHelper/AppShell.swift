import Cocoa
import SwiftUI

struct CodexHelperSwiftUIApp: App {
    @NSApplicationDelegateAdaptor(Assistant.self) var appDelegate
    @ObservedObject var model = CodexHelperAppModel.shared

    var body: some Scene {
        Settings {
            CodexHelperSettingsView(model: model)
        }
        .commands {
            SidebarCommands()
            CodexHelperCommands(model: model)
        }
    }
}

struct CodexHelperCommands: Commands {
    @ObservedObject var model: CodexHelperAppModel

    var body: some Commands {
        CommandGroup(replacing: .newItem) {
            Button("派发新任务…") {
                if model.selectedDestination.canCreateTaskCommand {
                    model.createTask()
                }
            }
            .keyboardShortcut("n", modifiers: .command)
            .disabled(!model.selectedDestination.canCreateTaskCommand || !model.canCreateTaskInOrchestra)

            Button("新建项目…") {
                if model.selectedDestination.canCreateProjectCommand {
                    model.createProject()
                }
            }
            .keyboardShortcut("n", modifiers: [.command, .shift])
            .disabled(!model.selectedDestination.canCreateProjectCommand || !model.canCreateProjectInOrchestra)
        }

        CommandMenu("导航") {
            ForEach(NavigationDestination.allCases) { destination in
                Button(destination.title) {
                    model.navigate(to: destination)
                }
                .keyboardShortcut(KeyEquivalent(destination.shortcutKey), modifiers: .command)
            }

            Divider()

            Button("刷新当前页面") {
                model.refreshCurrentPage()
            }
            .keyboardShortcut("r", modifiers: .command)

            Button("搜索会话…") {
                if model.selectedDestination.canFocusSearchCommand {
                    model.focusSessionSearch()
                }
            }
            .keyboardShortcut("f", modifiers: .command)
            .disabled(!model.selectedDestination.canFocusSearchCommand)
        }
    }
}

struct SemanticStatusBadge: View {
    let text: String
    let tint: NSColor

    var body: some View {
        if !text.isEmpty {
            Text(text)
                .font(.system(size: 11, weight: .medium))
                .foregroundColor(Color(nsColor: tint))
                .padding(.horizontal, 7)
                .padding(.vertical, 2)
                .background(
                    Capsule()
                        .fill(Color(nsColor: tint).opacity(0.14))
                )
                .accessibilityLabel(text)
        }
    }
}

struct NativeCardContainer<Content: View>: View {
    var title: String? = nil
    var subtitle: String? = nil
    var systemImage: String? = nil
    var badgeText: String? = nil
    var badgeTint: NSColor = .secondaryLabelColor
    var spacing: CGFloat = 10
    var padding: CGFloat = 14
    @ViewBuilder let content: () -> Content

    var body: some View {
        VStack(alignment: .leading, spacing: spacing) {
            if let title, !title.isEmpty {
                VStack(alignment: .leading, spacing: 3) {
                    HStack(spacing: 8) {
                        if let systemImage, !systemImage.isEmpty {
                            Image(systemName: systemImage)
                                .foregroundColor(.accentColor)
                                .font(.system(size: 13.5, weight: .semibold))
                        }
                        Text(title)
                            .font(.system(size: 13, weight: .semibold))
                            .lineLimit(1)
                        Spacer(minLength: 6)
                        if let badgeText, !badgeText.isEmpty {
                            SemanticStatusBadge(text: badgeText, tint: badgeTint)
                        }
                    }
                    if let subtitle, !subtitle.isEmpty {
                        Text(subtitle)
                            .font(.system(size: 11.5))
                            .foregroundColor(.secondary)
                            .lineLimit(2)
                    }
                }
                Divider()
            }
            content()
        }
        .padding(padding)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(
            RoundedRectangle(cornerRadius: 10, style: .continuous)
                .fill(Color(nsColor: .controlBackgroundColor))
        )
        .overlay(
            RoundedRectangle(cornerRadius: 10, style: .continuous)
                .stroke(Color(nsColor: .separatorColor).opacity(0.55), lineWidth: 0.5)
        )
    }
}

struct MainSplitView: View {
    @ObservedObject var model: CodexHelperAppModel

    private var destinationBinding: Binding<NavigationDestination?> {
        Binding(
            get: { model.selectedDestination },
            set: { newValue in
                if let dest = newValue {
                    model.navigate(to: dest)
                }
            }
        )
    }

    var body: some View {
        NavigationSplitView(columnVisibility: $model.sidebarVisibility) {
            sidebarContent
                .navigationSplitViewColumnWidth(min: 190, ideal: 215, max: 260)
        } detail: {
            detailContent
                .frame(maxWidth: .infinity, maxHeight: .infinity)
        }
        .background(
            GeometryReader { proxy in
                Color.clear
                    .onAppear {
                        model.updateWindowWidth(proxy.size.width)
                    }
                    .onChange(of: proxy.size.width) { newWidth in
                        model.updateWindowWidth(newWidth)
                    }
            }
        )
    }

    private var sidebarContent: some View {
        List(NavigationDestination.allCases, selection: destinationBinding) { dest in
            Label {
                HStack(spacing: 6) {
                    Text(dest.title)
                        .lineLimit(1)
                    Spacer(minLength: 4)
                    if let badge = sidebarBadge(for: dest), !badge.isEmpty {
                        Text(badge)
                            .font(.system(size: 11, weight: .medium).monospacedDigit())
                            .foregroundColor(.secondary)
                    }
                }
            } icon: {
                Image(systemName: dest.symbol)
            }
            .tag(dest)
            .accessibilityLabel("\(dest.title) \(sidebarBadge(for: dest) ?? "")")
        }
        .listStyle(.sidebar)
        .safeAreaInset(edge: .bottom) {
            VStack(alignment: .leading, spacing: 6) {
                Divider()
                HStack(spacing: 6) {
                    Circle()
                        .fill(model.statusIsWarning ? Color.orange : Color.green)
                        .frame(width: 7, height: 7)
                    Text("v\(model.versionString) (\(model.buildString))")
                        .font(.caption)
                        .foregroundColor(.secondary)
                    Spacer()
                    Button {
                        model.openSettingsWindow()
                    } label: {
                        Image(systemName: "gearshape")
                    }
                    .buttonStyle(.borderless)
                    .help("打开设置 (⌘,)")
                    .accessibilityLabel("设置")
                }
                if !model.feedbackLine.isEmpty {
                    Text(model.feedbackLine)
                        .font(.caption2)
                        .foregroundColor(.secondary)
                        .lineLimit(2)
                }
            }
            .padding(.horizontal, 12)
            .padding(.vertical, 8)
        }
    }

    @ViewBuilder
    private var detailContent: some View {
        switch model.selectedDestination {
        case .overview:
            OverviewPageView(model: model)
        case .accounts:
            AccountsPageView(model: model)
        case .sessions:
            SessionsPageView(model: model)
        case .projects:
            ProjectsPageView(model: model)
        }
    }

    @ToolbarContentBuilder
    private var toolbarContent: some ToolbarContent {
        ToolbarItemGroup(placement: .primaryAction) {
            switch model.selectedDestination {
            case .overview:
                Button {
                    model.openAuthorizationCenter()
                } label: {
                    Label("授权管理", systemImage: "person.crop.circle.badge.plus")
                }
                .help("打开账号授权管理")
                .disabled(model.isFixtureMode)
            case .accounts:
                Button {
                    model.openAuthorizationCenter()
                } label: {
                    Label("授权管理", systemImage: "person.crop.circle.badge.plus")
                }
                .help("添加或管理账号授权")
                .disabled(model.isFixtureMode)

                Button {
                    model.editWarmupSchedule()
                } label: {
                    Label("预热时段", systemImage: "calendar.badge.clock")
                }
                .help("自定义自动预热允许时段")
                .disabled(!model.canEditWarmupSchedule)
            case .sessions:
                Button {
                    model.checkHandoffPreflight()
                } label: {
                    Label("检查所选会话", systemImage: "checkmark.shield")
                }
                .help("只读检查所选会话的切号交接条件")
                .disabled(!model.canCheckHandoffPreflight)
            case .projects:
                Button {
                    model.createTask()
                } label: {
                    Label("新任务…", systemImage: "plus.circle")
                }
                .help("派发新任务 (⌘N)")
                .disabled(!model.canCreateTaskInOrchestra)

                Button {
                    model.createProject()
                } label: {
                    Label("新建项目…", systemImage: "folder.badge.plus")
                }
                .help("新建项目工作群 (⇧⌘N)")
                .disabled(!model.canCreateProjectInOrchestra)

                if model.ownedRunnerRunning {
                    Button {
                        model.stopRunner()
                    } label: {
                        Label("停止执行器", systemImage: "stop.fill")
                    }
                    .help("停止本小助手启动的执行器")
                    .disabled(!model.canStopRunner)
                } else {
                    Button {
                        model.startRunner()
                    } label: {
                        Label("启动执行器", systemImage: "play.fill")
                    }
                    .help("启动无人值守协作执行器")
                    .disabled(!model.canStartRunner)
                }
            }

            Button {
                model.refreshCurrentPage()
            } label: {
                Label(model.isLoading ? "刷新中…" : "刷新", systemImage: "arrow.clockwise")
            }
            .help("刷新当前页面 (⌘R)")
            .disabled(model.isLoading && model.selectedDestination != .projects)

            Button {
                model.openSettingsWindow()
            } label: {
                Label("设置", systemImage: "gearshape")
            }
            .help("打开设置 (⌘,)")
        }
    }

    private func sidebarBadge(for dest: NavigationDestination) -> String? {
        switch dest {
        case .overview:
            return nil
        case .accounts:
            return model.accounts.isEmpty ? nil : "\(model.accounts.count)"
        case .sessions:
            if model.handoffActive { return "切号" }
            if model.resumeEnabledForSelected { return "续跑" }
            if model.observingSession { return "观察" }
            let count = model.activeScope == "cli" ? model.cliSessionCount : model.desktopSessionCount
            return count > 0 ? "\(count)" : nil
        case .projects:
            let counts = model.orchestraTaskCounts
            if counts.runningCount > 0 { return "\(counts.runningCount) 执行" }
            if counts.queuedCount > 0 { return "\(counts.queuedCount) 排队" }
            if counts.receiptCount > 0 { return "\(counts.receiptCount) 待验" }
            return model.orchestraProjects.isEmpty ? nil : "\(model.orchestraProjects.count)"
        }
    }

    private func subtitle(for dest: NavigationDestination) -> String {
        switch dest {
        case .overview:
            return model.statusLine.components(separatedBy: .newlines).first ?? ""
        case .accounts:
            return model.accounts.isEmpty ? "尚未添加账号" : "已同步 \(model.accounts.count) 个账号 · \(model.warmupChipText)"
        case .sessions:
            let scopeName = model.activeScope == "cli" ? "CLI" : "Desktop"
            return "\(scopeName) · \(model.sessionCountSummary)"
        case .projects:
            let counts = model.orchestraTaskCounts
            let runnerText = model.runnerActive ? (model.runnerPaused ? "执行器已暂停" : "执行器运行中") : "执行器未运行"
            return "\(model.orchestraProjects.count) 个项目 · \(counts.runningCount) 执行中 · \(counts.queuedCount) 排队中 · \(runnerText)"
        }
    }
}
