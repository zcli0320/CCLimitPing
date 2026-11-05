import Cocoa
import SwiftUI
import Combine

final class AppBootstrapConfig {
    static let shared = AppBootstrapConfig()
    var fixture: URL?
    var orchestraFixture: URL?
    var directory: URL = FileManager.default.homeDirectoryForCurrentUser
        .appendingPathComponent("Library/Application Support/CodexHelper-AccountPreview")
    var uiSmokeTest: Bool = false
    private init() {}
}

final class CodexHelperAppModel: ObservableObject {
    static let shared = CodexHelperAppModel()

    weak var assistant: Assistant?

    @Published var revision: Int = 0
    @Published var selectedDestination: NavigationDestination = .overview
    @Published var selectedSettingsTab: HelperSettingsTab = .general
    @Published var sidebarVisibility: NavigationSplitViewVisibility = .all
    @Published var windowWidth: CGFloat = 1100
    @Published var isCompactTaskDetailPresented: Bool = false
    @Published var sessionSearchFocusTrigger: Int = 0
    @Published var handoffThresholdDraft: String = "10"
    @Published var sessionSearchDraft: String = ""

    var isCompactWidth: Bool {
        Int(windowWidth.rounded()) < 1100
    }

    func toggleSidebar() {
        if sidebarVisibility == .detailOnly {
            sidebarVisibility = .all
        } else {
            sidebarVisibility = .detailOnly
        }
        NSApp.keyWindow?.firstResponder?.tryToPerform(#selector(NSSplitViewController.toggleSidebar(_:)), with: nil)
        notifyChanged()
    }

    func bind(assistant: Assistant, orchestra: OrchestraWindowController?) {
        self.assistant = assistant
        orchestra?.onStateChanged = { [weak self] in
            self?.notifyChanged()
        }
        notifyChanged()
    }

    func selectDestination(_ destination: NavigationDestination) {
        navigate(to: destination)
    }

    func updateMainWindowWidth(_ width: CGFloat) {
        updateWindowWidth(width)
    }

    func dismissCompactTaskDetail() {
        isCompactTaskDetailPresented = false
        notifyChanged()
    }

    func checkpointResumeSelectedTask() {
        checkpointResumeTask()
    }

    func notifyChanged() {
        if let assistant = assistant {
            if selectedDestination != assistant.state.selectedDestination {
                selectedDestination = assistant.state.selectedDestination
            }
            if handoffThresholdDraft != assistant.handoffThreshold.stringValue {
                handoffThresholdDraft = assistant.handoffThreshold.stringValue
            }
            if sessionSearchDraft != assistant.search.stringValue {
                sessionSearchDraft = assistant.search.stringValue
            }
            assistant.syncWindowToolbar()
        }
        revision &+= 1
    }

    // MARK: - Navigation & Window Commands

    func navigate(to destination: NavigationDestination) {
        guard let assistant = assistant else {
            selectedDestination = destination
            return
        }
        assistant.selectDestination(destination)
    }

    func updateWindowWidth(_ width: CGFloat) {
        let rounded = CGFloat(Int(width.rounded()))
        guard abs(windowWidth - rounded) >= 1 else { return }
        windowWidth = rounded
        if rounded >= 1100 && isCompactTaskDetailPresented {
            isCompactTaskDetailPresented = false
        }
    }

    func refreshCurrentPage() {
        guard let assistant = assistant else { return }
        switch selectedDestination {
        case .overview:
            assistant.refreshNow()
            if assistant.fixture == nil {
                assistant.orchestraCenter?.refresh(force: true)
            }
        case .accounts:
            assistant.refreshNow()
        case .sessions:
            assistant.refreshData(forceQuota: false)
        case .projects:
            if assistant.fixture == nil {
                assistant.orchestraCenter?.refresh(force: true)
            } else {
                notifyChanged()
            }
        }
    }

    func focusSessionSearch() {
        guard selectedDestination == .sessions else { return }
        sessionSearchFocusTrigger &+= 1
        assistant?.focusSessionSearchField()
    }

    func openSettingsWindow(tab: HelperSettingsTab? = nil) {
        if let tab = tab {
            selectedSettingsTab = tab
        }
        assistant?.openSettingsSceneWindow()
    }

    // MARK: - Global Status & Metadata

    var versionString: String {
        Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "0.4.9"
    }

    var buildString: String {
        Bundle.main.object(forInfoDictionaryKey: "CFBundleVersion") as? String ?? "47"
    }

    var isFixtureMode: Bool {
        assistant?.fixture != nil
    }

    var stateDirectoryPath: String {
        assistant?.store.directory.path ?? AppBootstrapConfig.shared.directory.path
    }

    var statusLine: String {
        assistant?.status.stringValue ?? "正在读取小助手账号与会话记录…"
    }

    var statusIsWarning: Bool {
        assistant?.status.textColor == .systemOrange
    }

    var feedbackLine: String {
        assistant?.feedback.stringValue ?? ""
    }

    var isLoading: Bool {
        assistant?.loading == true
    }

    // MARK: - Quick Launcher & AGY Guard

    var quickLaunchFolders: [String] { assistant?.quickLaunchFolders ?? [] }
    var pinnedLaunchFolder: String? { assistant?.state.launchFolder }
    var recentLaunchFolders: [String] { assistant?.state.recentLaunchFolders ?? [] }
    var quickLaunchStatusText: String { assistant?.quickLaunchStatus.stringValue ?? "" }

    var isFixedLaunchDirectoryMode: Bool { pinnedLaunchFolder != nil }

    var agyGuardLaunchEnabled: Bool { assistant?.state.agyGuardLaunchEnabled == true }
    var agyAutoRecoveryEnabled: Bool { assistant?.agyGuardReport?.policy.auto_recovery_enabled == true }
    var agyGuardPaused: Bool { assistant?.agyGuardReport?.policy.paused == true }
    var agyGuardBusy: Bool { assistant?.agyGuardBusy == true }
    var agyGuardStateText: String { assistant?.agyGuardStateChip.text ?? "普通启动" }
    var agyGuardStateTint: NSColor { assistant?.agyGuardStateChip.tint ?? .secondaryLabelColor }
    var agyGuardReasonText: String { assistant?.agyGuardReasonLabel.stringValue ?? "" }
    var agyGuardReport: AGYGuardReport? { assistant?.agyGuardReport }
    var canPauseAGYGuard: Bool { assistant?.agyGuardPauseButton.isEnabled == true }

    func selectLaunchFolderOption(_ folder: String?) {
        assistant?.selectQuickLaunchFolderFromUI(folder)
    }

    func chooseQuickLaunchFolder() {
        assistant?.chooseQuickLaunchFolder()
    }

    func launchCodexCLI() {
        assistant?.launchQuickCodexCLI()
    }

    func launchAgyCLI() {
        assistant?.launchQuickAgyCLI()
    }

    func setAGYGuardLaunch(_ enabled: Bool) {
        guard let assistant = assistant else { return }
        assistant.agyGuardLaunchToggle.state = enabled ? .on : .off
        assistant.toggleAGYGuardLaunch()
    }

    func setAGYAutoRecovery(_ enabled: Bool) {
        guard let assistant = assistant else { return }
        assistant.agyAutoRecoveryToggle.state = enabled ? .on : .off
        assistant.toggleAGYAutoRecovery()
    }

    func toggleAGYGuardPause() {
        assistant?.toggleAGYGuardPause()
    }

    // MARK: - Accounts & Warmup

    var accounts: [HelperAccount] { assistant?.snapshot?.accounts ?? [] }
    var accountMetadata: [String: [String: Any]] { assistant?.accountMetadata ?? [:] }
    var accountMetadataReadable: Bool { assistant?.accountMetadataReadable == true }
    var accountMetadataPending: Bool { assistant?.accountMetadataPending == true }
    var accountActionBusy: Bool { assistant?.accountActionBusy == true }
    var desktopAccountOwner: DesktopAccountOwner { assistant?.desktopAccountOwner ?? .unknown }
    var accountActionNotice: String {
        guard let assistant = assistant, !assistant.accountActionNotice.isHidden else { return "" }
        return assistant.accountActionNotice.stringValue
    }
    var accountActionsEnabled: Bool {
        accountMetadataReadable && !accountMetadataPending && !accountActionBusy && !isFixtureMode
    }

    var warmupReport: WarmupReport? { assistant?.warmupReport }
    var warmupEnabled: Bool { assistant?.warmupReport?.enabled == true }
    var warmupHeadline: String { assistant?.warmupHeadline.stringValue ?? "正在读取预热状态…" }
    var warmupDetails: String { assistant?.warmupStatus.stringValue ?? "自动预热未开启" }
    var warmupChipText: String { assistant?.warmupStatusChip.text ?? "未开启" }
    var warmupStateTint: NSColor { assistant?.warmupStatusChip.tint ?? .secondaryLabelColor }
    var warmupScheduleCaption: String { assistant?.scheduleLabel.stringValue ?? "时段：全天允许 · UTC+8" }
    var warmupBusy: Bool { assistant?.warmupControlBusy == true }
    var warmupChecking: Bool { assistant?.warmupChecking == true }
    var canToggleWarmup: Bool { assistant?.warmup.isEnabled == true }
    var canCheckWarmup: Bool { assistant?.warmupCheck.isEnabled == true }
    var canEditWarmupSchedule: Bool { assistant?.scheduleButton.isEnabled == true }

    func openAuthorizationCenter() {
        assistant?.openNativeAccountCenter()
    }

    func openCCSwitch() {
        assistant?.openCCSwitch()
    }

    func showAccountQuotaDetails(index: Int) {
        guard let assistant = assistant else { return }
        let dummy = NSButton()
        dummy.tag = index
        assistant.showAccountQuotaDetails(dummy)
    }

    func applyAccount(accountID: String, scope: String) {
        guard let assistant = assistant else { return }
        let btn = AccountScopeButton(
            title: scope == "desktop" ? "应用到 Desktop" : "应用到 CLI",
            accountID: accountID,
            scope: scope,
            target: assistant,
            action: #selector(Assistant.applyAccountFromMain(_:))
        )
        assistant.applyAccountFromMain(btn)
    }

    func toggleWarmup() {
        assistant?.toggleWarmup()
    }

    func checkWarmupNow() {
        assistant?.checkWarmupNow()
    }

    func editWarmupSchedule() {
        assistant?.editWarmupSchedule()
    }

    // MARK: - Sessions, Observation, Resume & Auto-Handoff

    var activeScope: String { assistant?.state.activeScope ?? "desktop" }
    var desktopSessionCount: Int { assistant?.snapshot?.sessions(in: "desktop").count ?? 0 }
    var cliSessionCount: Int { assistant?.snapshot?.sessions(in: "cli").count ?? 0 }
    var sourceHintText: String { assistant?.sourceHint.stringValue ?? "" }
    var includeArchivedSessions: Bool { assistant?.archived.state == .on }
    var sessionCountSummary: String { assistant?.countLabel.stringValue ?? "" }
    var emptySessionsText: String { assistant?.emptyLabel.stringValue ?? "" }
    var hasVisibleSessions: Bool { !(assistant?.visibleSessions.isEmpty ?? true) }

    var selectedSession: HelperSession? { assistant?.selected }
    var selectedSessionKey: String? { assistant?.state.selectedKey }
    var observingSession: Bool { assistant?.state.observing == true }
    var selectionTitleText: String { assistant?.selection.stringValue ?? "选择一个会话后，可以开始观察其记录更新。" }
    var selectionDetailText: String { assistant?.detail.stringValue ?? "" }
    var selectionHintText: String {
        guard let assistant = assistant, !assistant.selectionHint.isHidden else { return "" }
        return assistant.selectionHint.stringValue
    }
    var canObserveSession: Bool { assistant?.observe.isEnabled == true }
    var canClearSessionSelection: Bool { assistant?.clearSelection.isEnabled == true }

    var resumeEnabledForSelected: Bool { assistant?.resumeToggle.state == .on }
    var canToggleResume: Bool { assistant?.resumeToggle.isEnabled == true }
    var canOpenManagedCLI: Bool { assistant?.cliResume.isEnabled == true && activeScope == "cli" }
    var resumeStatusText: String { assistant?.resumeStatus.stringValue ?? "未开启指定会话续跑" }

    var canCheckHandoffPreflight: Bool { assistant?.handoffPreflight.isEnabled == true }
    var handoffChecking: Bool { assistant?.handoffChecking == true }
    var handoffPreflightSummary: String { assistant?.handoffStatus.stringValue ?? "选择会话后可以检查当前交接条件。" }
    var handoffPreflightDetails: String { assistant?.handoffDetails.stringValue ?? "" }
    var hasHandoffPreflightReport: Bool { assistant?.handoffPreflightReport != nil }
    var handoffDetailsExpanded: Bool { assistant?.handoffDetailsExpanded == true }

    var canEditHandoffThreshold: Bool { assistant?.handoffThreshold.isEnabled == true }
    var canStartHandoff: Bool { assistant?.handoffStart.isEnabled == true }
    var handoffStartTitle: String { assistant?.handoffStart.title ?? "开启切号" }
    var handoffActive: Bool { !(assistant?.handoffStop.isHidden ?? true) }
    var canStopHandoff: Bool { assistant?.handoffStop.isEnabled == true }
    var handoffNeedsAcknowledge: Bool { !(assistant?.handoffAcknowledge.isHidden ?? true) }
    var handoffAcknowledgeTitle: String { assistant?.handoffAcknowledge.title ?? "已检查，解除暂停" }
    var canAcknowledgeHandoff: Bool { assistant?.handoffAcknowledge.isEnabled == true }
    var canOpenDesktopWithRuntimeBridge: Bool { assistant?.runtimeLaunch.isEnabled == true && activeScope == "desktop" }
    var runtimeLaunching: Bool { assistant?.runtimeLaunching == true }
    var handoffRunStatusText: String { assistant?.handoffRunStatus.stringValue ?? "" }
    var handoffChipText: String { assistant?.handoffStatusChip.text ?? "待命" }
    var handoffChipTint: NSColor { assistant?.handoffStatusChip.tint ?? .secondaryLabelColor }

    func setSessionScope(_ scope: String) {
        guard let assistant = assistant else { return }
        assistant.sourceTabs.selectedSegment = scope == "cli" ? 1 : 0
        assistant.changeSource()
    }

    func setSessionSearch(_ query: String) {
        sessionSearchDraft = query
        guard let assistant = assistant, assistant.search.stringValue != query else { return }
        assistant.search.stringValue = query
        assistant.filterChanged()
    }

    func setIncludeArchived(_ include: Bool) {
        guard let assistant = assistant else { return }
        assistant.archived.state = include ? .on : .off
        assistant.filterChanged()
    }

    func toggleObservation() { assistant?.toggleObservation() }
    func clearSessionSelection() { assistant?.clearTarget() }
    func toggleResume() { assistant?.toggleResume() }
    func openManagedCLI() { assistant?.openManagedCLI() }

    func setHandoffThreshold(_ text: String) {
        handoffThresholdDraft = text
        assistant?.handoffThreshold.stringValue = text
    }

    func checkHandoffPreflight() {
        assistant?.handoffThreshold.stringValue = handoffThresholdDraft
        assistant?.checkHandoffPreflight()
    }

    func toggleHandoffDetails() { assistant?.toggleHandoffDetails() }

    func startHandoff() {
        assistant?.handoffThreshold.stringValue = handoffThresholdDraft
        assistant?.startCLIHandoff()
    }

    func stopHandoff() { assistant?.stopHandoff() }
    func acknowledgeHandoff() { assistant?.acknowledgeHandoff() }
    func openDesktopWithRuntimeBridge() { assistant?.openDesktopWithRuntimeBridge() }

    // MARK: - Projects & Orchestra Collaboration

    var orchestra: OrchestraWindowController? { assistant?.orchestraCenter }
    var orchestraAgents: [OrchestraAgent] { orchestra?.currentAgents ?? [] }
    var orchestraProjects: [OrchestraProject] { orchestra?.currentProjects ?? [] }
    var orchestraAllProjects: [OrchestraProject] { orchestra?.currentAllProjects ?? [] }
    var orchestraAllTasks: [OrchestraTask] { orchestra?.currentAllTasks ?? [] }
    var orchestraFilteredTasks: [OrchestraTask] { orchestra?.currentFilteredTasks ?? [] }
    var orchestraProjectGroups: [OrchestraProjectGroup] { orchestra?.currentProjectGroups ?? [] }
    var orchestraCollapsedGroupKeys: Set<String> { orchestra?.currentCollapsedGroupKeys ?? [] }
    var orchestraTaskCounts: OrchestraTaskCounts { OrchestraTaskCounts(tasks: orchestraAllTasks) }
    var orchestraScopeSelection: OrchestraSidebarSelection { orchestra?.sidebarSelection ?? .all }
    var selectedTask: OrchestraTask? { orchestra?.currentSelectedTask }
    var selectedProjectGroup: OrchestraProjectGroup? { orchestra?.currentSelectedGroup }
    var activeProject: OrchestraProject? { orchestra?.currentActiveProject }
    var monitoredTask: OrchestraTask? { orchestra?.currentMonitoredTask }

    var runnerActive: Bool { orchestra?.isRunnerActive == true }
    var ownedRunnerRunning: Bool { orchestra?.isOwnedRunnerRunning == true }
    var runnerPaused: Bool { orchestra?.currentRunnerStatus?.paused == true }
    var runnerStatus: OrchestraRunnerStatus? { orchestra?.currentRunnerStatus }
    var runnerLabelText: String { orchestra?.currentRunnerLabel ?? "执行器未运行" }
    var orchestraSettings: OrchestraSettings {
        orchestra?.currentSettings ?? OrchestraSettings(account: "desktop", min_remaining: 10, auto_switch: false, auto_start_runner: false, auto_rework_max: 1)
    }
    var orchestraLoginInfo: OrchestraLoginInfo? { orchestra?.currentLoginInfo }
    var orchestraLoginBusy: Bool { orchestra?.isLoginBusy == true }
    var orchestraLoginCancelling: Bool { orchestra?.isLoginCancelling == true }
    var orchestraLoginLabel: String { orchestra?.currentLoginLabel ?? "正在读取执行账号…" }
    var orchestraLoginPath: String { orchestra?.currentLoginPath ?? "" }
    var orchestraLoginMessage: String { orchestra?.currentLoginMessage ?? "" }
    var orchestraActionBusy: Bool { orchestra?.isActionBusy == true }
    var orchestraNoticeText: String { orchestra?.currentNotice ?? "" }
    var showEvidenceDetails: Bool { orchestra?.isEvidenceExpanded == true }
    var mcpConnectionText: String { orchestra?.mcpConnectionText ?? "" }

    var canStartRunner: Bool { !runnerActive && !orchestraActionBusy && !ownedRunnerRunning && !isFixtureMode }
    var canStopRunner: Bool { ownedRunnerRunning && !orchestraActionBusy }
    var canCreateProjectInOrchestra: Bool { !orchestraActionBusy && orchestraAgents.contains(where: { $0.available }) && !isFixtureMode }
    var canCreateTaskInOrchestra: Bool { !orchestraActionBusy && (activeProject != nil || orchestraAgents.contains(where: { $0.available })) && !isFixtureMode }
    var canEditActiveProject: Bool { !orchestraActionBusy && activeProject != nil && !isFixtureMode }
    var canAckOrchestraAutoSwitch: Bool {
        !orchestraActionBusy && runnerStatus?.auto_switch?.stage == "uncertain" && runnerStatus?.auto_switch?.review_required == true && !isFixtureMode
    }

    func selectOrchestraScope(_ scope: OrchestraSidebarSelection) {
        isCompactTaskDetailPresented = false
        orchestra?.selectSidebarScope(scope)
        notifyChanged()
    }

    func selectOrchestraTask(_ taskID: String?, openDetailInCompact: Bool = true) {
        orchestra?.triggerSelectTask(taskID)
        if taskID != nil && isCompactWidth && openDetailInCompact {
            isCompactTaskDetailPresented = true
        } else if taskID == nil {
            isCompactTaskDetailPresented = false
        }
        notifyChanged()
    }

    func selectOrchestraGroup(_ groupKey: String?) {
        isCompactTaskDetailPresented = false
        orchestra?.triggerSelectGroup(groupKey)
        notifyChanged()
    }

    func toggleOrchestraGroupCollapsed(_ groupKey: String) {
        orchestra?.triggerToggleGroupCollapsed(groupKey)
    }

    func expandAllOrchestraGroups() { orchestra?.triggerExpandAllGroups() }
    func collapseAllOrchestraGroups() { orchestra?.triggerCollapseAllGroups() }
    func setShowEvidenceDetails(_ expanded: Bool) { orchestra?.triggerSetEvidenceExpanded(expanded) }
    func startRunner() { orchestra?.triggerStartRunner() }
    func stopRunner() { orchestra?.triggerStopRunner() }
    func createProject() { orchestra?.triggerCreateProject() }
    func editProject() { orchestra?.triggerEditProject() }
    func archiveProject() { orchestra?.triggerArchiveProject() }
    func revealProjectInFinder() { orchestra?.triggerRevealInFinder() }
    func openProjectRules() { orchestra?.triggerOpenProjectRules() }
    func createTask() { orchestra?.triggerCreateTask() }
    func assignSelectedTask() { orchestra?.triggerAssignSelectedTask() }
    func acceptSelectedTask() { orchestra?.triggerAcceptSelectedTask() }
    func reworkSelectedTask() { orchestra?.triggerReworkSelectedTask() }
    func checkpointResumeTask() { orchestra?.triggerCheckpointResumeTask() }
    func resumeSelectedTask() { orchestra?.triggerResumeSelectedTask() }
    func cancelSelectedTask() { orchestra?.triggerCancelSelectedTask() }
    func confirmStoppedSelectedTask() { orchestra?.triggerConfirmStoppedSelectedTask() }
    func beginExecutorLogin() { orchestra?.triggerBeginLogin() }
    func cancelExecutorLogin() { orchestra?.triggerCancelLogin() }
    func confirmExecutorLogout() { orchestra?.triggerConfirmLogout() }
    func copyMCPInstructions() { orchestra?.triggerCopyMCPInstructions() }
    func ackOrchestraAutoSwitch() { orchestra?.triggerAckAutoSwitch() }
    func setExecutorAccountMode(_ mode: String) { orchestra?.triggerSetAccountMode(mode) }
    func setOrchestraReworkLimit(_ limit: Int) { orchestra?.triggerSetReworkLimit(limit) }
    func setOrchestraAutoSwitch(_ enabled: Bool) { orchestra?.triggerSetAutoSwitch(enabled) }
    func setOrchestraAutoStartRunner(_ enabled: Bool) { orchestra?.triggerSetAutoStartRunner(enabled) }

    // MARK: - General Settings & Login Item

    var loginItemEnabled: Bool { assistant?.loginItem.state == .on }
    var loginItemMixed: Bool { assistant?.loginItem.state == .mixed }
    var loginItemCanToggle: Bool { assistant?.loginItem.isEnabled == true }
    var loginItemStatusText: String { assistant?.loginStatus.stringValue ?? "" }
    var loginItemSummaryText: String { loginItemStatusText.isEmpty ? (loginItemEnabled ? "已启用开机自启" : "未开启开机自启") : loginItemStatusText }
    var appVersionSummary: String { "v\(versionString) / build \(buildString)" }
    var askForFolderOnLaunch: Bool { !isFixedLaunchDirectoryMode }
    var recentFolders: [String] { quickLaunchFolders }
    var quickLaunchDirectorySummary: String {
        if let pinned = pinnedLaunchFolder, !pinned.isEmpty {
            return "固定目录：\((pinned as NSString).abbreviatingWithTildeInPath)"
        }
        return "每次启动时询问文件夹"
    }
    var agyLimitedAutoRecoveryEnabled: Bool { agyAutoRecoveryEnabled }
    var agyGuardStatusText: String { agyGuardStateText }
    var agyPauseButtonTitle: String { agyGuardPaused ? "解除暂停" : "暂停自动恢复 20 分钟" }
    var agyGuardMetaText: String { agyGuardReasonText }
    var agyGuardSignalText: String {
        guard let sigs = agyGuardReport?.sessions.first?.signals, !sigs.isEmpty else {
            return "当前无异常恢复信号记录"
        }
        return sigs.map { "[\($0.source)/\($0.confidence)] \($0.detail)" }.joined(separator: " · ")
    }
    var agyGuardRecoveryText: String {
        guard let s = agyGuardReport?.sessions.first else {
            return "受管会话：0 个运行中"
        }
        return "窗口内恢复次数：\(s.recovery_count_window) / \(agyGuardReport?.policy.max_recoveries_in_window ?? 2)"
    }
    var orchestraRunnerText: String { runnerLabelText }
    var orchestraRunnerReachable: Bool { runnerActive }
    var orchestraStartWithHelper: Bool { orchestraSettings.auto_start_runner == true }
    var orchestraAutoSwitchWhenDesktopClosed: Bool { orchestraSettings.auto_switch == true }
    var orchestraDefaultMaxReworks: Int { orchestraSettings.auto_rework_max ?? 1 }
    var orchestraCanClearAutoSwitchPause: Bool { canAckOrchestraAutoSwitch }
    var orchestraCodexAccessText: String { orchestraLoginLabel }
    var orchestraCodexAccountMode: String { orchestraSettings.account == "separate" ? "runner" : "desktop" }
    var orchestraCanLoginRunner: Bool {
        let state = orchestraLoginInfo?.login.state ?? ""
        return OrchestraLoginActions(state: state, busy: orchestraLoginBusy).canLogin && orchestraCodexAccountMode == "runner"
    }
    var orchestraCanLogoutRunner: Bool {
        let state = orchestraLoginInfo?.login.state ?? ""
        return OrchestraLoginActions(state: state, busy: orchestraLoginBusy).canLogout && orchestraCodexAccountMode == "runner"
    }
    var orchestraCanCancelRunnerLogin: Bool { orchestraLoginBusy && !orchestraLoginCancelling }
    var orchestraAgentText: String {
        let list = orchestraAgents.map { "\($0.label)(\($0.available ? "可用" : "未就绪"))" }
        return list.isEmpty ? "正在读取代理状态…" : list.joined(separator: " · ")
    }
    var orchestraMCPText: String { "将以下配置粘贴到 Claude Code 或 Codex 主控配置中即可调用协作 MCP" }
    var orchestraMCPConfigText: String { mcpConnectionText }

    func toggleLoginItem() { assistant?.toggleLoginItem() }
    func showIntegrationDetails() { assistant?.showDetails() }
    func openGuide() { showIntegrationDetails() }
    func selectAskForFolderOnLaunch() { selectLaunchFolderOption(nil) }
    func selectQuickLaunchFolder(_ folder: String) { selectLaunchFolderOption(folder) }
    func toggleAGYLimitedAutoRecovery() { setAGYAutoRecovery(!agyAutoRecoveryEnabled) }
    func launchManagedAGY() {
        if !agyGuardLaunchEnabled { setAGYGuardLaunch(true) }
        launchAgyCLI()
    }
    func toggleAGYPauseWindow() { toggleAGYGuardPause() }
    func setOrchestraStartWithHelper(_ enabled: Bool) { setOrchestraAutoStartRunner(enabled) }
    func setOrchestraMaxReworks(_ count: Int) { setOrchestraReworkLimit(count) }
    func clearOrchestraAutoSwitchPause() { ackOrchestraAutoSwitch() }
    func pingOrchestraRunner() { assistant?.orchestraCenter?.refresh(force: true) }
    func setOrchestraCodexAccountMode(_ mode: String) { setExecutorAccountMode(mode == "runner" ? "separate" : "desktop") }
    func loginOrchestraRunnerAccount() { beginExecutorLogin() }
    func logoutOrchestraRunnerAccount() { confirmExecutorLogout() }
    func cancelOrchestraRunnerLogin() { cancelExecutorLogin() }
    func copyOrchestraMCPConfig() { copyMCPInstructions() }
}
