# OmniDrive

All your cloud drives — Google Drive, OneDrive, Dropbox, pCloud, TeraBox, WebDAV,
S3 — in one mobile web app, served by a **single 6.7 MB binary** that runs on
your phone.

No Node, no Docker, no database, no server to rent. Start it in Termux, open
`127.0.0.1:8787` in your browser, and it behaves like a normal app.

```
$ omnidrive

  OmniDrive 0.1.0
  ─────────────────────────────────────────
  Open      http://127.0.0.1:8787
  Drives    4 connected
  Data      /data/data/com.termux/files/home/.omnidrive
  DNS       Android fix active (192.168.1.1:53, 1.1.1.1:53)

  Press Ctrl+C to stop.
```

---

## Screenshots

### Android — all connected drives
![All drives](docs/screenshots/all_drives.jpeg)

### Android — combined file view across providers
![Combined view](docs/screenshots/combined.jpeg)

### Android — settings screen
![Settings](docs/screenshots/setting.jpeg)

The same UI works on **Windows**, **Linux**, and **macOS** — it's a browser app served from a single binary. No separate desktop client needed.

---

## Why this is a rewrite, not a port

This project takes its concept and vocabulary from
[dimartarmizi/OmniCloud](https://github.com/dimartarmizi/OmniCloud) — a Vue +
Express + SQLite drive aggregator — and rebuilds it for a phone.

Running the original on Android would mean shipping Node (~150 MB in Termux)
plus a native SQLite module that routinely fails to compile on arm64. That is
the opposite of lightweight. So the backend became one static Go binary with the
UI embedded inside it:

| | OmniCloud (upstream) | OmniDrive (this) |
|---|---|---|
| Runtime | Node + npm tree | none — one static ELF |
| Install size | ~150–300 MB | **6.7 MB** |
| Database | SQLite (native module) | encrypted file, no CGO |
| Frontend | Vue 3 + Vite build | embedded, no build step |
| Dependencies | dozens | **zero** outside the Go standard library |
| Setup on a 2nd device | re-authorise every account | one command |

`go.mod` has no `require` block. That is deliberate: every dependency is a thing
that can break a cross-compile for a phone.

---

## Install on Android — the APK

Build it once, then install it like any other app. No Termux, no terminal, no
file copying afterwards.

```bash
ABI=arm64-v8a bash android/build-apk.sh      # 2.7 MB, every 64-bit phone
bash android/build-apk.sh                    # 8.5 MB, all three ABIs
```

Copy `build/omnidrive-0.1.0.apk` to your phone and tap it (allow "install from
unknown sources" when asked), or:

```bash
adb install -r build/omnidrive-0.1.0.apk
```

Open **OmniDrive** from your app drawer. It shows your files immediately; the
**Drives** tab is where you connect accounts, and **Move** is where you copy the
whole setup to another phone.

Requirements to build: **Go 1.24+**, a **JDK 17+**, and an **Android SDK** with
any recent build-tools and the `android-36` platform. No Gradle, no Android
Studio, no NDK — `android/build-apk.sh` drives `aapt2`, `javac`, `d8`,
`zipalign` and `apksigner` directly. On Windows run it from Git Bash.

The first build generates `android/omnidrive-release.keystore`. **Keep that
file** — Android refuses to upgrade an app signed with a different key.

### What the app is doing

The APK is a WebView plus a foreground service; the service runs the same Go
server as a child process. Four Android-specific details make that work on
current devices:

- **The binary ships as `lib/<abi>/libomnidrive.so`.** Android only extracts and
  grants execute permission to files matching `lib*.so`, and since Android 10
  the native library directory is the *only* place an app may execute from.
  Copying the binary to `getFilesDir()` and running it there fails with
  "Permission denied" on every modern device.
- **Sign-in opens in your real browser, not the WebView.** Google and Microsoft
  both reject OAuth from embedded WebViews (`disallowed_useragent`). The app
  hands non-loopback URLs to the browser, which then reaches the callback on
  this device's own loopback address. Returning to the app refreshes the
  drive list automatically.
- **DNS.** Android has no `/etc/resolv.conf`, so Go's pure-Go resolver falls
  back to `127.0.0.1:53` and every lookup fails — the usual reason a Go binary
  looks offline on a phone. OmniDrive detects this and builds a resolver from
  `getprop net.dns*`, with public fallbacks.
  ([internal/androidnet](internal/androidnet/androidnet.go))
- **Staying alive.** A `dataSync` foreground service plus a partial wake lock
  keeps transfers running with the screen off. The notification has a Stop
  button.

`minSdk 29` (Android 10) so downloads can use MediaStore and need no storage
permission at all; `targetSdk 36` (Android 16), including the predictive-back
callback that Android 16 requires. The Go binary uses 64 KB segment alignment,
so it is safe on the 16 KB-page devices Android 15+ ships.

### Alternative: Termux

If you would rather run it from a terminal — or want it on a device where you
cannot sideload — the same binary works standalone. Needs
[Termux **from F-Droid**](https://f-droid.org/packages/com.termux/); the Play
Store build is abandoned and too old.

```bash
./build.sh android                                    # on your computer
# copy build/omnidrive-android-arm64 to the phone, then in Termux:
termux-setup-storage
bash install-termux.sh ~/storage/downloads/omnidrive-android-arm64
omnidrive-start
```

Then open http://127.0.0.1:8787. Or build on the phone itself:
`pkg install golang git && go build -ldflags "-s -w" ./cmd/omnidrive`

---

## Moving your setup to another device

This is the part that usually hurts: eight accounts connected, new phone, and
now eight sign-in flows on a 6-inch screen.

Everything — accounts, OAuth refresh tokens, your registered app credentials,
starred files, settings — packs into one encrypted bundle. Pick whichever route
suits you; all three produce the same result.

### 1. Pair over Wi-Fi (fastest)

On the device that already works:

```bash
omnidrive pair
```

```
  Pairing 4 drive(s). Paste this one link on the other device —
  into the Move tab, or on a command line:

      omnidrive join "http://192.168.1.42:41235/pair/a3f9c1?c=K3M9P2QX"

  Single use · expires 11:47PM
```

One link, nothing to keep together. Paste it on the new device and it is done —
both devices' web UIs have the same thing under the **Move** tab.

### 2. Keep a copy inside a drive you already use

Store the encrypted bundle in one of your own clouds, as
`OmniDrive/omnidrive-config.omnibundle`:

```bash
omnidrive accounts                             # find the account id
omnidrive push a1b2c3d4 -passphrase "your passphrase"
```

On a new phone, connect **any one** account, then:

```bash
omnidrive pull a1b2c3d4 -passphrase "your passphrase"
```

Everything else arrives. This is the best answer to "I reinstall often" — there
is nothing to carry around and nothing to lose.

### 3. Export a file

```bash
omnidrive export backup.omnibundle -passphrase "your passphrase"
omnidrive import backup.omnibundle -passphrase "your passphrase"     # elsewhere
```

### How merging works

Accounts merge by ID, so importing the same bundle twice changes nothing and a
device that connected something new locally does not lose it. Pass `-replace`
to wipe local accounts first instead.

---

## Connecting drives

| Provider | Auth | What you need |
|---|---|---|
| Google Drive | OAuth | client ID + secret from [Google Cloud Console](https://console.cloud.google.com/apis/credentials) |
| OneDrive | OAuth | app registration in [Microsoft Entra](https://entra.microsoft.com) |
| Dropbox | OAuth | scoped app from the [Dropbox console](https://www.dropbox.com/developers/apps) |
| pCloud | email + password | swapped for a token on first use; the password is then discarded |
| TeraBox | sign-in page | **1 TB free.** Sign in on TeraBox's own page inside the app; on desktop, paste the `ndus` cookie |
| WebDAV | URL + user + password | Nextcloud, ownCloud, Yandex Disk, Koofr, Box, most NAS boxes |
| S3 compatible | access keys | AWS, Cloudflare R2, Backblaze B2, MinIO, Wasabi, Storj, Hetzner |

### Start with no setup at all

**TeraBox, pCloud, WebDAV and S3 need no developer console.** Open the app, tap
Connect a drive, sign in, done. TeraBox alone is 1 TB free; WebDAV covers Yandex
Disk (10 GB free), Nextcloud, ownCloud, Koofr, Box and most NAS boxes.

### TeraBox: why it has no client ID either

TeraBox offers no OAuth to third-party apps at all — its own clients sign in
with an ordinary session cookie, and so does this one. Tap **Connect a drive →
TeraBox → Sign in** and TeraBox's real sign-in page opens inside the app; the
only thing kept is the session it produces. On a desktop browser there is no
in-app page to open, so sign in at 1024terabox.com and paste the value of the `ndus`
cookie instead.

There is no refresh token in that design, so a connection lasts as long as the
cookie does — months in practice. When one lapses the app says so, and **Drives
→ the drive → Sign in to TeraBox again** is the whole repair. Everything else
works exactly as the other drives do: browse, upload, download, stream, copy,
move, rename, delete, share, and the recycle bin.

### OAuth: why it asks for a client ID

Google, Microsoft and Dropbox will not issue tokens to an unregistered app.
There is no API that trades an email and password for Drive access — that was
removed deliberately so third-party apps never handle your password. Every app
that appears to "just log you in" is shipping its own client ID inside the
binary; the registration still happened, just not by you.

So it has to happen once. You have two ways to make it *only* once:

**Bake it into the build** — then no device ever asks:

```bash
cp oauth.env.example oauth.env      # fill in your client IDs
ABI=arm64-v8a bash android/build-apk.sh
```

The build prints `preconfigured: Google Dropbox` to confirm. `oauth.env` is
gitignored, and every provider in it is optional.

**Or enter it once in the app** — it is stored encrypted and travels to every
device you later pair with, so the second phone never asks either.

Either way, register this **exact** redirect URI with the provider:

```
http://127.0.0.1:8787/oauth/callback
```

A client ID is not a password: it identifies the application, not you, and
anyone with the APK can read it. What actually protects the exchange is PKCE,
which this app always uses. A client secret is optional for providers that
support public clients.

`oauth.env.example` has click-by-click instructions for each console.

Scopes are requested for you, including the ones providers require for a
long-lived refresh token (`access_type=offline` on Google,
`token_access_type=offline` on Dropbox, `offline_access` on Microsoft). If a
provider returns no refresh token the connection is rejected outright rather
than dying silently a few hours later.

---

## Where uploads go

Upload from inside a folder and the file goes there. Upload from the top level
and a strategy picks the drive — the same five upstream OmniCloud offers:

| Strategy | Behaviour |
|---|---|
| `most_free` *(default)* | drive with the most free space |
| `least_used` | drive with the least data stored |
| `round_robin` | rotate evenly, position survives restarts |
| `weighted_round_robin` | rotate proportionally to each drive's weight |
| `manual` | your explicit priority order |

Drives with unknown capacity (S3 buckets, unlimited plans) sort as spacious
rather than full. If nothing looks like it fits, a drive is still chosen and the
provider's own error is surfaced — cached quota is often stale.

---

## Deleting

Delete means opposite things on the two halves of the app, on purpose.

| Where | What happens |
|---|---|
| Google Drive, OneDrive, pCloud, TeraBox | **Gone for good.** The space comes back at once. |
| Dropbox | Recoverable for 30 days — no API exists to purge it. |
| S3, WebDAV | Gone, as those protocols have always behaved. |
| Phone, SD card | **Into OmniDrive's recycle bin.** Restorable. |

Cloud recycle bins are a trap: a "deleted" file keeps eating the quota you pay
for, and the only way to reclaim it is to open the provider's own website — the
thing this app exists to avoid. So cloud deletes skip the bin entirely.

Phone storage is the reverse. Android has no recycle bin of its own, so a
mis-tap would destroy the file outright. Deletes there move into a hidden
`.omnidrive-trash` folder **on the same volume** (a rename, not a copy, so
deleting a 4 GB video is instant), with a sidecar recording where it came from.

Open **Drive Settings → the drive → Recycle bin** to restore an item to its
original folder, delete one forever, or empty the lot. Binned files still occupy
the device until you do — that is the trade for being able to undo.

Dropbox and OneDrive publish no API to purge a *personal* account's bin. That is
their limitation, not an omission here; those have to be emptied on
dropbox.com and onedrive.live.com.

**TeraBox's bin opens in the app.** Its API is one of the few that can both list
and restore, so **Drives → the drive → Recycle bin** shows whatever was deleted
before this app or on the TeraBox website, and puts it back. Worth a visit on a fresh
connection: a bin nobody has emptied is often the reason a 1 TB drive reports
itself full.

---

## Sharing a file with someone else

**File → ⋯ → Copy link** produces a public URL. Send it to anyone, on any
network — they download without signing in or installing anything.

The link is minted by the provider, not by OmniDrive, and that is the only
design that works. This app listens on the phone's loopback address behind
carrier NAT, so a link it served itself would be unreachable from anywhere else
and would die the moment the screen went off. A Google or Dropbox link is served
by their CDN and keeps working with the phone switched off.

| Drive | Link | Length |
|---|---|---|
| OneDrive | permanent, revocable | ~45 chars |
| Google Drive | permanent, revocable | ~65 |
| TeraBox | permanent, revocable | ~40 |
| pCloud | permanent, revocable | ~75 |
| Dropbox | permanent, revocable | ~110 |
| S3, WebDAV, phone storage | not offered — no public-link API | — |

Length is the provider's choice, not ours. **TeraBox and OneDrive give the
shortest links.**

pCloud advertises a short `u.pc.cd/XXX` form and OmniDrive deliberately does not
use it: that host has no TLS listener at all, so the link is cleartext `http://`,
and its entire body is a JavaScript redirect to the long URL. Phones warn about
it, Android blocks cleartext by default, and any client that does not run the
script gets a blank page instead of the file.

Two consequences worth knowing:

- **Anyone holding the link can download the file.** There is no password and no
  expiry. That is what makes it work from any network; it is also the risk.
- **Stop sharing** revokes it, on every provider above.

A file on phone storage has no link. Copy it to a cloud drive first — the
option only appears where it will actually work.

### Folders

Folders can be shared too, on all four providers. The recipient gets a page
listing the contents and picks what to download — useful for handing over a
holiday album without zipping it first.

### Several files at once

Select them, tap **Share → Copy links**, and you get one link per line, ready to
paste into a chat. Nothing leaves the phone: the other person fetches each file
from the cloud.

The same button also offers **Send the files**, which is the opposite trade —
that pushes every byte off this phone through Bluetooth, WhatsApp or whatever
you pick. Use it when the other person is standing next to you and the files are
small; use links for anything else.

### Keeping track of what is public

**Drive Settings → Shared links** lists every link this device has handed out,
with the drive and date, and revokes them one at a time or all at once.

OmniDrive keeps this register itself. None of the four providers offers a cheap
"list everything that is public" call that works across all of them, and a link
you cannot find is a file you have quietly published and forgotten. The
consequence is that links created on the provider's own website do not appear
here — only the ones made in this app.

---

## Security

- The state file is **AES-256-GCM** encrypted with a random per-device key.
- Portable bundles are **PBKDF2-SHA256 (210 000 iterations) + AES-256-GCM**
  under your passphrase. Sealed before leaving the device, so cloud sync hands
  your provider ciphertext it cannot read.
- Encryption is bound to its context, so a bundle cannot be opened as device
  state or vice versa, even with the right passphrase.
- Every write is atomic (temp file + rename) — an OOM kill mid-write cannot
  truncate your accounts away.
- The server binds to **127.0.0.1** only. Bind wider with `-addr 0.0.0.0` and it
  generates a required access token and prints it in the URL.
- Pairing runs on its own short-lived listener, exposing exactly one endpoint:
  single use, five wrong codes destroys it, ten-minute expiry, and the code is
  also the decryption key.
- Optional: `-device-passphrase` protects local state with a password too.
  (Applies when the data directory is first created.)

Everything uses the Go standard library — `crypto/pbkdf2`, `crypto/aes`,
`crypto/cipher`. No third-party crypto.

---

## Command reference

```
omnidrive [flags]                 start the server (default)
omnidrive accounts                list connected drives
omnidrive pair                    share this setup with another device
omnidrive join <pairing-link>     receive a setup from another device
omnidrive export <file>           write an encrypted backup bundle
omnidrive import <file>           read an encrypted backup bundle
omnidrive push <account-id>       save the setup into a connected drive
omnidrive pull <account-id>       restore the setup from a connected drive
omnidrive version

  -data string               data directory (default ~/.omnidrive)
  -addr string               bind address (default 127.0.0.1)
  -port int                  port (default 8787)
  -passphrase string         bundle passphrase        ($OMNIDRIVE_PASSPHRASE)
  -device-passphrase string  local state passphrase   ($OMNIDRIVE_DEVICE_PASSPHRASE)
  -replace                   on import, discard local accounts instead of merging
```

Flags and the subcommand may appear in any order.

---

## Troubleshooting

**"server misbehaving" / nothing loads, but the phone has internet.**
The Android DNS fix did not engage. Check `curl 127.0.0.1:8787/api/health` —
`dnsPatched` should be `true`. Also shown under **More → About**.

**The APK will not install: "App not installed".**
Usually an older copy signed with a different key. Uninstall the old one first.
On Android 13+ you may also need to allow your file manager to install unknown
apps (Settings → Apps → Special access → Install unknown apps).

**The app opens on the startup screen and stays there.**
Tap **Log**. The server's own output is there and names the cause — most often
port 8787 already being in use by another app.

**Sign-in bounces me to the browser.**
That is deliberate and required: Google and Microsoft reject OAuth inside
embedded WebViews. Finish signing in there, then switch back to OmniDrive; it
refreshes on its own.

**The server dies when I switch apps.**
Android 12+ kills background processes. Install `termux-api` (plus the
Termux:API app), use `omnidrive-start`, pull down the Termux notification and
tap **Acquire wakelock**, and exclude Termux from battery optimisation. On
heavy-handed OEM builds (Xiaomi, Samsung, Oppo) also lock Termux in the recents
screen.

**`Permission denied` when starting.**
The binary is on `/sdcard`, which is `noexec`. Reinstall with
`install-termux.sh`, which copies into `$PREFIX/bin`.

**Google: `Error 400: redirect_uri_mismatch`.**
Your OAuth client has no matching redirect URI registered. Easiest fix: create
the client as **Desktop app** instead of Web application — Google then accepts
loopback redirects on any port with nothing to register.

To keep an existing Web application client, add this under *Authorised redirect
URIs*, byte for byte, no trailing slash:

```
http://127.0.0.1:8787/oauth/callback
```

The connect screen shows the exact string to copy. If you run on a non-default
port, register that port instead.

**Uploads fail on large files.**
Google Drive and OneDrive use resumable/chunked sessions and stream without
buffering. Dropbox switches to a session above 100 MB. S3 is a single PUT, so
very large objects there need a stable connection.

**I forgot a bundle passphrase.**
It is unrecoverable by design. Export a new bundle from a device that still
works.

---

## Layout

```
cmd/omnidrive/          CLI entrypoint, flags, banner
internal/androidnet/    Android DNS resolver fix
internal/vault/         AES-256-GCM + PBKDF2 sealing
internal/store/         encrypted state, atomic writes
internal/provider/      driver interface + six backends + OAuth/PKCE
internal/alloc/         upload allocation strategies
internal/portable/      bundles, LAN pairing, cloud config sync
internal/server/        HTTP API, SSE progress, embedded UI serving
internal/web/dist/      the UI itself (HTML/CSS/JS, no build step)
android/                the APK: WebView activity + foreground service
android/build-apk.sh    Gradle-free APK build
scripts/                Termux installer
```

```bash
go test ./...     # unit + end-to-end tests against an in-process WebDAV server
```

---

## Not implemented

- **MEGA** — its client-side crypto is a substantial project on its own and
  would mean the first third-party dependency. Upstream OmniCloud supports it.
- **Cross-provider move/copy** — you can upload and download, but not move a
  file from Drive to Dropbox in one action.
- **Full-text search and a recents index** — listing is live per folder; only
  starred files are tracked across drives.
- **Multi-user / hosted mode** — this is deliberately single-user and local.
