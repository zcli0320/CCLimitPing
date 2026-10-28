# Desktop 自动切号实机实验检查点

## 当前交付状态：第二轮成功与安装版权限

run2结果为0，真实暂停、退出、CC Switch切号、冷启动、同轮执行设置和一次继续验证通过；仅来源低额度模拟。恢复事务清理完毕，原会话由正常交接继续恢复（本轮没有恢复程序发送的失败消息）。0.3.7/build16正式能力已启用并安装，回归/Swift/race/vet/build和签名通过；原0.3.6完整备份位于相邻CCLimitPing-deployment-20261028-v0.3.7。

用户已完成辅助功能授权；安装版GUI再次只读预检通过，显示CC Switch账号管理器切换路径，权限错误消失。检查当时来源16%、备用68%、阈值10%，切号关闭，无执行器和活动事务。本次没有再次切号。外部测试链的权限不能替代GUI链，现已从GUI实测确认。

安装前后auth/config、CCSwitch账号源/settings、预热和续跑policy哈希一致，预热与登录启动保留，续跑/切号关闭。当前GUI为核验选中了本对话，搜索过滤已清空，未开启续跑或切号。详细文档与最终权限状态须同步更新。

## 最新授权与第二轮准备

用户已明确授权继续实验、解决问题并更新；有实际登录/系统权限阻碍才询问。本轮核验当前目标账号及真实服务后，通过显式 review 将首次事务归档到原 backups/ccswitch-1928250464 下 reviewed-state.json 和 reviewed-transaction.json，原 auth/config 恢复副本保留；活动状态置 off，不标记旧实验成功，不重放旧切换。

新一轮证据目录为相邻 `CCLimitPing-desktop-live-20261028-run2/`，不要误读首次目录的结果。当前来源约77%，备用约52%，所以本次明确仅模拟来源低额度，备用用真实额度且须高于默认10%；暂停、退出、CC Switch操作、凭证变化、重启、原会话继续均真实执行。结果需明确标注触发模拟。读取新结果及 desktop-checkpoint.json 后决定下一步。

设置修复读取精确turn_context，不再比较被省略的初始请求。全过程未验收前 gate保持禁用；通过后再启用、部署并核验安装版。若失败，先检查新事务与 last_stage，不盲目重复切号；已有继续轮次则不要重复发继续。当前用户授权可继续修复并发起有清楚结果、独立记录的新实验。

## 2026-10-28 首次实验结果及修复

`CCLimitPing-desktop-live-20261028/result.json` 返回1，实验180秒超时；恢复消息成功发送。目标 CC Switch 事务为 installed，当前 auth 与目标账号和 provider 一致；Desktop 新主进程、专属后端均已运行。不能将这些局部成功标为完整验收通过。原 state.json、ccswitch-transaction.json、认证/配置恢复副本均保留，未重复执行切号。源码能力和公开入口保持禁用，安装版仍0.3.6。

只读诊断发现被冻结的初始设置将 effort 记为 default，而该准确轮次的持久 turn_context 是 high，重载后 Desktop 的该轮参数也为 high；权限参数集合的 hash 同时改变。已改为读取准确 session/turn 的持久执行上下文来冻结、比较和恢复参数，避免将未展开的初始请求当作最终生效配置。严格检查普通文件、会话元数据、准确轮次、完整记录及读取期间稳定性；缺少证据时阻止继续。测试覆盖重载表示差异不误报、真实权限变化会改变校验值，以及错会话/轮次/截断/重复记录拒绝。

已补充固定布尔字段的 desktop-checkpoint.json 和暂停前 last_stage；未来超时可保留最后未通过的核验条件。现有首次实验没有这份逐阶段记录，因此不能从超时日志单独证明最终卡住的所有条件。后续不可重放首次动作；若需新实机测试须保留首次证据，并明确选择新一轮授权。

回归时发现当前恢复实例继承了 CODEX_HELPER_LIVE_FULL_DESKTOP_* 环境变量，普通测试因此误进入旧实机探针，前置控制检查失败，未再次切号；随即增加 CODEX_HELPER_LIVE_FULL_DESKTOP_CONFIRM 显式条件，并在所有Desktop启动/恢复入口清理实验变量。不要在当前进程设置此确认变量。另修复定时tick与cancel同时就绪的循环竞争，10次隔离复测通过。最终 go test ./...、go test -race -count=1 ./internal/assistant、go vet 通过，bin/limitping 和 bin/codex-helper已重建。

用户授权：按照他已手动验证的「退出 Desktop → CC Switch 正常切号 → 重开原会话」自动化，并直接测试。不能再将旧 stash 不同步当成必须重新登录。

当前安装版 0.3.6；源码和 dist 为 0.3.7/build16。`desktop_handoff` 能力及公开入口仍被禁用，待真实完整实验通过后启用和部署。

本轮新增 `handoff_ccswitch.go` 和 `CCSwitchControl.swift`，通过本机 Accessibility 仅点击与目标账号唯一绑定的 CC Switch Codex 卡片，走其原始切换路径，不拼接旧 Token。只读检查实际返回 ready。切换前保存 auth/config 恢复副本，切换后核对 CC Switch 当前 provider、完整 auth 与管理器记录一致。完整 Go 测试、race、vet、Swift 编译及签名已通过。

实机实验 `TestLiveDesktopViaCCSwitch` 必须从 Desktop 进程树以外启动。查询两账号真实额度，将本次测试阈值设为二者剩余额度的中间值（来源必须低于备用）；所有额度、正常退出、CC Switch 点击、认证文件、冷启动、继续均为真实操作，不伪造额度。测试完成一轮后停止监控，不重放未知结果。恢复消息测试只在原会话空闲时发送一次。

重启后先查看相邻目录 `CCLimitPing-desktop-live-20261028/` 下 result.json 和 experiment.log。只有退出码0及明确测试 PASS、原会话新轮次开始且恢复事务已清理，才能标记真实交接通过。若失败，先读具体阶段，保留恢复记录，不重复点击或覆盖认证；不要仅凭 app 重开判定成功。

若实机成功：根据真实结果修正缺陷，启用 model.go 的 desktop_handoff 能力，更新对应的禁用门槛测试；完整测试、build和签名后，备份安装版，正常退出小助手并安装0.3.7。原用户预热策略保留，切号默认关闭，不能在未选定/未手动开启时自动切号。安装后的GUI和真实状态必须核验。不得把调整测试阈值描述成默认10%或自然额度耗尽实测。

如果实验尚未完成，最多等到独立执行器的测试超时（180秒，外层240秒），不要同时启动另一个实验。用户更换选择或关闭开关时取消授权。
