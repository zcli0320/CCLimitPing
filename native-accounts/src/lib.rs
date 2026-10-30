#![allow(dead_code)]

pub mod codex_config;
pub mod error;
pub mod service;

pub mod proxy {
    pub mod http_client {
        use std::sync::OnceLock;
        pub fn get() -> reqwest::Client {
            static CLIENT: OnceLock<reqwest::Client> = OnceLock::new();
            CLIENT
                .get_or_init(|| {
                    let builder =
                        reqwest::Client::builder().redirect(reqwest::redirect::Policy::none());
                    #[cfg(test)]
                    let builder = builder.no_proxy();
                    builder.build().expect("HTTP client configuration")
                })
                .clone()
        }
    }
    pub mod providers {
        pub mod copilot_auth {
            use serde::{Deserialize, Serialize};
            #[derive(Debug, Clone, Serialize, Deserialize)]
            pub struct GitHubAccount {
                pub id: String,
                pub login: String,
                pub avatar_url: Option<String>,
                pub authenticated_at: i64,
                pub github_domain: String,
                pub reauth_required: bool,
            }
            #[derive(Debug, Clone, Serialize, Deserialize)]
            pub struct GitHubDeviceCodeResponse {
                pub device_code: String,
                pub user_code: String,
                pub verification_uri: String,
                pub expires_in: u64,
                pub interval: u64,
            }
        }
        pub mod codex_oauth_auth;
    }
}
