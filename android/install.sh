#!/bin/sh
# Builds the release APK, installs it on the phone that adb is connected to,
# and opens the app, which turns gst on again if it was on. adb may reach the
# phone over gst, whose VPN the install ends, so the phone installs the APK
# and opens the app by itself, in a job that outlives the connection; it
# writes what happened to /data/local/tmp/gst-install.log.
set -eu
cd "$(dirname "$0")"

sdk=${ANDROID_HOME:-${ANDROID_SDK_ROOT:-}}
if [ -z "$sdk" ] && [ -f local.properties ]; then
	sdk=$(sed -n 's/^sdk\.dir=//p' local.properties)
fi
aapt2=$(ls -d "$sdk"/build-tools/*/aapt2 2>/dev/null | tail -n 1)
if [ -z "$aapt2" ]; then
	echo "install.sh: no Android SDK build tools: set ANDROID_HOME, or sdk.dir in android/local.properties" >&2
	exit 1
fi

./gradlew -q assembleRelease
apk=app/build/outputs/apk/release/app-release.apk
app=$("$aapt2" dump packagename "$apk")

adb push "$apk" /data/local/tmp/gst.apk >/dev/null
adb shell "nohup setsid sh -c 'pm install -r /data/local/tmp/gst.apk && am start -n $app/gst.app.MainActivity; rm -f /data/local/tmp/gst.apk' > /data/local/tmp/gst-install.log 2>&1 < /dev/null &"
echo "The phone installs $app and opens it; adb shell cat /data/local/tmp/gst-install.log says how it went."
