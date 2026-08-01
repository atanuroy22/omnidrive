# Provider setup — the complete walkthrough

Everything you need to reconnect a provider from scratch, written so you do not
have to remember any of it.

**You only ever do this once per provider.** The credentials are stored
encrypted and travel to every other device through the **Move** tab, so a new
phone needs none of this.

---

## Quick reference

| Provider | Console work | Redirect URI to register |
|---|---|---|
| TeraBox (1 TB free) | none, just sign in | — |
| pCloud | none | — |
| WebDAV (Koofr, Yandex, Nextcloud, NAS) | none, just an app password | — |
| S3 (R2, B2, MinIO, Wasabi) | none, just access keys | — |
| Google Drive | ~5 min | `http://127.0.0.1:8787/oauth/callback` |
| OneDrive | ~3 min | `http://127.0.0.1:8787/oauth/callback` |
| Dropbox | ~2 min | `http://127.0.0.1:8787/oauth/callback` |

The redirect URI must match **byte for byte** — `127.0.0.1`, not `localhost`,
no trailing slash. If you run OmniDrive on a different port, register that port.

---

## Google Drive

The fiddliest of the three, mostly because Google treats file access as a
reviewable privilege. Read the scope section before you start — it decides
whether your connection lasts forever or dies every 7 days.

### 1. Create a project

