//! Local, private account-center protocol. Credentials stay in this process and
//! protected files; the UI receives only account labels, status and login URLs.
use crate::codex_config;
use crate::proxy::providers::codex_oauth_auth::{CodexOAuthError, CodexOAuthManager};
use base64::{engine::general_purpose::URL_SAFE_NO_PAD, Engine as _};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use std::{
    collections::BTreeMap,
    fs::{self, File, OpenOptions},
    io::{self, Read, Write},
    os::unix::fs::{MetadataExt, OpenOptionsExt, PermissionsExt},
    path::{Path, PathBuf},
    process::Command,
};
use uuid::Uuid;

type Result<T> = std::result::Result<T, String>;
const MAX_FILE: u64 = 8 * 1024 * 1024;

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
struct Binding {
    account_id: String,
    // The fingerprint identifies the generation last published by the helper.
    refresh_digest: String,
    #[serde(default)]
    needs_reapply: bool,
}
#[derive(Debug, Clone, Serialize, Deserialize)]
struct Bindings {
    version: u8,
    desktop: Option<Binding>,
    cli: Option<Binding>,
}
impl Default for Bindings {
    fn default() -> Self {
        Self {
            version: 1,
            desktop: None,
            cli: None,
        }
    }
}
impl Bindings {
    fn get(&self, scope: &str) -> Option<&Binding> {
        match scope {
            "desktop" => self.desktop.as_ref(),
            "cli" => self.cli.as_ref(),
            _ => None,
        }
    }
    fn set(&mut self, scope: &str, value: Option<Binding>) -> Result<()> {
        match scope {
            "desktop" => self.desktop = value,
            "cli" => self.cli = value,
            _ => return Err("使用端无效".into()),
        }
        Ok(())
    }

    fn authoritative(&self, scope: &str, ignore_desktop: bool) -> Option<&Binding> {
        if scope == "desktop" && ignore_desktop {
            None
        } else {
            self.get(scope)
        }
    }

    fn needs_reapply(&self, account: &str) -> bool {
        ["desktop", "cli"].iter().any(|scope| {
            self.get(scope)
                .is_some_and(|b| b.account_id == account && b.needs_reapply)
        })
    }
}

struct LiveAuth {
    raw: Vec<u8>,
    access: String,
    refresh: String,
    id_token: String,
    workspace: String,
    last_refresh_ms: i64,
    access_expires: i64,
}

// Deserialize only metadata needed for a read-only identity comparison.
// Refresh/access tokens from CC Switch are intentionally not imported.
#[derive(Deserialize)]
struct CCSwitchAccountMetadata {
    #[serde(default)]
    account_id: Option<String>,
    #[serde(default)]
    chatgpt_account_id: Option<String>,
    #[serde(default)]
    id_token: Option<String>,
    #[serde(default)]
    email: Option<String>,
}

#[derive(Deserialize)]
struct CCSwitchMetadataStore {
    version: u64,
    accounts: BTreeMap<String, CCSwitchAccountMetadata>,
}

struct DisplayAccount {
    id: String,
    name: String,
    identity: Option<(String, String)>,
    managed: bool,
    cc_switch: bool,
    reauth_required: bool,
    bindings: Vec<Value>,
}
fn jwt_payload(token: &str) -> Option<Value> {
    let mut parts = token.split('.');
    parts.next()?;
    let payload = parts.next()?;
    parts.next()?;
    if parts.next().is_some() || token.len() > 128 * 1024 {
        return None;
    }
    serde_json::from_slice(&URL_SAFE_NO_PAD.decode(payload).ok()?).ok()
}
fn parse_auth(raw: Vec<u8>) -> Result<LiveAuth> {
    let value: Value = serde_json::from_slice(&raw).map_err(|_| "认证文件格式无效")?;
    if value
        .get("auth_mode")
        .and_then(Value::as_str)
        .is_some_and(|m| m != "chatgpt")
        || value
            .get("OPENAI_API_KEY")
            .and_then(Value::as_str)
            .is_some_and(|v| !v.is_empty())
    {
        return Err("该端不是受支持的 ChatGPT 登录".into());
    }
    let text = |key: &str| -> Result<String> {
        value
            .pointer(key)
            .and_then(Value::as_str)
            .filter(|v| !v.trim().is_empty())
            .map(str::to_string)
            .ok_or_else(|| "完整登录凭证不可用".into())
    };
    let refresh = text("/tokens/refresh_token")?;
    let id_token = text("/tokens/id_token")?;
    let workspace = text("/tokens/account_id")?;
    let access = text("/tokens/access_token")?;
    let access_payload = jwt_payload(&access).ok_or("访问凭证格式无效")?;
    let id_payload = jwt_payload(&id_token).ok_or("身份凭证格式无效")?;
    let auth_key = "https://api.openai.com/auth";
    let access_auth = access_payload.get(auth_key);
    let id_auth = id_payload.get(auth_key);
    for claims in [access_auth, id_auth].into_iter().flatten() {
        if claims
            .get("chatgpt_account_id")
            .and_then(Value::as_str)
            .is_some_and(|value| value != workspace)
        {
            return Err("凭证中的工作区不一致".into());
        }
    }
    let access_user = access_auth
        .and_then(|v| v.get("chatgpt_user_id"))
        .and_then(Value::as_str);
    let id_user = id_auth
        .and_then(|v| v.get("chatgpt_user_id"))
        .and_then(Value::as_str);
    if access_user.zip(id_user).is_some_and(|(a, b)| a != b) {
        return Err("凭证中的用户不一致".into());
    }
    let access_expires = access_payload
        .get("exp")
        .and_then(Value::as_i64)
        .ok_or("访问凭证缺少有效期")?;
    let last_refresh_ms = value
        .get("last_refresh")
        .and_then(Value::as_str)
        .and_then(|s| chrono::DateTime::parse_from_rfc3339(s).ok())
        .map(|t| t.timestamp_millis())
        .ok_or("登录更新时间不可确认")?;
    Ok(LiveAuth {
        raw,
        access,
        refresh,
        id_token,
        workspace,
        last_refresh_ms,
        access_expires,
    })
}
fn digest(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}
fn safe_read(path: &Path) -> Result<Option<Vec<u8>>> {
    let meta = match fs::symlink_metadata(path) {
        Ok(meta) => meta,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(None),
        Err(_) => return Err("文件不可读取".into()),
    };
    if !meta.is_file()
        || meta.len() > MAX_FILE
        || meta.nlink() != 1
        || meta.mode() & 0o077 != 0
        || meta.uid() != unsafe { libc::geteuid() }
    {
        return Err("文件类型、权限或归属不安全".into());
    }
    fs::read(path).map(Some).map_err(|_| "文件不可读取".into())
}
fn ensure_private_dir(path: &Path) -> Result<()> {
    fs::create_dir_all(path).map_err(|_| "无法创建私有目录")?;
    let meta = fs::symlink_metadata(path).map_err(|_| "私有目录不可读取")?;
    if !meta.is_dir() || meta.uid() != unsafe { libc::geteuid() } {
        return Err("私有目录类型或归属不安全".into());
    }
    fs::set_permissions(path, fs::Permissions::from_mode(0o700))
        .map_err(|_| "无法保护私有目录".into())
}
fn write_new_private(path: &Path, data: &[u8]) -> Result<()> {
    let mut f = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(path)
        .map_err(|_| "无法创建私有文件")?;
    f.write_all(data)
        .and_then(|_| f.sync_all())
        .map_err(|_| "无法写入私有文件".into())
}
fn write_private_atomic(path: &Path, data: &[u8]) -> Result<()> {
    let parent = path.parent().ok_or("无效文件路径")?;
    ensure_private_dir(parent)?;
    // Reject an unexpected existing symlink, device or hard link.
    let _ = safe_read(path)?;
    let tmp = parent.join(format!(".account-state-{}", Uuid::new_v4()));
    write_new_private(&tmp, data)?;
    if fs::rename(&tmp, path).is_err() {
        let _ = fs::remove_file(&tmp);
        return Err("无法提交状态文件".into());
    }
    if let Ok(dir) = File::open(parent) {
        let _ = dir.sync_all();
    }
    Ok(())
}
fn masked_name(name: &str) -> String {
    if let Some((user, domain)) = name.split_once('@') {
        return format!("{}***@{}", user.chars().next().unwrap_or('*'), domain);
    }
    // Upstream may fall back to a raw workspace identifier; keep it private.
    "ChatGPT 账号".into()
}
fn auth_error(err: CodexOAuthError) -> String {
    match err {
        CodexOAuthError::AuthorizationPending => "正在等待浏览器授权".into(),
        CodexOAuthError::AccessDenied => "登录已被拒绝".into(),
        CodexOAuthError::ExpiredToken => "登录已取消或过期，请重新发起".into(),
        CodexOAuthError::DuplicateAccount => "该账号已存在，请使用重新登录".into(),
        CodexOAuthError::RefreshTokenInvalid => "登录已失效，请重新登录".into(),
        CodexOAuthError::AccountNotFound(_) | CodexOAuthError::AccountUnavailable(_) => {
            "账号不可用，请刷新后重试".into()
        }
        CodexOAuthError::NetworkError(_) | CodexOAuthError::TokenFetchFailed(_) => {
            "登录或凭证请求未完成，请检查网络和账号后重试".into()
        }
        CodexOAuthError::ParseError(_) | CodexOAuthError::IoError(_) => {
            "账号状态无法读取或保存，已停止".into()
        }
    }
}

struct Flow {
    upstream_id: String,
    target: Option<String>,
}
pub struct AccountService {
    home: PathBuf,
    root: PathBuf,
    manager: CodexOAuthManager,
    flows: BTreeMap<String, Flow>,
    _lock: File,
    shared_service: bool,
    handoff_owner: Option<String>,
}

impl AccountService {
    pub fn new(home: PathBuf, root: PathBuf) -> Result<Self> {
        if !home.is_absolute() || !root.is_absolute() {
            return Err("账号目录无效".into());
        }
        ensure_private_dir(&root)?;
        let lock_path = root.join("manager.lock");
        if let Some(_) = safe_read(&lock_path)? {}
        let lock = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .mode(0o600)
            .open(lock_path)
            .map_err(|_| "账号管理器无法启动")?;
        if unsafe { libc::flock(lock.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) } != 0 {
            return Err("另一个账号管理窗口正在使用这份账号数据".into());
        }
        let store_path = root.join("codex_oauth_auth.json");
        if let Some(data) = safe_read(&store_path)? {
            let v: Value = serde_json::from_slice(&data).map_err(|_| "账号库损坏，已停止")?;
            if v.get("version").and_then(Value::as_u64) != Some(2)
                || !v.get("accounts").is_some_and(Value::is_object)
            {
                return Err("账号库格式不受支持，已停止".into());
            }
            let accounts = v
                .get("accounts")
                .and_then(Value::as_object)
                .ok_or("账号库格式无效")?;
            for (key, account) in accounts {
                if key.is_empty()
                    || account.get("account_id").and_then(Value::as_str) != Some(key.as_str())
                    || !account
                        .get("refresh_token")
                        .and_then(Value::as_str)
                        .is_some_and(|s| !s.is_empty())
                    || !account.get("authenticated_at").is_some_and(Value::is_i64)
                {
                    return Err("账号库内容不完整，已停止".into());
                }
            }
        }
        let manager = CodexOAuthManager::new(root.clone());
        Ok(Self {
            home,
            root,
            manager,
            flows: BTreeMap::new(),
            _lock: lock,
            shared_service: false,
            handoff_owner: None,
        })
    }

    fn bindings(&self) -> Result<Bindings> {
        match safe_read(&self.root.join("bindings.json"))? {
            None => Ok(Bindings::default()),
            Some(raw) => {
                let value: Bindings =
                    serde_json::from_slice(&raw).map_err(|_| "账号绑定记录损坏")?;
                if value.version != 1 {
                    return Err("账号绑定记录版本不受支持".into());
                }
                Ok(value)
            }
        }
    }
    fn save_bindings(&self, bindings: &Bindings) -> Result<()> {
        let raw = serde_json::to_vec(bindings).map_err(|_| "账号绑定无法编码")?;
        write_private_atomic(&self.root.join("bindings.json"), &raw)
    }
    fn target_dir(&self, scope: &str) -> Result<PathBuf> {
        match scope {
            "desktop" => Ok(self.home.join(".codex")),
            "cli" => Ok(self.home.join(".codex-cli")),
            _ => Err("使用端无效".into()),
        }
    }
    fn target_path(&self, scope: &str) -> Result<PathBuf> {
        Ok(self.target_dir(scope)?.join("auth.json"))
    }
    fn target_config_hash(&self, scope: &str) -> Result<String> {
        let path = self.target_dir(scope)?.join("config.toml");
        let raw = safe_read(&path)?.ok_or("目标端配置不可确认，请检查认证存储方式")?;
        let value: toml::Value = std::str::from_utf8(&raw)
            .map_err(|_| "目标端配置格式无效")?
            .parse()
            .map_err(|_| "目标端配置格式无效")?;
        if value
            .get("cli_auth_credentials_store")
            .and_then(toml::Value::as_str)
            != Some("file")
        {
            return Err("目标端不是明确的文件式认证，请先检查配置".into());
        }
        let provider = value
            .get("model_provider")
            .and_then(toml::Value::as_str)
            .unwrap_or("openai");
        if let Some(config) = value.get("model_providers").and_then(|p| p.get(provider)) {
            if config
                .get("requires_openai_auth")
                .and_then(toml::Value::as_bool)
                != Some(true)
                || [
                    "base_url",
                    "env_key",
                    "experimental_bearer_token",
                    "http_headers",
                    "env_http_headers",
                ]
                .iter()
                .any(|key| config.get(*key).is_some())
            {
                return Err("目标端模型服务配置不是已确认的官方认证路径".into());
            }
        } else if provider != "openai" {
            return Err("目标端模型服务配置不可确认".into());
        }
        Ok(digest(&raw))
    }
    fn require_distinct_roots(&self) -> Result<()> {
        let a = self.target_dir("desktop")?;
        let b = self.target_dir("cli")?;
        if let (Ok(a), Ok(b)) = (fs::canonicalize(a), fs::canonicalize(b)) {
            if a == b {
                return Err("两个端指向相同目录，已停止".into());
            }
        }
        Ok(())
    }
    fn check_no_pending(&self) -> Result<()> {
        if safe_read(&self.root.join("pending.json"))?.is_some() {
            return Err("上次账号操作结果待核查，请保留现状".into());
        }
        Ok(())
    }
    fn read_live(&self, scope: &str) -> Result<Option<LiveAuth>> {
        safe_read(&self.target_path(scope)?)?
            .map(parse_auth)
            .transpose()
    }
    async fn matches_account(&self, account: &str, auth: &LiveAuth) -> bool {
        let Some((identity, workspace)) = self.manager.helper_identity(account).await else {
            return false;
        };
        codex_config::extract_codex_id_token_user_identity(&auth.id_token).as_deref()
            == Some(identity.as_str())
            && auth.workspace == workspace
    }
    async fn managed_live(&self, scope: &str, binding: &Binding) -> Result<LiveAuth> {
        let live = self
            .read_live(scope)?
            .ok_or("受管端的登录文件不存在，请先检查")?;
        if !self.matches_account(&binding.account_id, &live).await {
            return Err("该端登录已被外部修改，不能覆盖".into());
        }
        Ok(live)
    }

    // For quota only: borrow an already-issued, still-valid access token from a
    // matching client without adopting or refreshing its credential chain.
    async fn matching_live_auth(&self, account: &str) -> Option<(String, LiveAuth)> {
        let mut candidates = Vec::new();
        for scope in ["desktop", "cli"] {
            if let Ok(Some(live)) = self.read_live(scope) {
                if live.access_expires > chrono::Utc::now().timestamp() + 60
                    && self.matches_account(account, &live).await
                {
                    candidates.push((scope.to_string(), live));
                }
            }
        }
        candidates.sort_by_key(|(_, live)| live.last_refresh_ms);
        if candidates.len() == 2
            && candidates[0].1.last_refresh_ms == candidates[1].1.last_refresh_ms
            && candidates[0].1.refresh != candidates[1].1.refresh
        {
            return None;
        }
        candidates.pop()
    }
    // Switch backups preserve complete bundles without changing the login-owned
    // account store or refreshing credentials. They remain usable after the
    // last client holding that account has been switched to another account.
    async fn matching_switch_backup(&self, account: &str) -> Option<LiveAuth> {
        let entries = fs::read_dir(self.root.join("backups")).ok()?;
        let mut candidates = Vec::new();
        for entry in entries.flatten() {
            let path = entry.path();
            if path.extension().and_then(|v| v.to_str()) != Some("auth") {
                continue;
            }
            let Some(raw) = safe_read(&path).ok().flatten() else {
                continue;
            };
            let Ok(auth) = parse_auth(raw) else { continue };
            if auth.access_expires > chrono::Utc::now().timestamp() + 60
                && self.matches_account(account, &auth).await
            {
                candidates.push(auth);
            }
        }
        candidates.sort_by_key(|auth| auth.last_refresh_ms);
        if candidates.len() > 1 {
            let newest = &candidates[candidates.len() - 1];
            if candidates.iter().any(|auth| {
                auth.last_refresh_ms == newest.last_refresh_ms && auth.refresh != newest.refresh
            }) {
                return None;
            }
        }
        candidates.pop()
    }

