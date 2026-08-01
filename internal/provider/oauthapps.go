package provider

import "strings"

// Build-time OAuth app credentials.
//
// Google, Microsoft and Dropbox will not issue tokens to an unregistered app —
// there is no password-based path to their storage APIs, by design. Somebody
// has to register a client once. These variables let that "once" happen at
// build time instead of on every device:
//
//	go build -ldflags "-X omnidrive/internal/provider.BuiltinGoogleClientID=..."
//
// build.sh and android/build-apk.sh fill them in from an oauth.env file, so a
// build made with that file present never prompts for credentials at all.
//
// Left empty, the app asks once and stores what you enter — which then travels
// to your other devices inside the encrypted bundle.
//
// These are not secrets in the usual sense: a client ID identifies the
// application, not the user, and anyone with the APK can extract it. That is
// why the flow also uses PKCE, which is what actually protects the exchange.
var (
	BuiltinGoogleClientID   string
	BuiltinGoogleSecret     string
	BuiltinOneDriveClientID string
	BuiltinOneDriveSecret   string
	BuiltinDropboxClientID  string
	BuiltinDropboxSecret    string
)

// BuiltinApp returns compiled-in credentials for a provider, if any.
func BuiltinApp(kind Kind) (clientID, secret string, ok bool) {
	switch kind {
	case KindGoogleDrive:
		clientID, secret = BuiltinGoogleClientID, BuiltinGoogleSecret
	case KindOneDrive:
		clientID, secret = BuiltinOneDriveClientID, BuiltinOneDriveSecret
	case KindDropbox:
		clientID, secret = BuiltinDropboxClientID, BuiltinDropboxSecret
	default:
		return "", "", false
	}
	clientID = strings.TrimSpace(clientID)
	return clientID, strings.TrimSpace(secret), clientID != ""
}
