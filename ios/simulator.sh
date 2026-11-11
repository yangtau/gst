#!/bin/sh
# Print the id of an iPhone simulator with the newest iOS that this Xcode
# has, for xcodebuild's -destination and for simctl.
set -eu
xcrun simctl list devices available -j | python3 -c '
import json, sys
devices = json.load(sys.stdin)["devices"]
# A runtime for particular devices has their name after the version, as in
# iOS-27-2-iPhone19-4; of the same version, the general one comes first.
def order(runtime):
    parts = runtime.rsplit("iOS-", 1)[1].split("-")
    version = []
    while parts and parts[0].isdigit():
        version.append(int(parts.pop(0)))
    return version, not parts
runtimes = sorted((r for r in devices if "iOS" in r), key=order)
for runtime in reversed(runtimes):
    for device in devices[runtime]:
        if device["name"].startswith("iPhone"):
            print(device["udid"])
            sys.exit(0)
sys.exit("no iPhone simulator is installed")
'
