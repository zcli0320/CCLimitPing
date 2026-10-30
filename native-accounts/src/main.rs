use std::path::PathBuf;

#[tokio::main]
async fn main() {
    let real_home = match std::env::var_os("HOME") {
        Some(value) => PathBuf::from(value),
        None => {
            eprintln!("无法定位用户目录");
            std::process::exit(1);
        }
    };
    let home = match std::env::var_os("CODEX_HELPER_TEST_HOME") {
        Some(value) => {
            let candidate = PathBuf::from(value);
            let marker = candidate.join(".codex-helper-test-root");
            let valid = candidate.is_absolute()
                && marker.is_file()
                && candidate
                    .canonicalize()
                    .ok()
                    .zip(real_home.canonicalize().ok())
                    .is_some_and(|(test, real)| test != real);
            if !valid {
                eprintln!("隔离测试目录无效");
                std::process::exit(1);
            }
            candidate
        }
        None => real_home,
    };
    let root = home.join("Library/Application Support/CodexHelper-AccountPreview/accounts");
    if let Err(error) = codex_helper_accounts::service::run(home, root).await {
        eprintln!("{error}");
        std::process::exit(1);
    }
}
