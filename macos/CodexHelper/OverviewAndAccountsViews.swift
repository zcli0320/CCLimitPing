import Cocoa
import SwiftUI

struct OverviewPageView: View {
    @ObservedObject var model: CodexHelperAppModel

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 18) {
                overviewStatusBanner
                quickLauncherSection
                HStack(alignment: .top, spacing: 16) {
                    compactAccountQuotaSection
                    automationStatusSection
                }
                actionableTasksSection
            }
            .padding(20)
        }
    }

    private var overviewStatusBanner: some View {
        NativeCardContainer(spacing: 8, padding: 12) {
            HStack(spacing: 10) {
                Image(systemName: model.statusIsWarning ? "exclamationmark.triangle.fill" : "checkmark.seal.fill")
                    .foregroundColor(model.statusIsWarning ? .orange : .blue)
                    .font(.system(size: 16, weight: .semibold))
                VStack(alignment: .leading, spacing: 2) {
                    Text("Codex 小助手 · 概览工作台")
                        .font(.system(size: 14, weight: .semibold))
                    Text(model.statusLine)
                        .font(.system(size: 11.5))
                        .foregroundColor(model.statusIsWarning ? .orange : .secondary)
                        .lineLimit(2)
                }
                Spacer()
                Button {
                    model.showIntegrationDetails()
                } label: {
                    Label("接入说明", systemImage: "questionmark.circle")
                }
                .controlSize(.small)
            }
        }
    }

    private var quickLauncherSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Label("终端快捷启动", systemImage: "terminal.fill")
                    .font(.system(size: 13, weight: .semibold))
                SemanticStatusBadge(text: "权限模式：YOLO（跳过权限确认）", tint: .systemOrange)
                SemanticStatusBadge(
                    text: model.agyGuardLaunchEnabled ? "AGY：受管监测 (\(model.agyGuardStateText))" : "AGY：普通启动",
                    tint: model.agyGuardLaunchEnabled ? model.agyGuardStateTint : .secondaryLabelColor
                )
                Spacer()
                Button {
                    model.openSettingsWindow(tab: .launchAndRecovery)
                } label: {
                    Label("启动与恢复设置…", systemImage: "slider.horizontal.3")
                }
                .buttonStyle(.link)
                .font(.system(size: 12))
            }

            NativeCardContainer(spacing: 10, padding: 14) {
                HStack(spacing: 10) {
                    Image(systemName: "folder.fill")
                        .foregroundColor(.blue)
                    Menu {
                        Button("每次启动时选择文件夹…") {
                            model.selectLaunchFolderOption(nil)
                        }
                        if !model.quickLaunchFolders.isEmpty {
                            Divider()
                            ForEach(model.quickLaunchFolders, id: \.self) { folder in
                                Button(folderMenuTitle(folder)) {
                                    model.selectLaunchFolderOption(folder)
                                }
                            }
                        }
                    } label: {
                        HStack {
                            Text(currentLaunchFolderLabel)
                                .lineLimit(1)
                                .truncationMode(.middle)
                            Spacer()
                        }
                    }
                    .frame(minWidth: 220, maxWidth: .infinity)
                    .help(model.pinnedLaunchFolder ?? "点击启动按钮时弹出文件夹选择框")

                    Button {
                        model.chooseQuickLaunchFolder()
                    } label: {
                        Label("选择文件夹…", systemImage: "folder.badge.plus")
                    }
                    .controlSize(.small)

                    Button {
                        model.launchCodexCLI()
                    } label: {
                        Label(QuickLaunchTool.codex.buttonTitle, systemImage: QuickLaunchTool.codex.symbolName)
                    }
                    .buttonStyle(.borderedProminent)
                    .controlSize(.regular)
                    .help(QuickLaunchTool.codex.buttonToolTip)

                    Button {
                        model.launchAgyCLI()
                    } label: {
                        Label(
                            model.agyGuardLaunchEnabled ? "受管启动 AGY CLI" : QuickLaunchTool.agy.buttonTitle,
                            systemImage: QuickLaunchTool.agy.symbolName
                        )
                    }
                    .buttonStyle(.bordered)
                    .controlSize(.regular)
                    .help(QuickLaunchTool.agy.buttonToolTip)
                }

                HStack(spacing: 8) {
                    Text(model.quickLaunchStatusText)
                        .font(.system(size: 11))
                        .foregroundColor(.secondary)
                        .lineLimit(2)
                    Spacer()
                    if model.agyGuardLaunchEnabled || model.agyAutoRecoveryEnabled {
                        Button(model.agyGuardPaused ? "继续监测恢复" : "暂停恢复") {
                            model.toggleAGYGuardPause()
                        }
                        .controlSize(.small)
                        .disabled(!model.canPauseAGYGuard)
                    }
                }
            }
        }
    }

    private var compactAccountQuotaSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Label("账号与额度摘要", systemImage: "person.2.fill")
                    .font(.system(size: 13, weight: .semibold))
                SemanticStatusBadge(
                    text: model.accounts.isEmpty ? "未添加" : "\(model.accounts.count) 个账号",
                    tint: model.accounts.isEmpty ? .systemOrange : .systemBlue
                )
                Spacer()
                Button("查看全部 →") {
                    model.navigate(to: .accounts)
                }
                .buttonStyle(.link)
                .font(.system(size: 12))
            }

            NativeCardContainer(spacing: 10, padding: 12) {
                if model.accounts.isEmpty {
                    HStack(spacing: 10) {
                        Image(systemName: "person.crop.circle.badge.plus")
                            .font(.system(size: 20))
                            .foregroundColor(.secondary)
                        VStack(alignment: .leading, spacing: 2) {
                            Text("尚未添加账号")
                                .font(.system(size: 12.5, weight: .semibold))
                            Text("前往授权管理登录账号后可查看 5 小时与周额度。")
                                .font(.system(size: 11))
                                .foregroundColor(.secondary)
                        }
                        Spacer()
                        Button("授权管理") {
                            model.openAuthorizationCenter()
                        }
                        .controlSize(.small)
                        .disabled(model.isFixtureMode)
                    }
                } else {
                    ForEach(Array(model.accounts.prefix(4).enumerated()), id: \.element.id) { index, account in
                        if index > 0 { Divider() }
                        compactAccountRow(account)
                    }
                    if model.accounts.count > 4 {
                        Divider()
                        Button("查看其余 \(model.accounts.count - 4) 个账号详情…") {
                            model.navigate(to: .accounts)
                        }
                        .buttonStyle(.link)
                        .font(.system(size: 11.5))
                    }
                }
            }
        }
        .frame(maxWidth: .infinity, alignment: .topLeading)
    }

    private func compactAccountRow(_ account: HelperAccount) -> some View {
        let meta = model.accountMetadataReadable ? model.accountMetadata[account.id] : nil
        let currentScopes = meta?["current_scopes"] as? [String] ?? []
        return HStack(spacing: 8) {
            Text(account.name)
                .font(.system(size: 12.5, weight: .medium))
                .lineLimit(1)
                .truncationMode(.tail)
            if currentScopes.contains("desktop") {
                SemanticStatusBadge(text: "Desktop 当前(文件)", tint: .systemBlue)
            }
            if currentScopes.contains("cli") {
                SemanticStatusBadge(text: "CLI 当前(文件)", tint: .systemTeal)
            }
            Spacer(minLength: 6)
            if let quota = account.quota, quota.status == "ready", !quota.windows.isEmpty {
                ForEach(quota.windows.sorted(by: { $0.seconds < $1.seconds }), id: \.seconds) { win in
                    let pct = win.remainingPercent
                    let tint: NSColor = pct <= 10 ? .systemRed : (pct <= 25 ? .systemOrange : (win.seconds == 18000 ? .systemTeal : .systemBlue))
                    SemanticStatusBadge(
                        text: "\(win.seconds == 604800 ? "周" : "5h") \(win.remainingText)",
                        tint: tint
                    )
                }
            } else {
                Text(account.quota?.message ?? "额度待查询")
                    .font(.system(size: 11))
                    .foregroundColor(.secondary)
                    .lineLimit(1)
            }
        }
    }

    private var automationStatusSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Label("自动化与守护状态", systemImage: "bolt.horizontal.circle.fill")
                    .font(.system(size: 13, weight: .semibold))
                Spacer()
                Text("支持在对应页面即时停止")
                    .font(.system(size: 11))
                    .foregroundColor(.secondary)
            }

            NativeCardContainer(spacing: 10, padding: 12) {
                HStack(spacing: 8) {
                    Image(systemName: "flame.fill")
                        .foregroundColor(Color(nsColor: model.warmupStateTint))
                    VStack(alignment: .leading, spacing: 2) {
                        HStack(spacing: 6) {
                            Text("自动预热")
                                .font(.system(size: 12.5, weight: .semibold))
                            SemanticStatusBadge(text: model.warmupChipText, tint: model.warmupStateTint)
                        }
                        Text(model.warmupHeadline)
                            .font(.system(size: 11))
                            .foregroundColor(.secondary)
                            .lineLimit(1)
                    }
                    Spacer()
                    if model.warmupEnabled {
                        Button("关闭预热") { model.toggleWarmup() }
                            .controlSize(.small)
                            .disabled(!model.canToggleWarmup)
                    }
                    Button("管理") { model.navigate(to: .accounts) }
                        .controlSize(.small)
                }

                Divider()

                HStack(spacing: 8) {
                    Image(systemName: "arrow.triangle.2.circlepath.circle.fill")
                        .foregroundColor(Color(nsColor: model.handoffChipTint))
                    VStack(alignment: .leading, spacing: 2) {
                        HStack(spacing: 6) {
                            Text("会话续跑与切号")
                                .font(.system(size: 12.5, weight: .semibold))
                            SemanticStatusBadge(text: model.handoffChipText, tint: model.handoffChipTint)
                        }
                        Text(model.selectedSession.map { "目标：\($0.title)" } ?? "未选择会话 · 选择会话本身不会启用自动任务")
                            .font(.system(size: 11))
                            .foregroundColor(.secondary)
                            .lineLimit(1)
                    }
                    Spacer()
                    if model.handoffActive {
                        Button("停止切号") { model.stopHandoff() }
                            .controlSize(.small)
                            .disabled(!model.canStopHandoff)
                    } else if model.resumeEnabledForSelected {
                        Button("关闭续跑") { model.toggleResume() }
                            .controlSize(.small)
                            .disabled(!model.canToggleResume)
                    }
                    Button("前往") { model.navigate(to: .sessions) }
                        .controlSize(.small)
                }

                Divider()

                HStack(spacing: 8) {
                    Image(systemName: "gearshape.2.fill")
                        .foregroundColor(model.runnerActive ? (model.runnerPaused ? .orange : .green) : .secondary)
                    VStack(alignment: .leading, spacing: 2) {
                        HStack(spacing: 6) {
                            Text("协作执行器")
                                .font(.system(size: 12.5, weight: .semibold))
                            SemanticStatusBadge(
                                text: model.runnerActive ? (model.runnerPaused ? "已暂停" : "运行中") : "未运行",
                                tint: model.runnerActive ? (model.runnerPaused ? .systemOrange : .systemGreen) : .secondaryLabelColor
                            )
                        }
                        Text(model.runnerLabelText.components(separatedBy: .newlines).first ?? "执行器未运行")
                            .font(.system(size: 11))
                            .foregroundColor(.secondary)
                            .lineLimit(1)
                    }
                    Spacer()
                    if model.ownedRunnerRunning {
                        Button("停止执行器") { model.stopRunner() }
                            .controlSize(.small)
                            .disabled(!model.canStopRunner)
                    }
                    Button("前往") { model.navigate(to: .projects) }
                        .controlSize(.small)
                }
            }
        }
        .frame(maxWidth: .infinity, alignment: .topLeading)
    }

    private var actionableTasksSection: some View {
        let counts = model.orchestraTaskCounts
        return VStack(alignment: .leading, spacing: 8) {
            HStack {
                Label("项目协作任务统计", systemImage: "folder.fill.badge.person.crop")
                    .font(.system(size: 13, weight: .semibold))
                Text("（排队中 ≠ 执行中）")
                    .font(.system(size: 11))
                    .foregroundColor(.secondary)
                Spacer()
                Button("进入项目协作 →") {
                    model.navigate(to: .projects)
                }
                .buttonStyle(.link)
                .font(.system(size: 12))
            }

            HStack(spacing: 12) {
                taskMetricCard(
                    title: "执行中 / 审查中",
                    value: "\(counts.runningCount)",
                    subtitle: "正在运行或自动审查",
                    symbol: "play.circle.fill",
                    tint: counts.runningCount > 0 ? .systemBlue : .secondaryLabelColor
                )
                taskMetricCard(
                    title: "排队等待中",
                    value: "\(counts.queuedCount)",
                    subtitle: "已入队 · 尚未开始运行",
                    symbol: "clock.badge",
                    tint: counts.queuedCount > 0 ? .systemTeal : .secondaryLabelColor
                )
                taskMetricCard(
                    title: "待验收 / 需处理",
                    value: "\(counts.actionableCount)",
                    subtitle: "含待验收 \(counts.receiptCount) 个",
                    symbol: "checkmark.seal",
                    tint: counts.actionableCount > 0 ? .systemOrange : .secondaryLabelColor
                )
                taskMetricCard(
                    title: "活跃项目工作群",
                    value: "\(model.orchestraProjects.count)",
                    subtitle: "累计 \(counts.totalCount) 个任务",
                    symbol: "folder.fill",
                    tint: .systemIndigo
                )
            }
        }
    }

    private func taskMetricCard(title: String, value: String, subtitle: String, symbol: String, tint: NSColor) -> some View {
        Button {
            model.navigate(to: .projects)
        } label: {
            NativeCardContainer(spacing: 6, padding: 12) {
                HStack {
                    Image(systemName: symbol)
                        .foregroundColor(Color(nsColor: tint))
                        .font(.system(size: 14, weight: .semibold))
                    Text(title)
                        .font(.system(size: 12, weight: .medium))
                        .foregroundColor(.secondary)
                    Spacer()
                }
                Text(value)
                    .font(.system(size: 22, weight: .bold).monospacedDigit())
                    .foregroundColor(.primary)
                Text(subtitle)
                    .font(.system(size: 11))
                    .foregroundColor(.secondary)
                    .lineLimit(1)
            }
        }
        .buttonStyle(.plain)
        .accessibilityLabel("\(title) \(value) \(subtitle)")
    }

    private var currentLaunchFolderLabel: String {
        guard let folder = model.pinnedLaunchFolder, !folder.isEmpty else {
            return "每次启动时选择文件夹…"
        }
        let name = folder == "/" ? "根目录" : (folder as NSString).lastPathComponent
        let display = (folder as NSString).abbreviatingWithTildeInPath
        return "固定目录：\(name) — \(display)"
    }

    private func folderMenuTitle(_ path: String) -> String {
        let name = path == "/" ? "根目录" : (path as NSString).lastPathComponent
        let display = (path as NSString).abbreviatingWithTildeInPath
        return "\(name) — \(display)"
    }
}

