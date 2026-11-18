# The gst app for Android

How to use the app is in the [README](../README.md#android). This is how to build it, and how it is put together.

## Build

`nix develop .#android` gives everything (on macOS and x86_64 Linux): Go, a JDK 21, and the Android SDK with the platform, build tools, and NDK that the app needs, at `ANDROID_HOME`. Using it accepts the Android SDK's license. Without Nix, Android Studio's SDK or the command-line tools' (`sdkmanager "platforms;android-37.0" "build-tools;36.0.0" "ndk;29.0.14206865"`) do too; tell Gradle where it is with `ANDROID_HOME` or `sdk.dir` in `android/local.properties`.

```sh
nix develop .#android -c make android          # the Go code for each ABI, then the debug APK
nix develop .#android -c make android-install  # the release APK, installed on the phone that adb is connected to, and opened
```

`make android-install` installs over the app from GitHub's releases, data and all, when the build has the release key: `gstKeystore` and `gstKeystorePassword` in `~/.gradle/gradle.properties` (below). Otherwise the phone refuses it until the app is uninstalled.

adb reaches the phone over gst, from wherever both are in the network: on the phone, turn on Developer options, then Wireless debugging in them, which needs Wi-Fi, and pair once with "Pair device with pairing code", using the phone's gst IP (`gst status`) and the port and code that it shows:

```sh
adb pair <gst IP>:<pairing port> <code>
adb connect <gst IP>:<port>   # the port on the Wireless debugging screen, which changes each time it turns on
```

The install ends the VPN, and with it the connection of adb over gst, so `android/install.sh` copies the APK to the phone and leaves the rest to a job there, which installs it and opens the app; the app turns gst on again when it opens, if it was on. Run `adb connect` again once gst is back; `adb shell cat /data/local/tmp/gst-install.log` says how the install went.

`make android` runs `android/build-core.sh`, which builds `gstkit` into `android/build/jniLibs/<abi>/libgstkit.so` for arm64-v8a, armeabi-v7a, and x86_64 with the newest NDK (or `ANDROID_NDK_HOME`), and then `./gradlew assembleDebug` in `android/`. Run `build-core.sh` again after changing Go code; Android Studio builds the rest. `./gradlew clean` removes the Go code too.

The app's identifier is `gst.app`; `-PgstApplicationId=…`, or `gstApplicationId` in `~/.gradle/gradle.properties`, gives a build of your own another. The release APK (`./gradlew assembleRelease`) is minified, and signed with the release key when `gstKeystore` and `gstKeystorePassword` name it (a PKCS12 keystore whose key has the alias `gst` and the store's password), else with the debug key. An APK installs over another only if the same key signed both, so the releases keep one key: CI takes it from the secrets `ANDROID_KEYSTORE` (the keystore in base64) and `ANDROID_KEYSTORE_PASSWORD`, and refuses to build a release without it.

## Layout

| Path | What |
| --- | --- |
| `../gstkit/` | gst's Go code as a C library, which the iOS app uses too; `jni.c` makes it the native methods of `Core` |
| `app/src/main/java/gst/app/` | the app: `Core` (JNI), `Gst` and `Wire` (the calls of gst's methods and what they return), `Machine`, `Ring`, and `AppModel` (what the screens show), `Tunnel` and `GstVpnService` (the VPN, in which gst's daemon runs), `BootReceiver`, and `Demo` (a made-up network) |
| `app/src/main/java/gst/app/ui/` | the screens, in Jetpack Compose: pages with large titles (the first screen has none, and its buttons in a bar at the bottom), groups of rows, bars of frosted glass ([Haze](https://github.com/chrisbanes/haze)) over what scrolls, and sheets that come up from the bottom, in gst's own colors, on white or black |
| `app/src/main/res/` | the texts (`values`, and `values-b+zh+Hans` in Simplified Chinese), and the icon |
| `app/src/test/` | the tests, below |
| `screenshots/` | pictures of the screens, which the tests take |
| `build-core.sh`, `install.sh` | build gst's Go code for the app; build the release APK, install it, and open it on the phone |

The app is to gst what the `gst` command is on other machines, and its `VpnService` what the daemon is. Unlike iOS, both run in one process: the app calls the `app.…` methods and the daemon's control methods (`daemon.status`, `mesh.topology`, `mesh.status`, `net.ping`, …) through the same `Core.call`. The design is in [`docs/design/mesh.md`](../docs/design/mesh.md#android).

## Tests

The unit tests run on the JVM, with `./gradlew testDebugUnitTest` in `android/`:

- `ModelTest` reads what gst reports, as Go writes it, and checks what the app makes of it and of what a person types.
- `CoreTest` runs gst's Go code through `Core` and its JNI: the app's methods, the daemon's start, and its call of the system for an interface. It needs gst built for the machine that runs the tests (`android/build-core.sh host`), and is skipped without it.
- `ScreenTest` opens every screen on the made-up network of `Demo`, in English and Chinese, light and dark, with Robolectric, checks that it shows, and keeps a picture of it in `app/build/screenshots`. The pictures in `screenshots/`, which the README shows, come from such a run.

`.github/workflows/android.yml` runs on every push that touches the app or the Go code under it. It builds the Go code and the debug and release APKs, which it keeps as `android-apks`, and for a tag `vX.Y.Z` adds the release APK to the GitHub release as `gst-android.apk`, and runs the tests besides `ScreenTest`, which only a developer's machine runs (`-PgstScreenTests=false` leaves it out).

A debug build opens on a screen of `Demo` with `adb shell am start -n gst.app/.MainActivity --es GstDemo home` (and `--es GstDemoAppearance dark`), and Android Studio's previews (`ui/Previews.kt`) show the same.

What only a device shows, and nothing here has checked: that the interface comes up, that `.gst` names resolve and other names still do, that links survive moving between Wi-Fi and mobile data, and that gst comes back after a restart.
