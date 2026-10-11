#!/bin/sh
# Build the app icons from assets/icon.svg: PNGs, icon.icns (macOS) and icon.ico (Windows).
# Needs rsvg-convert (librsvg) and, for the .ico, ImageMagick. iconutil ships with macOS.
set -e
cd "$(dirname "$0")"
python3 make_icons.py
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# macOS: the tile keeps Apple's padding and shadow
set_dir="$tmp/icon.iconset"; mkdir "$set_dir"
for s in 16 32 128 256 512; do
  src=icon.svg; [ $s -le 32 ] && src=icon-small.svg
  rsvg-convert -w $s -h $s $src -o "$set_dir/icon_${s}x${s}.png"
  d=$((s * 2)); src=icon.svg; [ $d -le 32 ] && src=icon-small.svg
  rsvg-convert -w $d -h $d $src -o "$set_dir/icon_${s}x${s}@2x.png"
done
if command -v iconutil >/dev/null; then iconutil -c icns "$set_dir" -o icon.icns; fi
rsvg-convert -w 512 -h 512 icon.svg -o icon-512.png

# Windows: crop to the tile, so the icon fills its square like other Windows icons
sed 's/viewBox="0 0 1024 1024"/viewBox="92 92 840 840"/' icon.svg > "$tmp/win.svg"
sed 's/viewBox="0 0 1024 1024"/viewBox="92 92 840 840"/' icon-small.svg > "$tmp/win-small.svg"
for s in 16 24 32 48 64 128 256; do
  src="$tmp/win.svg"; [ $s -le 32 ] && src="$tmp/win-small.svg"
  rsvg-convert -w $s -h $s "$src" -o "$tmp/w$s.png"
done
if command -v magick >/dev/null; then
  magick "$tmp/w16.png" "$tmp/w24.png" "$tmp/w32.png" "$tmp/w48.png" "$tmp/w64.png" "$tmp/w128.png" "$tmp/w256.png" icon.ico
fi
cp "$tmp/w256.png" icon-win-256.png
ls -l icon.icns icon.ico icon-512.png 2>/dev/null