    async fn matching_live_access(&self, account: &str) -> Option<(String, String)> {
        if let Some((_, live)) = self.matching_live_auth(account).await {
            return Some((live.access, live.workspace));
        }
        self.matching_switch_backup(account)
            .await
            .map(|auth| (auth.access, auth.workspace))
    }

    pub async fn list(&self) -> Result<Value> {
        let bindings = self.bindings()?;
        let desktop_owner = match cc_switch_targets("desktop", &self.home) {
            Ok(true) => "cc_switch",
            Ok(false) => "native",
            Err(_) => "unknown",
        };
        let ignore_desktop = desktop_owner != "native";
        let pending = safe_read(&self.root.join("pending.json"))?.is_some();
        let accounts = self.manager.list_accounts().await;
        let mut display = Vec::<DisplayAccount>::new();
        for account in accounts {
            let mut scopes = Vec::new();
            for scope in ["desktop", "cli"] {
                if let Some(binding) = bindings.authoritative(scope, ignore_desktop) {
                    if binding.account_id == account.id {
                        let state = if binding.needs_reapply {
                            "needs_reapply"
                        } else if self.managed_live(scope, binding).await.is_ok() {
                            "bound"
                        } else {
                            "unknown"
                        };
                        scopes.push(json!({"scope":scope,"state":state}));
                    }
                }
            }
            display.push(DisplayAccount {
                identity: self.manager.helper_identity(&account.id).await,
                name: masked_name(&account.login),
                id: account.id,
                managed: true,
                cc_switch: false,
                reauth_required: account.reauth_required,
                bindings: scopes,
            });
        }
        // CC Switch owns its own login lifecycle. Read its account metadata
        // on each list request, merge only exact user+workspace identities,
        // and expose unmatched records as read-only rows. Do not copy tokens,
        // mutate either store, or adopt its current provider selection.
        let cc_path = self.home.join(".cc-switch/codex_oauth_auth.json");
        let (cc_status, cc_store) = match safe_read(&cc_path) {
            Ok(Some(raw)) => match serde_json::from_slice::<CCSwitchMetadataStore>(&raw) {
                Ok(store)
                    if store.version == 2
                        && store.accounts.iter().all(|(key, a)| {
                            !key.is_empty() && a.account_id.as_deref().is_none_or(|id| id == key)
                        }) =>
                {
                    ("ready", Some(store))
                }
                _ => ("unavailable", None),
            },
            Ok(None) => ("absent", None),
            Err(_) => ("unavailable", None),
        };
        if let Some(store) = cc_store {
            for (key, account) in store.accounts {
                let identity = account
                    .id_token
                    .as_deref()
                    .and_then(codex_config::extract_codex_id_token_user_identity)
                    .zip(account.chatgpt_account_id.filter(|w| !w.trim().is_empty()));
                let matching: Vec<usize> = display
                    .iter()
                    .enumerate()
                    .filter(|(_, row)| identity.is_some() && row.identity == identity)
                    .map(|(i, _)| i)
                    .collect();
                if let [index] = matching.as_slice() {
                    display[*index].cc_switch = true;
                    continue;
                }
                let id_basis = match &identity {
                    Some((user, workspace)) if matching.is_empty() => {
                        format!("cc-only\0{user}\0{workspace}")
                    }
                    _ => format!("cc-record\0{key}"),
                };
                display.push(DisplayAccount {
                    id: format!("cc:{}", digest(id_basis.as_bytes())),
                    name: account
                        .email
                        .as_deref()
                        .map(masked_name)
                        .unwrap_or_else(|| "ChatGPT 账号".into()),
                    identity,
                    managed: false,
                    cc_switch: true,
                    reauth_required: false,
                    bindings: Vec::new(),
                });
            }
        }
        // Observe each client's current file independently of the helper's
        // binding. CC Switch may have switched a client without granting the
        // helper authority to write, and an on-disk login is not proof of a
        // running client's identity. Never persist or import the observation.
        let mut current_accounts = BTreeMap::<&str, String>::new();
        let mut expired_scopes = Vec::<&str>::new();
        let mut observations = BTreeMap::<&str, &str>::new();
        for scope in ["desktop", "cli"] {
            if self.target_config_hash(scope).is_err() {
                observations.insert(scope, "unavailable");
                continue;
            }
            let live = match self.read_live(scope) {
                Ok(Some(live)) => live,
                Ok(None) => {
                    observations.insert(scope, "empty");
                    continue;
                }
                Err(_) => {
                    observations.insert(scope, "unavailable");
                    continue;
                }
            };
            let identity = codex_config::extract_codex_id_token_user_identity(&live.id_token)
                .map(|user| (user, live.workspace.clone()));
            let matches: Vec<String> = display
                .iter()
                .filter(|row| identity.is_some() && row.identity == identity)
                .map(|row| row.id.clone())
                .collect();
            match matches.as_slice() {
                [account] => {
                    current_accounts.insert(scope, account.clone());
                    if live.access_expires <= chrono::Utc::now().timestamp() {
                        expired_scopes.push(scope);
                        observations.insert(scope, "expired");
                    } else {
                        observations.insert(scope, "matched");
                    }
                }
                [] => {
                    observations.insert(scope, "unmatched");
                }
                _ => {
                    observations.insert(scope, "ambiguous");
                }
            }
        }
        let mut rows = Vec::new();
        for account in display {
            let current_scopes: Vec<&str> = ["desktop", "cli"]
                .into_iter()
                .filter(|scope| current_accounts.get(scope) == Some(&account.id))
                .collect();
            let expired_current_scopes: Vec<&str> = current_scopes
                .iter()
                .copied()
                .filter(|scope| expired_scopes.contains(scope))
                .collect();
            let mut sources = Vec::new();
            if account.managed {
                sources.push("helper");
            }
            if account.cc_switch {
                sources.push("cc_switch");
            }
            rows.push(
                json!({"id":account.id,"name":account.name,"managed":account.managed,
                "reauth_required":account.reauth_required,"bindings":account.bindings,
                "sources":sources,"current_scopes":current_scopes,"expired_scopes":expired_current_scopes}),
            );
        }
        Ok(
            json!({"accounts":rows,"pending":pending,"scope_observations":observations,
			"cc_switch_status":cc_status,"desktop_owner":desktop_owner,
			"desktop_binding_present":bindings.desktop.is_some()}),
        )
    }

    async fn start_login(&mut self, mode: &str, target: Option<&str>) -> Result<Value> {
        self.check_no_pending()?;
        if !self.flows.is_empty() {
            return Err("已有登录正在进行，请先完成或取消".into());
        }
        if let Some(id) = target {
            // A reauthorization can supersede the old grant. Do not write any
            // live file here. Mark affected bindings before starting so a
            // cancellation or crash cannot silently claim they are up to date.
            let mut bindings = self.bindings()?;
            let ignore_desktop = cc_switch_targets("desktop", &self.home).unwrap_or(true);
            for scope in ["desktop", "cli"] {
                if bindings
                    .authoritative(scope, ignore_desktop)
                    .is_some_and(|b| b.account_id == id)
                {
                    require_stopped(scope, &self.home)?;
                }
            }
            for scope in ["desktop", "cli"] {
                if let Some(mut binding) = bindings.get(scope).cloned() {
                    if binding.account_id == id {
                        binding.needs_reapply = true;
                        bindings.set(scope, Some(binding))?;
                    }
                }
            }
            self.save_bindings(&bindings)?;
        }
        let reply = match mode {
            "browser" => self.manager.start_browser_flow(target).await,
            "device" => self.manager.start_device_flow(target).await,
            _ => return Err("登录方式无效".into()),
        }
        .map_err(|error| match (mode, error) {
            ("browser", CodexOAuthError::IoError(message))
                if message.starts_with("Cannot open the browser login callback") =>
            {
                "浏览器登录的本机回调端口 1455 不可用；请结束其他登录流程或使用设备码登录".into()
            }
            ("device", CodexOAuthError::NetworkError(_)) => {
                "无法连接设备码登录服务，请检查网络后重试".into()
            }
            (_, error) => auth_error(error),
        })?;
        let id = Uuid::new_v4().to_string();
        self.flows.insert(
            id.clone(),
            Flow {
                upstream_id: reply.device_code,
                target: target.map(str::to_string),
            },
        );
        Ok(
            json!({"flow_id":id,"mode":mode,"url":reply.verification_uri,
            "user_code":reply.user_code,"interval":reply.interval,"expires_in":reply.expires_in}),
        )
    }
    async fn poll_login(&mut self, id: &str) -> Result<Value> {
        let flow = self.flows.get(id).ok_or("登录已结束或取消")?;
        let upstream = flow.upstream_id.clone();
        match self.manager.poll_for_token(&upstream, || async {}).await {
            Ok(None) | Err(CodexOAuthError::AuthorizationPending) => Ok(json!({"state":"waiting"})),
            Ok(Some(_)) => {
                self.flows.remove(id);
                Ok(json!({"state":"complete"}))
            }
            Err(err) => {
                self.flows.remove(id);
                Err(auth_error(err))
            }
        }
    }
    async fn cancel_login(&mut self, id: &str) -> Result<Value> {
        if let Some(flow) = self.flows.remove(id) {
            self.manager.cancel_device_flow(&flow.upstream_id).await;
        }
        Ok(json!({"state":"cancelled"}))
    }
    async fn remove_account(&self, account: &str) -> Result<Value> {
        self.check_no_pending()?;
        self.manager
            .ensure_account_exists(account)
            .await
            .map_err(auth_error)?;
        // Removing the account from the helper does not log either client
        // out. Clear the helper's binding metadata first; if the following
        // manager write fails, the existing account can be safely re-added.
        let mut bindings = self.bindings()?;
        for scope in ["desktop", "cli"] {
            if bindings.get(scope).is_some_and(|b| b.account_id == account) {
                bindings.set(scope, None)?;
            }
        }
        self.save_bindings(&bindings)?;
        self.manager
            .remove_account(account)
            .await
            .map_err(auth_error)?;
        Ok(json!({"state":"removed","live_files":"unchanged"}))
    }

    fn clear_desktop_binding(&self) -> Result<Value> {
        self.check_no_pending()?;
        // An unknown writer must not allow even binding metadata to be changed.
        cc_switch_targets("desktop", &self.home)?;
        let path = self.root.join("bindings.json");
        let Some(raw) = safe_read(&path)? else {
            return Ok(json!({"state":"none"}));
        };
        let bindings: Bindings = serde_json::from_slice(&raw).map_err(|_| "账号绑定记录损坏")?;
        if bindings.version != 1 {
            return Err("账号绑定记录版本不受支持".into());
        }
        let Some(desktop) = bindings.desktop else {
            return Ok(json!({"state":"none"}));
        };
        if desktop.needs_reapply {
            return Err(
                "该 Desktop 绑定标记需重新应用；请先核查该账号登录及绑定，暂不可安全清理".into(),
            );
        }
        // Preserve every unrelated binding field, including fields introduced by
        // future versions; this action never modifies credentials or accounts.
        let mut value: Value = serde_json::from_slice(&raw).map_err(|_| "账号绑定记录损坏")?;
        let object = value.as_object_mut().ok_or("账号绑定记录损坏")?;
        object.insert("desktop".into(), Value::Null);
        if safe_read(&path)?.as_deref() != Some(raw.as_slice()) {
            return Err("账号绑定在操作期间变化，请重新核查".into());
        }
        cc_switch_targets("desktop", &self.home)?;
        write_private_atomic(
            &path,
            &serde_json::to_vec(&value).map_err(|_| "账号绑定无法编码")?,
        )?;
        Ok(json!({"state":"cleared"}))
    }

    async fn quota(&self, account: &str) -> Result<Value> {
        self.quota_mode(account).await
    }

    // Return only a currently issued access token over the private socket to
    // the local warmup worker. No refresh token or credential rotation occurs.
    async fn warmup_access(&self, account: &str) -> Result<Value> {
        self.check_no_pending()?;
        self.manager
            .ensure_account_exists(account)
            .await
            .map_err(auth_error)?;
        let bindings = self.bindings()?;
        let ignore_desktop = cc_switch_targets("desktop", &self.home).unwrap_or(true);
        if bindings.needs_reapply(account) {
            // Reuse only an independently bound, re-applied live source. A
            // stale Desktop binding can never authorize an old cached token.
            for scope in ["desktop", "cli"] {
                if let Some(binding) = bindings.authoritative(scope, ignore_desktop) {
                    if binding.account_id == account && !binding.needs_reapply {
                        let live = self.managed_live(scope, binding).await?;
                        if live.access_expires > chrono::Utc::now().timestamp() + 60 {
                            return Ok(
                                json!({"access":live.access,"workspace":live.workspace,"expires":live.access_expires}),
                            );
                        }
                    }
                }
            }
            return Err("账号重新登录后，请先重新应用到使用端".into());
        }
        if let Some((_, live)) = self.matching_live_auth(account).await {
            return Ok(
                json!({"access":live.access,"workspace":live.workspace,"expires":live.access_expires}),
            );
        }
        if let Some(bundle) = self.manager.helper_cached_bundle(account).await {
            if let Some(id) = bundle.id_token.as_deref() {
                let value = codex_config::codex_managed_oauth_auth_value(
                    &bundle.chatgpt_account_id,
                    &bundle.access_token,
                    Some(id),
                    &bundle.refresh_token,
                    &bundle.last_refresh,
                );
                if let Ok(raw) = serde_json::to_vec(&value) {
                    if let Ok(auth) = parse_auth(raw) {
                        if auth.access_expires > chrono::Utc::now().timestamp() + 60
                            && self.matches_account(account, &auth).await
                        {
                            return Ok(
                                json!({"access":auth.access,"workspace":auth.workspace,"expires":auth.access_expires}),
                            );
                        }
                    }
                }
            }
        }
        if let Some(auth) = self.matching_switch_backup(account).await {
            return Ok(
                json!({"access":auth.access,"workspace":auth.workspace,"expires":auth.access_expires}),
            );
        }
        Err("账号没有现有有效访问凭证，请重新登录".into())
    }

    async fn handoff_ready(&self, account: &str, scope: &str) -> Result<Value> {
        self.check_no_pending()?;
        require_native_desktop_owner(scope, &self.home)?;
        self.require_distinct_roots()?;
        self.target_config_hash(scope)?;
        require_cc_switch_stopped(scope, &self.home)?;
        self.manager
            .ensure_account_exists(account)
            .await
            .map_err(auth_error)?;
        let bindings = self.bindings()?;
        let ignore_desktop = cc_switch_targets("desktop", &self.home).unwrap_or(true);
        if let Some(current) = bindings.authoritative(scope, ignore_desktop) {
            self.managed_live(scope, current).await?;
        }
        for source in ["desktop", "cli"] {
            if let Some(binding) = bindings.authoritative(source, ignore_desktop) {
                if binding.account_id == account && !binding.needs_reapply {
                    let live = self.managed_live(source, binding).await?;
                    if live.access_expires > chrono::Utc::now().timestamp() + 60 {
                        return Ok(json!({"ready":true}));
                    }
                }
            }
        }
        let needs_reapply = bindings.needs_reapply(account);
        if !needs_reapply && self.matching_live_auth(account).await.is_some() {
            return Ok(json!({"ready":true}));
        }
        if let Some(bundle) = if needs_reapply {
            None
        } else {
            self.manager.helper_cached_bundle(account).await
        } {
            if let Some(id) = bundle.id_token.as_deref() {
                let value = codex_config::codex_managed_oauth_auth_value(
                    &bundle.chatgpt_account_id,
                    &bundle.access_token,
                    Some(id),
                    &bundle.refresh_token,
                    &bundle.last_refresh,
                );
                if let Ok(raw) = serde_json::to_vec(&value) {
                    if let Ok(auth) = parse_auth(raw) {
                        if auth.access_expires > chrono::Utc::now().timestamp() + 60
                            && self.matches_account(account, &auth).await
                        {
                            return Ok(json!({"ready":true}));
                        }
                    }
                }
            }
        }
        if !needs_reapply && self.matching_switch_backup(account).await.is_some() {
            return Ok(json!({"ready":true}));
        }
        Err("备用账号没有现有有效登录，请重新登录后再开启自动切号".into())
    }

