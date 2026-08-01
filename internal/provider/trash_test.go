package provider

import (
	"net/http"
	"testing"
)

// Deleting on these providers only moves an item to a recycle bin where it
// keeps consuming quota. The UI has to know that, or "delete" appears to free
// no space at all.
func TestCapabilitiesReportTrashBehaviour(t *testing.T) {
	hc := &http.Client{}

	cases := []struct {
		kind          Kind
		creds         Credentials
		wantTrash     bool
		wantPermanent bool
		wantEmpty     bool
	}{
		{
			kind:  KindGoogleDrive,
			creds: Credentials{CredAccessToken: "x", CredRefreshToken: "y"},
			// Google trashes by default but can destroy outright and empty the bin.
			wantTrash: true, wantPermanent: true, wantEmpty: true,
		},
		{
			kind:  KindPCloud,
			creds: Credentials{"auth": "token"},
			// pCloud trashes, but trash_clear takes a single id, so one file can
			// be purged without touching the rest of the bin.
			wantTrash: true, wantPermanent: true, wantEmpty: true,
		},
		{
			kind:  KindDropbox,
			creds: Credentials{CredAccessToken: "x", CredRefreshToken: "y"},
			// Dropbox keeps deleted files recoverable with no API to purge them.
			wantTrash: true, wantPermanent: false, wantEmpty: false,
		},
		{
			kind:  KindS3,
			creds: Credentials{"endpoint": "https://s3.example.com", "bucket": "b", "accessKey": "k", "secretKey": "s"},
			// S3 deletes are final already.
			wantTrash: false, wantPermanent: false, wantEmpty: false,
		},
		{
			kind:  KindLocal,
			creds: Credentials{"root": t.TempDir()},
			// Android offers no recycle bin of its own, so OmniDrive runs one.
			wantTrash: true, wantPermanent: true, wantEmpty: true,
		},
	}

	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			drv, err := Open(tc.kind, Config{Creds: tc.creds, HTTP: hc})
			if err != nil {
				t.Fatalf("open %s: %v", tc.kind, err)
			}
			c := CapabilitiesOf(drv)
			if c.DeletesToTrash != tc.wantTrash {
				t.Errorf("DeletesToTrash = %v, want %v", c.DeletesToTrash, tc.wantTrash)
			}
			if c.PermanentDelete != tc.wantPermanent {
				t.Errorf("PermanentDelete = %v, want %v", c.PermanentDelete, tc.wantPermanent)
			}
			if c.EmptyTrash != tc.wantEmpty {
				t.Errorf("EmptyTrash = %v, want %v", c.EmptyTrash, tc.wantEmpty)
			}
		})
	}
}

// Every backend must be able to serve byte ranges, or seeking in a video
// silently degrades to downloading the whole file.
func TestAllBackendsSupportRanges(t *testing.T) {
	hc := &http.Client{}
	creds := map[Kind]Credentials{
		KindGoogleDrive: {CredAccessToken: "x", CredRefreshToken: "y"},
		KindOneDrive:    {CredAccessToken: "x", CredRefreshToken: "y"},
		KindDropbox:     {CredAccessToken: "x", CredRefreshToken: "y"},
		KindPCloud:      {"auth": "token"},
		KindWebDAV:      {"url": "https://example.com/dav", "username": "u", "password": "p"},
		KindS3:          {"endpoint": "https://s3.example.com", "bucket": "b", "accessKey": "k", "secretKey": "s"},
		KindLocal:       {"root": t.TempDir()},
	}
	for kind, c := range creds {
		drv, err := Open(kind, Config{Creds: c, HTTP: hc})
		if err != nil {
			t.Fatalf("open %s: %v", kind, err)
		}
		if !CapabilitiesOf(drv).Range {
			t.Errorf("%s cannot serve byte ranges, so playback cannot seek", kind)
		}
	}
}
