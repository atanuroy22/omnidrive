// Package provider defines the storage-backend abstraction. Every cloud we
// support is reduced to the same small interface so the rest of the program
// never branches on which vendor a file came from.
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Kind identifies a backend implementation.
type Kind string

const (
	KindGoogleDrive Kind = "gdrive"
	KindOneDrive    Kind = "onedrive"
	KindDropbox     Kind = "dropbox"
	KindPCloud      Kind = "pcloud"
	KindTeraBox     Kind = "terabox"
	KindWebDAV      Kind = "webdav"
	KindS3          Kind = "s3"
	KindLocal       Kind = "local"
)

// IsLocal reports whether a backend lives on this device rather than a network.
func (k Kind) IsLocal() bool { return k == KindLocal }

// Descriptor describes a backend for the UI's "add account" screen.
type Descriptor struct {
	Kind     Kind          `json:"kind"`
	Label    string        `json:"label"`
	Auth     string        `json:"auth"` // "oauth" | "password" | "keys" | "url" | "path"
	Fields   []Field       `json:"fields,omitempty"`
	Notes    string        `json:"notes,omitempty"`
	Presets  []Preset      `json:"presets,omitempty"`
	OAuthDoc string        `json:"oauthDoc,omitempty"`
	Scopes   []ScopeChoice `json:"scopes,omitempty"`

	// Effort tells the user up front what they are in for: "none" needs no
	// account setup at all, "console" means a visit to a developer console.
	Effort string `json:"effort"`
	// Free summarises the free allowance, where there is a well-known one.
	Free string `json:"free,omitempty"`
	// Setup is the in-app walkthrough, so nobody has to find external docs.
	Setup []SetupStep `json:"setup,omitempty"`
}

// SetupStep is one instruction in a provider's connect guide.
//
// Copy carries a value worth offering a copy button for. The literal
// "{{redirect}}" is substituted by the UI with this device's actual OAuth
// callback URL, which depends on the port in use and so cannot be baked in.
type SetupStep struct {
	Text string `json:"text"`
	Link string `json:"link,omitempty"`
	Copy string `json:"copy,omitempty"`
	Warn string `json:"warn,omitempty"`
}

// Field is one input on the connect form.
type Field struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Type     string `json:"type"` // text | password | url
	Required bool   `json:"required"`
	Hint     string `json:"hint,omitempty"`
}

// Preset pre-fills a field set (e.g. Yandex Disk over WebDAV).
type Preset struct {
	Label  string            `json:"label"`
	Values map[string]string `json:"values"`
}

// Descriptors is the catalogue shown in the UI, in display order.
func Descriptors() []Descriptor {
	out := descriptors()
	for i := range out {
		out[i].Scopes = ScopeChoices(out[i].Kind)
	}
	return out
}