    // An automatic handoff must not independently rotate a refresh token.
    // It may only use an already-issued access token in this process or a
    // matching live client; otherwise the user can log in again.
    async fn quota_access(&self, account: &str) -> Result<(String, String)> {
        self.check_no_pending()?;
        self.manager
            .ensure_account_exists(account)
            .await
            .map_err(auth_error)?;
        let bindings = self.bindings()?;
        let ignore_desktop = cc_switch_targets("desktop", &self.home).unwrap_or(true);
        let mut sources = Vec::new();
        for scope in ["desktop", "cli"] {
            if let Some(binding) = bindings.authoritative(scope, ignore_desktop) {
                if binding.account_id == account && !binding.needs_reapply {
                    sources.push(self.managed_live(scope, binding).await?);
                }
            }
        }
        sources.sort_by_key(|source| source.last_refresh_ms);
        let (access, workspace) = if let Some(source) = sources.pop() {
            if source.access_expires <= chrono::Utc::now().timestamp() + 60 {
                return Err("端内访问凭证已过期，等待客户端更新或重新登录".into());
            }
            (source.access, source.workspace)
        } else {
            let bound_needs_reapply = bindings.needs_reapply(account);
            if bound_needs_reapply {
                return Err("账号重新登录后，请先重新应用到使用端".into());
            }
            let cached = self.manager.helper_cached_bundle(account).await;
            if let Some(bundle) = cached {
                (bundle.access_token, bundle.chatgpt_account_id)
            } else if let Some(live) = self.matching_live_access(account).await {
                live
            } else {
                return Err("备用账号没有现有有效访问凭证，请重新登录".into());
            }
        };
        Ok((access, workspace))
    }

    async fn quota_mode(&self, account: &str) -> Result<Value> {
        let (access, workspace) = self.quota_access(account).await?;
        let url = quota_endpoint();
        let mut response = crate::proxy::http_client::get()
            .get(url)
            .timeout(std::time::Duration::from_secs(10))
            .header("Authorization", format!("Bearer {access}"))
            .header("ChatGPT-Account-Id", workspace)
            .header("User-Agent", "codex-cli")
            .send()
            .await
            .map_err(|_| "额度查询失败，请检查网络")?;
        if !response.status().is_success() {
            return Err(match response.status().as_u16() {
                401 | 403 => "额度凭证被拒绝，请检查登录",
                429 => "额度查询暂时受限",
                _ => "额度服务暂不可用",
            }
            .into());
        }
        let mut bytes = Vec::new();
        while let Some(chunk) = response.chunk().await.map_err(|_| "额度响应不可读取")? {
            if bytes.len() + chunk.len() > 1024 * 1024 {
                return Err("额度响应超出上限".into());
            }
            bytes.extend_from_slice(&chunk);
        }
        let value: Value = serde_json::from_slice(&bytes).map_err(|_| "额度响应格式无效")?;
        let mut windows = Vec::new();
        for key in ["primary_window", "secondary_window"] {
            if let Some(window) = value.pointer(&format!("/rate_limit/{key}")) {
                let used = window
                    .get("used_percent")
                    .and_then(Value::as_f64)
                    .ok_or("额度数据不完整")?;
                let seconds = window
                    .get("limit_window_seconds")
                    .and_then(Value::as_i64)
                    .ok_or("额度数据不完整")?;
                if !used.is_finite() || !(0.0..=100.0).contains(&used) || seconds <= 0 {
                    return Err("额度数据无效".into());
                }
                windows.push(json!({"seconds":seconds,"remaining":100.0-used,
                    "reset_at":window.get("reset_at").and_then(Value::as_i64)}));
            }
        }
        if windows.is_empty() {
            return Err("额度数据暂不可用".into());
        }
        Ok(
            json!({"windows":windows,"queried_at":chrono::Utc::now().to_rfc3339(),
            "limit_reached":value.pointer("/rate_limit/limit_reached").and_then(Value::as_bool)}),
        )
    }

    async fn apply(&self, account: &str, scope: &str, inspect_processes: bool) -> Result<Value> {
        self.apply_mode(account, scope, inspect_processes).await
    }

    async fn apply_mode(
        &self,
        account: &str,
        scope: &str,
        inspect_processes: bool,
    ) -> Result<Value> {
        self.check_no_pending()?;
        require_native_desktop_owner(scope, &self.home)?;
        self.require_distinct_roots()?;
        let config_hash = self.target_config_hash(scope)?;
        let target_dir = self.target_dir(scope)?;
        let target_path = self.target_path(scope)?;
        if inspect_processes {
            require_stopped(scope, &self.home)?;
        }
        if !target_dir.exists() {
            fs::create_dir_all(&target_dir).map_err(|_| "无法创建目标目录")?;
        }
        let meta = fs::symlink_metadata(&target_dir).map_err(|_| "目标目录不可读取")?;
        if !meta.is_dir() || meta.uid() != unsafe { libc::geteuid() } {
            return Err("目标目录类型或归属不安全".into());
        }
        let mut bindings = self.bindings()?;
        let old_raw = safe_read(&target_path)?;

        // Verify the outgoing identity without adopting live credentials into
        // the account store. Only an explicit login updates that store.
        if let Some(old_binding) = bindings.get(scope) {
            if let Err(err) = self.managed_live(scope, old_binding).await {
                // A manually restored, exact helper backup can be rebound to
                // its selected account without overwriting any client file.
                // Arbitrary external account changes remain rejected.
                let known_restore = if let Some(raw) = old_raw.as_ref() {
                    self.matching_switch_backup(account)
                        .await
                        .is_some_and(|auth| auth.raw == *raw)
                } else {
                    false
                };
                if !known_restore {
                    return Err(err);
                }
                let live = parse_auth(old_raw.clone().ok_or("恢复登录不可读取")?)?;
                require_native_desktop_owner(scope, &self.home)?;
                bindings.set(
                    scope,
                    Some(Binding {
                        account_id: account.to_string(),
                        refresh_digest: digest(live.refresh.as_bytes()),
                        needs_reapply: false,
                    }),
                )?;
                self.save_bindings(&bindings)?;
                return Ok(json!({"state":"already_bound","runtime_identity":"unverified"}));
            }
            if !old_binding.needs_reapply {
                if old_binding.account_id == account {
                    return Ok(json!({"state":"already_bound"}));
                }
            }
        }

        self.manager
            .ensure_account_exists(account)
            .await
            .map_err(auth_error)?;
        let mut sources: Vec<(String, LiveAuth)> = Vec::new();
        let ignore_desktop = cc_switch_targets("desktop", &self.home).unwrap_or(true);
        for source_scope in ["desktop", "cli"] {
            if let Some(binding) = bindings.authoritative(source_scope, ignore_desktop) {
                if binding.account_id == account && !binding.needs_reapply {
                    sources.push((
                        source_scope.to_string(),
                        self.managed_live(source_scope, binding).await?,
                    ));
                }
            }
        }
        let needs_reapply = bindings.needs_reapply(account);
        if sources.is_empty()
            && !needs_reapply
            && self.manager.helper_cached_bundle(account).await.is_none()
        {
            if let Some(source) = self.matching_live_auth(account).await {
                sources.push(source);
            }
        }
        sources.sort_by_key(|(_, source)| source.last_refresh_ms);
        if sources.len() == 2
            && sources[0].1.refresh != sources[1].1.refresh
            && sources[0].1.last_refresh_ms == sources[1].1.last_refresh_ms
        {
            return Err("两端凭证先后无法判断，请先检查账号".into());
        }

        let (candidate, source_guard) = if let Some((source_scope, source)) = sources.pop() {
            if source.access_expires <= chrono::Utc::now().timestamp() + 60 {
                return Err("该账号当前访问凭证已过期，请先在已登录端更新或重新登录".into());
            }
            let source_hash = digest(&source.raw);
            (source.raw, Some((source_scope, source_hash)))
        } else if !needs_reapply
            && self.manager.helper_cached_bundle(account).await.is_none()
            && self.matching_switch_backup(account).await.is_some()
        {
            (
                self.matching_switch_backup(account)
                    .await
                    .ok_or("切号备份不可用")?
                    .raw,
                None,
            )
        } else {
            if needs_reapply {
                return Err("账号重新登录后，请先重新应用到使用端".into());
            }
            let bundle = self
                .manager
                .helper_cached_bundle(account)
                .await
                .ok_or("备用账号没有现有有效登录，请重新登录")?;
            let id = bundle
                .id_token
                .as_deref()
                .ok_or("账号缺少身份凭证，请重新登录")?;
            let value = codex_config::codex_managed_oauth_auth_value(
                &bundle.chatgpt_account_id,
                &bundle.access_token,
                Some(id),
                &bundle.refresh_token,
                &bundle.last_refresh,
            );
            (
                serde_json::to_vec(&value).map_err(|_| "认证无法编码")?,
                None,
            )
        };
        let candidate_auth = parse_auth(candidate.clone())?;
        if !self.matches_account(account, &candidate_auth).await {
            return Err("目标账号身份不可确认".into());
        }
        let previous_digest = old_raw.as_ref().map(|v| digest(v));
        let backup_name = format!("{}.auth", Uuid::new_v4());
        if let Some(raw) = &old_raw {
            let backups = self.root.join("backups");
            ensure_private_dir(&backups)?;
            write_new_private(&backups.join(&backup_name), raw)?;
        }
        let stage_name = format!(".codex-helper-auth-{}", Uuid::new_v4());
        let staged = target_dir.join(&stage_name);
        let pending = json!({"version":1,"scope":scope,"account_id":account,
            "refresh_digest":digest(candidate_auth.refresh.as_bytes()),"previous":previous_digest,
            "candidate":digest(&candidate),"staged":stage_name,
            "backup":if old_raw.is_some() {Some(&backup_name)} else {None}});
        write_private_atomic(
            &self.root.join("pending.json"),
            &serde_json::to_vec(&pending).map_err(|_| "事务无法编码")?,
        )?;
        if let Err(e) = write_new_private(&staged, &candidate) {
            let _ = fs::remove_file(self.root.join("pending.json"));
            return Err(e);
        }
        if inspect_processes {
            if let Err(e) = require_stopped(scope, &self.home) {
                let _ = fs::remove_file(&staged);
                let _ = fs::remove_file(self.root.join("pending.json"));
                return Err(e);
            }
        }
        if self.target_config_hash(scope).ok().as_deref() != Some(config_hash.as_str()) {
            let _ = fs::remove_file(&staged);
            let _ = fs::remove_file(self.root.join("pending.json"));
            return Err("目标端配置在操作期间变化，已停止".into());
        }
        let current = safe_read(&target_path)?;
        if current.as_ref().map(|v| digest(v)) != previous_digest {
            let _ = fs::remove_file(&staged);
            let _ = fs::remove_file(self.root.join("pending.json"));
            return Err("目标登录在操作期间变化，已停止".into());
        }
        if let Some((source_scope, source_hash)) = source_guard {
            let current_source = safe_read(&self.target_path(&source_scope)?)?;
            if current_source.as_ref().map(|v| digest(v)).as_deref() != Some(source_hash.as_str()) {
                let _ = fs::remove_file(&staged);
                let _ = fs::remove_file(self.root.join("pending.json"));
                return Err("来源登录在操作期间变化，已停止".into());
            }
        }
        if let Err(e) = require_native_desktop_owner(scope, &self.home) {
            let _ = fs::remove_file(&staged);
            let _ = fs::remove_file(self.root.join("pending.json"));
            return Err(e);
        }
        fs::rename(&staged, &target_path).map_err(|_| "认证提交失败，请检查恢复记录")?;
        if let Ok(dir) = File::open(&target_dir) {
            let _ = dir.sync_all();
        }
        // If ownership changes after publishing, retain the pending journal for
        // review rather than silently creating a binding for the new owner.
        require_native_desktop_owner(scope, &self.home)?;
        bindings.set(
            scope,
            Some(Binding {
                account_id: account.to_string(),
                refresh_digest: digest(candidate_auth.refresh.as_bytes()),
                needs_reapply: false,
            }),
        )?;
        self.save_bindings(&bindings)?;
        fs::remove_file(self.root.join("pending.json"))
            .map_err(|_| "认证已写入，事务状态待核查")?;
        Ok(json!({"state":"installed","runtime_identity":"unverified"}))
    }

    async fn resolve_pending(&self, inspect_processes: bool) -> Result<Value> {
        let Some(raw) = safe_read(&self.root.join("pending.json"))? else {
            return Ok(json!({"state":"none"}));
        };
        let pending: Value =
            serde_json::from_slice(&raw).map_err(|_| "事务记录损坏，需人工检查")?;
        if pending.get("version").and_then(Value::as_u64) != Some(1) {
            return Err("事务记录版本无效".into());
        }
        let scope = pending
            .get("scope")
            .and_then(Value::as_str)
            .ok_or("事务记录缺少使用端")?;
        require_native_desktop_owner(scope, &self.home)?;
        let account = pending
            .get("account_id")
            .and_then(Value::as_str)
            .ok_or("事务记录缺少账号")?;
        let expected_new = pending
            .get("candidate")
            .and_then(Value::as_str)
            .ok_or("事务记录缺少目标")?;
        let expected_old = pending.get("previous").and_then(Value::as_str);
        let refresh_digest = pending
            .get("refresh_digest")
            .and_then(Value::as_str)
            .ok_or("事务记录缺少凭证代次")?;
        let stage_name = pending
            .get("staged")
            .and_then(Value::as_str)
            .ok_or("事务记录缺少临时文件")?;
        if !stage_name.starts_with(".codex-helper-auth-") || stage_name.contains('/') {
            return Err("事务记录中的临时文件无效".into());
        }
        if inspect_processes {
            require_stopped(scope, &self.home)?;
        }
        self.require_distinct_roots()?;
        let target = self.target_path(scope)?;
        let current = safe_read(&target)?;
        let current_hash = current.as_ref().map(|v| digest(v));
        let state;
        if current_hash.as_deref() == Some(expected_new) {
            let live = parse_auth(current.ok_or("当前认证不存在")?)?;
            if digest(live.refresh.as_bytes()) != refresh_digest
                || !self.matches_account(account, &live).await
            {
                return Err("当前认证与事务不一致，需人工检查".into());
            }
            self.manager
                .ensure_account_exists(account)
                .await
                .map_err(auth_error)?;
            let mut bindings = self.bindings()?;
            require_native_desktop_owner(scope, &self.home)?;
            bindings.set(
                scope,
                Some(Binding {
                    account_id: account.to_string(),
                    refresh_digest: refresh_digest.to_string(),
                    needs_reapply: false,
                }),
            )?;
            self.save_bindings(&bindings)?;
            state = "completed";
        } else if current_hash.as_deref() == expected_old {
            state = "not_published";
        } else {
            return Err("目标认证已被外部修改，请人工核查".into());
        }
        require_native_desktop_owner(scope, &self.home)?;
        let staged = self.target_dir(scope)?.join(stage_name);
        if let Some(staged_raw) = safe_read(&staged)? {
            if digest(&staged_raw) != expected_new {
                return Err("临时文件与事务不一致，请人工核查".into());
            }
            fs::remove_file(staged).map_err(|_| "无法清理临时认证文件")?;
        }
        fs::remove_file(self.root.join("pending.json")).map_err(|_| "无法完成事务核查")?;
        Ok(json!({"state":state,"runtime_identity":"unverified"}))
    }
}

use std::os::fd::AsRawFd;

