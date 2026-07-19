# Sola — Android client

A thin Android wrapper around the Sola web dashboard. On first launch it asks for
the dashboard's IP/hostname and port, verifies the server is actually reachable
(by probing `/api/status`), then loads the dashboard in a full-screen WebView.
The address is remembered, so later launches go straight to the dashboard.

## Requirements

- JDK 17
- Android SDK (this project was set up against `~/Android/Sdk`)
- No system Gradle needed — use the bundled wrapper (`./gradlew`)

`local.properties` (pointing at your SDK) is generated locally and git-ignored.
If it's missing, create it:

```
sdk.dir=/absolute/path/to/Android/Sdk
```

## Build & install

```bash
# From clients/android/
./gradlew assembleDebug                 # app/build/outputs/apk/debug/app-debug.apk
./gradlew installDebug                  # build + install onto a connected device/emulator
./gradlew assembleRelease               # app/build/outputs/apk/release/app-release.apk (signed, R8-minified)
```

Or just open `clients/android/` in Android Studio and hit Run.

### Release signing

`assembleRelease` reads signing config from `keystore.properties` (git-ignored),
which points at a keystore (`sola-release.jks`, also git-ignored). Both are local
secrets — **keep the keystore safe**: Android requires the *same* key to install
future updates over an existing install. If `keystore.properties` is absent the
release build still runs but produces an unsigned APK that won't install.

`keystore.properties` format:

```
storeFile=sola-release.jks
storePassword=...
keyAlias=sola
keyPassword=...
```

Regenerate a keystore with:

```bash
keytool -genkeypair -v -keystore sola-release.jks -alias sola \
  -keyalg RSA -keysize 2048 -validity 10000
```

### Sideloading onto a phone

Copy `app-release.apk` to the phone (USB, cloud drive, etc.), open it in a file
manager, and allow "install from unknown sources" when prompted. Or, with USB
debugging on: `adb install app-release.apk`.

## How it works

| File | Role |
|------|------|
| `SettingsActivity.kt` | Launch screen: collect IP + port, validate format, probe reachability, hand off. Auto-connects to a remembered server. |
| `ConnectionChecker.kt` | Off-main-thread `GET /api/status` probe — the real "is it there?" check. |
| `ServerConfig.kt` | Format validation + `SharedPreferences` persistence. Default port `8088`. |
| `WebViewActivity.kt` | Full-screen WebView (JS + DOM storage on), back-button → WebView history. Toolbar (**Reload** / **Change server**) is hidden by default; swipe down from the top edge to reveal it, and it auto-hides after ~3.5s idle. |

## Notes & gotchas

- **Cleartext HTTP** — the dashboard is served over plain `http://` on a LAN, so
  `res/xml/network_security_config.xml` re-enables cleartext (blocked by default
  on Android 9+). It's permitted globally because the user types an arbitrary LAN
  IP at runtime and Android's `<domain>` rules don't accept CIDR ranges. If you
  ever move the dashboard to HTTPS, tighten that file.
- **minSdk 26** (Android 8.0) — chosen so adaptive launcher icons work without
  shipping raster PNG fallbacks. Covers essentially all active devices.
- **Change server** — from the dashboard's toolbar overflow; it clears the saved
  address and returns to the launch screen.

## Roadmap ideas (not in v1)

- Multiple saved servers
- mDNS/Bonjour auto-discovery instead of typing an IP
- Pull-to-refresh
- iOS sibling under `clients/ios/` (WKWebView + App Transport Security exception)
