import AppKit
import SwiftUI

struct CodexHelperSettingsView: View {
    @ObservedObject var model: CodexHelperAppModel

    var body: some View {
        TabView(selection: $model.selectedSettingsTab) {
            generalTabView
                .tabItem {
                    Label(HelperSettingsTab.general.title, systemImage: HelperSettingsTab.general.systemImage)
                }
                .tag(HelperSettingsTab.general)

            launchAndRecoveryTabView
                .tabItem {
                    Label(HelperSettingsTab.launchAndRecovery.title, systemImage: HelperSettingsTab.launchAndRecovery.systemImage)
                }
                .tag(HelperSettingsTab.launchAndRecovery)

            collaborationTabView
                .tabItem {
                    Label(HelperSettingsTab.collaboration.title, systemImage: HelperSettingsTab.collaboration.systemImage)
                }
                .tag(HelperSettingsTab.collaboration)
        }
        .padding(16)
        .frame(minWidth: 640, idealWidth: 680, minHeight: 460, idealHeight: 500)
    }

    // MARK: - Tab 1: General

    private var generalTabView: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                NativeCardContainer(
                    title: "开机自动启动",
                    subtitle: model.loginItemSummaryText,
                    systemImage: "power.circle.fill",
                    badgeText: model.loginItemEnabled ? "已启用" : "未启用",
                    badgeTint: model.loginItemEnabled ? .systemGreen : .secondaryLabelColor
                ) {
                    HStack(spacing: 12) {
                        Toggle(
                            "登录 macOS 时自动启动 Codex 小助手",
                            isOn: Binding(
                                get: { model.loginItemEnabled },
                                set: { _ in model.toggleLoginItem() }
                            )
                        )
                        .disabled(model.isFixtureMode)
                        Spacer()
                    }
                }

