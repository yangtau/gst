# The gst app for iPhone and iPad

How to use the app is in the [README](../README.md#iphone-and-ipad). This is how to build it, and how it is put together.

## Build

You need a Mac with Xcode 26 or later, Go, [XcodeGen](https://github.com/yonaskolb/XcodeGen) (`brew install xcodegen`), and an Apple Developer Program membership: the app is a VPN, and a free account cannot sign the Network Extension entitlement that a VPN needs.

```sh
cp ios/Local.xcconfig.example ios/Local.xcconfig   # then set your team id and a bundle identifier of your own
make ios                                           # builds the Go code and generates ios/Gst.xcodeproj
open ios/Gst.xcodeproj
```

In Xcode, choose the `Gst` scheme and an iPhone or iPad with iOS 26, and run. Xcode registers the two bundle identifiers and the app group with your team the first time (Signing & Capabilities shows them: Network Extensions with Packet Tunnel, and App Groups).

Run `make ios` again after changing Go code, the release, or `project.yml`. Everything it makes is in `ios/build/` and `ios/Gst.xcodeproj`, which git ignores.

A VPN does not run in the Simulator: the app starts there and shows its screens, and turning gst on fails. To see the screens with a network in them there, add `-GstDemo home` to the scheme's launch arguments (see Tests below).

## Layout

| Path | What |
| --- | --- |
| `gstkit/` | gst's Go code as a C library: `internal/mobile` behind the five functions of `gstkit.h`. `build-core.sh` builds it into `build/GstKit.xcframework` |
| `Shared/` | `Core`, the Swift face of that library, for both targets |
| `App/` | the app: `Model/` (the calls of gst's methods, the VPN, what the screens show) and `Views/` (SwiftUI, with Liquid Glass for the controls) |
| `Tunnel/` | the packet tunnel extension, in which gst's daemon runs |
| `Tests/`, `UITests/` | the tests, below |
| `screenshots/` | pictures of the screens, which the tests take |
| `project.yml`, `Config.xcconfig` | the Xcode project, for XcodeGen, and what names and signs the app |

The app is to gst what the `gst` command is on other machines, and the packet tunnel what the daemon is. The app changes the configuration through the `app.…` methods of gst in its own process, and reads everything else from the daemon with the same control methods that the command uses (`daemon.status`, `mesh.topology`, `net.ping`, …), sent as provider messages. The design is in [`docs/design/mesh.md`](../docs/design/mesh.md).

The texts of the app are in `App/Localizable.xcstrings`, in English and Simplified Chinese.

## Tests

`.github/workflows/ios.yml` runs on every push that touches the app or the Go code under it. It generates the project, builds the app for a device, and runs the scheme's tests in the simulator, as `xcodebuild -project ios/Gst.xcodeproj -scheme Gst -destination 'platform=iOS Simulator,name=…' test` does on a Mac:

- `Tests/` (GstTests) runs gst's Go code through `Core`: the app's methods, the daemon's start and its answers, and what the app makes of them.
- `UITests/` (GstUITests) opens every screen on a made-up network, in English and Chinese, light and dark, and checks that it shows. `App/Demo.swift` is that network: a debug build opens on one of its screens with `-GstDemo <screen>` on its command line, and Xcode's previews (`App/Previews.swift`) show the same.

The tests of the screens keep a picture of each screen as an attachment, and as files in the directory that `TEST_RUNNER_GST_SCREENSHOTS` names for xcodebuild. The workflow keeps those as the `screenshots` artifact of a run. The pictures in `screenshots/`, which the README shows, are the workflow's too: it commits them, made smaller, when the message of a commit has `[screenshots]`, or when it is run by hand. Ask for that after a change to what the screens look like.

`internal/mobile`, the daemon without a socket, and the DNS and ping inside the interface have Go unit tests (`make test`), which join a phone to a network through an SSH server, a real daemon, and a fake interface.

What only a device shows, and nothing here has checked: that the interface comes up and `.gst` names resolve, that links survive moving between Wi-Fi and cellular, and how much memory the extension uses (iOS ends it at 50 MB).
