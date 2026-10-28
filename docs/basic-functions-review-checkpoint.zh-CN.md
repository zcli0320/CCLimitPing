# 基本功能 review 暂停检查点

2026-10-28。用户要求review基本功能；复核期间发现用户已手动开启Desktop切号，保留授权，没有重启/替换安装版、没有手动触发切号。

已隔离复现三项：
1. Desktop会话从active变为failed/usageLimitExceeded、来源剩余0时，coordinator没有设置AlreadyLimited，核心watching要求Running或AlreadyLimited，结果永远留在watching且actions=0。位置handoff_desktop_coordinator.go约209-215及handoff_core.go353。
2. watching阶段的targetEvidence第一次检查就冻结来源auth哈希，同账号last_refresh等正常变化后preparedOK比较拒绝，后续检查也不重新准备，备用持续标为不可用。位置handoff_desktop_coordinator.go53-79。
3. Desktop执行器缺失时Snapshot仍返回Handoff.Enabled=true、RunnerReady=false，消息仍“等待所选会话触发交接”，UI可能误报已监控。位置reader.go167-177。

临时诊断文件 internal/assistant/review_regression_probe_test.go 已创建，三个诊断PASS，日志确认上述现象；这是验证当前缺陷的临时探针，后续应清理，不能将其当修复回归测试。用户仅要求review，本轮尚未修复或部署。go test ./...通过；race和Swift状态/桥接检查已启动，最后结果见本轮工具回收输出。

待完成：整理按优先级的review报告、补齐基本功能覆盖说明、清理临时诊断文件；如获准修复，先补能复现的正确行为回归测试再修改。不得因普通测试全绿就忽略这三项。用户的自动交接可能继续执行，恢复后先核对handoff/state.json和事务，不重复切号。


## 2026-10-28 恢复后的修复

恢复时安装版0.3.7/build16的真实状态为watching、阈值8%、执行器存活，消息显示交接完成并继续监控，无CC Switch待恢复事务。没有重复触发实机切号。

三项缺陷已修复：
- Desktop支持本执行器先见过运行、随后因结构化usageLimitExceeded停止的准确轮次；核对完整工具列表、工具结束、同控制者/会话/设置、无新增手动消息和落盘task_complete。覆盖watching、ready_stop和waiting_stop；拒绝普通错误、未观察过的旧错误、工具未结束、缺失/截断工具列表、新轮次等。新实例仍须通过原有身份、设置和一次继续核验。请求结果不明确时仍uncertain，不重发。
- watching每次重新准备同账号凭证，允许正常刷新；触发交接后仍冻结凭证代次，变化后返回监控重新准备或停止核查。
- Snapshot将缺失Desktop执行器显示为未就绪/暂停，保留单独authorization_pending状态，GUI继续提供停止按钮；中途掉线显示待核查，原事务不删除。只读Snapshot不改写授权。

原临时探针已替换为handoff_desktop_regression_test.go中的正确行为测试。完整Go、race、vet、Swift状态/桥接测试通过；独立本机原生后端+loopback模型的额度错误测试通过，一次本地请求且准确task_started/task_complete，未发真实模型请求。该证据不等同于真实Desktop自然耗尽。

0.3.8/build17已构建签名并安装，bin/limitping也已更新。更新前在watching安全边界正常停止旧执行器，退出小助手并备份0.3.7；保留原会话选择、8%阈值、登录启动及07:00-23:00 UTC+8预热策略。部署记录见相邻CCLimitPing-deployment-20261028-v0.3.8目录，最终GUI权限和监控状态见该目录review.md。


## 用户重新允许后的复核

用户确认已重新允许。安装版GUI实际重试仍返回permissionRequired；在state=off且无执行器/事务的安全边界正常退出并重新打开小助手后，同一会话8%启动仍返回同一错误。只读查看系统设置，Codex小助手的辅助功能开关确实为on；没有代用户更改开关，也没有改TCC数据库。旧签名条目可能仍被保留，尚不能断言已确定其内部原因。已请求用户移除该条目并重新添加~/Applications/Codex 小助手.app。等待期间handoff保持off，认证、配置和策略哈希仍与更新前一致。


## 签名冲突进一步定位与单应用重置

用户确认移除并重新添加当前应用后，安装版GUI及正常重启后的复测仍permissionRequired。系统日志明确记录新授权Allowed，CodeReq=b868b34d2a28a6c2bb2ade0c4a59ff4c77bb4198，但AX检查仍比较旧版cb3841ac5d621279f3df3487e8c2182ede8c874d和新版哈希并失败。两者分别与0.3.7备份、0.3.8安装版CDHash吻合。日志片段存accessibility-mismatch.log。

在handoff=off且GUI/执行器退出后，用构建包新建暂存目录，再rename整个应用目录到原安装路径；未重新签名，二进制字节保持一致，旧安装目录保留为“Codex 小助手 0.3.8 prior-inplace.app”。可执行文件inode已变化，fresh-install.json记录，但权限仍失败。临时Security框架只读探针确认文件和当前运行进程都是新版CDHash，因此不能仅归因于旧GUI进程未退出或原地覆盖的inode。

随后正常退出小助手，以系统命令tccutil reset Accessibility local.codex-helper重置仅该应用授权，返回成功；没有编辑TCC数据库、没有修改其他应用权限、没有代用户授予权限。结果存accessibility-reset-result.txt。重新打开相同签名的0.3.8，已请求用户在重置后再次添加/允许当前安装文件。自动切号保持off。

后续安装应使用新暂存目录和整体rename，避免覆盖正在使用或旧身份缓存的可执行文件；仍须从实际安装版GUI核验权限，不能把系统开关on或静态签名通过当作AX访问成功。


## 最终复核通过：2026-10-28 11:11 UTC+8

用户确认当前0.3.8已允许后，从实际安装版GUI恢复原会话<selected-session-id>、8%阈值的自动切号监控成功。界面已从“正在启动”变为“等待所选会话触发交接”；持久状态enabled=true/stage=watching，执行器PID4752存活、授权/会话匹配，独立Snapshot返回enabled=true、runner_ready=true且无authorization_pending。此前权限阻塞已解除，无需继续重新授权。

本次只恢复监控，未触发账号切换，未退出/重启Codex Desktop；没有待恢复切号事务。认证、配置、CC Switch账号源和预热/续跑策略哈希仍与更新前一致。11:11界面额度为当前44%、备用4%；备用尚未高于8%阈值，因此监控会等待符合条件的备用额度。其显示重置时间为今天12:01，此为当时服务返回值。

结论：辅助功能问题经系统日志确认旧/新CDHash冲突，整体新文件替换未单独解决；app级tccutil重置并由用户重新允许后，实际安装版访问与监控通过。该结论不扩展为每次更新均需重置，也未验证长期稳定签名方案。
