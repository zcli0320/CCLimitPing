import Cocoa
import SwiftUI

struct ProjectsPageView: View {
    enum DetailSection: String, CaseIterable, Identifiable {
        case progressAndStages = "实时进度与阶段"
        case cardAndReview = "任务卡与审查"
        case receiptAndEvidence = "回执与检查证据"
        var id: String { rawValue }
    }

    @ObservedObject var model: CodexHelperAppModel
    @State private var detailSection: DetailSection = .progressAndStages

    private var currentScopeTitle: String {
        switch model.orchestraScopeSelection {
        case .all:
            return "全部项目与任务"
        case .unassigned:
            return "未归入项目"
        case .project(let pid):
            if let proj = model.orchestraProjects.first(where: { $0.id == pid }) {
                return "\(proj.name)（\(proj.id)）"
            }
            return pid
        }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            projectsOperationalHeader
            if !model.orchestraNoticeText.isEmpty {
                NativeCardContainer(spacing: 6, padding: 10) {
                    HStack(spacing: 8) {
                        Image(systemName: "info.circle.fill")
                            .foregroundColor(.orange)
                        Text(model.orchestraNoticeText)
                            .font(.system(size: 11.5, weight: .medium))
                            .foregroundColor(.orange)
                        Spacer()
                    }
                }
            }
            responsiveWorkbenchContent
        }
        .padding(16)
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
    }

    private var projectsOperationalHeader: some View {
        let counts = model.orchestraTaskCounts
        return NativeCardContainer(spacing: 10, padding: 12) {
            Label("项目协作工作台", systemImage: "folder")
                .font(.headline)
            Text("\(model.runnerActive ? (model.runnerPaused ? "执行器已暂停" : "执行器运行中") : "执行器未运行") · \(counts.runningCount) 执行中 · \(counts.queuedCount) 排队中 · \(counts.receiptCount) 待验收")
                .font(.subheadline)
                .foregroundColor(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            HStack {
                if model.canAckOrchestraAutoSwitch {
                    Button("解除切号暂停") { model.ackOrchestraAutoSwitch() }
                }
                Button("启动执行器") { model.startRunner() }
                    .disabled(!model.canStartRunner)
                Button("停止执行器") { model.stopRunner() }
                    .disabled(!model.canStopRunner)
                Spacer(minLength: 0)
                Button("协作设置…") { model.openSettingsWindow(tab: .collaboration) }
            }
            .controlSize(.small)
            Divider()
            HStack {
                Menu {
                    Button("全部项目与任务") { model.selectOrchestraScope(.all) }
                    ForEach(model.orchestraProjects, id: \.id) { project in
                        Button(project.name) { model.selectOrchestraScope(.project(project.id)) }
                    }
                    Button("未归入项目") { model.selectOrchestraScope(.unassigned) }
                } label: {
                    Label(currentScopeTitle, systemImage: "folder")
                        .lineLimit(1)
                }
                .disabled(model.orchestraActionBusy)
                Spacer(minLength: 8)
                Menu("项目操作") {
                    Button("编辑项目…") { model.editProject() }
                        .disabled(!model.canEditActiveProject)
                    Button("归档项目") { model.archiveProject() }
                        .disabled(!model.canEditActiveProject)
                    Button("在 Finder 中显示") { model.revealProjectInFinder() }
                        .disabled(model.orchestraActionBusy || model.activeProject == nil)
                    Button("打开项目规则") { model.openProjectRules() }
                        .disabled(model.orchestraActionBusy || model.activeProject == nil || model.isFixtureMode)
                    Divider()
                    Button("全部展开") { model.expandAllOrchestraGroups() }
                        .disabled(model.orchestraActionBusy || model.orchestraProjectGroups.isEmpty)
                    Button("全部收起") { model.collapseAllOrchestraGroups() }
                        .disabled(model.orchestraActionBusy || model.orchestraProjectGroups.count <= 1)
                }
                .fixedSize()
            }
            .controlSize(.small)
        }
    }

    @ViewBuilder
    private var responsiveWorkbenchContent: some View {
        if model.isCompactWidth {
            if model.isCompactTaskDetailPresented && (model.selectedTask != nil || model.selectedProjectGroup != nil) {
                VStack(alignment: .leading, spacing: 10) {
                    HStack(spacing: 10) {
                        Button {
                            model.isCompactTaskDetailPresented = false
                        } label: {
                            Label("返回任务列表", systemImage: "chevron.left")
                        }
                        .buttonStyle(.bordered)
                        .controlSize(.small)

                        if let task = model.selectedTask {
                            Text("任务详情：\(task.id)")
                                .font(.system(size: 13, weight: .semibold))
                        } else if let group = model.selectedProjectGroup {
                            Text("项目详情：\(group.title)")
                                .font(.system(size: 13, weight: .semibold))
                        }
                        Spacer()
                    }
                    selectionDetailPane
                }
            } else {
                taskListPane
            }
        } else {
            HStack(alignment: .top, spacing: 14) {
                taskListPane
                    .frame(minWidth: 360, idealWidth: 420, maxWidth: 470, maxHeight: .infinity)
                selectionDetailPane
                    .frame(minWidth: 400, maxWidth: .infinity, maxHeight: .infinity)
            }
        }
    }

    private var taskListPane: some View {
        NativeCardContainer(spacing: 10, padding: 12) {
            HStack(spacing: 8) {
                Text("项目任务分组")
                    .font(.system(size: 12.5, weight: .semibold))
                SemanticStatusBadge(text: "共 \(model.orchestraAllTasks.count) 项", tint: .secondaryLabelColor)

                Spacer()

                Button {
                    model.assignSelectedTask()
                } label: {
                    Label("归入项目…", systemImage: "folder.badge.gearshape")
                }
                .controlSize(.small)
                .disabled(model.orchestraActionBusy || model.selectedTask == nil || model.orchestraProjects.isEmpty || model.isFixtureMode)

                if model.isCompactWidth, model.selectedTask != nil {
                    Button {
                        model.isCompactTaskDetailPresented = true
                    } label: {
                        Label("查看详情", systemImage: "sidebar.right")
                    }
                    .controlSize(.small)
                }
            }

            Divider()

            ScrollView {
                LazyVStack(alignment: .leading, spacing: 10) {
                    if model.orchestraProjectGroups.isEmpty {
                        VStack(spacing: 10) {
                            Text("暂无项目或任务。可使用右上角工具栏（⌘N / ⇧⌘N）建立项目工作群或派发任务。")
                                .font(.system(size: 12))
                                .foregroundColor(.secondary)
                                .multilineTextAlignment(.center)
                            HStack(spacing: 8) {
                                Button("新建项目…") { model.createProject() }
                                    .controlSize(.small)
                                    .disabled(!model.canCreateProjectInOrchestra)
                                Button("派发任务…") { model.createTask() }
                                    .buttonStyle(.borderedProminent)
                                    .controlSize(.small)
                                    .disabled(!model.canCreateTaskInOrchestra)
                            }
                        }
                        .padding(.vertical, 24)
                        .frame(maxWidth: .infinity, alignment: .center)
                    } else {
                        ForEach(model.orchestraProjectGroups, id: \.key) { group in
                            projectGroupBlock(group)
                        }
                    }
                }
            }
        }
    }

    private func projectGroupBlock(_ group: OrchestraProjectGroup) -> some View {
        let isCollapsed = model.orchestraCollapsedGroupKeys.contains(group.key) && model.orchestraProjectGroups.count > 1
        let grpCounts = OrchestraTaskCounts(tasks: group.tasks)
        let isSelectedGroup = model.selectedTask == nil && model.selectedProjectGroup?.key == group.key

        return VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 6) {
                if model.orchestraProjectGroups.count > 1 {
                    Button {
                        model.toggleOrchestraGroupCollapsed(group.key)
                    } label: {
                        Image(systemName: isCollapsed ? "chevron.right" : "chevron.down")
                            .font(.system(size: 11, weight: .semibold))
                            .foregroundColor(.secondary)
                            .frame(width: 16, height: 16)
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel(isCollapsed ? "展开 \(group.title)" : "收起 \(group.title)")
                }

                Button {
                    model.selectOrchestraGroup(group.key)
                    if model.isCompactWidth {
                        model.isCompactTaskDetailPresented = true
                    }
                } label: {
                    HStack(spacing: 6) {
                        Image(systemName: group.isUnassigned ? "tray.full.fill" : "folder.fill")
                            .foregroundColor(group.isUnassigned ? .secondary : .blue)
                        Text(group.title)
                            .font(.system(size: 12.5, weight: .semibold))
                            .lineLimit(1)
                        Text(group.subtitle)
                            .font(.system(size: 11))
                            .foregroundColor(.secondary)
                            .lineLimit(1)
                        Spacer(minLength: 4)
                        if grpCounts.runningCount > 0 {
                            SemanticStatusBadge(text: "\(grpCounts.runningCount) 执行中", tint: .systemBlue)
                        } else if grpCounts.queuedCount > 0 {
                            SemanticStatusBadge(text: "\(grpCounts.queuedCount) 排队中", tint: .systemTeal)
                        } else if grpCounts.receiptCount > 0 {
                            SemanticStatusBadge(text: "\(grpCounts.receiptCount) 待验", tint: .systemOrange)
                        } else {
                            SemanticStatusBadge(text: "\(grpCounts.totalCount)", tint: .secondaryLabelColor)
                        }
                    }
                    .padding(.horizontal, 8)
                    .padding(.vertical, 5)
                    .background(
                        RoundedRectangle(cornerRadius: 6, style: .continuous)
                            .fill(isSelectedGroup ? Color.accentColor.opacity(0.14) : Color.secondary.opacity(0.08))
                    )
                }
                .buttonStyle(.plain)
            }

            if !isCollapsed {
                if group.tasks.isEmpty {
                    Text("该项目下暂无任务")
                        .font(.system(size: 11))
                        .foregroundColor(.secondary)
                        .padding(.leading, 24)
                        .padding(.vertical, 4)
                } else {
                    VStack(alignment: .leading, spacing: 4) {
                        ForEach(group.tasks, id: \.id) { task in
                            taskListRow(task)
                        }
                    }
                    .padding(.leading, 12)
                }
            }
        }
    }


    private func taskListRow(_ task: OrchestraTask) -> some View {
        let isSelected = model.selectedTask?.id == task.id
        let isChild = !(task.review_of ?? "").trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
        let leadName = (task.lead ?? "").isEmpty ? "user" : (task.lead ?? "user")
        let rowSubtitle = task.progress?.latest_stage ?? task.progress?.summary ?? task.lastResult
        let verdict = (task.review_verdict ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
        let runCount = task.runs?.count ?? 0

        return Button {
            model.selectOrchestraTask(task.id)
        } label: {
            VStack(alignment: .leading, spacing: 3) {
                HStack(spacing: 6) {
                    Text(isChild ? "↳ \(task.id)" : task.id)
                        .font(.system(size: 11, weight: .semibold, design: .monospaced))
                        .foregroundColor(.secondary)
                    SemanticStatusBadge(text: task.statusLabel, tint: taskStatusColor(task))
                    Text(task.cardHeadline)
                        .font(.system(size: 12.5, weight: .semibold))
                        .foregroundColor(.primary)
                        .lineLimit(1)
                    Spacer(minLength: 4)
                    Text("\(leadName) → \(task.executor)")
                        .font(.system(size: 10.5))
                        .foregroundColor(.secondary)
                        .lineLimit(1)
                }

                HStack(spacing: 6) {
                    Text(rowSubtitle)
                        .font(.system(size: 11))
                        .foregroundColor(.secondary)
                        .lineLimit(1)
                    Spacer(minLength: 4)
                    if !verdict.isEmpty {
                        SemanticStatusBadge(text: "审查 \(task.reviewLabel)", tint: reviewVerdictColor(verdict))
                    }
                    if runCount > 1 {
                        SemanticStatusBadge(text: "第 \(runCount) 轮", tint: .systemOrange)
                    }
                }
            }
            .padding(.horizontal, 8)
            .padding(.vertical, 6)
            .background(
                RoundedRectangle(cornerRadius: 6, style: .continuous)
                    .fill(isSelected ? Color.accentColor.opacity(0.16) : Color(nsColor: .controlBackgroundColor))
            )
            .overlay(
                RoundedRectangle(cornerRadius: 6, style: .continuous)
                    .stroke(isSelected ? Color.accentColor.opacity(0.45) : Color.secondary.opacity(0.15), lineWidth: 1)
            )
        }
        .buttonStyle(.plain)
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(task.id) \(task.statusLabel) \(task.cardHeadline)")
    }

    // MARK: - Selection Detail Pane

    @ViewBuilder
    private var selectionDetailPane: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 12) {
                if let task = model.selectedTask {
                    selectedTaskDetailContent(task)
                } else {
                    projectOverviewContent(model.selectedProjectGroup)
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    private func selectedTaskDetailContent(_ task: OrchestraTask) -> some View {
        let leadName = (task.lead ?? "").isEmpty ? "user" : (task.lead ?? "user")
        return VStack(alignment: .leading, spacing: 12) {
            NativeCardContainer(
                title: "\(task.id) · \(task.cardHeadline)",
                subtitle: "状态：\(task.statusLabel) · \(leadName) 派给 \(task.executor) · 文件夹：\((task.folder as NSString).lastPathComponent)",
                systemImage: "doc.text.magnifyingglass",
                badgeText: task.statusLabel,
                badgeTint: taskStatusColor(task)
            ) {
                VStack(alignment: .leading, spacing: 8) {
                    HStack(spacing: 8) {
                        Button("验收通过") {
                            model.acceptSelectedTask()
                        }
                        .buttonStyle(.borderedProminent)
                        .tint(.green)
                        .controlSize(.small)
                        .disabled(model.orchestraActionBusy || !task.canAccept || model.isFixtureMode)
                        .help(task.acceptDisabledReason ?? "验收通过当前任务回执")

                        Button("打回返工…") {
                            model.reworkSelectedTask()
                        }
                        .controlSize(.small)
                        .disabled(model.orchestraActionBusy || !task.canRework || model.isFixtureMode)
                        .help(task.reworkDisabledReason ?? "填写返工说明并继续执行")

                        Button("一键断点续跑") {
                            model.checkpointResumeSelectedTask()
                        }
                        .buttonStyle(.borderedProminent)
                        .controlSize(.small)
                        .disabled(model.orchestraActionBusy || !task.canCheckpointResume || model.isFixtureMode)
                        .help(task.checkpointResumeDisabledReason ?? "绑定同一会话 ID 原地断点续跑")
                        Spacer(minLength: 0)
                    }

                    HStack(spacing: 8) {
                        Button("补充指令继续…") {
                            model.resumeSelectedTask()
                        }
                        .controlSize(.small)
                        .disabled(model.orchestraActionBusy || !task.canResume || model.isFixtureMode)
                        .help(task.resumeDisabledReason ?? "补充说明并继续此任务")

                        Button("停止任务") {
                            model.cancelSelectedTask()
                        }
                        .controlSize(.small)
                        .disabled(model.orchestraActionBusy || !task.canCancel || model.isFixtureMode)
                        .help(task.cancelDisabledReason ?? "请求停止排队或执行中的任务")

                        if task.canConfirmStopped {
                            Button("确认已停止") {
                                model.confirmStoppedSelectedTask()
                            }
                            .controlSize(.small)
                            .disabled(model.orchestraActionBusy || !task.canConfirmStopped || model.isFixtureMode)
                            .help(task.confirmStoppedDisabledReason ?? "确认外部进程已终止")
                        }
                        Spacer(minLength: 0)
                    }

                    Text(task.actionAvailabilitySummary)
                        .font(.system(size: 11.5, weight: .medium))
                        .foregroundColor(.secondary)

                    disabledReasonsRow(for: task)
                }
            }

            Picker("详情分区", selection: $detailSection) {
                ForEach(DetailSection.allCases) { sec in
                    Text(sec.rawValue).tag(sec)
                }
            }
            .pickerStyle(.segmented)
            .labelsHidden()

            switch detailSection {
            case .progressAndStages:
                taskProgressAndStagesCard(task)
            case .cardAndReview:
                taskCardAndReviewCard(task)
            case .receiptAndEvidence:
                taskReceiptAndEvidenceCard(task)
            }
        }
    }

    private func disabledReasonsRow(for task: OrchestraTask) -> some View {
        let reasons: [(String, String)] = [
            ("验收", task.acceptDisabledReason),
            ("返工", task.reworkDisabledReason),
            ("断点续跑", task.checkpointResumeDisabledReason),
            ("继续", task.resumeDisabledReason),
            ("停止", task.cancelDisabledReason)
        ].compactMap { label, reason in
            guard let reason else { return nil }
            return (label, reason)
        }

        return Group {
            if !reasons.isEmpty {
                VStack(alignment: .leading, spacing: 3) {
                    Text("不可用按钮原因说明：")
                        .font(.system(size: 11, weight: .semibold))
                        .foregroundColor(.secondary)
                    ForEach(reasons, id: \.0) { item in
                        Text("• \(item.0)：\(item.1)")
                            .font(.system(size: 11))
                            .foregroundColor(.secondary)
                    }
                }
                .padding(8)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(
                    RoundedRectangle(cornerRadius: 6, style: .continuous)
                        .fill(Color.secondary.opacity(0.07))
                )
            }
        }
    }

    private func taskProgressAndStagesCard(_ task: OrchestraTask) -> some View {
        let boundID = task.boundSessionOrConversationID
        let warnText = (task.progress?.warning ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
        let summaryText = (task.progress?.summary ?? "").trimmingCharacters(in: .whitespacesAndNewlines)

        return NativeCardContainer(
            title: "实时进度与阶段记录",
            subtitle: boundID.isEmpty ? "尚未绑定执行器会话 ID" : "绑定会话 ID：\(boundID)",
            systemImage: "waveform.path.ecg"
        ) {
            VStack(alignment: .leading, spacing: 8) {
                if !warnText.isEmpty {
                    Text("⚠️ 警告：\(warnText)")
                        .font(.system(size: 11.5, weight: .medium))
                        .foregroundColor(.orange)
                }

                Text(summaryText.isEmpty ? "暂无实时进度文本" : summaryText)
                    .font(.system(size: 12))
                    .textSelection(.enabled)

                if let stages = task.progress?.stages, !stages.isEmpty {
                    Divider()
                    Text("阶段反馈（共 \(stages.count) 条）：")
                        .font(.system(size: 11.5, weight: .semibold))
                        .foregroundColor(.secondary)
                    ForEach(Array(stages.suffix(8).enumerated()), id: \.offset) { _, stage in
                        Text("• \(stage)")
                            .font(.system(size: 11, design: .monospaced))
                            .foregroundColor(.primary)
                            .textSelection(.enabled)
                    }
                }

                if let actions = task.progress?.recent_actions, !actions.isEmpty {
                    Divider()
                    Text("最近执行动作（最新 \(min(actions.count, 8)) 条）：")
                        .font(.system(size: 11.5, weight: .semibold))
                        .foregroundColor(.secondary)
                    ForEach(Array(actions.suffix(8).enumerated()), id: \.offset) { _, act in
                        Text("• \(act)")
                            .font(.system(size: 11, design: .monospaced))
                            .foregroundColor(.secondary)
                            .textSelection(.enabled)
                    }
                }
            }
        }
    }

    private func taskCardAndReviewCard(_ task: OrchestraTask) -> some View {
        let verdict = task.review_verdict ?? ""
        let reviewBody = (task.review_text ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
        let runCount = task.runs?.count ?? 0

        return NativeCardContainer(
            title: "任务卡与自动审查",
            subtitle: "自动审查结论：\(task.reviewLabel) · 已执行轮次：\(runCount)",
            systemImage: "doc.plaintext",
            badgeText: task.reviewLabel,
            badgeTint: reviewVerdictColor(verdict)
        ) {
            VStack(alignment: .leading, spacing: 8) {
                if !reviewBody.isEmpty {
                    Text("审查意见：\(reviewBody)")
                        .font(.system(size: 12, weight: .medium))
                        .foregroundColor(.primary)
                        .textSelection(.enabled)
                    Divider()
                }
                Text("任务卡原文：")
                    .font(.system(size: 11.5, weight: .semibold))
                    .foregroundColor(.secondary)
                Text(task.card.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty ? "（无任务卡正文）" : task.card)
                    .font(.system(size: 11.5, design: .monospaced))
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
    }


    private func taskReceiptAndEvidenceCard(_ task: OrchestraTask) -> some View {
        let outSet = Set(task.evidence?.changes?.out_of_scope ?? [])
        let changedPaths = task.evidence?.changes?.changed ?? []

        return NativeCardContainer(
            title: "执行回执与检查证据",
            subtitle: task.evidence?.summary.isEmpty == false ? (task.evidence?.summary ?? "") : "回执正文、自动检查与文件变更明细",
            systemImage: "checkmark.seal"
        ) {
            VStack(alignment: .leading, spacing: 8) {
                if let checks = task.evidence?.checks, !checks.isEmpty {
                    Text("自动检查命令（\(checks.count) 项）：")
                        .font(.system(size: 11.5, weight: .semibold))
                        .foregroundColor(.secondary)
                    ForEach(Array(checks.enumerated()), id: \.offset) { _, chk in
                        let passed = chk.status == "passed" || chk.exit == 0
                        let tailText = (chk.tail ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
                        VStack(alignment: .leading, spacing: 2) {
                            HStack(spacing: 6) {
                                SemanticStatusBadge(
                                    text: passed ? "PASS" : "FAIL(\(chk.exit))",
                                    tint: passed ? .systemGreen : .systemRed
                                )
                                Text(chk.command)
                                    .font(.system(size: 11, weight: .semibold, design: .monospaced))
                                    .textSelection(.enabled)
                                Spacer()
                            }
                            if !tailText.isEmpty {
                                Text(tailText)
                                    .font(.system(size: 10.5, design: .monospaced))
                                    .foregroundColor(.secondary)
                                    .lineLimit(6)
                                    .textSelection(.enabled)
                            }
                        }
                    }
                    Divider()
                }

                if !changedPaths.isEmpty {
                    Text("文件变更（\(changedPaths.count) 项）：")
                        .font(.system(size: 11.5, weight: .semibold))
                        .foregroundColor(.secondary)
                    ForEach(Array(changedPaths.prefix(20).enumerated()), id: \.offset) { _, path in
                        let isOut = outSet.contains(path)
                        HStack(spacing: 6) {
                            SemanticStatusBadge(
                                text: isOut ? "越界变更" : "已修改",
                                tint: isOut ? .systemRed : .secondaryLabelColor
                            )
                            Text(path)
                                .font(.system(size: 11, design: .monospaced))
                                .foregroundColor(isOut ? .red : .primary)
                                .textSelection(.enabled)
                        }
                    }
                    Divider()
                }

                Text("回执正文：")
                    .font(.system(size: 11.5, weight: .semibold))
                    .foregroundColor(.secondary)
                Text(task.lastResult.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty ? "（尚无回执输出）" : task.lastResult)
                    .font(.system(size: 11.5, design: .monospaced))
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
    }

    private func projectOverviewContent(_ group: OrchestraProjectGroup?) -> some View {
        let proj = group?.project ?? model.activeProject
        let counts = OrchestraTaskCounts(tasks: group?.tasks ?? model.orchestraAllTasks)
        let summaryBadge = "\(counts.runningCount) 执行中 · \(counts.queuedCount) 排队中 · 共 \(counts.totalCount)"
        let badgeColor: NSColor = counts.runningCount > 0 ? .systemBlue : (counts.queuedCount > 0 ? .systemTeal : .secondaryLabelColor)

        return VStack(alignment: .leading, spacing: 12) {
            NativeCardContainer(
                title: proj?.name ?? (group?.title ?? "项目工作群概览"),
                subtitle: proj?.folder ?? (group?.folder.isEmpty == false ? (group?.folder ?? "") : "选择左侧具体任务查看进度、阶段反馈、审查结论与回执"),
                systemImage: "folder.badge.gearshape",
                badgeText: summaryBadge,
                badgeTint: badgeColor
            ) {
                VStack(alignment: .leading, spacing: 6) {
                    if let proj {
                        Text("默认派工：\(proj.lead ?? "user") → \(proj.executor)（审查：\(proj.reviewer ?? "无")）")
                            .font(.system(size: 12, weight: .medium))
                        Text("规则文件：\(proj.rulesPath.isEmpty ? "未配置规则路径" : proj.rulesPath)")
                            .font(.system(size: 11.5))
                            .foregroundColor(.secondary)
                            .textSelection(.enabled)
                        if let checks = proj.checks, !checks.isEmpty {
                            Text("默认检查命令：\(checks.joined(separator: " ; "))")
                                .font(.system(size: 11, design: .monospaced))
                                .foregroundColor(.secondary)
                        }
                    } else {
                        Text("从左侧列表点选任一任务即可查看实时阶段、审查意见、检查证据及可执行操作。")
                            .font(.system(size: 12))
                            .foregroundColor(.secondary)
                    }
                }
            }
        }
    }

    private func taskStatusColor(_ task: OrchestraTask) -> NSColor {
        switch task.status {
        case "running", "reviewing": return .systemBlue
        case "queued": return .systemTeal
        case "waiting_review": return .systemOrange
        case "accepted": return .systemGreen
        case "failed", "blocked", "needs_human": return .systemRed
        default: return .secondaryLabelColor
        }
    }

    private func reviewVerdictColor(_ verdict: String) -> NSColor {
        switch verdict.lowercased() {
        case "pass": return .systemGreen
        case "rework": return .systemOrange
        case "block": return .systemRed
        default: return .secondaryLabelColor
        }
    }
}