/// Never terminate clients. A target change requires the user to close the
/// affected client and CC Switch, whose locks are not shared with this helper.
fn require_stopped(scope: &str, home: &Path) -> Result<()> {
    let output = Command::new("/bin/ps")
        .args(["-axo", "uid=,pid=,command="])
        .output()
        .map_err(|_| "无法检查运行中的客户端")?;
    if !output.status.success() || output.stdout.len() > MAX_FILE as usize {
        return Err("无法检查运行中的客户端".into());
    }
    let text = String::from_utf8(output.stdout).map_err(|_| "无法检查运行中的客户端")?;
    let uid = unsafe { libc::geteuid() }.to_string();
    for line in text.lines() {
        let mut parts = line.split_whitespace();
        let (Some(owner), Some(pid)) = (parts.next(), parts.next()) else {
            continue;
        };
        if owner != uid {
            continue;
        }
        let command = parts.collect::<Vec<_>>().join(" ");
        if command.contains("/CC Switch.app/Contents/MacOS/cc-switch")
            && running_cc_switch_targets(scope, home)?
        {
            return Err("CC Switch 也管理该端，请先退出它以避免同时写入".into());
        }
        if scope == "desktop"
            && (command.contains("/ChatGPT.app/Contents/MacOS/ChatGPT")
                || command.contains("/Codex.app/Contents/MacOS/"))
        {
            return Err("请先退出 Desktop，再应用该账号".into());
        }
        if is_codex_process(pid, &command)? {
            if process_targets_scope(pid, scope, home)? {
                return Err(if scope == "desktop" {
                    "请先退出使用 ~/.codex 的 Codex 后端，再应用该账号"
                } else {
                    "请先退出使用 ~/.codex-cli 的终端，再应用该账号"
                }
                .into());
            }
        }
    }
    Ok(())
}

// `ps ... command=` is not shell quoted: splitting it on whitespace loses an
// executable path containing spaces. Only inspect plausible candidates, then
// ask ps for its executable field separately before reading its environment.
fn is_codex_process(pid: &str, command: &str) -> Result<bool> {
    let first = command.split_whitespace().next().unwrap_or("");
    if Path::new(first).file_name().and_then(|s| s.to_str()) == Some("codex") {
        return Ok(true);
    }
    let candidate = command.match_indices("/codex").any(|(index, _)| {
        command[index + "/codex".len()..]
            .chars()
            .next()
            .is_none_or(char::is_whitespace)
    });
    if !candidate {
        return Ok(false);
    }
    let output = Command::new("/bin/ps")
        .args(["-p", pid, "-o", "comm="])
        .output()
        .map_err(|_| "无法核对 CLI 进程")?;
    if !output.status.success() || output.stdout.len() > MAX_FILE as usize {
        return Err("无法核对 CLI 进程".into());
    }
    let executable = std::str::from_utf8(&output.stdout)
        .map_err(|_| "无法核对 CLI 进程")?
        .trim();
    Ok(Path::new(executable).file_name().and_then(|s| s.to_str()) == Some("codex"))
}

#[cfg(any(target_os = "macos", target_os = "linux"))]
#[derive(Default)]
struct ProcessHomes {
    codex: Vec<String>,
    user: Vec<String>,
}

#[cfg(any(target_os = "macos", target_os = "linux"))]
fn codex_homes_target_scope(homes: &ProcessHomes, scope: &str, home: &Path) -> bool {
    let target = home.join(if scope == "desktop" {
        ".codex"
    } else {
        ".codex-cli"
    });
    if homes.codex.len() > 1 || (homes.codex.is_empty() && homes.user.len() != 1) {
        return true; // The effective directory is ambiguous.
    }
    let path = if let Some(value) = homes.codex.first() {
        PathBuf::from(value)
    } else {
        PathBuf::from(&homes.user[0]).join(".codex")
    };
    if !path.is_absolute() {
        return true;
    }
    if path == target {
        return true;
    }
    match (fs::canonicalize(path), fs::canonicalize(&target)) {
        (Ok(a), Ok(b)) => a == b,
        _ => true, // Failure to prove separation must not permit overwrites.
    }
}

// Read the kernel's NUL-delimited environment rather than splitting ps output:
// CODEX_HOME can contain spaces or be a symlink to the protected target.
#[cfg(target_os = "macos")]
fn process_targets_scope(pid: &str, scope: &str, home: &Path) -> Result<bool> {
    let pid = pid
        .parse::<libc::c_int>()
        .map_err(|_| "无法核对 CLI 进程")?;
    let mut limit_mib = [libc::CTL_KERN, libc::KERN_ARGMAX];
    let mut max_args: libc::c_int = 0;
    let mut limit_len = std::mem::size_of_val(&max_args);
    let limit_status = unsafe {
        libc::sysctl(
            limit_mib.as_mut_ptr(),
            limit_mib.len() as _,
            (&mut max_args as *mut libc::c_int).cast(),
            &mut limit_len,
            std::ptr::null_mut(),
            0,
        )
    };
    if limit_status != 0
        || limit_len != std::mem::size_of_val(&max_args)
        || max_args <= 0
        || max_args as u64 > MAX_FILE
    {
        return Err("无法核对 CLI 进程".into());
    }
    let mut mib = [libc::CTL_KERN, libc::KERN_PROCARGS2, pid];
    let mut data = vec![0u8; max_args as usize];
    let mut len = data.len();
    let status = unsafe {
        libc::sysctl(
            mib.as_mut_ptr(),
            mib.len() as _,
            data.as_mut_ptr().cast(),
            &mut len,
            std::ptr::null_mut(),
            0,
        )
    };
    if status != 0 || len > data.len() {
        return Err("无法核对 CLI 进程".into());
    }
    let homes = codex_homes_from_procargs(&data[..len])?;
    Ok(codex_homes_target_scope(&homes, scope, home))
}

#[cfg(target_os = "linux")]
fn process_targets_scope(pid: &str, scope: &str, home: &Path) -> Result<bool> {
    let pid = pid.parse::<u32>().map_err(|_| "无法核对 CLI 进程")?;
    let data = fs::read(format!("/proc/{pid}/environ")).map_err(|_| "无法核对 CLI 进程")?;
    if data.len() > MAX_FILE as usize {
        return Err("无法核对 CLI 进程".into());
    }
    let homes = codex_homes_from_environment(&data)?;
    Ok(codex_homes_target_scope(&homes, scope, home))
}

#[cfg(not(any(target_os = "macos", target_os = "linux")))]
fn process_targets_scope(pid: &str, scope: &str, home: &Path) -> Result<bool> {
    let env = Command::new("/bin/ps")
        .args(["eww", "-p", pid])
        .output()
        .map_err(|_| "无法核对 CLI 进程")?;
    if !env.status.success() || env.stdout.len() > MAX_FILE as usize {
        return Err("无法核对 CLI 进程".into());
    }
    let text = std::str::from_utf8(&env.stdout).map_err(|_| "无法核对 CLI 进程")?;
    Ok(process_uses_scope(text, scope, home))
}

#[cfg(any(target_os = "macos", target_os = "linux"))]
fn codex_homes_from_environment(data: &[u8]) -> Result<ProcessHomes> {
    let mut homes = ProcessHomes::default();
    for entry in data.split(|byte| *byte == 0) {
        let destination = if entry.starts_with(b"CODEX_HOME=") {
            Some((&mut homes.codex, &entry[b"CODEX_HOME=".len()..]))
        } else if entry.starts_with(b"HOME=") {
            Some((&mut homes.user, &entry[b"HOME=".len()..]))
        } else {
            None
        };
        if let Some((target, value)) = destination {
            target.push(
                std::str::from_utf8(value)
                    .map_err(|_| "无法核对 CLI 进程")?
                    .to_string(),
            );
        }
    }
    Ok(homes)
}

#[cfg(target_os = "macos")]
fn codex_homes_from_procargs(data: &[u8]) -> Result<ProcessHomes> {
    let argc = i32::from_ne_bytes(
        data.get(..4)
            .ok_or("无法核对 CLI 进程")?
            .try_into()
            .unwrap(),
    );
    if !(1..=32768).contains(&argc) {
        return Err("无法核对 CLI 进程".into());
    }
    let mut offset = 4;
    offset += data[offset..]
        .iter()
        .position(|byte| *byte == 0)
        .ok_or("无法核对 CLI 进程")?
        + 1;
    while offset < data.len() && data[offset] == 0 {
        offset += 1;
    }
    for _ in 0..argc {
        offset += data
            .get(offset..)
            .ok_or("无法核对 CLI 进程")?
            .iter()
            .position(|byte| *byte == 0)
            .ok_or("无法核对 CLI 进程")?
            + 1;
    }
    codex_homes_from_environment(data.get(offset..).ok_or("无法核对 CLI 进程")?)
}

#[cfg(any(test, not(any(target_os = "macos", target_os = "linux"))))]
fn process_uses_scope(env: &str, scope: &str, home: &Path) -> bool {
    let target = home.join(if scope == "desktop" {
        ".codex"
    } else {
        ".codex-cli"
    });
    let homes: Vec<_> = env
        .split_whitespace()
        .filter_map(|v| v.strip_prefix("CODEX_HOME="))
        .collect();
    // A codex process without CODEX_HOME uses the default Desktop directory.
    // Ambiguous multiple values are not safe to ignore.
    let contains_home = |path: &Path| {
        let Some(path) = path.to_str() else {
            return false;
        };
        let needle = format!("CODEX_HOME={path}");
        env.match_indices(&needle).any(|(index, _)| {
            (index == 0
                || env[..index]
                    .chars()
                    .next_back()
                    .is_some_and(char::is_whitespace))
                && env[index + needle.len()..]
                    .chars()
                    .next()
                    .is_none_or(char::is_whitespace)
        })
    };
    homes.len() > 1
        || (homes.is_empty() && scope == "desktop")
        || contains_home(&target)
        || fs::canonicalize(&target).is_ok_and(|path| contains_home(&path))
        || homes.iter().any(|value| {
            let path = PathBuf::from(value);
            if path == target {
                return true;
            }
            matches!((fs::canonicalize(path), fs::canonicalize(&target)), (Ok(a), Ok(b)) if a == b)
        })
}

const DESKTOP_CC_OWNER: &str = "Desktop 由 CC Switch 管理，请在 CC Switch 中切换账号";
const DESKTOP_OWNER_UNKNOWN: &str =
    "CC Switch 设置不可读取或无效，无法确认 Desktop 管理方；已停止写入";

fn clean_absolute_path(path: &Path) -> Option<PathBuf> {
    use std::path::Component;
    if !path.is_absolute() {
        return None;
    }
    let mut cleaned = PathBuf::new();
    for part in path.components() {
        match part {
            Component::RootDir => cleaned.push("/"),
            Component::Normal(value) => cleaned.push(value),
            Component::ParentDir => {
                cleaned.pop();
            }
            Component::CurDir => {}
            Component::Prefix(_) => return None,
        }
    }
    Some(cleaned)
}

fn cc_switch_targets(scope: &str, home: &Path) -> Result<bool> {
    let target = match scope {
        "desktop" => home.join(".codex"),
        "cli" => home.join(".codex-cli"),
        _ => return Err("使用端无效".into()),
    };
    let settings = home.join(".cc-switch/settings.json");
    match fs::symlink_metadata(&settings) {
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(false),
        Err(_) => return Err(DESKTOP_OWNER_UNKNOWN.into()),
        Ok(_) => {}
    }
    let metadata = fs::metadata(&settings).map_err(|_| DESKTOP_OWNER_UNKNOWN)?;
    if !metadata.is_file() || metadata.len() > MAX_FILE {
        return Err(DESKTOP_OWNER_UNKNOWN.into());
    }
    let file = File::open(&settings).map_err(|_| DESKTOP_OWNER_UNKNOWN)?;
    let metadata = file.metadata().map_err(|_| DESKTOP_OWNER_UNKNOWN)?;
    if !metadata.is_file() || metadata.len() > MAX_FILE {
        return Err(DESKTOP_OWNER_UNKNOWN.into());
    }
    let mut raw = Vec::new();
    file.take(MAX_FILE + 1)
        .read_to_end(&mut raw)
        .map_err(|_| DESKTOP_OWNER_UNKNOWN)?;
    if raw.len() > MAX_FILE as usize {
        return Err(DESKTOP_OWNER_UNKNOWN.into());
    }
    let value: Value = serde_json::from_slice(&raw).map_err(|_| DESKTOP_OWNER_UNKNOWN)?;
    let object = value.as_object().ok_or(DESKTOP_OWNER_UNKNOWN)?;
    let configured = match object.get("codexConfigDir") {
        None => "",
        Some(Value::String(value)) => value.trim(),
        Some(_) => return Err(DESKTOP_OWNER_UNKNOWN.into()),
    };
    let configured = if configured.is_empty() {
        home.join(".codex")
    } else if configured == "~" {
        home.to_path_buf()
    } else if let Some(rest) = configured.strip_prefix("~/") {
        home.join(rest)
    } else {
        PathBuf::from(configured)
    };
    let configured = clean_absolute_path(&configured).ok_or(DESKTOP_OWNER_UNKNOWN)?;
    let target = clean_absolute_path(&target).ok_or(DESKTOP_OWNER_UNKNOWN)?;
    if configured == target {
        return Ok(true);
    }
    if let (Ok(a), Ok(b)) = (fs::canonicalize(&configured), fs::canonicalize(&target)) {
        return Ok(a == b);
    }
    Ok(configured == target)
}

// Existing running-process guards remain conservative if CC Switch is running
// but its settings are absent: its actual target cannot then be established.
fn running_cc_switch_targets(scope: &str, home: &Path) -> Result<bool> {
    if fs::symlink_metadata(home.join(".cc-switch/settings.json")).is_err() {
        return Err("无法确认 CC Switch 的目标目录，请先退出它".into());
    }
    cc_switch_targets(scope, home).map_err(|_| "无法确认 CC Switch 的目标目录，请先退出它".into())
}

fn require_native_desktop_owner(scope: &str, home: &Path) -> Result<()> {
    if scope == "desktop" {
        if cc_switch_targets("desktop", home)? {
            return Err(DESKTOP_CC_OWNER.into());
        }
    }
    Ok(())
}

impl AccountService {
    async fn handle(&mut self, request: Value, owner: Option<&str>) -> Result<Value> {
        let method = request
            .get("method")
            .and_then(Value::as_str)
            .ok_or("操作无效")?;
        if method == "handoff_acquire" {
            let owner = owner.ok_or("交接连接不可确认")?;
            if !self.shared_service || self.handoff_owner.is_some() || !self.flows.is_empty() {
                return Err("账号管理器正忙，请先完成其他账号操作".into());
            }
            self.check_no_pending()?;
            self.handoff_owner = Some(owner.to_string());
            return Ok(json!({"acquired":true}));
        }
        if self.shared_service
            && method.starts_with("handoff_")
            && (owner.is_none() || self.handoff_owner.as_deref() != owner)
        {
            return Err("交接管理权不可确认".into());
        }
        if self.handoff_owner.is_some()
            && !method.starts_with("handoff_")
            && method != "list"
            && method != "warmup_access"
        {
            return Err("自动交接正在管理账号，请先停止交接".into());
        }
        let field = |key: &str| {
            request
                .get(key)
                .and_then(Value::as_str)
                .filter(|v| v.len() <= 128)
                .ok_or("操作参数无效")
        };
        match method {
            "list" => self.list().await,
            "login_start" => {
                self.start_login(
                    field("mode")?,
                    request.get("account_id").and_then(Value::as_str),
                )
                .await
            }
            "login_poll" => self.poll_login(field("flow_id")?).await,
            "login_cancel" => self.cancel_login(field("flow_id")?).await,
            "apply" => {
                if !self.flows.is_empty() {
                    return Err("请先完成或取消当前登录".into());
                }
                self.apply(field("account_id")?, field("scope")?, true)
                    .await
            }
            "remove" => {
                if !self.flows.is_empty() {
                    return Err("请先完成或取消当前登录".into());
                }
                self.remove_account(field("account_id")?).await
            }
            "clear_desktop_binding" => {
                if !self.flows.is_empty() {
                    return Err("请先完成或取消当前登录".into());
                }
                self.clear_desktop_binding()
            }
            "resolve_pending" => self.resolve_pending(true).await,
            "quota" => {
                if !self.flows.is_empty() {
                    return Err("登录期间暂不刷新额度".into());
                }
                self.quota(field("account_id")?).await
            }
            "warmup_access" => {
                if owner.is_none() || !self.shared_service || !self.flows.is_empty() {
                    return Err("账号登录或本机连接尚未就绪".into());
                }
                self.warmup_access(field("account_id")?).await
            }
            "handoff_quota" => {
                if !self.flows.is_empty() {
                    return Err("登录期间暂不检查自动切号额度".into());
                }
                self.quota_mode(field("account_id")?).await
            }
            "handoff_ready" => {
                if !self.flows.is_empty() {
                    return Err("请先完成或取消当前登录".into());
                }
                self.handoff_ready(field("account_id")?, field("scope")?)
                    .await
            }
            "handoff_apply" => {
                if !self.flows.is_empty() {
                    return Err("请先完成或取消当前登录".into());
                }
                self.apply_mode(field("account_id")?, field("scope")?, true)
                    .await
            }
            // This method exists only in the unit-test executable. It lets a
            // synthetic Desktop coordinator exercise the real transaction
            // while the user's actual Desktop remains running. The real
            // process guard is independently covered by live-process tests.
            #[cfg(test)]
            "handoff_fixture_apply_isolated" => {
                let real = PathBuf::from(std::env::var_os("HOME").ok_or("测试目录不可确认")?);
                let marked = self.home.join(".codex-helper-test-root").is_file();
                let distinct = matches!((fs::canonicalize(&self.home), fs::canonicalize(real)),
                    (Ok(test), Ok(actual)) if test != actual);
                if std::env::var("CODEX_HELPER_TEST_RUST_SERVICE_FIXTURE").as_deref()
                    != Ok("run-isolated-manager")
                    || !marked
                    || !distinct
                {
                    return Err("测试目录不可确认".into());
                }
                self.apply_mode(field("account_id")?, field("scope")?, false)
                    .await
            }
            _ => Err("操作不受支持".into()),
        }
    }
}

