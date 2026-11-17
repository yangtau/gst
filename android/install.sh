#!/bin/sh
# Builds the release APK and installs it on the phone that adb is connected
# to, then has the phone open the app, which turns gst on again if it was
# on. adb may reach the phone over gst, whose VPN the install ends, so the
# phone opens the app by itself: a job on it waits for the update.
set -eu
cd "$(dirname "$0")"

./gradlew -q assembleRelease
apk=app/build/outputs/apk/release/app-release.apk
aapt2=$(ls -d "$ANDROID_HOME"/build-tools/*/aapt2 | tail -n 1)
app=$("$aapt2" dump packagename "$apk")

adb shell "cat > /data/local/tmp/gst-open.sh" <<EOF
updated() { dumpsys package $app | grep -m 1 lastUpdateTime; }
before=\$(updated)
i=0
while [ \$i -lt 300 ] && [ "\$(updated)" = "\$before" ]; do sleep 1; i=\$((i + 1)); done
[ "\$(updated)" != "\$before" ] && sleep 2 && am start -n $app/gst.app.MainActivity
rm -f /data/local/tmp/gst-open.sh
EOF
adb shell "nohup setsid sh /data/local/tmp/gst-open.sh >/dev/null 2>&1 < /dev/null &"

./gradlew -q installRelease
