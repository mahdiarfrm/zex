#!/bin/sh
# Build the Zex desktop app into dist/.
#
#   desktop/build.sh            # macOS (on a Mac) and Windows; Linux too when run on Linux
#   desktop/build.sh mac        # Zex.app, universal (Apple silicon + Intel), zipped
#   desktop/build.sh windows    # Zex.exe for x64 and arm64, zipped
#   desktop/build.sh linux      # this machine's architecture: .tar.gz and .deb
#
# Needs Go. The macOS build must run on a Mac (it links WebKit) and the Linux build on Linux
# (it links WebKitGTK: apt install libgtk-3-dev libwebkit2gtk-4.1-dev). Windows cross-compiles anywhere.
set -e
cd "$(dirname "$0")/.."
VERSION=0.2.0
BUNDLE_ID=${BUNDLE_ID:-app.zex.desktop}
what=${1:-all}
mkdir -p dist

[ -f assets/icon.icns ] && [ -f assets/icon.ico ] || sh assets/build_icons.sh

build_mac() {
  if [ "$(uname)" != Darwin ]; then echo "skip mac: needs macOS"; return; fi
  out=dist/mac; rm -rf "$out"; mkdir -p "$out"
  for arch in arm64 amd64; do
    clang_arch=$arch; [ $arch = amd64 ] && clang_arch=x86_64
    CGO_ENABLED=1 GOOS=darwin GOARCH=$arch MACOSX_DEPLOYMENT_TARGET=11.0 \
      CC="clang -arch $clang_arch" CGO_CFLAGS="-mmacosx-version-min=11.0" CGO_LDFLAGS="-mmacosx-version-min=11.0" \
      go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$out/zex-$arch" ./desktop 2>&1 | grep -v 'warning:\|webview.h\|^In file\|^# github.com/webview\|^ *[0-9]* |\|^ *|' || true
    [ -f "$out/zex-$arch" ] || { echo "mac $arch build failed"; exit 1; }
  done
  app="$out/Zex.app"
  mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
  lipo -create -output "$app/Contents/MacOS/zex" "$out/zex-arm64" "$out/zex-amd64"
  rm "$out/zex-arm64" "$out/zex-amd64"
  cp assets/icon.icns "$app/Contents/Resources/icon.icns"
  cat > "$app/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>Zex</string>
  <key>CFBundleDisplayName</key><string>Zex</string>
  <key>CFBundleIdentifier</key><string>$BUNDLE_ID</string>
  <key>CFBundleExecutable</key><string>zex</string>
  <key>CFBundleIconFile</key><string>icon</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>$VERSION</string>
  <key>CFBundleVersion</key><string>$VERSION</string>
  <key>LSMinimumSystemVersion</key><string>11.0</string>
  <key>LSApplicationCategoryType</key><string>public.app-category.developer-tools</string>
  <key>NSHighResolutionCapable</key><true/>
  <key>NSHumanReadableCopyright</key><string>MIT License</string>
  <key>NSAppTransportSecurity</key><dict><key>NSAllowsLocalNetworking</key><true/></dict>
</dict>
</plist>
EOF
  # Ad-hoc signature: enough to run locally. For public downloads, sign with a Developer ID and notarize.
  codesign --force --deep --sign - "$app"
  (cd "$out" && ditto -c -k --keepParent Zex.app "../Zex-$VERSION-macOS.zip")
  echo "built dist/Zex-$VERSION-macOS.zip"
}

build_windows() {
  go run github.com/tc-hib/go-winres@v0.3.3 make --in desktop/winres/winres.json --out desktop/rsrc --arch amd64,arm64
  for arch in amd64 arm64; do
    name=x64; [ $arch = arm64 ] && name=arm64
    out=dist/windows-$name; rm -rf "$out"; mkdir -p "$out"
    CGO_ENABLED=0 GOOS=windows GOARCH=$arch go build -trimpath -ldflags "-s -w -H windowsgui -X main.version=$VERSION" -o "$out/Zex.exe" ./desktop
    (cd "$out" && rm -f "../Zex-$VERSION-Windows-$name.zip" && zip -q -9 "../Zex-$VERSION-Windows-$name.zip" Zex.exe)
    echo "built dist/Zex-$VERSION-Windows-$name.zip"
  done
  rm -f desktop/rsrc_windows_*.syso
}

build_linux() {
  if [ "$(uname)" != Linux ]; then echo "skip linux: needs Linux"; return; fi
  arch=$(go env GOARCH); name=x64; [ "$arch" = arm64 ] && name=arm64
  # The webview package asks pkg-config for WebKitGTK 4.0, which current distros replaced
  # with 4.1 (same API, newer HTTP stack). This shim points it at 4.1.
  shim=$(mktemp -d)
  printf 'Name: webkit2gtk-4.0\nDescription: WebKitGTK 4.1 under the 4.0 name\nVersion: 2.40\nRequires: webkit2gtk-4.1\n' > "$shim/webkit2gtk-4.0.pc"
  out=dist/linux-$name/zex; rm -rf "dist/linux-$name"; mkdir -p "$out"
  PKG_CONFIG_PATH="$shim${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}" CGO_ENABLED=1 \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$out/zex" ./desktop
  rm -rf "$shim"
  cp assets/icon-linux-512.png "$out/zex.png"; cp desktop/linux/zex.desktop desktop/linux/install.sh LICENSE "$out/"
  tar -C "dist/linux-$name" -czf "dist/Zex-$VERSION-Linux-$name.tar.gz" zex
  echo "built dist/Zex-$VERSION-Linux-$name.tar.gz"
  if command -v dpkg-deb >/dev/null; then
    root=$(mktemp -d); deb=$root/zex
    mkdir -p "$deb/DEBIAN" "$deb/usr/bin" "$deb/usr/share/applications" "$deb/usr/share/icons/hicolor/512x512/apps" "$deb/usr/share/doc/zex"
    install -m 755 "$out/zex" "$deb/usr/bin/zex"
    install -m 644 desktop/linux/zex.desktop "$deb/usr/share/applications/zex.desktop"
    install -m 644 assets/icon-linux-512.png "$deb/usr/share/icons/hicolor/512x512/apps/zex.png"
    install -m 644 LICENSE "$deb/usr/share/doc/zex/copyright"
    {
      echo "Package: zex"
      echo "Version: $VERSION"
      echo "Architecture: $arch"
      echo "Maintainer: Zex <29659769+mahdiarfrm@users.noreply.github.com>"
      echo "Depends: libgtk-3-0t64 | libgtk-3-0, libwebkit2gtk-4.1-0"
      echo "Section: devel"
      echo "Priority: optional"
      echo "Homepage: https://github.com/mahdiarfrm/zex"
      echo "Description: Desktop workbench for typed-decision models"
      echo " Write typed questions, run them on messages, and read every answer"
      echo " with its probability. Works with local and hosted decision models."
    } > "$deb/DEBIAN/control"
    dpkg-deb --build --root-owner-group "$deb" "dist/zex_${VERSION}_$arch.deb" >/dev/null
    rm -rf "$root"
    echo "built dist/zex_${VERSION}_$arch.deb"
  fi
}

case $what in
  mac) build_mac ;;
  windows) build_windows ;;
  linux) build_linux ;;
  all) build_mac; build_windows; build_linux ;;
  *) echo "usage: desktop/build.sh [mac|windows|linux|all]"; exit 2 ;;
esac
