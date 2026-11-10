#!/bin/sh
# Print the id of an iPhone simulator with the newest iOS that this Xcode
# has, for xcodebuild's -destination and for simctl.
set -eu
xcrun simctl list devices available -j | python3 -c '
import json, sys
devices = json.load(sys.stdin)["devices"]
runtimes = sorted((r for r in devices if "iOS" in r), key=lambda r: [int(n) for n in r.rsplit("iOS-", 1)[1].split("-")])
for runtime in reversed(runtimes):
    for device in devices[runtime]:
        if device["name"].startswith("iPhone"):
            print(device["udid"])
            sys.exit(0)
sys.exit("no iPhone simulator is installed")
'
