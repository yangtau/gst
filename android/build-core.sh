#!/bin/sh
# Build gst's Go code for the Android app: android/build/jniLibs/<abi>/
# libgstkit.so, a shared library (gstkit, with its jni.c) for each ABI that
# the app ships. It needs Go and the Android NDK: ANDROID_NDK_HOME, or the
# newest NDK under ANDROID_HOME (or the sdk.dir of android/local.properties).
#
# With "host", it builds android/build/host/libgstkit.so instead, for this
# machine's Java, which the app's unit tests load (CoreTest); that needs
# Go, a C compiler, and a JDK.
set -eu

api=29 # the Android that the app needs; see app/build.gradle.kts
cd "$(dirname "$0")/.."

if [ "${1:-}" = host ]; then
	java_home=${JAVA_HOME:-$(dirname "$(dirname "$(readlink -f "$(command -v javac)")")")}
	os=$(uname -s | tr '[:upper:]' '[:lower:]')
	mkdir -p android/build/host
	CGO_ENABLED=1 CGO_CFLAGS="-I$java_home/include -I$java_home/include/$os" \
		go build -trimpath -tags jni -buildmode=c-shared -o android/build/host/libgstkit.so ./gstkit
	rm -f android/build/host/libgstkit.h
	echo "built android/build/host/libgstkit.so"
	exit 0
fi

out=android/build/jniLibs

sdk=${ANDROID_HOME:-${ANDROID_SDK_ROOT:-}}
if [ -z "$sdk" ] && [ -f android/local.properties ]; then
	sdk=$(sed -n 's/^sdk\.dir=//p' android/local.properties)
fi
ndk=${ANDROID_NDK_HOME:-}
if [ -z "$ndk" ] && [ -n "$sdk" ] && [ -d "$sdk/ndk" ]; then
	ndk="$sdk/ndk/$(ls "$sdk/ndk" | sort -V | tail -n 1)"
fi
[ -n "$ndk" ] && [ -d "$ndk" ] || { echo "error: building the Android app needs the Android NDK; set ANDROID_NDK_HOME" >&2; exit 1; }
toolchain=$(echo "$ndk"/toolchains/llvm/prebuilt/*/bin)

rm -rf "$out"

# build GOARCH GOARM ABI CLANG-TARGET
build() {
	mkdir -p "$out/$3"
	CGO_ENABLED=1 GOOS=android GOARCH=$1 GOARM=$2 CC="$toolchain/$4$api-clang" \
		go build -trimpath -buildmode=c-shared \
		-ldflags "-s -w -extldflags=-Wl,-z,max-page-size=16384" \
		-o "$out/$3/libgstkit.so" ./gstkit
	rm -f "$out/$3/libgstkit.h" # cgo's own header; gstkit.h is the one
}
build arm64 "" arm64-v8a aarch64-linux-android
build arm 7 armeabi-v7a armv7a-linux-androideabi
build amd64 "" x86_64 x86_64-linux-android

echo "built $out for gst $(sed -n 's/^const Release = "\(.*\)"$/\1/p' internal/version/version.go)"