// The target Codex client can still be running while the handoff is being
// observed, but CC Switch must not be a competing writer before we stop it.
fn require_cc_switch_stopped(scope: &str, home: &Path) -> Result<()> {
    let output = Command::new("/bin/ps")
        .args(["-axo", "uid=,command="])
        .output()
        .map_err(|_| "无法检查 CC Switch")?;
    if !output.status.success() || output.stdout.len() > MAX_FILE as usize {
        return Err("无法检查 CC Switch".into());
    }
    let text = String::from_utf8(output.stdout).map_err(|_| "无法检查 CC Switch")?;
    let uid = unsafe { libc::geteuid() }.to_string();
    for line in text.lines() {
        let line = line.trim_start();
        let Some((owner, command)) = line.split_once(char::is_whitespace) else {
            continue;
        };
        let command = command.trim_start();
        if owner == uid
            && command.starts_with('/')
            && command.contains("/CC Switch.app/Contents/MacOS/cc-switch")
            && running_cc_switch_targets(scope, home)?
        {
            return Err("CC Switch 也管理该端，请先退出它以避免同时写入".into());
        }
    }
    Ok(())
}

#[cfg(test)]
static TEST_QUOTA_URL: std::sync::OnceLock<String> = std::sync::OnceLock::new();
fn quota_endpoint() -> String {
    #[cfg(test)]
    if let Some(url) = TEST_QUOTA_URL.get() {
        return url.clone();
    }
    "https://chatgpt.com/backend-api/wham/usage".to_string()
}

pub async fn run(home: PathBuf, root: PathBuf) -> Result<()> {
    let mut service = AccountService::new(home, root.clone())?;
    if std::env::var_os("CODEX_HELPER_ACCOUNT_SERVICE").as_deref()
        == Some(std::ffi::OsStr::new("1"))
    {
        service.shared_service = true;
        return run_shared(service, &root).await;
    }
    let mut input = tokio::io::BufReader::new(tokio::io::stdin());
    let stdout = io::stdout();
    let mut output = stdout.lock();
    while let Some(line) = read_protocol_line(&mut input)
        .await
        .map_err(|_| "本机请求超出上限或连接已关闭")?
    {
        let request: Value = match serde_json::from_str(&line) {
            Ok(v) => v,
            Err(_) => continue,
        };
        let id = request.get("id").cloned().unwrap_or(Value::Null);
        let response = match service.handle(request, None).await {
            Ok(result) => json!({"id":id,"result":result}),
            Err(error) => json!({"id":id,"error":error}),
        };
        serde_json::to_writer(&mut output, &response).map_err(|_| "本机连接不可写")?;
        writeln!(output)
            .and_then(|_| output.flush())
            .map_err(|_| "本机连接不可写")?;
    }
    Ok(())
}

// The helper and its handoff worker share a single manager. The private
// directory and account-store lock prevent other users or another manager
// from replacing the socket. The worker's lease ends on disconnect.
fn shared_socket_path(root: &Path) -> Result<PathBuf> {
    use std::os::unix::ffi::OsStrExt;
    let root = fs::canonicalize(root).map_err(|_| "账号目录不可确认")?;
    let hash = digest(root.as_os_str().as_bytes());
    let directory = PathBuf::from(format!(
        "/tmp/codex-helper-account-{}-{}",
        unsafe { libc::geteuid() },
        &hash[..16]
    ));
    ensure_private_dir(&directory)?;
    Ok(directory.join("manager.sock"))
}

fn shared_listener(root: &Path) -> Result<(tokio::net::UnixListener, PathBuf)> {
    use std::os::unix::fs::FileTypeExt;
    let path = shared_socket_path(root)?;
    match fs::symlink_metadata(&path) {
        Ok(meta) if meta.file_type().is_socket() && meta.uid() == unsafe { libc::geteuid() } => {
            fs::remove_file(&path).map_err(|_| "无法清理旧账号连接")?;
        }
        Ok(_) => return Err("账号连接路径不安全".into()),
        Err(e) if e.kind() == io::ErrorKind::NotFound => {}
        Err(_) => return Err("账号连接路径不可检查".into()),
    }
    let listener = tokio::net::UnixListener::bind(&path).map_err(|_| "无法启动账号连接")?;
    fs::set_permissions(&path, fs::Permissions::from_mode(0o600))
        .map_err(|_| "无法保护账号连接")?;
    Ok((listener, path))
}

// Enforce the bound while reading, including inputs that never send a newline.
async fn read_protocol_line<R: tokio::io::AsyncBufRead + Unpin>(
    reader: &mut R,
) -> io::Result<Option<String>> {
    use tokio::io::AsyncBufReadExt;
    let mut bytes = Vec::new();
    let mut bounded = tokio::io::AsyncReadExt::take(reader, 64 * 1024 + 1);
    if bounded.read_until(b'\n', &mut bytes).await? == 0 {
        return Ok(None);
    }
    if bytes.len() > 64 * 1024 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "request too large",
        ));
    }
    String::from_utf8(bytes)
        .map(Some)
        .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "invalid request encoding"))
}

async fn shared_connection(
    stream: tokio::net::UnixStream,
    service: std::sync::Arc<tokio::sync::Mutex<AccountService>>,
) {
    use tokio::io::{AsyncWriteExt, BufReader};
    let owner = Uuid::new_v4().to_string();
    let (read, mut write) = stream.into_split();
    let mut lines = BufReader::new(read);
    while let Ok(Some(line)) = read_protocol_line(&mut lines).await {
        if line.len() > 64 * 1024 {
            break;
        }
        let Ok(request) = serde_json::from_str::<Value>(&line) else {
            break;
        };
        let method = request.get("method").and_then(Value::as_str).unwrap_or("");
        if method != "handoff_acquire"
            && !method.starts_with("handoff_")
            && method != "list"
            && method != "warmup_access"
        {
            break;
        }
        let id = request.get("id").cloned().unwrap_or(Value::Null);
        let response = {
            let mut service = service.lock().await;
            match service.handle(request, Some(&owner)).await {
                Ok(result) => json!({"id":id,"result":result}),
                Err(error) => json!({"id":id,"error":error}),
            }
        };
        let Ok(mut bytes) = serde_json::to_vec(&response) else {
            break;
        };
        bytes.push(b'\n');
        if write.write_all(&bytes).await.is_err() {
            break;
        }
    }
    let mut service = service.lock().await;
    if service.handoff_owner.as_deref() == Some(owner.as_str()) {
        service.handoff_owner = None;
    }
}

