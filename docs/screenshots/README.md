# Install page screenshots

Sources for the images on `/show/{token}`. Full-resolution captures live here; the page
serves downscaled WebP from `internal/api/web/static/img/`, which is what ships in the
binary — this directory does not, since the Dockerfile copies `internal/` and nothing else.

| file | what it shows |
| --- | --- |
| `add-plus.png` | Hiddify home, arrow on the **+** button |
| `add-clipboard.png` | the add-profile sheet, arrow on **Clipboard** |
| `add-qr.png` | the add-profile sheet, arrow on **Scan QR** |
| `connect.png` | a profile loaded, arrow on the connect button |
| `connected.png` | connected state |

Captured on iOS. Android's Hiddify is the same Flutter app, so both device tabs use these;
if the two ever diverge, add `android-*.png` here and point that platform's `Screenshots`
list in [internal/api/clients.go](../../internal/api/clients.go) at them.

The PNGs here are already cropped: the top 126 px of status bar is gone, since a stranger's
carrier, battery and clock are noise the reader has to look past to find the arrow.

## Regenerating

A fresh capture needs the status bar taken off first (1125×2436 → 1125×2310):

```sh
magick shot.png -crop 1125x2310+0+126 +repage docs/screenshots/<name>.png
```

Then rebuild the WebP the page actually serves:

```sh
for f in docs/screenshots/*.png; do
  cwebp -quiet -q 78 -resize 540 0 "$f" \
    -o "internal/api/web/static/img/$(basename "${f%.png}").webp"
done
```

540 px wide renders crisply at the ~140 CSS px the page displays them at, and keeps the
whole set around 60 KB. Commit both the PNG and the WebP.

If the crop changes the aspect ratio, update `shotWidth`/`shotHeight` in `clients.go` to
match — the page uses them to reserve space before the images load, and
`TestScreenshotsMatchDeclaredSize` fails until they agree.

Names are load-bearing: `hiddifyShots` in `clients.go` lists them, and every locale file
needs a matching entry under `shots` for the alt text. A test fails if those three lists
disagree.
