#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."

# GO_BIN may select an existing local toolchain; no implicit downloads/install.
export MACOSX_DEPLOYMENT_TARGET=13.0
arch="$(uname -m)"
go_bin="${GO_BIN:-go}"
if [ -n "${CARGO_BIN:-}" ]; then
  cargo_bin="$CARGO_BIN"
elif command -v cargo >/dev/null 2>&1; then
  cargo_bin="cargo"
elif [ -x "$HOME/.cargo/bin/cargo" ]; then
  cargo_bin="$HOME/.cargo/bin/cargo"
else
  cargo_bin="cargo"
fi
app_dir="$PWD/dist/Codex 小助手.app"
mkdir -p "$app_dir/Contents/MacOS" "$app_dir/Contents/Resources" bin
icon_source="$PWD/assets/codex-helper-icon-minimal-v2/codex-helper-icon.png"
iconset="$PWD/dist/CodexHelper.iconset"
mkdir -p "$iconset"
for size in 16 32 128 256 512; do
  sips -z "$size" "$size" "$icon_source" --out "$iconset/icon_${size}x${size}.png" >/dev/null
  retina=$((size * 2))
  sips -z "$retina" "$retina" "$icon_source" --out "$iconset/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil --convert icns "$iconset" --output "$app_dir/Contents/Resources/CodexHelper.icns"
"$go_bin" build -o bin/limitping ./cmd/limitping
"$go_bin" build -o bin/codex-helper ./cmd/codex-helper
"$go_bin" build -o bin/codex-runtime-bridge ./cmd/codex-runtime-bridge
"$cargo_bin" build --manifest-path native-accounts/Cargo.toml --release --locked
cp bin/codex-helper "$app_dir/Contents/MacOS/codex-helper"
cp bin/codex-runtime-bridge "$app_dir/Contents/MacOS/codex-runtime-bridge"
cp native-accounts/target/release/codex-helper-accounts "$app_dir/Contents/MacOS/codex-accounts"
cp native-accounts/CC-SWITCH-LICENSE "$app_dir/Contents/Resources/CC-SWITCH-LICENSE"
swiftc -O -target "${arch}-apple-macosx13.0" \
  macos/CodexHelper/State.swift \
  macos/CodexHelper/Bridge.swift \
  macos/CodexHelper/Theme.swift \
  macos/CodexHelper/QuotaCard.swift \
  macos/CodexHelper/CCSwitchControl.swift \
  macos/CodexHelper/NativeAccounts.swift \
  macos/CodexHelper/Orchestra.swift \
  macos/CodexHelper/AppModel.swift \
  macos/CodexHelper/AppShell.swift \
  macos/CodexHelper/OverviewAndAccountsViews.swift \
  macos/CodexHelper/SessionsPageView.swift \
  macos/CodexHelper/ProjectsPageView.swift \
  macos/CodexHelper/SettingsViews.swift \
  macos/CodexHelper/main.swift \
  -o "$app_dir/Contents/MacOS/CodexHelper" \
  -framework Cocoa -framework SwiftUI -framework ServiceManagement
cat > "$app_dir/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>local.codex-helper.account-preview</string>
<key>CFBundleName</key><string>Codex 小助手</string>
<key>CFBundleDisplayName</key><string>Codex 小助手</string>
<key>CFBundleExecutable</key><string>CodexHelper</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleIconFile</key><string>CodexHelper.icns</string>
<key>CFBundleShortVersionString</key><string>0.5.0</string>
<key>CFBundleVersion</key><string>48</string>
<key>NSHighResolutionCapable</key><true/>
<key>LSMinimumSystemVersion</key><string>13.0</string>
</dict></plist>
PLIST
bash scripts/sign-macos-helper.sh "$app_dir"
printf '%s\n' "$app_dir"
