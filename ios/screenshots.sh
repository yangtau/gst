#!/bin/sh
# Take screenshots of the app in the simulator, on a made-up network
# (App/Demo.swift): screenshots.sh APP DIRECTORY, where APP is a debug
# build of gst.app for the simulator. Each file is LANGUAGE-APPEARANCE-SCREEN.png.
set -eu

app=$1
out=$2
bundle=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$app/Info.plist")
udid=$("$(dirname "$0")/simulator.sh")
mkdir -p "$out"

xcrun simctl boot "$udid" 2>/dev/null || true
xcrun simctl bootstatus "$udid" -b >/dev/null
xcrun simctl install "$udid" "$app"
xcrun simctl status_bar "$udid" override --time 9:41 --dataNetwork wifi --wifiBars 3 \
	--cellularBars 4 --batteryState charged --batteryLevel 100

# shot LANGUAGE LOCALE APPEARANCE SCREEN
shot() {
	xcrun simctl ui "$udid" appearance "$3"
	xcrun simctl launch "$udid" "$bundle" -GstDemo "$4" -AppleLanguages "($1)" -AppleLocale "$2" >/dev/null
	sleep 5 # for the app to draw, and its sheet to settle
	xcrun simctl io "$udid" screenshot "$out/$1-$3-$4.png" 2>/dev/null
	xcrun simctl terminate "$udid" "$bundle"
}

for screen in home problems welcome off host-key authorize machine settings; do
	shot zh-Hans zh_CN light "$screen"
	shot en en_US light "$screen"
done
for screen in home welcome machine; do
	shot zh-Hans zh_CN dark "$screen"
done
ls "$out"
