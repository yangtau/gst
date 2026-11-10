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

# within SECONDS COMMAND...: runs a command of simctl, which now and then
# waits forever for the simulator, and ends it after that long.
within() {
	limit=$1
	shift
	echo "+ $*"
	"$@" &
	command=$!
	(sleep "$limit" && kill "$command" 2>/dev/null) &
	watch=$!
	status=0
	wait "$command" || status=$?
	kill "$watch" 2>/dev/null || true
	return "$status"
}

xcrun simctl boot "$udid" 2>/dev/null || true
within 300 xcrun simctl bootstatus "$udid" -b
within 120 xcrun simctl install "$udid" "$app"
within 30 xcrun simctl status_bar "$udid" override --time 9:41 --dataNetwork wifi --wifiBars 3 \
	--cellularBars 4 --batteryState charged --batteryLevel 100 || true

# shot LANGUAGE LOCALE APPEARANCE SCREEN
shot() {
	within 30 xcrun simctl ui "$udid" appearance "$3" || true
	within 60 xcrun simctl launch "$udid" "$bundle" -GstDemo "$4" -AppleLanguages "($1)" -AppleLocale "$2" || true
	sleep 5 # for the app to draw, and its sheet to settle
	within 60 xcrun simctl io "$udid" screenshot "$out/$1-$3-$4.png" || true
	within 30 xcrun simctl terminate "$udid" "$bundle" || true
}

for screen in home problems welcome off host-key authorize machine settings; do
	shot zh-Hans zh_CN light "$screen"
	shot en en_US light "$screen"
done
for screen in home welcome machine; do
	shot zh-Hans zh_CN dark "$screen"
done
ls "$out"
