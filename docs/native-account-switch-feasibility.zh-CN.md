# 参考 CC Switch 源码实现原生切号：可行性核查

实施更新：0.3.9已实现原生双端协调首版；本文件保留前期方案，实际范围、测试证据及搁置事项见[当前使用记录](native-dual-handoff.zh-CN.md)。

日期：2026-10-28，Asia/Singapore。此次仅核查公开源码与设计边界，未修改生产凭证、未切号、未替换已安装后端。

## 已核对的版本与许可

本机CC Switch为4.0.21。公开tag v4.0.21指向commit c4db63c60d2a1ddcb4bd9f81152b9c6126413659（annotated tag对象2f04decf8d91e6b6b545fbbf4460062734057533）。该版本LICENSE为MIT，Copyright (c) 2025 Jason Young。小助手所在项目LICENSE同为MIT。若复制/改编实质代码，随相应代码及分发包保留上游版权及许可文本，建立来源/版本记录。

## 实际调用链

- src-tauri/src/commands/provider.rs:178，Tauri内部switch_provider命令，依赖进程内AppState，调用ProviderService::switch；不是已验证的外部CLI/API。
- src-tauri/src/services/provider/codex_direct.rs:174、1119：prepare → plan → run，准备托管账号完整登录、采纳旧账号轮换记录，再事务性发布auth/config/marker/stash/catalog并提交当前供应商状态。
- src-tauri/src/proxy/providers/codex_oauth_auth.rs:1079：在同一账号代次锁内读取完整access/id/refresh及真实last_refresh，处理CLI轮换采纳、过期刷新和旧响应拒绝；有效access token主要在内存缓存，不代表账号JSON已含可直接搬用的完整最新登录。
- src-tauri/src/services/provider/codex_login.rs:1：托管账号由manager管理，不进入native-login stash；不能把stash当成托管账号最新凭证库。
- src-tauri/src/mode/operation.rs：多文件操作pending、崩溃恢复、provider/mode状态提交。
- src-tauri/src/live/engine.rs:199：AppWriteGuard使用OnceLock+Mutex+Condvar，账号refresh也使用进程内Mutex。复制到另一进程不会与现有CC Switch共享同一把锁。仅新增小助手flock也不能约束未改造的CC Switch。

## 可以如何接入

现有交接的“暂停准确会话 → 确认结束 → 完全退出Desktop → 切号 → 冷启动 → 核对设置 → 一次继续”保留。替换中间CC Switch AX按钮适配器即可消除该按钮的辅助功能依赖；不因此省略Desktop完整退出或重启核验。

方案A：给CC Switch增加范围受限的本地IPC桥，复用同一AppState/manager/ProviderService。小助手只传稳定provider ID、预期来源/文件摘要、唯一请求ID，收到固定结果及事务ID，不传出原始token。需编译部署定制CC Switch，但账号管理仍为一个写入者。推荐在保留CC Switch账号管理前提下优先采用。

方案B：将仅Codex OAuth直连切换所需逻辑提取为小助手内置后端（Rust子程序或经过对照测试的Go移植）。需要接管完整凭证刷新、持久化、账号绑定、live marker、当前provider指针和崩溃恢复。与未改造CC Switch共享同一store时，必须建立实际排他所有权和重载机制，不能两进程各用各的内存缓存并同时写。若CC Switch不参与共享锁，可在专门的独占切号模式中让它正常退出，完成写入后再重开并核验加载状态；是否采用此方式需作为架构选择明确。

两种路线均不应同时启用两个能刷新同一账号的独立manager。只读对照先行，测试用合成凭证、隔离目录和本地OAuth服务，不在研究阶段轮换真实凭证。

## 验收要求

1. 同账号/工作区映射无歧义，完整token束来自同一次受锁保护的解析。
2. 有效缓存、正常刷新、CLI已轮换、旧刷新响应、并发切换、配置文件外部修改、任意提交阶段崩溃均有正反例。
3. auth/config/provider/marker及恢复记录一致，日志/IPC无原始凭证。
4. 使用生产交接状态机验证旧Desktop完全退出、新实例实际配置目录、准确会话和设置、仅一次继续。
5. 真实验收通过前保留现有已验证AX后端，原生后端默认不启用，不将研究结果记作已安装功能。

## 上游来源

- https://github.com/farion1231/cc-switch/blob/v4.0.21/LICENSE
- https://github.com/farion1231/cc-switch/blob/v4.0.21/src-tauri/src/commands/provider.rs
- https://github.com/farion1231/cc-switch/blob/v4.0.21/src-tauri/src/services/provider/codex_direct.rs
- https://github.com/farion1231/cc-switch/blob/v4.0.21/src-tauri/src/proxy/providers/codex_oauth_auth.rs
- https://github.com/farion1231/cc-switch/blob/v4.0.21/src-tauri/src/services/provider/codex_login.rs
- https://github.com/farion1231/cc-switch/blob/v4.0.21/src-tauri/src/live/engine.rs
- https://github.com/farion1231/cc-switch/blob/v4.0.21/src-tauri/src/mode/operation.rs
