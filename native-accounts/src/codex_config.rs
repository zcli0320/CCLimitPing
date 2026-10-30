//! Scoped adapters for the CC Switch account manager. The source manager's
//! original global Codex path is deliberately not used here.
use crate::error::AppError;
use base64::{engine::general_purpose::URL_SAFE_NO_PAD, Engine as _};
use serde_json::{json, Value};

pub(crate) struct CodexManagedLiveRefresh {
    pub(crate) refresh_token: String,
    pub(crate) id_token: Option<String>,
    pub(crate) last_refresh_ms: Option<i64>,
    pub(crate) chatgpt_account_id: String,
}

pub(crate) fn extract_codex_id_token_subject(id_token: &str) -> Option<String> {
    let mut segments = id_token.split('.');
    let header = segments.next()?;
    let payload = segments.next()?;
    segments.next()?;
    if segments.next().is_some() {
        return None;
    }
    let header: Value = serde_json::from_slice(&URL_SAFE_NO_PAD.decode(header).ok()?).ok()?;
    header
        .get("alg")?
        .as_str()
        .map(str::trim)
        .filter(|s| !s.is_empty())?;
    let claims: Value = serde_json::from_slice(&URL_SAFE_NO_PAD.decode(payload).ok()?).ok()?;
    claims
        .get("sub")?
        .as_str()
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(str::to_string)
}

pub(crate) fn extract_codex_id_token_user_identity(id_token: &str) -> Option<String> {
    extract_codex_id_token_subject(id_token).map(|s| format!("sub:{s}"))
}

#[cfg(test)]
pub(crate) fn test_codex_id_token(subject: &str) -> String {
    let header = URL_SAFE_NO_PAD.encode(br#"{"alg":"none"}"#);
    let payload = URL_SAFE_NO_PAD.encode(json!({"sub": subject}).to_string());
    format!("{header}.{payload}.")
}

pub(crate) fn codex_managed_oauth_auth_value(
    account_id: &str,
    access: &str,
    id: Option<&str>,
    refresh: &str,
    last_refresh: &str,
) -> Value {
    json!({"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{
        "id_token":id,"access_token":access,"refresh_token":refresh,"account_id":account_id
    },"last_refresh":last_refresh})
}

// The upstream OAuth manager's automatic live-file synchronization is not
// routed to any Codex home. Target publication must go through the helper's
// explicit scoped transaction, which also reconciles the current live record.
pub(crate) fn read_codex_live_auth_refresh_for_managed_account(
    _account_id: &str,
    _id_token: Option<&str>,
) -> Result<Option<CodexManagedLiveRefresh>, AppError> {
    Ok(None)
}

pub(crate) fn sync_codex_managed_oauth_live_auth_after_refresh(
    _account_id: &str,
    _expected: &str,
    _refreshed: &Value,
) -> Result<bool, AppError> {
    Ok(false)
}

pub(crate) fn ensure_codex_live_auth_unchanged_for_managed_account(
    _account_id: &str,
    _expected: &str,
) -> Result<(), AppError> {
    Err(AppError::Message("目标登录需由端事务重新核验".into()))
}

pub(crate) fn prepare_codex_live_auth_for_managed_account_removal(
    _account_id: &str,
    _id_token: Option<&str>,
) -> Result<(), AppError> {
    Ok(())
}

pub(crate) fn clear_codex_live_auth_for_managed_account(_account_id: &str) -> Result<(), AppError> {
    Ok(())
}