func descriptors() []Descriptor {
	return []Descriptor{
		{
			Kind: KindLocal, Label: "Phone storage", Auth: "path",
			Effort: "none",
			Setup: []SetupStep{
				{Text: "Nothing to sign up for — this is the device you are holding."},
				{Text: "Tap Add, then allow \"All files access\" if Android asks. " +
					"Without it only OmniDrive's own folder is visible."},
				{Text: "Your internal storage is added automatically the first time it becomes readable."},
			},
			Notes: "Browse this device and move files straight into a cloud drive. " +
				"Android 11+ needs \"All files access\" before anything outside " +
				"OmniDrive's own folder is visible.",
			Fields: []Field{
				{Key: "root", Label: "Folder", Type: "text", Required: true,
					Hint: "/storage/emulated/0"},
			},
		},
		{
			Kind: KindGoogleDrive, Label: "Google Drive", Auth: "oauth",
			OAuthDoc: "https://console.cloud.google.com/apis/credentials",
			Effort:   "console", Free: "15 GB",
			Setup: []SetupStep{
				{Text: "Open the Google Cloud console and create a project (any name).",
					Link: "https://console.cloud.google.com/projectcreate"},
				{Text: "Enable the Google Drive API for that project.",
					Link: "https://console.cloud.google.com/apis/library/drive.googleapis.com"},
				{Text: "Fill in the OAuth consent screen: External, then your own email in the required fields.",
					Link: "https://console.cloud.google.com/apis/credentials/consent"},
				{Text: "Choose the access level below. \"Only files OmniDrive creates\" needs no Google review " +
					"and never expires; full Drive access needs you added under Test users and Google " +
					"expires it every 7 days.",
					Warn: "This is the choice that decides whether your connection keeps working."},
				{Text: "Create credentials -> OAuth client ID -> application type Desktop app.",
					Link: "https://console.cloud.google.com/apis/credentials"},
				{Text: "If you pick Web application instead, add this exact redirect URI or sign-in fails " +
					"with redirect_uri_mismatch:", Copy: "{{redirect}}"},
				{Text: "Copy the Client ID and Client secret, then tap Connect below."},
			},
			// Desktop app, not Web application: Google accepts loopback
			// redirects on any port for installed clients without registering
			// them, which removes the redirect_uri_mismatch trap entirely.
			Notes: "Create an OAuth client of type \"Desktop app\" — loopback redirects then need no registration. " +
				"If you pick \"Web application\" instead, you must add the exact redirect URI shown here, " +
				"or sign-in fails with redirect_uri_mismatch.",
		},
		{
			Kind: KindOneDrive, Label: "OneDrive", Auth: "oauth",
			OAuthDoc: "https://entra.microsoft.com/#view/Microsoft_AAD_RegisteredApps",
			Effort:   "console", Free: "5 GB",
			Setup: []SetupStep{
				{Text: "Open Microsoft Entra and choose App registrations -> New registration.",
					Link: "https://entra.microsoft.com/#view/Microsoft_AAD_RegisteredApps"},
				{Text: "For account types pick the option that includes personal Microsoft accounts, " +
					"or an @outlook.com address will not work.",
					Warn: "Easy to miss, and it cannot be changed without re-registering."},
				{Text: "Add a Redirect URI, platform \"Web\", with exactly this value:", Copy: "{{redirect}}"},
				{Text: "Register, then copy the Application (client) ID — it is a GUID."},
				{Text: "Optionally add a client secret under Certificates & secrets, and copy its Value " +
					"(not the Secret ID). Leave it blank for a public client."},
			},
			Notes: "Register an app, add the redirect URI as a \"Web\" platform, and allow personal Microsoft accounts.",
		},
		{
			Kind: KindDropbox, Label: "Dropbox", Auth: "oauth",
			OAuthDoc: "https://www.dropbox.com/developers/apps",
			Effort:   "console", Free: "2 GB",
			Setup: []SetupStep{
				{Text: "Open the Dropbox App Console and choose Create app.",
					Link: "https://www.dropbox.com/developers/apps/create"},
				{Text: "Pick Scoped access, then Full Dropbox, and give it a name."},
				{Text: "On the Permissions tab tick account_info.read, files.metadata.read, " +
					"files.metadata.write, files.content.read and files.content.write, then Submit.",
					Warn: "Do this before connecting: Dropbox bakes the permissions into the token, " +
						"so adding them later means reconnecting."},
				{Text: "On the Settings tab add this OAuth 2 redirect URI:", Copy: "{{redirect}}"},
				{Text: "Copy the App key and App secret, then tap Connect below."},
			},
			Notes: "Scoped app with files.content.read, files.content.write, files.metadata.read and account_info.read.",
		},
		{
			Kind: KindPCloud, Label: "pCloud", Auth: "password",
			Effort: "none", Free: "10 GB",
			Setup: []SetupStep{
				{Text: "Create a free pCloud account if you do not have one.",
					Link: "https://www.pcloud.com"},
				{Text: "Enter that email and password below. There is no developer console " +
					"and nothing to register."},
				{Text: "Leave Region blank — pCloud runs two datacentres and OmniDrive tries both."},
				{Text: "If pCloud asks for a verification code, two-factor authentication is on. " +
					"Turn it off, or use Koofr or Backblaze instead, whose credentials work alongside 2FA.",
					Warn: "pCloud's password API has no way to submit a 2FA code."},
			},
			Notes: "10 GB free and nothing to register: your normal login works directly.",
			Fields: []Field{
				{Key: "username", Label: "Email", Type: "text", Required: true},
				{Key: "password", Label: "Password", Type: "password", Required: true},
				{Key: "region", Label: "Region", Type: "text", Hint: "leave blank — both regions are tried automatically"},
			},
		},
		{
			// TeraBox is the largest free drive here by a wide margin, and the
			// only one with no OAuth at all: its own apps sign in with a session
			// cookie, so that is what this connects with. "webview" tells the UI
			// to offer an in-app sign-in page rather than a credentials form,
			// with the fields below as the manual fallback on desktop.
			Kind: KindTeraBox, Label: "TeraBox", Auth: "webview",
			Effort: "easy", Free: "1 TB",
			Setup: []SetupStep{
				{Text: "Create a free TeraBox account if you do not have one — it comes with 1 TB. " +
					"TeraBox has moved to 1024terabox.com; the old terabox.com address no longer " +
					"resolves on many networks, so use this link rather than one you have saved.",
					Link: teraboxSignInURL},
				{Text: "Tap Connect and sign in on the TeraBox page that opens. OmniDrive keeps " +
					"only the session cookie that sign-in produces."},
				{Text: "Use an email and password on that page. A Google or Apple button may be " +
					"refused inside an app — that is Google's rule, not TeraBox's. If your account " +
					"was created with one, set a password in TeraBox first.",
					Warn: "Sign in with email and password if the social buttons do not work."},
				{Text: "On a desktop browser there is no in-app page to open. Sign in at " +
					"1024terabox.com, then copy the value of the \"ndus\" cookie from your browser's " +
					"developer tools (Application → Cookies) and paste it below.",
					Link: teraboxSignInURL},
				{Text: "The connection lasts as long as that cookie does. If TeraBox ever says the " +
					"sign-in expired, connect again — nothing else is needed.",
					Warn: "There is no refresh token, because TeraBox offers no OAuth to third-party apps."},
			},
			Notes: "1 TB free. Sign in with the in-app TeraBox page; on desktop paste the ndus cookie instead. " +
				"Deletes go to TeraBox's recycle bin, which keeps counting against the 1 TB until it is emptied.",
			Fields: []Field{
				{Key: "cookie", Label: "ndus cookie", Type: "password", Required: true,
					Hint: "paste the ndus value, or the whole cookie header"},
				{Key: "domain", Label: "Domain", Type: "text",
					Hint: "leave blank — 1024terabox.com and its mirrors are tried automatically"},
			},
		},
		{
			Kind: KindWebDAV, Label: "WebDAV", Auth: "url",
			Effort: "easy", Free: "Koofr and Yandex: 10 GB",
			Setup: []SetupStep{
				{Text: "Pick a provider. Koofr and Yandex Disk both give 10 GB free; Nextcloud, " +
					"ownCloud, Box and most NAS boxes also speak WebDAV.",
					Link: "https://app.koofr.net/signup"},
				{Text: "Generate an app password in that provider's security settings. " +
					"Koofr: Preferences -> Password -> Generate. Yandex: id.yandex.com -> Security -> App passwords.",
					Warn: "Your normal login password will be rejected. An app password also keeps " +
						"working when two-factor authentication is on — which is why this route is the most robust."},
				{Text: "Enter the server URL, your username and that app password below. " +
					"Koofr and Yandex URLs are offered as presets."},
			},
			Notes: "Nextcloud, ownCloud, Yandex Disk, Koofr, Box, most NAS boxes. " +
				"Most providers require an app password generated in their security " +
				"settings — your normal login password will be rejected.",
			Fields: []Field{
				{Key: "url", Label: "Server URL", Type: "url", Required: true, Hint: "https://cloud.example.com/remote.php/dav/files/USER/"},
				{Key: "username", Label: "Username", Type: "text", Required: true},
				{Key: "password", Label: "App password", Type: "password", Required: true, Hint: "not your login password on Yandex or Koofr"},
			},
			Presets: []Preset{
				{Label: "Yandex Disk", Values: map[string]string{"url": "https://webdav.yandex.com"}},
				{Label: "Koofr", Values: map[string]string{"url": "https://app.koofr.net/dav/Koofr"}},
			},
		},
		{
			Kind: KindS3, Label: "S3 compatible", Auth: "keys",
			Effort: "easy", Free: "Backblaze B2 and Cloudflare R2: 10 GB",
			Setup: []SetupStep{
				{Text: "Create a bucket with any S3-compatible provider — Backblaze B2, " +
					"Cloudflare R2, Wasabi, MinIO or AWS.",
					Link: "https://www.backblaze.com/sign-up/cloud-storage"},
				{Text: "Create an access key with read and write permission on that bucket."},
				{Text: "Copy the endpoint URL, region, bucket name, access key and secret below."},
				{Text: "API keys are unaffected by two-factor authentication, so this route keeps " +
					"working whatever you turn on for the account."},
			},
			Notes: "AWS S3, Cloudflare R2, Backblaze B2, MinIO, Wasabi, Storj, Hetzner.",
			Fields: []Field{
				{Key: "endpoint", Label: "Endpoint", Type: "url", Required: true, Hint: "https://s3.amazonaws.com"},
				{Key: "region", Label: "Region", Type: "text", Hint: "us-east-1"},
				{Key: "bucket", Label: "Bucket", Type: "text", Required: true},
				{Key: "accessKey", Label: "Access key ID", Type: "text", Required: true},
				{Key: "secretKey", Label: "Secret access key", Type: "password", Required: true},
				{Key: "prefix", Label: "Path prefix", Type: "text", Hint: "optional, e.g. omnidrive/"},
			},
		},
	}
}

