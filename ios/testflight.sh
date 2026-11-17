#!/bin/sh
# Upload the iOS app to TestFlight: archive a Release build of the current
# commit, sign it for App Store Connect, and upload it, with the API key
# that ios/Local.xcconfig names (ASC_KEY_ID, ASC_ISSUER_ID), whose file is
# ~/.appstoreconnect/private_keys/AuthKey_<ASC_KEY_ID>.p8. Xcode makes the
# certificate and profiles that it needs. `make testflight` runs `make ios`
# first, which sets the build number.
set -eu

cd "$(dirname "$0")/.."
out=ios/build/testflight

# A build number is the commits up to HEAD, and App Store Connect takes
# each once: what it names must be what was built.
if [ -n "$(git status --porcelain)" ]; then
	echo "error: commit first: the build number names the commit, and the tree has changes" >&2
	exit 1
fi

setting() { sed -n "s/^$1 *= *//p" ios/Local.xcconfig | tail -n 1; }
key=$(setting ASC_KEY_ID)
issuer=$(setting ASC_ISSUER_ID)
team=$(setting DEVELOPMENT_TEAM)
if [ -z "$key" ] || [ -z "$issuer" ] || [ -z "$team" ]; then
	echo "error: set DEVELOPMENT_TEAM, ASC_KEY_ID, and ASC_ISSUER_ID in ios/Local.xcconfig" >&2
	exit 1
fi
keyfile=$HOME/.appstoreconnect/private_keys/AuthKey_$key.p8
[ -f "$keyfile" ] || { echo "error: no API key at $keyfile" >&2; exit 1; }
set -- -allowProvisioningUpdates \
	-authenticationKeyPath "$keyfile" -authenticationKeyID "$key" -authenticationKeyIssuerID "$issuer"

rm -rf "$out"
mkdir -p "$out"
xcodebuild -project ios/Gst.xcodeproj -scheme Gst -configuration Release \
	-destination 'generic/platform=iOS' -archivePath "$out/Gst.xcarchive" "$@" -quiet archive

cat >"$out/ExportOptions.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>method</key><string>app-store-connect</string>
	<key>destination</key><string>upload</string>
	<key>teamID</key><string>$team</string>
	<key>signingStyle</key><string>automatic</string>
	<key>manageAppVersionAndBuildNumber</key><false/>
</dict>
</plist>
PLIST
xcodebuild -exportArchive -archivePath "$out/Gst.xcarchive" \
	-exportOptionsPlist "$out/ExportOptions.plist" -exportPath "$out" "$@"
echo "uploaded build $(git rev-list --count HEAD) ($(git rev-parse --short HEAD)); App Store Connect processes it before TestFlight offers it"