1. Go to [console.cloud.google.com](https://console.cloud.google.com)
2. Project dropdown (top left) → **New project**
3. Name it anything (`OmniDrive`) → **Create**
4. Make sure the new project is selected in that dropdown before continuing

### 2. Enable the Drive API

1. Search bar → **Google Drive API** → open it
2. **Enable**

Without this, sign-in succeeds but every file listing fails.

### 3. Configure the consent screen

1. **APIs & Services → OAuth consent screen**
2. User type: **External** → **Create**
3. Fill in the required fields:
   - App name: `OmniDrive` *(this is what you see on the sign-in page — check
     the spelling, it is easy to typo)*
   - User support email: your address
   - Developer contact email: your address
4. **Save and continue** through the remaining steps

### 4. Choose your scope — read this

This is the decision that matters.

| | Only files OmniDrive creates | Full Drive access |
|---|---|---|
| Google scope | `drive.file` | `drive` |
| Google's classification | non-sensitive | **restricted** |
| Verification needed | no | yes, a security assessment |
| Can publish the app | yes, instantly | no |
| **Connection expires** | **never** | **every 7 days** |
| Sees files already in Drive | no | yes |
| Sees files OmniDrive uploads | yes | yes |

Google's own documentation:

> A project with an OAuth consent screen configured for an external user type
> and a publishing status of "Testing" is issued a refresh token expiring in
> **7 days**.

Because the full `drive` scope is restricted, a personal project can never
leave "Testing" without a security assessment — so choosing it means
reconnecting Google every week, forever.

**Recommended: "Only files OmniDrive creates".** For using OmniDrive to spread
uploads across several clouds, it does everything you need, and you can publish
the app so the connection never expires.

**If you picked the narrow scope**, publish it now — this is what stops the
7-day expiry:

- OAuth consent screen → **Publish app** → confirm

With only non-sensitive scopes there is no review; it publishes immediately.

**If you picked full access**, you must stay in Testing and add yourself:

- OAuth consent screen → **Audience** (or *Test users*) → **+ ADD USERS**
- Enter your own Google address → **Save**

Skipping this gives `Error 403: access_denied`.

### 5. Create the OAuth client

1. **APIs & Services → Credentials → + Create credentials → OAuth client ID**
2. Application type: **Desktop app** *(recommended — loopback redirects are
   accepted on any port with nothing to register)*
   - If you choose **Web application** instead, you must add
     `http://127.0.0.1:8787/oauth/callback` under **Authorised redirect URIs**,
     or sign-in fails with `Error 400: redirect_uri_mismatch`
3. **Create** → copy the **Client ID** and **Client secret**

### 6. Connect in OmniDrive

**Drives → Connect a drive → Google Drive → Enter client ID and secret**

Paste the values only — no quotes, no trailing comma. (OmniDrive strips those
anyway and rejects a secret pasted into the ID field, but it is easier to paste
cleanly.)

Then pick the access level matching what you configured in step 4.

### Google troubleshooting

| Symptom | Cause |
|---|---|
| `Error 400: redirect_uri_mismatch` | URI not registered, or a mismatch in port/scheme/trailing slash |
| `Error 403: access_denied` | Full scope chosen but you are not in **Test users** |
| `400 ... malformed` on the account chooser | Stale `accounts.google.com` cookies — retry in an Incognito window |
| Sign-in works, listing fails | Drive API not enabled |
| Stops working after a week | Full scope + Testing status. Switch to the narrow scope and publish |
| `disallowed_useragent` | Google blocks embedded WebViews. OmniDrive already opens your real browser, so this should not appear |

---

## OneDrive

1. [entra.microsoft.com](https://entra.microsoft.com) → **App registrations → New registration**
2. Name: `OmniDrive`
3. Supported account types: **Accounts in any organizational directory and
   personal Microsoft accounts** — the personal option matters for a normal
   @outlook.com or @hotmail.com account
4. Redirect URI: platform **Web**, value `http://127.0.0.1:8787/oauth/callback`
5. **Register**, then copy the **Application (client) ID** — a GUID
6. Optional: **Certificates & secrets → New client secret** → copy the
   **Value** (not the Secret ID). You can leave this blank if you registered a
   public client
7. **API permissions** should include `Files.ReadWrite.All`, `User.Read` and
   `offline_access` — OmniDrive requests these itself

Then: **Drives → Connect a drive → OneDrive → Enter client ID and secret**

---

## Dropbox

1. [dropbox.com/developers/apps](https://www.dropbox.com/developers/apps) →
   **Create app**
2. Choose **Scoped access** → **Full Dropbox** → name it
3. **Permissions** tab, tick all of:
   `account_info.read`, `files.metadata.read`, `files.metadata.write`,
   `files.content.read`, `files.content.write` → **Submit**
4. **Settings** tab → OAuth 2 → Redirect URIs → add
   `http://127.0.0.1:8787/oauth/callback` → **Add**
5. Copy the **App key** and **App secret**

Set the permissions *before* connecting. Dropbox bakes the granted scopes into
the token, so adding them afterwards requires reconnecting.

---

## The providers with no console at all

### TeraBox — 1 TB free

Sign up at [1024terabox.com](https://www.1024terabox.com/login), then in OmniDrive:
**Connect a drive → TeraBox → Sign in**. TeraBox's own sign-in page opens inside
the app, and the session it produces is the only thing kept.

**Use 1024terabox.com, not terabox.com.** TeraBox has migrated, and the original
address no longer resolves on a good many networks — India included — so a saved
bookmark or an old guide will just show "webpage not available". OmniDrive tries
`www.1024terabox.com` first and falls back through `1024tera.com`,
`terabox.app`, `terabox.com`, `4funbox.com` and `mirrobox.com`, keeping whichever
answers; the **Domain** field is only there to pin one by hand.

**There is no client ID because there is no OAuth.** TeraBox offers none to
third-party apps; its own clients authenticate with a session cookie, and so
does this one. That also means there is no refresh token: the connection lasts
as long as the cookie, which is months in practice. When it lapses, go to
**Drives → the drive → Sign in to TeraBox again** — nothing else is needed, and
the drive keeps its name and settings.

**The sign-in page loads as a desktop browser, deliberately.** TeraBox sends
every mobile browser to `/wap`, a page advertising its own Android app with no
way to sign in at all; the identical request from a desktop browser gets the real
login form. So the page will look like the desktop site on your phone — pinch to
zoom. That is the page working, not the wrong one.

**Sign in with an email and password.** A Google or Apple button may be refused
inside an embedded page — that is Google's policy on in-app browsers, not
something TeraBox or OmniDrive controls. If the account was created with one,
set a password in TeraBox first.

**On a desktop browser** there is no in-app page to open, so the cookie is
pasted by hand instead:

1. Sign in at [1024terabox.com](https://www.1024terabox.com/login) in your browser.
2. Open developer tools → Application → Cookies → `https://www.1024terabox.com`.
3. Copy the **value** of the `ndus` cookie.
4. In OmniDrive: **Connect a drive → TeraBox → Paste the ndus cookie**.

Pasting the whole `Cookie:` header works too — OmniDrive keeps the session and
discards the tracking cookies around it.

**Check the recycle bin on a new connection.** TeraBox deletes to a bin that
keeps counting against the 1 TB, and it is the usual reason a nearly empty
account reports itself full. **Drives → the drive → Recycle bin** lists it,
restores from it, and empties it.

### pCloud

Sign up at [pcloud.com](https://www.pcloud.com) (10 GB free), then in OmniDrive:
**Connect a drive → pCloud** → email and password.

**Region does not matter.** pCloud runs two independent datacentres and an
account exists in exactly one; OmniDrive tries both and keeps whichever
answers, so the field can be left alone.

If pCloud does ask for a verification code, that is genuine two-factor
authentication — turn it off under pCloud → Settings → Security, or use one of
the app-password providers below, whose credentials work alongside 2FA.

### WebDAV — Koofr, Yandex, Nextcloud, Box, NAS

Generate an **app password** in the provider's security settings. Your normal
login password will be rejected, and an app password keeps working alongside
2FA — which is exactly why this route is the most robust.

| Provider | URL | Where to generate the app password |
|---|---|---|
| Koofr (10 GB free) | `https://app.koofr.net/dav/Koofr` | Preferences → Password → Generate |
| Yandex Disk (10 GB free) | `https://webdav.yandex.com` | id.yandex.com → Security → App passwords → WebDAV |
| Nextcloud / ownCloud | `https://YOURHOST/remote.php/dav/files/USERNAME/` | Settings → Security → Create new app password |

Then: **Connect a drive → WebDAV** → URL, username, app password.

### S3 — Cloudflare R2, Backblaze B2, MinIO, Wasabi

Create an access key in the provider's dashboard, then **Connect a drive → S3
compatible** with endpoint, region, bucket, access key and secret.

API keys are unaffected by 2FA, so this route also stays working no matter what
you turn on for your account.

| Provider | Endpoint | Free tier |
|---|---|---|
| Backblaze B2 | `https://s3.us-west-004.backblazeb2.com` | 10 GB |
| Cloudflare R2 | `https://ACCOUNT_ID.r2.cloudflarestorage.com` | 10 GB |
| Wasabi | `https://s3.wasabisys.com` | trial |
| MinIO (self-hosted) | your own URL | — |

---

## Phone and SD card storage

No setup, but Android needs one permission before an app may browse the whole
filesystem.

1. **Drives → Connect a drive → Phone storage**
2. If prompted, tap **Grant** — Android opens its *All files access* screen
3. Turn OmniDrive on, then press back

Without it OmniDrive can still see its own folder, but not your Downloads,
Pictures or SD card. Android 11 and later require this special permission for
any general-purpose file manager; there is no way around it.

---

## Never do this twice

Once one device works, do **not** repeat any of the above. Use **Move**:

- **Pair over Wi-Fi** — one link to paste, both devices on the same network
- **Save into a drive** — the encrypted setup lives in one of your own clouds;
  a new phone connects one account and pulls the rest
- **Export a file** — a `.omnibundle` you keep wherever you like

All three carry the client IDs, tokens and settings, so the new device never
sees a console.

Related: [readme.MD](readme.MD) · [oauth.env.example](oauth.env.example) for
compiling credentials into the build so the app never asks at all.
