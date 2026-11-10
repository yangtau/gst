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

A VPN does not run in the Simulator: the app starts there and shows its screens, and turning gst on fails.

## Layout

| Path | What |
| --- | --- |
| `gstkit/` | gst's Go code as a C library: `internal/mobile` behind the five functions of `gstkit.h`. `build-core.sh` builds it into `build/GstKit.xcframework` |
| `Shared/` | `Core`, the Swift face of that library, for both targets |
| `App/` | the app: `Model/` (the calls of gst's methods, the VPN, what the screens show) and `Views/` (SwiftUI, with Liquid Glass for the controls) |
| `Tunnel/` | the packet tunnel extension, in which gst's daemon runs |
| `project.yml`, `Config.xcconfig` | the Xcode project, for XcodeGen, and what names and signs the app |

The app is to gst what the `gst` command is on other machines, and the packet tunnel what the daemon is. The app changes the configuration through the `app.…` methods of gst in its own process, and reads everything else from the daemon with the same control methods that the command uses (`daemon.status`, `mesh.topology`, `net.ping`, …), sent as provider messages. The design is in [`docs/design/mesh.md`](../docs/design/mesh.md).

The texts of the app are in `App/Localizable.xcstrings`, in English and Simplified Chinese.

## What is tested, and what is not

- `internal/mobile`, the daemon without a socket, and the DNS and ping inside the interface have unit tests (`make test`), which join a phone to a network through an SSH server, a real daemon, and a fake interface.
- `gstkit` builds and runs as a library on Linux, and the Swift files that need only Foundation (`Shared/Core.swift`, `App/Model/Wire.swift`, `Gst.swift`, `Machine.swift`) were compiled and run against it there.
- The rest of the Swift code, `build-core.sh`, and `project.yml` have not been built with Xcode or run on a device yet. Expect to fix small things on the first build, and check on a device: that the interface comes up and `.gst` names resolve, that links survive moving between Wi-Fi and cellular, and how much memory the extension uses (iOS ends it at 50 MB).