                NativeCardContainer(
                    title: "应用状态与版本信息",
                    subtitle: "最小支持系统：macOS 13.0 (Ventura)",
                    systemImage: "info.circle.fill",
                    badgeText: model.appVersionSummary,
                    badgeTint: .systemBlue
                ) {
                    VStack(alignment: .leading, spacing: 6) {
                        Text("版本标识：\(model.appVersionSummary)")
                            .font(.system(size: 12, weight: .medium))
                        Text("状态存储目录：\(model.stateDirectoryPath)")
                            .font(.system(size: 11.5, design: .monospaced))
                            .foregroundColor(.secondary)
                            .textSelection(.enabled)
                        Text("开启自动预热、同号续跑或自动切号监控等后台自动任务时，关闭主窗口会保留在菜单栏继续运行；未开启后台自动任务时，关闭主窗口将直接退出小助手。")
                            .font(.system(size: 11.5))
                            .foregroundColor(.secondary)
                        HStack(spacing: 10) {
                            Button("查看接入说明…") {
                                model.openGuide()
                            }
                            .controlSize(.small)
                            Spacer()
                        }
                        .padding(.top, 4)
                    }
                }
            }
            .padding(4)
        }
    }

    // MARK: - Tab 2: Launch & Recovery

    private var launchAndRecoveryTabView: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                NativeCardContainer(
                    title: "快速启动目录策略",
                    subtitle: model.quickLaunchDirectorySummary,
                    systemImage: "folder.badge.gearshape",
                    badgeText: model.askForFolderOnLaunch ? "每次询问" : "固定目录",
                    badgeTint: .systemBlue
                ) {
                    VStack(alignment: .leading, spacing: 8) {
                        HStack(spacing: 10) {
                            Menu {
                                Button("每次启动时询问文件夹") {
                                    model.selectAskForFolderOnLaunch()
                                }
                                if !model.recentFolders.isEmpty {
                                    Divider()
                                    ForEach(model.recentFolders, id: \.self) { folder in
                                        Button("固定目录：\((folder as NSString).lastPathComponent) — \(folder)") {
                                            model.selectQuickLaunchFolder(folder)
                                        }
                                    }
                                }
                            } label: {
                                Label(
                                    model.askForFolderOnLaunch
                                        ? "模式：每次启动时询问文件夹"
                                        : "模式：\(model.quickLaunchDirectorySummary)",
                                    systemImage: "folder"
                                )
                            }
                            .frame(maxWidth: 360, alignment: .leading)

                            Button("选择文件夹…") {
                                model.chooseQuickLaunchFolder()
                            }
                            .controlSize(.small)
                            .disabled(model.isFixtureMode)
                            Spacer()
                        }
                        Text("默认使用完全权限模式（YOLO）启动选中目录中的 Codex / Claude / AGY 会话。")
                            .font(.system(size: 11.5))
                            .foregroundColor(.secondary)
                    }
                }

                NativeCardContainer(
                    title: "AGY 监控与有限自动恢复",
                    subtitle: model.agyGuardStatusText,
                    systemImage: "shield.lefthalf.filled",
                    badgeText: model.agyLimitedAutoRecoveryEnabled ? "已启用自动恢复" : "默认关闭",
                    badgeTint: model.agyLimitedAutoRecoveryEnabled ? .systemGreen : .secondaryLabelColor
                ) {
                    VStack(alignment: .leading, spacing: 8) {
                        HStack(spacing: 12) {
                            Toggle(
                                "网络异常有限自动恢复（默认关闭）",
                                isOn: Binding(
                                    get: { model.agyLimitedAutoRecoveryEnabled },
                                    set: { _ in model.toggleAGYLimitedAutoRecovery() }
                                )
                            )
                            Spacer()
                            Button("受管启动 AGY") {
                                model.launchManagedAGY()
                            }
                            .controlSize(.small)
                            .disabled(model.isFixtureMode)
                            Button(model.agyPauseButtonTitle) {
                                model.toggleAGYPauseWindow()
                            }
                            .controlSize(.small)
                        }
                        Text(model.agyGuardMetaText)
                            .font(.system(size: 11.5))
                            .foregroundColor(.secondary)
                        Text(model.agyGuardSignalText)
                            .font(.system(size: 11, design: .monospaced))
                            .foregroundColor(.secondary)
                            .textSelection(.enabled)
                        Text(model.agyGuardRecoveryText)
                            .font(.system(size: 11))
                            .foregroundColor(.secondary)
                            .textSelection(.enabled)
                    }
                }
            }
            .padding(4)
        }
    }

    // MARK: - Tab 3: Collaboration

    private var collaborationTabView: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                NativeCardContainer(
                    title: "执行器 (Runner) 偏好设置",
                    subtitle: model.orchestraRunnerText,
                    systemImage: "gearshape.2.fill",
                    badgeText: model.orchestraRunnerReachable ? "执行器在线" : "未连接",
                    badgeTint: model.orchestraRunnerReachable ? .systemGreen : .systemOrange
                ) {
                    VStack(alignment: .leading, spacing: 8) {
                        HStack(spacing: 16) {
                            Toggle(
                                "随小助手启动执行器",
                                isOn: Binding(
                                    get: { model.orchestraStartWithHelper },
                                    set: { val in model.setOrchestraStartWithHelper(val) }
                                )
                            )
                            .disabled(model.orchestraActionBusy || model.isFixtureMode)

                            Toggle(
                                "Desktop 已关闭时自动切换账号",
                                isOn: Binding(
                                    get: { model.orchestraAutoSwitchWhenDesktopClosed },
                                    set: { val in model.setOrchestraAutoSwitch(val) }
                                )
                            )
                            .disabled(model.orchestraActionBusy || model.isFixtureMode)
                        }

                        HStack(spacing: 12) {
                            Picker(
                                "自动返工上限：",
                                selection: Binding(
                                    get: { model.orchestraDefaultMaxReworks },
                                    set: { val in model.setOrchestraMaxReworks(val) }
                                )
                            ) {
                                Text("自动返工 0 次（仅审查不自动打回）").tag(0)
                                Text("自动返工 1 次").tag(1)
                                Text("自动返工 2 次").tag(2)
                                Text("自动返工 3 次").tag(3)
                            }
                            .frame(maxWidth: 280)
                            .disabled(model.orchestraActionBusy || model.isFixtureMode)

                            if model.orchestraCanClearAutoSwitchPause {
                                Button("解除自动切号暂停") {
                                    model.clearOrchestraAutoSwitchPause()
                                }
                                .controlSize(.small)
                                .disabled(model.orchestraActionBusy || model.isFixtureMode)
                            }

                            Button("测试执行器连通") {
                                model.pingOrchestraRunner()
                            }
                            .controlSize(.small)
                            .disabled(model.orchestraActionBusy || model.isFixtureMode)
                            Spacer()
                        }
                    }
                }

                NativeCardContainer(
                    title: "Codex 执行器账号访问与可用代理",
                    subtitle: model.orchestraCodexAccessText,
                    systemImage: "person.badge.key.fill"
                ) {
                    VStack(alignment: .leading, spacing: 8) {
                        HStack(spacing: 12) {
                            Picker(
                                "Codex 账号来源：",
                                selection: Binding(
                                    get: { model.orchestraCodexAccountMode == "runner" ? "runner" : "desktop" },
                                    set: { val in model.setOrchestraCodexAccountMode(val) }
                                )
                            ) {
                                Text("使用 Desktop 账号").tag("desktop")
                                Text("使用独立账号 (~/.codex-runner)").tag("runner")
                            }
                            .frame(maxWidth: 320)
                            .disabled(model.orchestraActionBusy || model.isFixtureMode)

                            Button("登录独立账号") {
                                model.loginOrchestraRunnerAccount()
                            }
                            .controlSize(.small)
                            .disabled(model.orchestraActionBusy || !model.orchestraCanLoginRunner || model.isFixtureMode)

                            Button("退出独立账号") {
                                model.logoutOrchestraRunnerAccount()
                            }
                            .controlSize(.small)
                            .disabled(model.orchestraActionBusy || !model.orchestraCanLogoutRunner || model.isFixtureMode)

                            if model.orchestraCanCancelRunnerLogin {
                                Button("取消登录") {
                                    model.cancelOrchestraRunnerLogin()
                                }
                                .controlSize(.small)
                                .disabled(model.orchestraActionBusy || model.isFixtureMode)
                            }
                            Spacer()
                        }

                        Text("可用代理：\(model.orchestraAgentText)")
                            .font(.system(size: 11.5))
                            .foregroundColor(.secondary)
                    }
                }

                NativeCardContainer(
                    title: "MCP 连接配置",
                    subtitle: model.orchestraMCPText,
                    systemImage: "cable.connector"
                ) {
                    VStack(alignment: .leading, spacing: 8) {
                        Text(model.orchestraMCPConfigText.isEmpty ? "（点击「复制 MCP 配置」或刷新协作状态获取配置片段）" : model.orchestraMCPConfigText)
                            .font(.system(size: 11, design: .monospaced))
                            .foregroundColor(.secondary)
                            .lineLimit(6)
                            .textSelection(.enabled)
                            .frame(maxWidth: .infinity, alignment: .leading)

                        HStack(spacing: 10) {
                            Button("复制 MCP 配置") {
                                model.copyOrchestraMCPConfig()
                            }
                            .controlSize(.small)
                            Spacer()
                        }
                    }
                }
            }
            .padding(4)
        }
    }
}
