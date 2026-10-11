#!/bin/sh
# Install Zex for the current user: the app in ~/.local/bin, plus its menu entry and icon.
# Zex needs WebKitGTK 4.1 (Debian and Ubuntu: sudo apt install libwebkit2gtk-4.1-0).
set -e
cd "$(dirname "$0")"
bin="$HOME/.local/bin"; apps="$HOME/.local/share/applications"; icons="$HOME/.local/share/icons/hicolor/512x512/apps"
mkdir -p "$bin" "$apps" "$icons"
install -m 755 zex "$bin/zex"
install -m 644 zex.png "$icons/zex.png"
sed "s|^Exec=.*|Exec=$bin/zex|" zex.desktop > "$apps/zex.desktop"
command -v update-desktop-database >/dev/null && update-desktop-database "$apps" || true
command -v gtk-update-icon-cache >/dev/null && gtk-update-icon-cache -q "$HOME/.local/share/icons/hicolor" 2>/dev/null || true
echo "Zex is installed. Open it from your app menu, or run: $bin/zex"