async fn run_shared(service: AccountService, root: &Path) -> Result<()> {
    use tokio::io::{AsyncWriteExt, BufReader};
    let (listener, path) = shared_listener(root)?;
    let service = std::sync::Arc::new(tokio::sync::Mutex::new(service));
    let shared = service.clone();
    let accept = tokio::spawn(async move {
        while let Ok((stream, _)) = listener.accept().await {
            tokio::spawn(shared_connection(stream, shared.clone()));
        }
    });
    let mut lines = BufReader::new(tokio::io::stdin());
    let mut output = tokio::io::stdout();
    while let Ok(Some(line)) = read_protocol_line(&mut lines).await {
        if line.len() > 64 * 1024 {
            break;
        }
        let Ok(request) = serde_json::from_str::<Value>(&line) else {
            continue;
        };
        let id = request.get("id").cloned().unwrap_or(Value::Null);
        let response = {
            let mut service = service.lock().await;
            match service.handle(request, None).await {
                Ok(result) => json!({"id":id,"result":result}),
                Err(error) => json!({"id":id,"error":error}),
            }
        };
        let mut bytes = serde_json::to_vec(&response).map_err(|_| "本机连接不可写")?;
        bytes.push(b'\n');
        output
            .write_all(&bytes)
            .await
            .map_err(|_| "本机连接不可写")?;
        output.flush().await.map_err(|_| "本机连接不可写")?;
    }
    accept.abort();
    let _ = fs::remove_file(path);
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn stopped_guard_checks_default_and_explicit_codex_homes() {
        let home = Path::new("/tmp/codex-home-test");
        assert!(process_uses_scope("PATH=/bin", "desktop", home));
        assert!(!process_uses_scope("PATH=/bin", "cli", home));
        assert!(process_uses_scope(
            "CODEX_HOME=/tmp/codex-home-test/.codex-cli",
            "cli",
            home
        ));
        assert!(!process_uses_scope(
            "CODEX_HOME=/tmp/codex-home-test/.codex-cli",
            "desktop",
            home
        ));
        assert!(process_uses_scope(
            "CODEX_HOME=/x CODEX_HOME=/y",
            "desktop",
            home
        ));
        let spaced_home = Path::new("/tmp/codex home-test");
        let spaced_env = "TERM=xterm CODEX_HOME=/tmp/codex home-test/.codex-cli USER=test";
        assert!(process_uses_scope(spaced_env, "cli", spaced_home));
        assert!(!process_uses_scope(spaced_env, "desktop", spaced_home));
    }

    #[cfg(any(target_os = "macos", target_os = "linux"))]
    #[test]
    fn process_homes_use_real_default_and_fail_closed_when_unknown() {
        let (_temp, service) = setup();
        let default = ProcessHomes {
            codex: vec![],
            user: vec![service.home.to_str().unwrap().into()],
        };
        assert!(codex_homes_target_scope(&default, "desktop", &service.home));
        assert!(!codex_homes_target_scope(&default, "cli", &service.home));
        assert!(codex_homes_target_scope(
            &ProcessHomes::default(),
            "cli",
            &service.home
        ));
        let invalid = ProcessHomes {
            codex: vec!["relative/path".into()],
            user: vec![],
        };
        assert!(codex_homes_target_scope(&invalid, "cli", &service.home));
        let duplicate = ProcessHomes {
            codex: vec!["/x".into(), "/y".into()],
            user: vec![],
        };
        assert!(codex_homes_target_scope(&duplicate, "cli", &service.home));
        let other_home = service.home.join("other-home");
        fs::create_dir_all(&other_home).unwrap();
        std::os::unix::fs::symlink(service.home.join(".codex-cli"), other_home.join(".codex"))
            .unwrap();
        let aliased_default = ProcessHomes {
            codex: vec![],
            user: vec![other_home.to_str().unwrap().into()],
        };
        assert!(codex_homes_target_scope(
            &aliased_default,
            "cli",
            &service.home
        ));
    }

    #[cfg(target_os = "macos")]
    #[test]
    fn procargs_parser_preserves_spaced_values_and_rejects_truncation() {
        let mut raw = 2i32.to_ne_bytes().to_vec();
        raw.extend_from_slice(b"/tmp/codex\0\0\0/tmp/codex\0argument with space\0HOME=/tmp/home\0CODEX_HOME=/tmp/path with space\0");
        let parsed = codex_homes_from_procargs(&raw).unwrap();
        assert_eq!(parsed.codex, vec!["/tmp/path with space"]);
        assert_eq!(parsed.user, vec!["/tmp/home"]);
        let mut truncated = 2i32.to_ne_bytes().to_vec();
        truncated.extend_from_slice(b"/tmp/codex\0\0/tmp/codex\0missing second argument");
        assert!(codex_homes_from_procargs(&truncated).is_err());
    }

    static CLI_PROCESS_GUARD_TEST_LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());

    fn start_test_codex(bin: &Path, codex_home: &Path) -> std::process::Child {
        fs::create_dir_all(bin).unwrap();
        let executable = bin.join("codex");
        let source = bin.join("test.c");
        fs::write(
            &source,
            b"#include <unistd.h>\nint main(void) { sleep(15); return 0; }\n",
        )
        .unwrap();
        let compiler = Command::new("/usr/bin/cc")
            .arg("-o")
            .arg(&executable)
            .arg(&source)
            .output()
            .unwrap();
        assert!(
            compiler.status.success(),
            "cannot compile isolated process fixture"
        );
        let mut child = Command::new(&executable)
            .env("CODEX_HOME", codex_home)
            .spawn()
            .unwrap();
        std::thread::sleep(std::time::Duration::from_millis(100));
        assert!(
            child.try_wait().unwrap().is_none(),
            "isolated process exited before guard check"
        );
        child
    }

    #[test]
    fn stopped_guard_detects_codex_executable_in_path_with_spaces() {
        let _guard = CLI_PROCESS_GUARD_TEST_LOCK.lock().unwrap();
        let (_temp, service) = setup();
        fs::create_dir_all(service.home.join(".cc-switch")).unwrap();
        write_new_private(
            &service.home.join(".cc-switch/settings.json"),
            br#"{"codexConfigDir":"~/.codex"}"#,
        )
        .unwrap();
        let bin = service.home.join("executable directory with spaces");
        let mut child = start_test_codex(&bin, &service.home.join(".codex-cli"));
        let direct = process_targets_scope(&child.id().to_string(), "cli", &service.home);
        let result = require_stopped("cli", &service.home);
        let still_running = child.try_wait().unwrap().is_none();
        let _ = child.kill();
        let _ = child.wait();
        assert_eq!(
            direct,
            Ok(true),
            "isolated process environment was not classified"
        );
        assert!(still_running, "isolated process exited during guard check");
        assert_eq!(
            result.unwrap_err(),
            "请先退出使用 ~/.codex-cli 的终端，再应用该账号"
        );
    }

    #[test]
    fn stopped_guard_detects_spaced_codex_home_symlink() {
        let _guard = CLI_PROCESS_GUARD_TEST_LOCK.lock().unwrap();
        let (_temp, service) = setup();
        fs::create_dir_all(service.home.join(".cc-switch")).unwrap();
        write_new_private(
            &service.home.join(".cc-switch/settings.json"),
            br#"{"codexConfigDir":"~/.codex"}"#,
        )
        .unwrap();
        let bin = service.home.join("bin");
        let aliases = service.home.join("home alias with spaces");
        fs::create_dir_all(&aliases).unwrap();
        let alias = aliases.join("cli");
        std::os::unix::fs::symlink(service.home.join(".codex-cli"), &alias).unwrap();
        let mut child = start_test_codex(&bin, &alias);
        let direct = process_targets_scope(&child.id().to_string(), "cli", &service.home);
        let result = require_stopped("cli", &service.home);
        let still_running = child.try_wait().unwrap().is_none();
        let _ = child.kill();
        let _ = child.wait();
        assert_eq!(
            direct,
            Ok(true),
            "isolated alias environment was not classified"
        );
        assert!(still_running, "isolated process exited during guard check");
        assert_eq!(
            result.unwrap_err(),
            "请先退出使用 ~/.codex-cli 的终端，再应用该账号"
        );
    }

    // Compiled only into the Rust unit-test executable. The Go integration
    // harness uses the real shared service through its private Unix socket.
    #[tokio::test]
    #[ignore = "Run only through the isolated Go integration harness"]
    async fn isolated_integration_service_fixture() {
        if std::env::var("CODEX_HELPER_TEST_RUST_SERVICE_FIXTURE").as_deref()
            != Ok("run-isolated-manager")
        {
            return;
        }
        let real = PathBuf::from(std::env::var_os("HOME").expect("HOME required"));
        let home =
            PathBuf::from(std::env::var_os("CODEX_HELPER_TEST_HOME").expect("test root required"));
        assert!(home.is_absolute() && home.join(".codex-helper-test-root").is_file());
        assert_ne!(home.canonicalize().unwrap(), real.canonicalize().unwrap());
        assert_eq!(
            std::env::var("CODEX_HELPER_ACCOUNT_SERVICE").as_deref(),
            Ok("1")
        );
        let quota =
            std::env::var("CODEX_HELPER_TEST_QUOTA_ENDPOINT").expect("local quota required");
        let url = url::Url::parse(&quota).unwrap();
        assert_eq!(url.scheme(), "http");
        assert_eq!(url.host_str(), Some("127.0.0.1"));
        assert!(url.port().is_some() && url.username().is_empty() && url.password().is_none());
        TEST_QUOTA_URL.set(quota).unwrap();
        let root = home.join("Library/Application Support/CodexHelper-AccountPreview/accounts");
        run(home, root).await.unwrap();
    }

    #[tokio::test]
    async fn protocol_rejects_oversized_input_without_waiting_for_newline() {
        use tokio::io::{AsyncWriteExt, BufReader};
        let (mut sender, receiver) = tokio::io::duplex(128 * 1024);
        sender.write_all(&vec![b'x'; 64 * 1024 + 1]).await.unwrap();
        let mut reader = BufReader::new(receiver);
        let error = tokio::time::timeout(
            std::time::Duration::from_secs(1),
            read_protocol_line(&mut reader),
        )
        .await
        .expect("oversized request waited for a newline")
        .unwrap_err();
        assert_eq!(error.kind(), io::ErrorKind::InvalidData);
    }

    #[tokio::test]
    async fn shared_manager_lease_blocks_mutation_and_releases_on_disconnect() {
        use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
        let (_temp, mut service) = setup();
        service.shared_service = true;
        let (listener, path) = shared_listener(&service.root).unwrap();
        let service = std::sync::Arc::new(tokio::sync::Mutex::new(service));
        let shared = service.clone();
        let accept = tokio::spawn(async move {
            while let Ok((stream, _)) = listener.accept().await {
                tokio::spawn(shared_connection(stream, shared.clone()));
            }
        });
        async fn request(
            stream: &mut BufReader<tokio::net::UnixStream>,
            id: u8,
            method: &str,
        ) -> Value {
            let mut bytes = serde_json::to_vec(&json!({"id":id,"method":method})).unwrap();
            bytes.push(b'\n');
            stream.get_mut().write_all(&bytes).await.unwrap();
            let mut line = String::new();
            tokio::time::timeout(
                std::time::Duration::from_secs(3),
                stream.read_line(&mut line),
            )
            .await
            .unwrap()
            .unwrap();
            serde_json::from_str(&line).unwrap()
        }
        let mut first = BufReader::new(tokio::net::UnixStream::connect(&path).await.unwrap());
        let mut second = BufReader::new(tokio::net::UnixStream::connect(&path).await.unwrap());
        assert_eq!(
            request(&mut first, 1, "handoff_acquire").await["result"]["acquired"],
            true
        );
        assert!(request(&mut second, 1, "handoff_acquire").await["error"].is_string());
        {
            let mut locked = service.lock().await;
            assert!(locked.handle(json!({"method":"list"}), None).await.is_ok());
            assert!(locked
                .handle(json!({"method":"remove","account_id":"x"}), None)
                .await
                .is_err());
            assert!(locked
                .handle(
                    json!({"method":"handoff_ready","scope":"cli","account_id":"x"}),
                    None
                )
                .await
                .is_err());
        }
        drop(first);
        for _ in 0..50 {
            if service.lock().await.handoff_owner.is_none() {
                break;
            }
            tokio::time::sleep(std::time::Duration::from_millis(10)).await;
        }
        assert_eq!(
            request(&mut second, 2, "handoff_acquire").await["result"]["acquired"],
            true
        );
        drop(second);
        accept.abort();
        let _ = fs::remove_file(&path);
        let _ = fs::remove_dir(path.parent().unwrap());
    }
    use axum::{
        extract::State, http::StatusCode, response::IntoResponse, routing::post, Json, Router,
    };
    use base64::Engine;
    use std::sync::{
        atomic::{AtomicUsize, Ordering},
        Arc,
    };

    fn fake_access() -> String {
        let header = URL_SAFE_NO_PAD.encode(br#"{"alg":"none"}"#);
        let body =
            URL_SAFE_NO_PAD.encode(json!({"exp":chrono::Utc::now().timestamp()+3600}).to_string());
        format!("{header}.{body}.")
    }
    fn setup() -> (tempfile::TempDir, AccountService) {
        let temp = tempfile::tempdir().unwrap();
        let home = temp.path().join("home");
        fs::create_dir_all(home.join(".codex")).unwrap();
        fs::create_dir_all(home.join(".codex-cli")).unwrap();
        for name in [".codex", ".codex-cli"] {
            write_new_private(
                &home.join(name).join("config.toml"),
                b"cli_auth_credentials_store = \"file\"\n",
            )
            .unwrap();
        }
        let service = AccountService::new(home, temp.path().join("state")).unwrap();
        (temp, service)
    }
    async fn add(service: &AccountService, id: &str, sub: &str, workspace: &str) {
        service
            .manager
            .add_test_account_with_workspace_and_access_token(
                id,
                workspace,
                &fake_access(),
                Some(&codex_config::test_codex_id_token(sub)),
            )
            .await
            .unwrap();
    }
    #[tokio::test]
    async fn same_account_can_be_assigned_to_both_without_overwriting_other_scope() {
        let (_temp, service) = setup();
        add(&service, "a", "alice", "workspace-a").await;
        add(&service, "b", "bob", "workspace-b").await;
        assert_eq!(
            service.apply("a", "desktop", false).await.unwrap()["state"],
            "installed"
        );
        assert_eq!(
            service.apply("a", "cli", false).await.unwrap()["state"],
            "installed"
        );
        let cli_before = fs::read(service.target_path("cli").unwrap()).unwrap();
        let listed = service.list().await.unwrap();
        let a = listed["accounts"]
            .as_array()
            .unwrap()
            .iter()
            .find(|row| row["id"] == "a")
            .unwrap();
        assert_eq!(a["bindings"].as_array().unwrap().len(), 2);
        assert_eq!(
            service.apply("b", "desktop", false).await.unwrap()["state"],
            "installed"
        );
        assert_eq!(
            fs::read(service.target_path("cli").unwrap()).unwrap(),
            cli_before
        );
        let desktop =
            parse_auth(fs::read(service.target_path("desktop").unwrap()).unwrap()).unwrap();
        assert_eq!(desktop.workspace, "workspace-b");
        assert!(!service.root.join("pending.json").exists());
    }

    #[tokio::test]
    async fn list_observes_external_account_changes_without_writing_bindings() {
        let (_temp, service) = setup();
        add(&service, "a", "alice", "workspace-a").await;
        add(&service, "b", "bob", "workspace-b").await;
        service.apply("b", "cli", false).await.unwrap();
        let cli = fs::read(service.target_path("cli").unwrap()).unwrap();
        let desktop = service.target_path("desktop").unwrap();
        let auth = codex_config::codex_managed_oauth_auth_value(
            "workspace-a",
            &fake_access(),
            Some(&codex_config::test_codex_id_token("alice")),
            "external-a-refresh",
            &chrono::Utc::now().to_rfc3339(),
        );
        write_new_private(&desktop, &serde_json::to_vec(&auth).unwrap()).unwrap();
        let before_auth = fs::read(&desktop).unwrap();
        let before_bindings = fs::read(service.root.join("bindings.json")).unwrap();
        let before_store = fs::read(service.root.join("codex_oauth_auth.json")).unwrap();
        let first = service.list().await.unwrap();
        let rows = first["accounts"].as_array().unwrap();
        let a = rows.iter().find(|r| r["id"] == "a").unwrap();
        let b = rows.iter().find(|r| r["id"] == "b").unwrap();
        assert_eq!(a["current_scopes"], json!(["desktop"]));
        assert_eq!(b["current_scopes"], json!(["cli"]));
        assert!(a["bindings"].as_array().unwrap().is_empty());
        assert_eq!(first["scope_observations"]["desktop"], "matched");
        assert_eq!(fs::read(&desktop).unwrap(), before_auth);
        assert_eq!(
            fs::read(service.root.join("bindings.json")).unwrap(),
            before_bindings
        );
        assert_eq!(
            fs::read(service.root.join("codex_oauth_auth.json")).unwrap(),
            before_store
        );

        // An external switch is reflected on the very next list without
        // adopting its credential or rewriting either binding.
        write_private_atomic(&desktop, &cli).unwrap();
        let second = service.list().await.unwrap();
        let rows = second["accounts"].as_array().unwrap();
        assert_eq!(
            rows.iter().find(|r| r["id"] == "a").unwrap()["current_scopes"],
            json!([])
        );
        assert_eq!(
            rows.iter().find(|r| r["id"] == "b").unwrap()["current_scopes"],
            json!(["desktop", "cli"])
        );
        assert_eq!(
            fs::read(service.root.join("bindings.json")).unwrap(),
            before_bindings
        );
        assert_eq!(
            fs::read(service.root.join("codex_oauth_auth.json")).unwrap(),
            before_store
        );

        let header = URL_SAFE_NO_PAD.encode(br#"{"alg":"none"}"#);
        let expired_payload =
            URL_SAFE_NO_PAD.encode(json!({"exp": chrono::Utc::now().timestamp() - 1}).to_string());
        let expired = codex_config::codex_managed_oauth_auth_value(
            "workspace-a",
            &format!("{header}.{expired_payload}."),
            Some(&codex_config::test_codex_id_token("alice")),
            "expired-refresh",
            &chrono::Utc::now().to_rfc3339(),
        );
        write_private_atomic(&desktop, &serde_json::to_vec(&expired).unwrap()).unwrap();
        let expired_list = service.list().await.unwrap();
        assert_eq!(expired_list["scope_observations"]["desktop"], "expired");
        let expired_row = expired_list["accounts"]
            .as_array()
            .unwrap()
            .iter()
            .find(|r| r["id"] == "a")
            .unwrap();
        assert_eq!(expired_row["current_scopes"], json!(["desktop"]));
        assert_eq!(expired_row["expired_scopes"], json!(["desktop"]));

        // Broken or unsupported data must not be displayed as an identified
        // account and must not escape through the IPC response.
        write_private_atomic(&desktop, b"SECRET_NOT_A_CREDENTIAL").unwrap();
        let third = service.list().await.unwrap();
        assert_eq!(third["scope_observations"]["desktop"], "unavailable");
        assert!(!third.to_string().contains("SECRET_NOT_A_CREDENTIAL"));
        assert!(third["accounts"]
            .as_array()
            .unwrap()
            .iter()
            .all(|r| !r["current_scopes"]
                .as_array()
                .unwrap()
                .contains(&json!("desktop"))));
    }

    #[tokio::test]
    async fn external_current_identity_does_not_authorize_overwriting_stale_binding() {
        let (_temp, service) = setup();
        add(&service, "a", "alice", "workspace-a").await;
        add(&service, "b", "bob", "workspace-b").await;
        service.apply("a", "desktop", false).await.unwrap();
        service.apply("b", "cli", false).await.unwrap();
        let b_raw = fs::read(service.target_path("cli").unwrap()).unwrap();
        write_private_atomic(&service.target_path("desktop").unwrap(), &b_raw).unwrap();
        let result = service.list().await.unwrap();
        let rows = result["accounts"].as_array().unwrap();
        let a = rows.iter().find(|r| r["id"] == "a").unwrap();
        let b = rows.iter().find(|r| r["id"] == "b").unwrap();
        assert_eq!(a["bindings"][0]["state"], "unknown");
        assert_eq!(b["current_scopes"], json!(["desktop", "cli"]));
        assert!(service.apply("b", "desktop", false).await.is_err());
        assert_eq!(
            fs::read(service.target_path("desktop").unwrap()).unwrap(),
            b_raw
        );
    }

    #[tokio::test]
    async fn list_merges_cc_switch_accounts_without_importing_credentials() {
        let (_temp, service) = setup();
        add(&service, "a", "alice", "workspace-a").await;
        let cc = service.home.join(".cc-switch");
        fs::create_dir_all(&cc).unwrap();
        let cc_store = cc.join("codex_oauth_auth.json");
        let store = json!({"version":2,"accounts":{
            "cc-a":{"account_id":"cc-a","chatgpt_account_id":"workspace-a","email":"alice@example.test","id_token":codex_config::test_codex_id_token("alice"),"refresh_token":"CC_SECRET_A"},
            "cc-c":{"account_id":"cc-c","chatgpt_account_id":"workspace-c","email":"cathy@example.test","id_token":codex_config::test_codex_id_token("cathy"),"refresh_token":"CC_SECRET_C"}
        }});
        write_new_private(&cc_store, &serde_json::to_vec(&store).unwrap()).unwrap();
        let desktop = service.target_path("desktop").unwrap();
        let external = codex_config::codex_managed_oauth_auth_value(
            "workspace-c",
            &fake_access(),
            Some(&codex_config::test_codex_id_token("cathy")),
            "FILE_SECRET",
            &chrono::Utc::now().to_rfc3339(),
        );
        write_new_private(&desktop, &serde_json::to_vec(&external).unwrap()).unwrap();
        let before_cc = fs::read(&cc_store).unwrap();
        let before_helper = fs::read(service.root.join("codex_oauth_auth.json")).unwrap();
        let before_auth = fs::read(&desktop).unwrap();
        let first = service.list().await.unwrap();
        let rows = first["accounts"].as_array().unwrap();
        assert_eq!(rows.len(), 2);
        let merged = rows.iter().find(|r| r["id"] == "a").unwrap();
        assert_eq!(merged["sources"], json!(["helper", "cc_switch"]));
        assert_eq!(merged["managed"], true);
        let cc_only = rows.iter().find(|r| r["managed"] == false).unwrap();
        assert_eq!(cc_only["sources"], json!(["cc_switch"]));
        assert_eq!(cc_only["current_scopes"], json!(["desktop"]));
        assert_eq!(first["scope_observations"]["desktop"], "matched");
        assert_eq!(first["cc_switch_status"], "ready");
        assert!(!first.to_string().contains("SECRET"));
        assert_eq!(fs::read(&cc_store).unwrap(), before_cc);
        assert_eq!(
            fs::read(service.root.join("codex_oauth_auth.json")).unwrap(),
            before_helper
        );
        assert_eq!(fs::read(&desktop).unwrap(), before_auth);
        assert!(!service.root.join("bindings.json").exists());

        // The next read reflects a removal from CC Switch while retaining the
        // independent helper account and without rewriting either store.
        let mut changed = store.clone();
        changed["accounts"].as_object_mut().unwrap().remove("cc-c");
        write_private_atomic(&cc_store, &serde_json::to_vec(&changed).unwrap()).unwrap();
        let second = service.list().await.unwrap();
        assert_eq!(second["accounts"].as_array().unwrap().len(), 1);
        assert_eq!(second["scope_observations"]["desktop"], "unmatched");
        assert_eq!(
            fs::read(service.root.join("codex_oauth_auth.json")).unwrap(),
            before_helper
        );
        assert_eq!(fs::read(&desktop).unwrap(), before_auth);
    }

    #[tokio::test]
    async fn unreadable_cc_switch_store_is_reported_without_erasing_helper_accounts() {
        let (_temp, service) = setup();
        add(&service, "a", "alice", "workspace-a").await;
        let cc = service.home.join(".cc-switch");
        fs::create_dir_all(&cc).unwrap();
        write_new_private(&cc.join("codex_oauth_auth.json"), b"BAD_CC_STORE_SECRET").unwrap();
        let result = service.list().await.unwrap();
        assert_eq!(result["cc_switch_status"], "unavailable");
        assert_eq!(result["accounts"].as_array().unwrap().len(), 1);
        assert!(!result.to_string().contains("BAD_CC_STORE_SECRET"));
        let path = cc.join("codex_oauth_auth.json");
        fs::remove_file(&path).unwrap();
        std::os::unix::fs::symlink(cc.join("missing"), &path).unwrap();
        assert_eq!(
            service.list().await.unwrap()["cc_switch_status"],
            "unavailable"
        );
    }
    #[tokio::test]
    async fn legacy_account_can_switch_out_and_back_after_manager_restart() {
        let (_temp, service) = setup();
        fs::create_dir_all(service.home.join(".cc-switch")).unwrap();
        write_new_private(
            &service.home.join(".cc-switch/settings.json"),
            br#"{"codexConfigDir":"~/other"}"#,
        )
        .unwrap();
        add(&service, "a", "alice", "workspace-a").await;
        add(&service, "b", "bob", "workspace-b").await;
        service.apply("a", "cli", false).await.unwrap();
        service.apply("b", "desktop", false).await.unwrap();
        let original = fs::read(service.target_path("cli").unwrap()).unwrap();
        let desktop_before = fs::read(service.target_path("desktop").unwrap()).unwrap();
        let home = service.home.clone();
        let root = service.root.clone();
        let store_path = root.join("codex_oauth_auth.json");
        // Reproduce a legacy account that has access only in its client file.
        let mut store: Value = serde_json::from_slice(&fs::read(&store_path).unwrap()).unwrap();
        store["accounts"]["a"]
            .as_object_mut()
            .unwrap()
            .remove("login_access");
        write_private_atomic(&store_path, &serde_json::to_vec(&store).unwrap()).unwrap();
        let store_before = fs::read(&store_path).unwrap();
        drop(service);
        let service = AccountService::new(home.clone(), root.clone()).unwrap();
        service.apply("b", "cli", false).await.unwrap();
        drop(service);
        let service = AccountService::new(home, root).unwrap();
        assert!(service.matching_live_auth("a").await.is_none());
        assert_eq!(
            service.handoff_ready("a", "cli").await.unwrap()["ready"],
            true
        );
        service.apply("a", "cli", false).await.unwrap();
        assert_eq!(
            fs::read(service.target_path("cli").unwrap()).unwrap(),
            original
        );
        assert_eq!(
            fs::read(service.target_path("desktop").unwrap()).unwrap(),
            desktop_before
        );
        assert_eq!(fs::read(&store_path).unwrap(), store_before);
        // A known backup restored manually can rebind without another write.
        service.apply("b", "cli", false).await.unwrap();
        write_private_atomic(&service.target_path("cli").unwrap(), &original).unwrap();
        assert_eq!(
            service.apply("a", "cli", false).await.unwrap()["state"],
            "already_bound"
        );
        assert_eq!(service.bindings().unwrap().cli.unwrap().account_id, "a");
        assert_eq!(
            fs::read(service.target_path("cli").unwrap()).unwrap(),
            original
        );
    }

    #[tokio::test]
    async fn external_change_to_managed_target_fails_closed() {
        let (_temp, service) = setup();
        add(&service, "a", "alice", "workspace-a").await;
        add(&service, "b", "bob", "workspace-b").await;
        service.apply("a", "desktop", false).await.unwrap();
        let changed = codex_config::codex_managed_oauth_auth_value(
            "workspace-b",
            &fake_access(),
            Some(&codex_config::test_codex_id_token("bob")),
            "different",
            "2026-01-01T00:00:00Z",
        );
        let target = service.target_path("desktop").unwrap();
        fs::write(&target, serde_json::to_vec(&changed).unwrap()).unwrap();
        let before = fs::read(&target).unwrap();
        assert!(service.apply("b", "desktop", false).await.is_err());
        assert_eq!(fs::read(&target).unwrap(), before);
    }
    #[tokio::test]
    async fn pending_transaction_blocks_further_writes() {
        let (_temp, service) = setup();
        add(&service, "a", "alice", "workspace-a").await;
        write_private_atomic(&service.root.join("pending.json"), br#"{"version":1}"#).unwrap();
        assert!(service.apply("a", "desktop", false).await.is_err());
        assert!(!service.target_path("desktop").unwrap().exists());
    }
    #[tokio::test]
    async fn removing_account_does_not_change_either_clients_auth() {
        let (_temp, service) = setup();
        add(&service, "a", "alice", "workspace-a").await;
        service.apply("a", "desktop", false).await.unwrap();
        service.apply("a", "cli", false).await.unwrap();
        let a = fs::read(service.target_path("desktop").unwrap()).unwrap();
        let b = fs::read(service.target_path("cli").unwrap()).unwrap();
        service.remove_account("a").await.unwrap();
        assert_eq!(
            fs::read(service.target_path("desktop").unwrap()).unwrap(),
            a
        );
        assert_eq!(fs::read(service.target_path("cli").unwrap()).unwrap(), b);
        assert!(service.list().await.unwrap()["accounts"]
            .as_array()
            .unwrap()
            .is_empty());
    }

    #[tokio::test]
    async fn unbound_quota_can_use_matching_live_access_without_refreshing_or_writing() {
        let (temp, service) = setup();
        add(&service, "a", "alice", "workspace-a").await;
        let home = service.home.clone();
        let root = service.root.clone();
        let target = service.target_path("desktop").unwrap();
        let auth = codex_config::codex_managed_oauth_auth_value(
            "workspace-a",
            &fake_access(),
            Some(&codex_config::test_codex_id_token("alice")),
            "other-generation",
            "2026-01-01T00:00:00Z",
        );
        write_new_private(&target, &serde_json::to_vec(&auth).unwrap()).unwrap();
        let before_live = fs::read(&target).unwrap();
        let before_store = fs::read(root.join("codex_oauth_auth.json")).unwrap();
        drop(service);
        let service = AccountService::new(home, root).unwrap();
        assert!(service.matching_live_access("a").await.is_some());
        assert_eq!(fs::read(&target).unwrap(), before_live);
        assert_eq!(
            fs::read(service.root.join("codex_oauth_auth.json")).unwrap(),
            before_store
        );
        drop(temp);
    }
    #[tokio::test]
    async fn unbound_manager_never_refreshes_an_identical_live_credential() {
        let (_temp, service) = setup();
        add(&service, "a", "alice", "workspace-a").await;
        let home = service.home.clone();
        let root = service.root.clone();
        let target = service.target_path("desktop").unwrap();
        let auth = codex_config::codex_managed_oauth_auth_value(
            "workspace-a",
            &fake_access(),
            Some(&codex_config::test_codex_id_token("alice")),
            "test-refresh-token",
            "2026-01-01T00:00:00Z",
        );
        write_new_private(&target, &serde_json::to_vec(&auth).unwrap()).unwrap();
        let source_before = fs::read(&target).unwrap();
        let store_before = fs::read(root.join("codex_oauth_auth.json")).unwrap();
        drop(service);
        let service = AccountService::new(home, root).unwrap();
        service.apply("a", "cli", false).await.unwrap();
        assert_eq!(source_before, fs::read(&target).unwrap());
        assert_eq!(
            source_before,
            fs::read(service.target_path("cli").unwrap()).unwrap()
        );
        assert_eq!(
            store_before,
            fs::read(service.root.join("codex_oauth_auth.json")).unwrap()
        );
    }

    #[tokio::test]
    async fn automatic_handoff_never_refreshes_an_unavailable_account() {
        let (_temp, service) = setup();
        add(&service, "a", "alice", "workspace-a").await;
        let home = service.home.clone();
        let root = service.root.clone();
        let before = fs::read(root.join("codex_oauth_auth.json")).unwrap();
        drop(service);
        let service = AccountService::new(home, root.clone()).unwrap();
        assert!(service.quota_mode("a").await.is_err());
        assert!(service.warmup_access("a").await.is_err());
        assert!(service.apply_mode("a", "cli", false).await.is_err());
        assert!(!service.target_path("cli").unwrap().exists());
        assert_eq!(
            before,
            fs::read(root.join("codex_oauth_auth.json")).unwrap()
        );
    }

    #[tokio::test]
    async fn warmup_can_borrow_login_access_without_exposing_refresh_or_writing() {
        let (_temp, service) = setup();
        add(&service, "a", "alice", "workspace-a").await;
        let before = fs::read(service.root.join("codex_oauth_auth.json")).unwrap();
        let response = service.warmup_access("a").await.unwrap();
        assert_eq!(response["workspace"], "workspace-a");
        assert!(response["access"].is_string());
        assert!(!response.to_string().contains("test-refresh-token"));
        assert_eq!(
            before,
            fs::read(service.root.join("codex_oauth_auth.json")).unwrap()
        );
    }

    #[tokio::test]
    async fn automatic_handoff_can_use_an_existing_bound_login_without_refresh() {
        let (_temp, service) = setup();
        add(&service, "a", "alice", "workspace-a").await;
        service.apply("a", "desktop", false).await.unwrap();
        let home = service.home.clone();
        let root = service.root.clone();
        let before_store = fs::read(root.join("codex_oauth_auth.json")).unwrap();
        let before_source = fs::read(service.target_path("desktop").unwrap()).unwrap();
        drop(service);
        let service = AccountService::new(home, root.clone()).unwrap();
        service.apply_mode("a", "cli", false).await.unwrap();
        assert_eq!(
            before_source,
            fs::read(service.target_path("cli").unwrap()).unwrap()
        );
        assert_eq!(
            before_source,
            fs::read(service.target_path("desktop").unwrap()).unwrap()
        );
        assert_eq!(
            before_store,
            fs::read(root.join("codex_oauth_auth.json")).unwrap()
        );
    }
    #[tokio::test]
    async fn applying_live_credentials_does_not_update_account_store() {
        let (_temp, service) = setup();
        add(&service, "a", "alice", "workspace-a").await;
        add(&service, "b", "bob", "workspace-b").await;
        service.apply("a", "desktop", false).await.unwrap();
        service.apply("b", "cli", false).await.unwrap();

        // Simulate each client independently obtaining a newer login after
        // the helper stored the original account data. Applying a target may
        // copy the existing bundle but must not adopt it into the account
        // store; only an explicit login is allowed to update that store.
        let updated_at = (chrono::Utc::now() + chrono::Duration::seconds(5)).to_rfc3339();
        for (scope, token) in [("desktop", "outgoing-live"), ("cli", "incoming-live")] {
            let path = service.target_path(scope).unwrap();
            let mut value: Value = serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
            value["tokens"]["refresh_token"] = json!(token);
            value["last_refresh"] = json!(updated_at);
            fs::write(path, serde_json::to_vec(&value).unwrap()).unwrap();
        }
        let root = service.root.clone();
        let home = service.home.clone();
        let store_before = fs::read(root.join("codex_oauth_auth.json")).unwrap();
        let incoming = fs::read(service.target_path("cli").unwrap()).unwrap();
        drop(service);

        let restarted = AccountService::new(home, root.clone()).unwrap();
        assert_eq!(
            restarted.apply("a", "desktop", false).await.unwrap()["state"],
            "already_bound"
        );
        assert!(
            fs::read(root.join("codex_oauth_auth.json")).unwrap() == store_before,
            "reapplying the current account changed the account store"
        );
        assert_eq!(
            restarted.apply("b", "desktop", false).await.unwrap()["state"],
            "installed"
        );
        assert!(fs::read(restarted.target_path("desktop").unwrap()).unwrap() == incoming);
        assert!(fs::read(restarted.target_path("cli").unwrap()).unwrap() == incoming);
        assert!(
            fs::read(root.join("codex_oauth_auth.json")).unwrap() == store_before,
            "applying live credentials changed the account store"
        );
    }
    #[tokio::test]
    async fn automatic_handoff_readiness_checks_current_binding_and_existing_bundle() {
        let (_temp, service) = setup();
        fs::create_dir_all(service.home.join(".cc-switch")).unwrap();
        write_new_private(&service.home.join(".cc-switch/settings.json"), b"{}").unwrap();
        add(&service, "a", "alice", "workspace-a").await;
        add(&service, "b", "bob", "workspace-b").await;
        service.apply("a", "cli", false).await.unwrap();
        assert_eq!(
            service.handoff_ready("b", "cli").await.unwrap()["ready"],
            true
        );
        let other = codex_config::codex_managed_oauth_auth_value(
            "workspace-b",
            &fake_access(),
            Some(&codex_config::test_codex_id_token("bob")),
            "test-refresh-token",
            "2026-01-01T00:00:00Z",
        );
        fs::write(
            service.target_path("cli").unwrap(),
            serde_json::to_vec(&other).unwrap(),
        )
        .unwrap();
        assert!(service.handoff_ready("b", "cli").await.is_err());
    }
    #[tokio::test]
    async fn mismatched_or_expired_live_access_is_not_borrowed() {
        let (_temp, service) = setup();
        add(&service, "a", "alice", "workspace-a").await;
        let target = service.target_path("desktop").unwrap();
        let wrong = codex_config::codex_managed_oauth_auth_value(
            "workspace-a",
            &fake_access(),
            Some(&codex_config::test_codex_id_token("someone-else")),
            "other-generation",
            "2026-01-01T00:00:00Z",
        );
        write_new_private(&target, &serde_json::to_vec(&wrong).unwrap()).unwrap();
        assert!(service.matching_live_access("a").await.is_none());
        let header = URL_SAFE_NO_PAD.encode(br#"{"alg":"none"}"#);
        let payload =
            URL_SAFE_NO_PAD.encode(json!({"exp":chrono::Utc::now().timestamp()-1}).to_string());
        let expired = codex_config::codex_managed_oauth_auth_value(
            "workspace-a",
            &format!("{header}.{payload}."),
            Some(&codex_config::test_codex_id_token("alice")),
            "other-generation",
            "2026-01-01T00:00:00Z",
        );
        fs::write(&target, serde_json::to_vec(&expired).unwrap()).unwrap();
        assert!(service.matching_live_access("a").await.is_none());
    }
    #[tokio::test]
    async fn non_file_credential_store_is_never_overwritten() {
        let (_temp, service) = setup();
        add(&service, "a", "alice", "workspace-a").await;
        let config = service.target_dir("cli").unwrap().join("config.toml");
        fs::write(config, b"cli_auth_credentials_store = \"keyring\"\n").unwrap();
        assert!(service.apply("a", "cli", false).await.is_err());
        assert!(!service.target_path("cli").unwrap().exists());
    }
    #[test]
    fn rejects_mixed_access_and_workspace_claims() {
        let header = URL_SAFE_NO_PAD.encode(br#"{"alg":"none"}"#);
        let body = URL_SAFE_NO_PAD.encode(
            json!({"exp":chrono::Utc::now().timestamp()+3600,
            "https://api.openai.com/auth":{"chatgpt_account_id":"other-workspace"}})
            .to_string(),
        );
        let access = format!("{header}.{body}.");
        let value = codex_config::codex_managed_oauth_auth_value(
            "workspace-a",
            &access,
            Some(&codex_config::test_codex_id_token("alice")),
            "test-refresh",
            "2026-01-01T00:00:00Z",
        );
        assert!(parse_auth(serde_json::to_vec(&value).unwrap()).is_err());
    }
    #[tokio::test]
    async fn same_physical_home_and_symlinked_auth_are_rejected() {
        let (_temp, service) = setup();
        add(&service, "a", "alice", "workspace-a").await;
        let desktop = service.target_path("desktop").unwrap();
        std::os::unix::fs::symlink(service.root.join("other"), &desktop).unwrap();
        assert!(service.apply("a", "desktop", false).await.is_err());
        fs::remove_file(&desktop).unwrap();
        fs::remove_file(service.target_dir("cli").unwrap().join("config.toml")).unwrap();
        fs::remove_dir(service.target_dir("cli").unwrap()).unwrap();
        std::os::unix::fs::symlink(
            service.target_dir("desktop").unwrap(),
            service.target_dir("cli").unwrap(),
        )
        .unwrap();
        assert!(service.apply("a", "cli", false).await.is_err());
    }
    #[test]
    fn account_store_cannot_be_opened_by_two_managers() {
        let (_temp, service) = setup();
        assert!(AccountService::new(service.home.clone(), service.root.clone()).is_err());
    }

    #[tokio::test]
    async fn device_login_and_existing_credentials_use_only_a_local_mock() {
        struct MockCalls {
            polls: AtomicUsize,
            tokens: AtomicUsize,
        }
        let calls = Arc::new(MockCalls {
            polls: AtomicUsize::new(0),
            tokens: AtomicUsize::new(0),
        });
        async fn start() -> Json<Value> {
            Json(
                json!({"device_auth_id":"local-device","user_code":"TEST-CODE","interval":1,"expires_in":60}),
            )
        }
        async fn poll(State(calls): State<Arc<MockCalls>>) -> impl IntoResponse {
            if calls.polls.fetch_add(1, Ordering::SeqCst) == 0 {
                (StatusCode::FORBIDDEN, Json(json!({})))
            } else {
                (
                    StatusCode::OK,
                    Json(
                        json!({"authorization_code":"local-code","code_verifier":"local-verifier"}),
                    ),
                )
            }
        }
        async fn token(State(calls): State<Arc<MockCalls>>) -> Json<Value> {
            calls.tokens.fetch_add(1, Ordering::SeqCst);
            let header = URL_SAFE_NO_PAD.encode(br#"{"alg":"none"}"#);
            let claims = URL_SAFE_NO_PAD.encode(
                json!({"sub":"local-user","email":"local@example.test",
                "chatgpt_account_id":"local-workspace","exp":chrono::Utc::now().timestamp()+3600})
                .to_string(),
            );
            let jwt = format!("{header}.{claims}.");
            Json(
                json!({"access_token":jwt,"id_token":jwt,"refresh_token":"local-refresh","expires_in":3600}),
            )
        }
        async fn quota() -> Json<Value> {
            Json(
                json!({"rate_limit":{"limit_reached":false,"primary_window":{"used_percent":20,"limit_window_seconds":18000,"reset_at":1900000000},
                "secondary_window":{"used_percent":35,"limit_window_seconds":604800,"reset_at":1900005000}}}),
            )
        }
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let base = format!("http://{}", listener.local_addr().unwrap());
        crate::proxy::providers::codex_oauth_auth::TEST_OAUTH_BASE
            .set(base.clone())
            .unwrap();
        TEST_QUOTA_URL.set(format!("{base}/usage")).unwrap();
        let router = Router::new()
            .route("/device/user", post(start))
            .route("/device/token", post(poll))
            .route("/oauth/token", post(token))
            .route("/usage", axum::routing::get(quota))
            .with_state(calls.clone());
        let server = tokio::spawn(async move { axum::serve(listener, router).await.unwrap() });
        let (temp, mut service) = setup();
        let start = service.start_login("device", None).await.unwrap();
        assert_eq!(start["user_code"], "TEST-CODE");
        assert!(!start.to_string().contains("local-device"));
        let flow = start["flow_id"].as_str().unwrap();
        assert_eq!(service.poll_login(flow).await.unwrap()["state"], "waiting");
        assert_eq!(service.poll_login(flow).await.unwrap()["state"], "complete");
        let listed = service.list().await.unwrap();
        assert!(!listed.to_string().contains("local-refresh"));
        assert!(!listed.to_string().contains("local@example.test"));
        let account = listed["accounts"][0]["id"].as_str().unwrap().to_string();
        let external = codex_config::codex_managed_oauth_auth_value(
            "local-workspace",
            &fake_access(),
            Some(&codex_config::test_codex_id_token("local-user")),
            "separate-external-refresh",
            "2026-01-01T00:00:00Z",
        );
        let desktop = service.target_path("desktop").unwrap();
        write_new_private(&desktop, &serde_json::to_vec(&external).unwrap()).unwrap();
        let external_before = fs::read(&desktop).unwrap();
        drop(service);
        // After restart, quota can use the already-issued live access token
        // without refreshing the separately authorised manager credential.
        let restarted =
            AccountService::new(temp.path().join("home"), temp.path().join("state")).unwrap();
        let quota = restarted.quota(&account).await.unwrap();
        assert_eq!(quota["windows"][0]["remaining"], 80.0);
        assert_eq!(quota["limit_reached"], false);
        assert!(!quota.to_string().contains("local-refresh"));
        assert_eq!(calls.tokens.load(Ordering::SeqCst), 1);
        // The explicit login bundle survives restart and can be applied
        // without borrowing an unrelated live generation or refreshing it.
        restarted.apply(&account, "cli", false).await.unwrap();
        assert_eq!(calls.tokens.load(Ordering::SeqCst), 1);
        assert_eq!(fs::read(&desktop).unwrap(), external_before);
        restarted.apply(&account, "desktop", false).await.unwrap();
        let desktop_before = fs::read(restarted.target_path("desktop").unwrap()).unwrap();
        let cli_before = fs::read(restarted.target_path("cli").unwrap()).unwrap();
        assert_eq!(desktop_before, cli_before);
        assert_eq!(
            restarted.list().await.unwrap()["accounts"]
                .as_array()
                .unwrap()
                .len(),
            1
        );
        server.abort();
    }

    #[tokio::test]
    async fn recovery_finishes_only_an_unchanged_published_transaction() {
        let (_temp, service) = setup();
        add(&service, "a", "alice", "workspace-a").await;
        service.apply("a", "desktop", false).await.unwrap();
        let target = service.target_path("desktop").unwrap();
        let raw = fs::read(&target).unwrap();
        let parsed = parse_auth(raw.clone()).unwrap();
        let pending = json!({"version":1,"scope":"desktop","account_id":"a",
            "refresh_digest":digest(parsed.refresh.as_bytes()),"previous":null,
            "candidate":digest(&raw),"staged":".codex-helper-auth-local-test","backup":null});
        write_private_atomic(
            &service.root.join("pending.json"),
            &serde_json::to_vec(&pending).unwrap(),
        )
        .unwrap();
        assert_eq!(
            service.resolve_pending(false).await.unwrap()["state"],
            "completed"
        );
        assert_eq!(fs::read(target).unwrap(), raw);
        assert!(!service.root.join("pending.json").exists());
    }

    #[tokio::test]
    async fn recovery_discards_unpublished_staging_without_touching_target() {
        let (_temp, service) = setup();
        add(&service, "a", "alice", "workspace-a").await;
        service.apply("a", "desktop", false).await.unwrap();
        let target = service.target_path("desktop").unwrap();
        let original = fs::read(&target).unwrap();
        let stage_name = ".codex-helper-auth-local-stage";
        let stage = service.target_dir("desktop").unwrap().join(stage_name);
        let candidate = b"synthetic candidate";
        write_new_private(&stage, candidate).unwrap();
        let pending = json!({"version":1,"scope":"desktop","account_id":"a",
            "refresh_digest":"test","previous":digest(&original),"candidate":digest(candidate),
            "staged":stage_name,"backup":null});
        write_private_atomic(
            &service.root.join("pending.json"),
            &serde_json::to_vec(&pending).unwrap(),
        )
        .unwrap();
        assert_eq!(
            service.resolve_pending(false).await.unwrap()["state"],
            "not_published"
        );
        assert_eq!(fs::read(target).unwrap(), original);
        assert!(!stage.exists());
    }

    #[test]
    fn cc_switch_guard_is_scoped_to_its_configured_directory() {
        let (_temp, service) = setup();
        let cc = service.home.join(".cc-switch");
        ensure_private_dir(&cc).unwrap();
        write_new_private(
            &cc.join("settings.json"),
            br#"{"codexConfigDir":"~/.codex"}"#,
        )
        .unwrap();
        assert!(cc_switch_targets("desktop", &service.home).unwrap());
        assert!(!cc_switch_targets("cli", &service.home).unwrap());
    }

    #[test]
    fn desktop_owner_default_missing_invalid_and_cli_are_distinct() {
        let (_temp, service) = setup();
        let cc = service.home.join(".cc-switch");
        fs::create_dir_all(&cc).unwrap();
        let path = cc.join("settings.json");
        assert!(!cc_switch_targets("desktop", &service.home).unwrap());
        for text in [
            "{}",
            r#"{"note":"\ud83d\ude80"}"#,
            r#"{"codexConfigDir":""}"#,
            r#"{"codexConfigDir":"  "}"#,
        ] {
            fs::write(&path, text).unwrap();
            assert!(cc_switch_targets("desktop", &service.home).unwrap());
            assert!(!cc_switch_targets("cli", &service.home).unwrap());
        }
        fs::write(&path, r#"{"codexConfigDir":"~/.codex-cli"}"#).unwrap();
        assert!(!cc_switch_targets("desktop", &service.home).unwrap());
        assert!(cc_switch_targets("cli", &service.home).unwrap());
        std::os::unix::fs::symlink(
            service.home.join(".codex"),
            service.home.join("desktop-alias"),
        )
        .unwrap();
        for text in [
            r#"{"codexConfigDir":"~/desktop-alias"}"#,
            r#"{"codexConfigDir":"~/.codex/../.codex"}"#,
        ] {
            fs::write(&path, text).unwrap();
            assert!(cc_switch_targets("desktop", &service.home).unwrap());
        }
        for invalid in [
            "{",
            "null",
            "[]",
            r#"{"codexConfigDir":null}"#,
            r#"{"codexConfigDir":42}"#,
            r#"{"codexConfigDir":"\ud800"}"#,
            r#"{"note":"\udc00"}"#,
            r#"{"codexConfigDir":"relative"}"#,
        ] {
            fs::write(&path, invalid).unwrap();
            assert_eq!(
                cc_switch_targets("desktop", &service.home).unwrap_err(),
                DESKTOP_OWNER_UNKNOWN
            );
            assert_eq!(
                require_native_desktop_owner("desktop", &service.home).unwrap_err(),
                DESKTOP_OWNER_UNKNOWN
            );
        }
        fs::write(&path, b"{\"x\":\"\xff\"}").unwrap();
        assert_eq!(
            cc_switch_targets("desktop", &service.home).unwrap_err(),
            DESKTOP_OWNER_UNKNOWN
        );
        fs::remove_file(&path).unwrap();
        assert!(!cc_switch_targets("desktop", &service.home).unwrap());
        assert!(running_cc_switch_targets("desktop", &service.home).is_err());
        fs::create_dir(&path).unwrap();
        assert_eq!(
            cc_switch_targets("desktop", &service.home).unwrap_err(),
            DESKTOP_OWNER_UNKNOWN
        );
    }

    #[tokio::test]
    async fn desktop_owned_apply_and_recovery_are_blocked_without_process_guard() {
        let (_temp, service) = setup();
        add(&service, "a", "alice", "workspace-a").await;
        add(&service, "b", "bob", "workspace-b").await;
        service.apply("a", "desktop", false).await.unwrap();
        let target = service.target_path("desktop").unwrap();
        let before_auth = fs::read(&target).unwrap();
        let before_binding = fs::read(service.root.join("bindings.json")).unwrap();
        let cc = service.home.join(".cc-switch");
        fs::create_dir_all(&cc).unwrap();
        fs::write(cc.join("settings.json"), b"{}").unwrap();
        // No process inspection is invoked: ownership alone must refuse.
        assert_eq!(
            service.apply("b", "desktop", false).await.unwrap_err(),
            DESKTOP_CC_OWNER
        );
        assert_eq!(fs::read(&target).unwrap(), before_auth);
        assert_eq!(
            fs::read(service.root.join("bindings.json")).unwrap(),
            before_binding
        );
        fs::write(cc.join("settings.json"), b"{").unwrap();
        assert_eq!(
            service.apply("b", "desktop", false).await.unwrap_err(),
            DESKTOP_OWNER_UNKNOWN
        );
        assert_eq!(fs::read(&target).unwrap(), before_auth);
        assert_eq!(
            fs::read(service.root.join("bindings.json")).unwrap(),
            before_binding
        );
        fs::write(cc.join("settings.json"), b"{}").unwrap();
        let parsed = parse_auth(before_auth.clone()).unwrap();
        let pending = json!({"version":1,"scope":"desktop","account_id":"a",
			"refresh_digest":digest(parsed.refresh.as_bytes()),"previous":null,
			"candidate":digest(&before_auth),"staged":".codex-helper-auth-test","backup":null});
        let journal = service.root.join("pending.json");
        write_private_atomic(&journal, &serde_json::to_vec(&pending).unwrap()).unwrap();
        let before_pending = fs::read(&journal).unwrap();
        assert_eq!(
            service.resolve_pending(false).await.unwrap_err(),
            DESKTOP_CC_OWNER
        );
        assert_eq!(fs::read(journal).unwrap(), before_pending);
        assert_eq!(fs::read(target).unwrap(), before_auth);
        assert_eq!(
            fs::read(service.root.join("bindings.json")).unwrap(),
            before_binding
        );
        fs::write(cc.join("settings.json"), b"{").unwrap();
        assert_eq!(
            service.resolve_pending(false).await.unwrap_err(),
            DESKTOP_OWNER_UNKNOWN
        );
    }

    #[tokio::test]
    async fn cc_owner_stale_binding_is_not_authority_but_reapply_stays_conservative() {
        let (_temp, service) = setup();
        add(&service, "a", "alice", "workspace-a").await;
        add(&service, "b", "bob", "workspace-b").await;
        service.apply("a", "desktop", false).await.unwrap();
        service.apply("a", "cli", false).await.unwrap();
        let desktop = service.target_path("desktop").unwrap();
        let external = codex_config::codex_managed_oauth_auth_value(
            "workspace-b",
            &fake_access(),
            Some(&codex_config::test_codex_id_token("bob")),
            "external-refresh",
            &chrono::Utc::now().to_rfc3339(),
        );
        write_private_atomic(&desktop, &serde_json::to_vec(&external).unwrap()).unwrap();
        let cc = service.home.join(".cc-switch");
        fs::create_dir_all(&cc).unwrap();
        fs::write(
            cc.join("settings.json"),
            br#"{"codexConfigDir":"~/.codex"}"#,
        )
        .unwrap();
        let result = service.list().await.unwrap();
        assert_eq!(result["desktop_owner"], "cc_switch");
        assert_eq!(result["desktop_binding_present"], true);
        let a = result["accounts"]
            .as_array()
            .unwrap()
            .iter()
            .find(|r| r["id"] == "a")
            .unwrap();
        assert_eq!(a["bindings"].as_array().unwrap().len(), 1);
        assert_eq!(a["bindings"][0]["scope"], "cli");
        assert!(service.quota_access("a").await.is_ok());
        assert!(service.warmup_access("a").await.is_ok());
        assert_eq!(
            service.handoff_ready("a", "cli").await.unwrap()["ready"],
            true
        );
        let mut bindings = service.bindings().unwrap();
        bindings.desktop.as_mut().unwrap().needs_reapply = true;
        service.save_bindings(&bindings).unwrap();
        assert!(service.quota_access("a").await.is_ok());
        assert!(service.warmup_access("a").await.is_ok());
        bindings.cli = None;
        service.save_bindings(&bindings).unwrap();
        assert!(service.quota_access("a").await.is_err());
        assert!(service.warmup_access("a").await.is_err());
        assert!(service.handoff_ready("a", "cli").await.is_err());
    }

    #[tokio::test]
    async fn clear_desktop_binding_preserves_accounts_tokens_and_cli() {
        let (_temp, mut service) = setup();
        add(&service, "a", "alice", "workspace-a").await;
        service.apply("a", "desktop", false).await.unwrap();
        service.apply("a", "cli", false).await.unwrap();
        let path = service.root.join("bindings.json");
        let mut original: Value = serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
        original["future"] = json!({"preserve":true});
        write_private_atomic(&path, &serde_json::to_vec(&original).unwrap()).unwrap();
        let account_file = service.root.join("codex_oauth_auth.json");
        let before_account = fs::read(&account_file).unwrap();
        let before_desktop = fs::read(service.target_path("desktop").unwrap()).unwrap();
        let before_cli = fs::read(service.target_path("cli").unwrap()).unwrap();
        assert_eq!(
            service
                .handle(json!({"method":"clear_desktop_binding"}), None)
                .await
                .unwrap()["state"],
            "cleared"
        );
        let cleared: Value = serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
        assert!(cleared["desktop"].is_null());
        assert_eq!(cleared["cli"], original["cli"]);
        assert_eq!(cleared["future"], original["future"]);
        assert_eq!(
            fs::metadata(&path).unwrap().permissions().mode() & 0o777,
            0o600
        );
        assert_eq!(fs::read(account_file).unwrap(), before_account);
        assert_eq!(
            fs::read(service.target_path("desktop").unwrap()).unwrap(),
            before_desktop
        );
        assert_eq!(
            fs::read(service.target_path("cli").unwrap()).unwrap(),
            before_cli
        );
        assert_eq!(
            service
                .handle(json!({"method":"clear_desktop_binding"}), None)
                .await
                .unwrap()["state"],
            "none"
        );
    }

    #[tokio::test]
    async fn clear_desktop_binding_refuses_pending_reapply() {
        let (_temp, service) = setup();
        add(&service, "a", "alice", "workspace-a").await;
        service.apply("a", "desktop", false).await.unwrap();
        let mut bindings = service.bindings().unwrap();
        bindings.desktop.as_mut().unwrap().needs_reapply = true;
        service.save_bindings(&bindings).unwrap();
        let path = service.root.join("bindings.json");
        let before = fs::read(&path).unwrap();
        let error = service.clear_desktop_binding().unwrap_err();
        assert!(error.contains("暂不可安全清理"));
        assert_eq!(fs::read(path).unwrap(), before);
        bindings.desktop.as_mut().unwrap().needs_reapply = false;
        service.save_bindings(&bindings).unwrap();
        let path = service.root.join("bindings.json");
        let before = fs::read(&path).unwrap();
        let cc = service.home.join(".cc-switch");
        fs::create_dir_all(&cc).unwrap();
        fs::write(cc.join("settings.json"), b"{").unwrap();
        assert_eq!(
            service.clear_desktop_binding().unwrap_err(),
            DESKTOP_OWNER_UNKNOWN
        );
        assert_eq!(fs::read(&path).unwrap(), before);
        fs::remove_file(cc.join("settings.json")).unwrap();
        write_private_atomic(&service.root.join("pending.json"), b"{}").unwrap();
        assert!(service.clear_desktop_binding().is_err());
        assert_eq!(fs::read(path).unwrap(), before);
    }
}
