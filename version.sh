#!/usr/bin/env bash
# Global version configuration for OmniDrive
# Source this file to get VERSION and VERSION_CODE
# 
# Usage:
#   . ./version.sh
#   echo $VERSION          # 1.0.0
#   echo $VERSION_CODE     # 1
#
# Override from command line:
#   VERSION=2.0.0 VERSION_CODE=2 ./build.sh
#   VERSION=2.0.0 VERSION_CODE=2 bash android/build-apk.sh

# Release version (used for all platforms: Linux, Windows, macOS, APK)
VERSION="${VERSION:-0.15.4}"

# APK version code (must be an integer, incremented with each update)
VERSION_CODE="${VERSION_CODE:-43}"

# Export for use in subshells
export VERSION VERSION_CODE
