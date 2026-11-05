import Cocoa
import SwiftUI

struct SessionOutlineRepresentable: NSViewRepresentable {
    let assistant: Assistant?
    let revision: Int

    func makeNSView(context: Context) -> NSView {
        guard let assistant = assistant else {
            return NSView()
        }
        return assistant.makeOrReuseSessionOutlineContainer()
    }

    func updateNSView(_ nsView: NSView, context: Context) {
        _ = revision
        assistant?.refreshSessionOutlineLayout()
    }
}

struct SessionsPageView: View {
    @ObservedObject var model: CodexHelperAppModel
    @FocusState private var isSearchFocused: Bool

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 18) {
                sessionBrowserSection
                selectedSessionObservationAndResumeSection
                selectedSessionHandoffSection
            }
            .padding(20)
        }
        .onChange(of: model.sessionSearchFocusTrigger) { _ in
            isSearchFocused = true
        }
    }

    private var sessionBrowserSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Label("本机会话浏览", systemImage: "bubble.left.and.bubble.right.fill")
                    .font(.system(size: 13, weight: .semibold))
                Spacer()
                Text(model.sessionCountSummary)
                    .font(.system(size: 11.5))
                    .foregroundColor(.secondary)
            }

            NativeCardContainer(spacing: 10, padding: 14) {
                HStack(spacing: 12) {
                    Picker("会话来源", selection: Binding(
                        get: { model.activeScope },
                        set: { model.setSessionScope($0) }
                    )) {
                        Text("Desktop（\(model.desktopSessionCount)）").tag("desktop")
                        Text("CLI（\(model.cliSessionCount)）").tag("cli")
                    }
                    .pickerStyle(.segmented)
                    .labelsHidden()
                    .frame(maxWidth: 280)

                    HStack(spacing: 6) {
                        Image(systemName: "magnifyingglass")
                            .foregroundColor(.secondary)
                        TextField("搜索标题、项目或会话 ID (⌘F)", text: Binding(
                            get: { model.sessionSearchDraft },
                            set: { model.setSessionSearch($0) }
                        ))
                        .textFieldStyle(.plain)
                        .focused($isSearchFocused)
                        if !model.sessionSearchDraft.isEmpty {
                            Button {
                                model.setSessionSearch("")
                            } label: {
                                Image(systemName: "xmark.circle.fill")
                                    .foregroundColor(.secondary)
                            }
                            .buttonStyle(.plain)
                            .accessibilityLabel("清除搜索")
                        }
                    }
                    .padding(.horizontal, 8)
                    .padding(.vertical, 5)
                    .background(
                        RoundedRectangle(cornerRadius: 6, style: .continuous)
                            .fill(Color(nsColor: .textBackgroundColor))
                    )
                    .overlay(
                        RoundedRectangle(cornerRadius: 6, style: .continuous)
                            .stroke(Color(nsColor: .separatorColor), lineWidth: 0.5)
                    )

                    Toggle("包括归档", isOn: Binding(
                        get: { model.includeArchivedSessions },
                        set: { model.setIncludeArchived($0) }
                    ))
                    .toggleStyle(.checkbox)
                    .controlSize(.small)
                }

                Text(model.sourceHintText)
                    .font(.system(size: 11))
                    .foregroundColor(.secondary)
                    .lineLimit(2)

                Divider()

                ZStack {
                    SessionOutlineRepresentable(assistant: model.assistant, revision: model.revision)
                        .frame(minHeight: 230, idealHeight: 260, maxHeight: 320)
                        .clipShape(RoundedRectangle(cornerRadius: 6, style: .continuous))

                    if !model.hasVisibleSessions {
                        VStack(spacing: 6) {
                            Image(systemName: "tray")
                                .font(.system(size: 20))
                                .foregroundColor(.secondary)
                            Text(model.emptySessionsText)
                                .font(.system(size: 12))
                                .foregroundColor(.secondary)
                                .multilineTextAlignment(.center)
                        }
                        .padding(16)
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                        .background(Color(nsColor: .controlBackgroundColor).opacity(0.92))
                    }
                }
            }
        }
    }

    private var selectedSessionObservationAndResumeSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Label("所选会话 · 观察与同号续跑", systemImage: "eye.circle")
                    .font(.system(size: 13, weight: .semibold))
                if model.observingSession {
                    SemanticStatusBadge(text: "观察中", tint: .systemBlue)
                }
                if model.resumeEnabledForSelected {
                    SemanticStatusBadge(text: "同号续跑中", tint: .systemGreen)
                }
                Spacer()
            }

            NativeCardContainer(spacing: 10, padding: 14) {
                VStack(alignment: .leading, spacing: 3) {
                    Text(model.selectionTitleText)
                        .font(.system(size: 13.5, weight: .semibold))
                        .textSelection(.enabled)
                    Text(model.selectionDetailText)
                        .font(.system(size: 11.5))
                        .foregroundColor(.secondary)
                        .textSelection(.enabled)
                }

                if !model.selectionHintText.isEmpty {
                    HStack(spacing: 6) {
                        Image(systemName: "exclamationmark.triangle.fill")
                            .foregroundColor(.orange)
                        Text(model.selectionHintText)
                            .font(.system(size: 11))
                            .foregroundColor(.orange)
                    }
                }

                HStack(spacing: 8) {
                    Text("仅观察本机记录，不会切号或发送消息")
                        .font(.system(size: 11))
                        .foregroundColor(.secondary)
                    Spacer()
                    Button {
                        model.clearSessionSelection()
                    } label: {
                        Label("取消选择", systemImage: "xmark.circle")
                    }
                    .controlSize(.small)
                    .disabled(!model.canClearSessionSelection)

                    Button {
                        model.toggleObservation()
                    } label: {
                        Label(model.observingSession ? "停止观察" : "开始观察", systemImage: model.observingSession ? "eye.slash" : "eye")
                    }
                    .controlSize(.small)
                    .disabled(!model.canObserveSession)
                    .help("只刷新本机记录；不会暂停、切换账号或发送继续消息。")
                }

                Divider()

                HStack(spacing: 10) {
                    Toggle("为选中会话自动续跑（不切号）", isOn: Binding(
                        get: { model.resumeEnabledForSelected },
                        set: { _ in model.toggleResume() }
                    ))
                    .font(.system(size: 12, weight: .medium))
                    .disabled(!model.canToggleResume)

                    Spacer()

                    if model.activeScope == "cli" {
                        Button {
                            model.openManagedCLI()
                        } label: {
                            Label("在终端托管续跑", systemImage: "terminal")
                        }
                        .controlSize(.small)
                        .disabled(!model.canOpenManagedCLI)
                    }
                }

                Text(model.resumeStatusText)
                    .font(.system(size: 11))
                    .foregroundColor(.secondary)
            }
        }
    }

    private var selectedSessionHandoffSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Label("自动切号交接", systemImage: "arrow.triangle.2.circlepath.circle")
                    .font(.system(size: 13, weight: .semibold))
                SemanticStatusBadge(text: model.handoffChipText, tint: model.handoffChipTint)
                Spacer()
                if model.handoffActive {
                    Button {
                        model.stopHandoff()
                    } label: {
                        Label("紧急停止切号", systemImage: "stop.circle.fill")
                    }
                    .controlSize(.small)
                    .disabled(!model.canStopHandoff)
                }
            }

            NativeCardContainer(spacing: 10, padding: 14) {
                HStack(spacing: 6) {
                    Text("五小时剩余额度低于")
                        .font(.system(size: 12))
                    TextField("10", text: Binding(
                        get: { model.handoffThresholdDraft },
                        set: { model.setHandoffThreshold($0) }
                    ))
                    .textFieldStyle(.roundedBorder)
                    .multilineTextAlignment(.trailing)
                    .frame(width: 54)
                    .disabled(!model.canEditHandoffThreshold)
                    Text("% 时切换")
                        .font(.system(size: 12))

                    Spacer()

                    Button {
                        model.checkHandoffPreflight()
                    } label: {
                        Label(model.handoffChecking ? "检查中…" : "检查所选会话", systemImage: "checkmark.shield")
                    }
                    .controlSize(.small)
                    .disabled(!model.canCheckHandoffPreflight)
                }

                Text(model.handoffPreflightSummary)
                    .font(.system(size: 11.5))
                    .foregroundColor(.secondary)

                if model.hasHandoffPreflightReport {
                    DisclosureGroup(
                        isExpanded: Binding(
                            get: { model.handoffDetailsExpanded },
                            set: { _ in model.toggleHandoffDetails() }
                        )
                    ) {
                        Text(model.handoffPreflightDetails)
                            .font(.system(size: 11))
                            .foregroundColor(.secondary)
                            .textSelection(.enabled)
                            .padding(.top, 4)
                    } label: {
                        Text(model.handoffDetailsExpanded ? "收起核验详情" : "查看核验详情")
                            .font(.system(size: 11.5, weight: .medium))
                    }
                }

                Divider()

                HStack(spacing: 8) {
                    Button {
                        model.startHandoff()
                    } label: {
                        Label(model.handoffStartTitle, systemImage: "arrow.triangle.2.circlepath")
                    }
                    .buttonStyle(.borderedProminent)
                    .disabled(!model.canStartHandoff)

                    if model.handoffActive {
                        Button {
                            model.stopHandoff()
                        } label: {
                            Label("停止切号", systemImage: "stop.circle")
                        }
                        .disabled(!model.canStopHandoff)
                    }

                    if model.handoffNeedsAcknowledge && !model.handoffActive {
                        Button {
                            model.acknowledgeHandoff()
                        } label: {
                            Label(model.handoffAcknowledgeTitle, systemImage: "checkmark.shield")
                        }
                        .disabled(!model.canAcknowledgeHandoff)
                    }

                    Spacer()

                    if model.activeScope == "desktop" {
                        Button {
                            model.openDesktopWithRuntimeBridge()
                        } label: {
                            Label("核验方式打开 Desktop", systemImage: "desktopcomputer")
                        }
                        .disabled(!model.canOpenDesktopWithRuntimeBridge)
                        .help("请先正常退出 Desktop；打开后可独立核对运行账号。")
                    }
                }

                Text(model.handoffRunStatusText)
                    .font(.system(size: 11))
                    .foregroundColor(.secondary)

                if model.activeScope == "desktop" {
                    HStack(spacing: 6) {
                        Image(systemName: "exclamationmark.triangle")
                            .foregroundColor(.orange)
                        Text("Desktop 切号触发时会退出整个 Desktop，其他会话及未提交输入可能受影响。")
                            .font(.system(size: 11))
                            .foregroundColor(.orange)
                    }
                }
            }
        }
    }
}
