# The gst app for Android

How to use the app is in the [README](../README.md#android). This is how to build it, and how it is put together.

## Build

You need Go, a JDK 21 (which `nix develop` provides), and the Android SDK with an NDK: Android Studio's, or the command-line tools' (`sdkmanager "platforms;android-37.0" "ndk;29.0.14206865"`). Tell Gradle where the SDK is with `ANDROID_HOME` or `sdk.dir` in `android/local.properties`.

```sh
make android    # builds the Go code for each ABI, then the debug APK
adb install android/app/build/outputs/apk/debug/app-debug.apk
```

`make android` runs `android/build-core.sh`, which builds `gstkit` into `android/build/jniLibs/<abi>/libgstkit.so` for arm64-v8a, armeabi-v7a, and x86_64 with the newest NDK (or `ANDROID_NDK_HOME`), and then `./gradlew assembleDebug` in `android/`. Run `build-core.sh` again after changing Go code; Android Studio builds the rest. `./gradlew clean` removes the Go code too.

The app's identifier is `gst.app`; `-PgstApplicationId=…`, or `gstApplicationId` in `~/.gradle/gradle.properties`, gives a build of your own another. The release APK (`./gradlew assembleRelease`) is minified and signed with the debug key; sign it with your own key to publish it.

## Layout

| Path | What |
| --- | --- |
| `../gstkit/` | gst's Go code as a C library, which the iOS app uses too; `jni.c` makes it the native methods of `Core` |
| `app/src/main/java/gst/app/` | the app: `Core` (JNI), `Gst` and `Wire` (the calls of gst's methods and what they return), `Machine` and `AppModel` (what the screens show), `Tunnel` and `GstVpnService` (the VPN, in which gst's daemon runs), `BootReceiver`, and `Demo` (a made-up network) |
| `app/src/main/java/gst/app/ui/` | the screens, in Jetpack Compose with Material 3 Expressive, in the phone's own colors from Android 12 on and in gst's teal before |
| `app/src/main/res/` | the texts (`values`, and `values-b+zh+Hans` in Simplified Chinese), and the icon |
| `app/src/test/` | the tests, below |
| `screenshots/` | pictures of the screens, which the tests take |

The app is to gst what the `gst` command is on other machines, and its `VpnService` what the daemon is. Unlike iOS, both run in one process: the app calls the `app.…` methods and the daemon's control methods (`daemon.status`, `mesh.topology`, `net.ping`, …) through the same `Core.call`. The design is in [`docs/design/mesh.md`](../docs/design/mesh.md#android).

## Tests

`.github/workflows/android.yml` runs on every push that touches the app or the Go code under it. It builds the Go code, the debug and release APKs, and runs the unit tests on the JVM, as `./gradlew testDebugUnitTest` does in `android/`:

- `ModelTest` reads what gst reports, as Go writes it, and checks what the app makes of it and of what a person types.
- `CoreTest` runs gst's Go code through `Core` and its JNI: the app's methods, the daemon's start, and its call of the system for an interface. It needs gst built for the machine that runs the tests (`android/build-core.sh host`), and is skipped without it.
- `ScreenTest` opens every screen on the made-up network of `Demo`, in English and Chinese, light and dark, with Robolectric, checks that it shows, and keeps a picture of it in `app/build/screenshots`. The workflow keeps those as the `android-screenshots` artifact, and the APKs as `android-apks`. The pictures in `screenshots/`, which the README shows, come from there.

A debug build opens on a screen of `Demo` with `adb shell am start -n gst.app/.MainActivity --es GstDemo home` (and `--es GstDemoAppearance dark`), and Android Studio's previews (`ui/Previews.kt`) show the same.

What only a device shows, and nothing here has checked: that the interface comes up, that `.gst` names resolve and other names still do, that links survive moving between Wi-Fi and mobile data, and that gst comes back after a restart.
