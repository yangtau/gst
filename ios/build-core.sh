#!/bin/sh
# Build gst's Go code for the iOS app: ios/build/GstKit.xcframework, a
# static library (gstkit) for devices and for the simulator on Apple
# silicon, and ios/build/Version.xcconfig, which gives the app the release
# of gst. It needs Xcode and Go.
set -eu

min=26.0 # the iOS that the app needs; see project.yml
cd "$(dirname "$0")/.."
out=ios/build

command -v xcrun >/dev/null || { echo "error: building the iOS app needs Xcode" >&2; exit 1; }
rm -rf "$out/GstKit.xcframework" "$out/core"
mkdir -p "$out/core/include"
cp gstkit/gstkit.h ios/module.modulemap "$out/core/include/"

# build SDK TARGET DIRECTORY
build() {
	sdk=$(xcrun --sdk "$1" --show-sdk-path)
	flags="-isysroot $sdk -target $2"
	CGO_ENABLED=1 GOOS=ios GOARCH=arm64 CC="$(xcrun --sdk "$1" --find clang)" \
		CGO_CFLAGS="$flags" CGO_LDFLAGS="$flags" \
		go build -trimpath -buildmode=c-archive -o "$out/core/$3/libgstkit.a" ./gstkit
	rm -f "$out/core/$3/libgstkit.h" # cgo's own header; gstkit.h is the one to use
}
build iphoneos "arm64-apple-ios$min" device
build iphonesimulator "arm64-apple-ios$min-simulator" simulator

xcodebuild -create-xcframework \
	-library "$out/core/device/libgstkit.a" -headers "$out/core/include" \
	-library "$out/core/simulator/libgstkit.a" -headers "$out/core/include" \
	-output "$out/GstKit.xcframework"

version=$(sed -n 's/^const Release = "\(.*\)"$/\1/p' internal/version/version.go)
[ -n "$version" ] || { echo "error: no release in internal/version/version.go" >&2; exit 1; }
printf 'MARKETING_VERSION = %s\n' "$version" >"$out/Version.xcconfig"
echo "built $out/GstKit.xcframework for gst $version"