// File is the normalised entry every driver returns. ID is opaque and
// provider-specific; the UI only ever echoes it back.
type File struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	IsDir    bool      `json:"isDir"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	MIME     string    `json:"mime,omitempty"`

	// Filled in by the aggregator, not by drivers.
	AccountID    string `json:"accountId,omitempty"`
	AccountLabel string `json:"accountLabel,omitempty"`
	Starred      bool   `json:"starred,omitempty"`
	Path         string `json:"path,omitempty"`
}

// Quota reports storage usage. Total <= 0 means "unknown / unlimited".
type Quota struct {
	Used  int64 `json:"used"`
	Total int64 `json:"total"`
}

// Progress is called during transfers with the cumulative byte count.
type Progress func(sent int64)

// Driver is the contract each backend implements.
type Driver interface {
	Kind() Kind

	// List returns the direct children of folderID. The empty string means the
	// account root.
	List(ctx context.Context, folderID string) ([]File, error)

	// Stat resolves a single entry by ID.
	Stat(ctx context.Context, id string) (File, error)

	// Download opens a read stream. The caller closes it.
	Download(ctx context.Context, id string) (io.ReadCloser, int64, error)

	// Upload writes size bytes from r into parentID under name. size may be -1
	// when unknown, which some drivers reject.
	Upload(ctx context.Context, parentID, name string, size int64, r io.Reader, p Progress) (File, error)

	Mkdir(ctx context.Context, parentID, name string) (File, error)
	Rename(ctx context.Context, id, newName string) error
	Delete(ctx context.Context, id string) error
	Quota(ctx context.Context) (Quota, error)
}

// RangeDownloader is implemented by backends that can serve part of a file.
// This is what makes seeking in a video work and what lets a two-hour film
// start playing immediately instead of downloading in full first.
//
// start is the first byte wanted; end is the last, inclusive, or -1 for "to
// the end of the file".
type RangeDownloader interface {
	DownloadRange(ctx context.Context, id string, start, end int64) (io.ReadCloser, error)
}

// rangeHeader formats an HTTP Range value for the byte window.
func rangeHeader(start, end int64) string {
	if end < 0 {
		return fmt.Sprintf("bytes=%d-", start)
	}
	return fmt.Sprintf("bytes=%d-%d", start, end)
}

// PermanentDeleter is implemented by backends whose ordinary Delete only moves
// an item to a recycle bin, and which offer a way to destroy it outright.
//
// Deleting to a trash that still counts against quota is not what a user means
// by "delete" when they are trying to free space.
type PermanentDeleter interface {
	DeletePermanently(ctx context.Context, id string) error
}

// TrashEmptier is implemented by backends that can discard everything already
// sitting in their recycle bin.
type TrashEmptier interface {
	EmptyTrash(ctx context.Context) error
}

// TrashItem is one recoverable entry in a backend's recycle bin.
type TrashItem struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	OriginalPath string    `json:"originalPath,omitempty"`
	Size         int64     `json:"size"`
	IsDir        bool      `json:"isDir"`
	Deleted      time.Time `json:"deleted"`
}

// TrashBrowser is implemented by backends whose recycle bin we can show, put
// items back from, and purge one entry at a time.
type TrashBrowser interface {
	ListTrash(ctx context.Context) ([]TrashItem, error)
	RestoreTrash(ctx context.Context, id string) error
}

// Sharer is implemented by backends that can publish a file at a public URL.
//
// The link has to be created by the provider, not by us: OmniDrive runs on a
// phone behind carrier NAT, so a link it served itself would be unreachable
// from anywhere else and would die the moment the phone slept. A provider link
// is served by their CDN, works from any network, and keeps working when the
// phone is switched off.
type Sharer interface {
	// ShareLink returns a public URL for the item, creating one if needed.
	// Calling it twice on the same file must return the same link rather than
	// littering the account with duplicates.
	ShareLink(ctx context.Context, id string) (ShareLink, error)
	// Unshare revokes the public URL again.
	Unshare(ctx context.Context, id string) error
}

// ShareLink is a public URL for one file.
type ShareLink struct {
	URL string `json:"url"`
	// Direct is a URL that starts the download immediately where the provider
	// offers one; URL may be a preview page instead.
	Direct string `json:"direct,omitempty"`
	// Expires is set only by backends whose links cannot be permanent.
	Expires time.Time `json:"expires,omitempty"`
}

// Capabilities describes the optional behaviour a driver supports, so the UI
// can offer only what will actually work rather than failing after the tap.
type Capabilities struct {
	// DeletesToTrash is true when a plain delete is recoverable rather than
	// final, which is worth saying out loud.
	DeletesToTrash  bool `json:"deletesToTrash"`
	PermanentDelete bool `json:"permanentDelete"`
	EmptyTrash      bool `json:"emptyTrash"`
	// BrowseTrash means the bin can be listed and restored from in-app.
	BrowseTrash bool `json:"browseTrash"`
	// Share means a public download link can be created for a file.
	Share bool `json:"share"`
	Range bool `json:"range"`
}

// CapabilitiesOf inspects a driver.
func CapabilitiesOf(d Driver) Capabilities {
	var c Capabilities
	if d == nil {
		return c
	}
	_, c.PermanentDelete = d.(PermanentDeleter)
	_, c.EmptyTrash = d.(TrashEmptier)
	_, c.BrowseTrash = d.(TrashBrowser)
	_, c.Share = d.(Sharer)
	_, c.Range = d.(RangeDownloader)

	switch d.Kind() {
	// A plain delete on these is recoverable rather than final. For the cloud
	// backends that is the vendor's own bin; for local storage it is ours.
	case KindGoogleDrive, KindOneDrive, KindDropbox, KindPCloud, KindTeraBox, KindLocal:
		c.DeletesToTrash = true
	}
	return c
}

// ErrUnsupported is returned by drivers for operations a backend cannot do.
var ErrUnsupported = errors.New("operation not supported by this provider")

// ErrNotFound signals a missing object; the API layer maps it to HTTP 404.
var ErrNotFound = errors.New("not found")

// Credentials is the per-account secret bag. It is always stored encrypted.
type Credentials map[string]string

func (c Credentials) Get(k string) string { return c[k] }

func (c Credentials) Set(k, v string) {
	if c != nil {
		c[k] = v
	}
}

// Config carries everything a driver needs to be constructed.
type Config struct {
	AccountID string
	Creds     Credentials
	HTTP      *http.Client

	// Save persists mutated credentials (refreshed OAuth tokens, session
	// cookies). Drivers call it whenever Creds changes.
	Save func(Credentials) error
}

// Open builds a driver for an account.
func Open(kind Kind, cfg Config) (Driver, error) {
	if cfg.HTTP == nil {
		cfg.HTTP = http.DefaultClient
	}
	if cfg.Creds == nil {
		cfg.Creds = Credentials{}
	}
	if cfg.Save == nil {
		cfg.Save = func(Credentials) error { return nil }
	}
	switch kind {
	case KindGoogleDrive:
		return newGoogleDrive(cfg), nil
	case KindOneDrive:
		return newOneDrive(cfg), nil
	case KindDropbox:
		return newDropbox(cfg), nil
	case KindPCloud:
		return newPCloud(cfg)
	case KindTeraBox:
		return newTeraBox(cfg)
	case KindWebDAV:
		return newWebDAV(cfg)
	case KindS3:
		return newS3(cfg)
	case KindLocal:
		return newLocal(cfg)
	}
	return nil, fmt.Errorf("unknown provider %q", kind)
}

// SortFiles orders a listing folders-first then case-insensitively by name,
// which is what every file manager does and what the UI expects.
func SortFiles(f []File) {
	sort.SliceStable(f, func(i, j int) bool {
		if f[i].IsDir != f[j].IsDir {
			return f[i].IsDir
		}
		return strings.ToLower(f[i].Name) < strings.ToLower(f[j].Name)
	})
}

// progressReader counts bytes as they are consumed by an HTTP request body.
type progressReader struct {
	r io.Reader
	n int64
	p Progress
}

func newProgressReader(r io.Reader, p Progress) io.Reader {
	if p == nil {
		return r
	}
	return &progressReader{r: r, p: p}
}

func (pr *progressReader) Read(b []byte) (int, error) {
	n, err := pr.r.Read(b)
	if n > 0 {
		pr.n += int64(n)
		pr.p(pr.n)
	}
	return n, err
}