struct AccountsPageView: View {
    @ObservedObject var model: CodexHelperAppModel

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 18) {
                identityAndOwnershipBanner
                if !model.accountActionNotice.isEmpty {
                    NativeCardContainer(spacing: 6, padding: 12) {
                        HStack(spacing: 8) {
                            Image(systemName: "exclamationmark.triangle.fill")
                                .foregroundColor(.orange)
                            Text(model.accountActionNotice)
                                .font(.system(size: 12, weight: .medium))
                                .foregroundColor(.orange)
                            Spacer()
                        }
                    }
                }
                quotaCardsSection
                warmupSection
            }
            .padding(20)
        }
    }

    private var identityAndOwnershipBanner: some View {
        NativeCardContainer(spacing: 8, padding: 12) {
            HStack(alignment: .top, spacing: 10) {
                Image(systemName: "person.badge.shield.checkmark.fill")
                    .foregroundColor(.blue)
                    .font(.system(size: 16, weight: .semibold))
                VStack(alignment: .leading, spacing: 4) {
                    HStack(spacing: 8) {
                        Text("文件归属识别 vs 运行实例身份")
                            .font(.system(size: 13, weight: .semibold))
                        SemanticStatusBadge(text: desktopOwnerBadgeText, tint: desktopOwnerBadgeTint)
                    }
                    Text("各端「当前」标签根据对应目录（~/.codex 与 ~/.codex-cli）的本地登录文件只读识别，不证明已运行客户端实例的实际账号。切换账号前请先完全退出目标客户端及其后端。")
                        .font(.system(size: 11.5))
                        .foregroundColor(.secondary)
                    Text(model.statusLine)
                        .font(.system(size: 11))
                        .foregroundColor(model.statusIsWarning ? .orange : .secondary)
                }
                Spacer()
                VStack(alignment: .trailing, spacing: 6) {
                    Button {
                        model.openAuthorizationCenter()
                    } label: {
                        Label("添加 / 管理授权", systemImage: "key.fill")
                    }
                    .buttonStyle(.borderedProminent)
                    .controlSize(.small)
                    .disabled(model.isFixtureMode)

                    if model.desktopAccountOwner == .ccSwitch {
                        Button {
                            model.openCCSwitch()
                        } label: {
                            Label("打开 CC Switch", systemImage: "arrow.up.forward.app")
                        }
                        .controlSize(.small)
                    }
                }
            }
        }
    }

    private var quotaCardsSection: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Label("账号额度卡片", systemImage: "chart.bar.doc.horizontal")
                    .font(.system(size: 13, weight: .semibold))
                SemanticStatusBadge(
                    text: model.accounts.isEmpty ? "未添加" : "\(model.accounts.count) 个账号",
                    tint: model.accounts.isEmpty ? .systemOrange : .systemBlue
                )
                Spacer()
            }

            if model.accounts.isEmpty {
                NativeCardContainer(spacing: 8, padding: 16) {
                    HStack(spacing: 12) {
                        Image(systemName: "person.crop.circle.badge.plus")
                            .font(.system(size: 24))
                            .foregroundColor(.secondary)
                        VStack(alignment: .leading, spacing: 3) {
                            Text("尚未添加账号")
                                .font(.system(size: 13, weight: .semibold))
                            Text("在「授权管理」中登录账号后，即可查看 5 小时与周额度、绑定客户端或开启自动预热。")
                                .font(.system(size: 11.5))
                                .foregroundColor(.secondary)
                        }
                        Spacer()
                        Button {
                            model.openAuthorizationCenter()
                        } label: {
                            Label("前往授权管理", systemImage: "person.crop.circle.badge.plus")
                        }
                        .buttonStyle(.borderedProminent)
                        .controlSize(.small)
                        .disabled(model.isFixtureMode)
                    }
                }
            } else {
                ForEach(Array(model.accounts.enumerated()), id: \.element.id) { index, account in
                    accountQuotaCard(account, index: index)
                }
            }
        }
    }

    private func accountQuotaCard(_ account: HelperAccount, index: Int) -> some View {
        let meta = model.accountMetadataReadable ? model.accountMetadata[account.id] : nil
        let managed = meta?["managed"] as? Bool == true
        let bindings = meta?["bindings"] as? [[String: String]] ?? []
        let currentScopes = meta?["current_scopes"] as? [String] ?? []
        let expiredScopes = meta?["expired_scopes"] as? [String] ?? []
        let sources = meta?["sources"] as? [String] ?? []
        let sourceLabel = meta == nil ? "状态待核查" : (sources.contains("cc_switch") ? (managed ? "小助手 + CC Switch" : "CC Switch") : "小助手")
        let desktopPres = scopePresentation(meta: meta, scope: "desktop", title: "Desktop", bindings: bindings, currentScopes: currentScopes, expiredScopes: expiredScopes)
        let cliPres = scopePresentation(meta: meta, scope: "cli", title: "CLI", bindings: bindings, currentScopes: currentScopes, expiredScopes: expiredScopes)

        return NativeCardContainer(spacing: 10, padding: 14) {
            HStack(spacing: 8) {
                Circle()
                    .fill(Color.accentColor.opacity(0.16))
                    .frame(width: 26, height: 26)
                    .overlay(
                        Text(account.name.first.map { String($0).uppercased() } ?? "?")
                            .font(.system(size: 12, weight: .semibold))
                            .foregroundColor(.accentColor)
                    )
                Text(account.name)
                    .font(.system(size: 13.5, weight: .semibold))
                    .lineLimit(1)
                    .help(([account.name] + account.provider_names).joined(separator: "\n"))
                SemanticStatusBadge(text: sourceLabel, tint: .secondaryLabelColor)
                if let quota = account.quota, !quota.queried_at.isEmpty {
                    Text("查询：\(formattedQuotaTime(quota.queried_at))")
                        .font(.system(size: 11))
                        .foregroundColor(.secondary)
                    if !quota.credential_sources.isEmpty {
                        Text("来源：\(quota.credential_sources.joined(separator: "/"))")
                            .font(.system(size: 11))
                            .foregroundColor(.secondary)
                    }
                }
                Spacer()
                Button {
                    model.showAccountQuotaDetails(index: index)
                } label: {
                    Image(systemName: "info.circle")
                }
                .buttonStyle(.borderless)
                .help("额度查询时间、凭证来源与完整说明")
                .accessibilityLabel("\(account.name) 额度详情")
            }

            if let quota = account.quota, quota.status == "ready", !quota.windows.isEmpty {
                HStack(alignment: .top, spacing: 20) {
                    ForEach(quota.windows.sorted(by: { $0.seconds < $1.seconds }), id: \.seconds) { win in
                        quotaWindowMetricView(win)
                    }
                }
            } else {
                HStack(spacing: 6) {
                    Image(systemName: "chart.bar.xaxis")
                        .foregroundColor(.secondary)
                    Text(account.quota?.message ?? "正在读取剩余额度…")
                        .font(.system(size: 11.5))
                        .foregroundColor(.secondary)
                }
            }

            Divider()

            HStack(spacing: 8) {
                SemanticStatusBadge(text: desktopPres.0, tint: desktopPres.1)
                    .help(desktopPres.2)
                SemanticStatusBadge(text: cliPres.0, tint: cliPres.1)
                    .help(cliPres.2)
                Spacer()
                if managed {
                    let desktopBlocked = model.desktopAccountOwner != .native
                    if desktopBlocked {
                        Text(model.desktopAccountOwner.blockedMessage)
                            .font(.system(size: 10.5))
                            .foregroundColor(.secondary)
                            .lineLimit(1)
                    }
                    Button("应用到 Desktop") {
                        model.applyAccount(accountID: account.id, scope: "desktop")
                    }
                    .controlSize(.small)
                    .disabled(!model.accountActionsEnabled || desktopBlocked)
                    .help(desktopBlocked ? model.desktopAccountOwner.blockedMessage : "将此账号应用到 Desktop (~/.codex)")

                    Button("应用到 CLI") {
                        model.applyAccount(accountID: account.id, scope: "cli")
                    }
                    .controlSize(.small)
                    .disabled(!model.accountActionsEnabled)
                    .help("将此账号应用到 CLI (~/.codex-cli)")
                } else {
                    Button("前往授权管理") {
                        model.openAuthorizationCenter()
                    }
                    .controlSize(.small)
                    .disabled(meta == nil || model.isFixtureMode)
                    .help("账号仅来自 CC Switch，需在授权管理中登录后才能由小助手切换")
                }
            }
        }
    }

    private func quotaWindowMetricView(_ window: QuotaWindow) -> some View {
        let percent = window.remainingPercent
        let tint: NSColor = percent <= 10 ? .systemRed : (percent <= 25 ? .systemOrange : (window.seconds == 18000 ? .systemTeal : .systemBlue))
        return VStack(alignment: .leading, spacing: 5) {
            HStack(alignment: .firstTextBaseline, spacing: 6) {
                Text("\(window.title == "7 天" ? "周额度" : window.title)剩余")
                    .font(.system(size: 11))
                    .foregroundColor(.secondary)
                Spacer()
                Text(window.remainingText)
                    .font(.system(size: 17, weight: .semibold).monospacedDigit())
                    .foregroundColor(percent <= 25 ? Color(nsColor: tint) : .primary)
                    .accessibilityLabel("\(window.title)剩余 \(window.remainingText)")
            }
            GeometryReader { geo in
                ZStack(alignment: .leading) {
                    Capsule()
                        .fill(Color.secondary.opacity(0.18))
                    Capsule()
                        .fill(Color(nsColor: tint))
                        .frame(width: max(0, CGFloat(Int((geo.size.width * percent / 100).rounded()))))
                }
            }
            .frame(height: 6)
            Text(window.resetCaption)
                .font(.system(size: 10.5))
                .foregroundColor(.secondary)
                .lineLimit(1)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private var warmupSection: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Label("自动预热与时段计划", systemImage: "flame.fill")
                    .font(.system(size: 13, weight: .semibold))
                SemanticStatusBadge(text: model.warmupChipText, tint: model.warmupStateTint)
                Spacer()
                if model.warmupEnabled {
                    Button {
                        model.toggleWarmup()
                    } label: {
                        Label("停止自动预热", systemImage: "stop.circle")
                    }
                    .controlSize(.small)
                    .disabled(!model.canToggleWarmup)
                }
            }

            NativeCardContainer(spacing: 10, padding: 14) {
                HStack(spacing: 10) {
                    Toggle(isOn: Binding(
                        get: { model.warmupEnabled },
                        set: { _ in model.toggleWarmup() }
                    )) {
                        Text("自动预热五小时窗口")
                            .font(.system(size: 12.5, weight: .medium))
                    }
                    .disabled(!model.canToggleWarmup)

                    Spacer()

                    Button {
                        model.checkWarmupNow()
                    } label: {
                        Label(model.warmupChecking ? "刷新中…" : "刷新状态", systemImage: "arrow.clockwise")
                    }
                    .controlSize(.small)
                    .disabled(!model.canCheckWarmup)
                    .help("只重新查询额度和预热状态，不会发送预热请求。")
                }

                HStack(spacing: 7) {
                    Image(systemName: model.warmupEnabled ? "clock.badge.checkmark" : "pause.circle")
                        .foregroundColor(Color(nsColor: model.warmupStateTint))
                    Text(model.warmupHeadline)
                        .font(.system(size: 12, weight: .semibold))
                        .foregroundColor(Color(nsColor: model.warmupStateTint))
                }

                Text(model.warmupDetails)
                    .font(.system(size: 11.5))
                    .foregroundColor(.secondary)
                    .textSelection(.enabled)

                Divider()

                HStack(spacing: 8) {
                    Image(systemName: "clock")
                        .foregroundColor(.secondary)
                    Text(model.warmupScheduleCaption)
                        .font(.system(size: 12))
                    Spacer()
                    Button {
                        model.editWarmupSchedule()
                    } label: {
                        Label("自定义时段", systemImage: "calendar.badge.clock")
                    }
                    .controlSize(.small)
                    .disabled(!model.canEditWarmupSchedule)
                }
            }
        }
    }

    private var desktopOwnerBadgeText: String {
        switch model.desktopAccountOwner {
        case .native: return "Desktop：小助手管理"
        case .ccSwitch: return "Desktop：由 CC Switch 管理"
        case .unknown: return "Desktop：管理方待核查"
        }
    }

    private var desktopOwnerBadgeTint: NSColor {
        switch model.desktopAccountOwner {
        case .native: return .systemGreen
        case .ccSwitch: return .systemBlue
        case .unknown: return .systemOrange
        }
    }

    private func scopePresentation(meta: [String: Any]?, scope: String, title: String, bindings: [[String: String]], currentScopes: [String], expiredScopes: [String]) -> (String, NSColor, String) {
        guard meta != nil else {
            return (title + " · 待核查", .secondaryLabelColor, "账号状态暂不可读取。")
        }
        return accountScopePresentation(
            scope: scope,
            title: title,
            bindings: bindings,
            currentScopes: currentScopes,
            expiredScopes: expiredScopes,
            desktopOwner: model.desktopAccountOwner
        )
    }

    private func formattedQuotaTime(_ iso: String) -> String {
        let parser = ISO8601DateFormatter()
        parser.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        guard let date = parser.date(from: iso) ?? ISO8601DateFormatter().date(from: iso) else { return "时间未知" }
        return DateFormatter.localizedString(from: date, dateStyle: .none, timeStyle: .short)
    }
}
