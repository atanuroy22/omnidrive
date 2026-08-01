package provider

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// s3 implements the S3 REST API with hand-rolled SigV4 signing. Pulling in the
// AWS SDK would add tens of megabytes to a binary meant to live on a phone,
// and we only need six operations.
type s3 struct {
	cfg       Config
	endpoint  *url.URL
	region    string
	bucket    string
	accessKey string
	secretKey string
	prefix    string
	pathStyle bool
}

func newS3(cfg Config) (Driver, error) {
	raw := strings.TrimSpace(cfg.Creds["endpoint"])
	if raw == "" {
		raw = "https://s3.amazonaws.com"
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("s3: bad endpoint: %w", err)
	}
	bucket := strings.Trim(cfg.Creds["bucket"], "/")
	if bucket == "" {
		return nil, errors.New("s3: bucket is required")
	}
	if cfg.Creds["accessKey"] == "" || cfg.Creds["secretKey"] == "" {
		return nil, errors.New("s3: access key and secret key are required")
	}
	region := cfg.Creds["region"]
	if region == "" {
		region = "us-east-1"
	}
	prefix := strings.TrimPrefix(cfg.Creds["prefix"], "/")
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return &s3{
		cfg: cfg, endpoint: u, region: region, bucket: bucket,
		accessKey: cfg.Creds["accessKey"], secretKey: cfg.Creds["secretKey"],
		prefix: prefix,
		// Path style works everywhere; virtual-host style needs wildcard DNS
		// and breaks against MinIO and most self-hosted gateways.
		pathStyle: true,
	}, nil
}

func (s *s3) Kind() Kind { return KindS3 }

// key maps an object ID (a bucket-relative path) to a full object key.
func (s *s3) key(id string) string { return s.prefix + strings.TrimPrefix(id, "/") }

func (s *s3) urlFor(id string, query url.Values) string {
	u := *s.endpoint
	if s.pathStyle {
		u.Path = "/" + s.bucket + "/" + s.key(id)
	} else {
		u.Host = s.bucket + "." + u.Host
		u.Path = "/" + s.key(id)
	}
	if query != nil {
		u.RawQuery = query.Encode()
	}
	return u.String()
}

func (s *s3) do(ctx context.Context, method, id string, query url.Values, body io.Reader, size int64, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.urlFor(id, query), body)
	if err != nil {
		return nil, err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if err := s.sign(req, size); err != nil {
		return nil, err
	}
	return s.cfg.HTTP.Do(req)
}

// sign applies AWS Signature Version 4 with an unsigned payload, which lets us
// stream uploads without hashing them first.
func (s *s3) sign(req *http.Request, size int64) error {
	const payloadHash = "UNSIGNED-PAYLOAD"
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if req.Host == "" {
		req.Host = req.URL.Host
	}
	req.Header.Set("Host", req.Host)

	// Canonical headers: lowercase names, sorted, values trimmed.
	var names []string
	values := map[string]string{}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if lk != "host" && !strings.HasPrefix(lk, "x-amz-") && lk != "content-type" {
			continue
		}
		names = append(names, lk)
		values[lk] = strings.TrimSpace(strings.Join(v, ","))
	}
	if _, ok := values["host"]; !ok {
		names = append(names, "host")
		values["host"] = req.Host
	}
	sort.Strings(names)

	var canonHeaders strings.Builder
	for _, n := range names {
		canonHeaders.WriteString(n + ":" + values[n] + "\n")
	}
	signedHeaders := strings.Join(names, ";")

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		canonicalQuery(req.URL),
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, s.region, "s3", "aws4_request"}, "/")
	crHash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, hex.EncodeToString(crHash[:]),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+s.secretKey), dateStamp)
	kRegion := hmacSHA256(kDate, s.region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.accessKey, scope, signedHeaders, signature))
	return nil
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// canonicalURI re-encodes each path segment per the SigV4 rules, which are
// stricter than url.PathEscape (slashes are kept, everything else escaped).
func canonicalURI(u *url.URL) string {
	if u.Path == "" {
		return "/"
	}
	segs := strings.Split(u.Path, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s)
	}
	return strings.Join(segs, "/")
}

func canonicalQuery(u *url.URL) string {
	q := u.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, uriEncode(k)+"="+uriEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

func uriEncode(s string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

type listBucketResult struct {
	XMLName        xml.Name `xml:"ListBucketResult"`
	IsTruncated    bool     `xml:"IsTruncated"`
	NextContinueTk string   `xml:"NextContinuationToken"`
	Contents       []struct {
		Key          string `xml:"Key"`
		Size         int64  `xml:"Size"`
		LastModified string `xml:"LastModified"`
	} `xml:"Contents"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
}

func (s *s3) List(ctx context.Context, folderID string) ([]File, error) {
	dir := strings.Trim(folderID, "/")
	if dir != "" {
		dir += "/"
	}
	fullPrefix := s.prefix + dir

	var out []File
	token := ""
	for {
		q := url.Values{}
		q.Set("list-type", "2")
		q.Set("delimiter", "/")
		q.Set("max-keys", "1000")
		if fullPrefix != "" {
			q.Set("prefix", fullPrefix)
		}
		if token != "" {
			q.Set("continuation-token", token)
		}

		// Listing targets the bucket itself, so the object id is empty.
		resp, err := s.do(ctx, http.MethodGet, "", q, nil, 0, nil)
		if err != nil {
			return nil, err
		}
		if err := checkResponse(resp); err != nil {
			return nil, err
		}
		var res listBucketResult
		err = xml.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("s3: parse listing: %w", err)
		}

		for _, cp := range res.CommonPrefixes {
			rel := strings.TrimSuffix(strings.TrimPrefix(cp.Prefix, s.prefix), "/")
			out = append(out, File{
				ID:    rel,
				Name:  lastSegment(rel),
				IsDir: true,
			})
		}
		for _, c := range res.Contents {
			rel := strings.TrimPrefix(c.Key, s.prefix)
			// A zero-byte key ending in "/" is the conventional folder marker.
			if strings.HasSuffix(rel, "/") {
				continue
			}
			mod, _ := time.Parse(time.RFC3339, c.LastModified)
			out = append(out, File{ID: rel, Name: lastSegment(rel), Size: c.Size, Modified: mod})
		}
		if !res.IsTruncated || res.NextContinueTk == "" {
			break
		}
		token = res.NextContinueTk
	}
	SortFiles(out)
	return out, nil
}

func (s *s3) Stat(ctx context.Context, id string) (File, error) {
	resp, err := s.do(ctx, http.MethodHead, id, nil, nil, 0, nil)
	if err != nil {
		return File{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return File{}, ErrNotFound
	}
	if err := checkResponse(resp); err != nil {
		return File{}, err
	}
	mod, _ := time.Parse(time.RFC1123, resp.Header.Get("Last-Modified"))
	return File{
		ID: id, Name: lastSegment(id), Size: resp.ContentLength,
		Modified: mod, MIME: resp.Header.Get("Content-Type"),
	}, nil
}

func (s *s3) Download(ctx context.Context, id string) (io.ReadCloser, int64, error) {
	resp, err := s.do(ctx, http.MethodGet, id, nil, nil, 0, nil)
	if err != nil {
		return nil, 0, err
	}
	if err := checkResponse(resp); err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

// DownloadRange requests a byte window. Range is part of the signed request
// only as a header, so no signature changes are needed.
func (s *s3) DownloadRange(ctx context.Context, id string, start, end int64) (io.ReadCloser, error) {
	resp, err := s.do(ctx, http.MethodGet, id, nil, nil, 0,
		map[string]string{"Range": rangeHeader(start, end)})
	if err != nil {
		return nil, err
	}
	if err := checkResponse(resp); err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (s *s3) Upload(ctx context.Context, parentID, name string, size int64, r io.Reader, p Progress) (File, error) {
	if size < 0 {
		return File{}, errors.New("s3: upload requires a known content length")
	}
	id := joinID(parentID, name)
	resp, err := s.do(ctx, http.MethodPut, id, nil, newProgressReader(r, p), size,
		map[string]string{"Content-Type": "application/octet-stream"})
	if err != nil {
		return File{}, err
	}
	if err := checkResponse(resp); err != nil {
		return File{}, err
	}
	resp.Body.Close()
	return File{ID: id, Name: name, Size: size, Modified: time.Now()}, nil
}

func (s *s3) Mkdir(ctx context.Context, parentID, name string) (File, error) {
	id := joinID(parentID, name)
	// S3 has no directories; a zero-byte marker object is the convention every
	// console and sync tool understands.
	resp, err := s.do(ctx, http.MethodPut, id+"/", nil, strings.NewReader(""), 0, nil)
	if err != nil {
		return File{}, err
	}
	if err := checkResponse(resp); err != nil {
		return File{}, err
	}
	resp.Body.Close()
	return File{ID: id, Name: name, IsDir: true, Modified: time.Now()}, nil
}

func (s *s3) Rename(ctx context.Context, id, newName string) error {
	parent := ""
	if i := strings.LastIndex(strings.TrimSuffix(id, "/"), "/"); i >= 0 {
		parent = id[:i]
	}
	dest := joinID(parent, newName)

	// S3 renames are copy-then-delete; there is no server-side move.
	source := "/" + s.bucket + "/" + s.key(id)
	resp, err := s.do(ctx, http.MethodPut, dest, nil, nil, 0, map[string]string{
		"X-Amz-Copy-Source": uriEncodePath(source),
	})
	if err != nil {
		return err
	}
	if err := checkResponse(resp); err != nil {
		return err
	}
	resp.Body.Close()
	return s.Delete(ctx, id)
}

func uriEncodePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s)
	}
	return strings.Join(segs, "/")
}

func (s *s3) Delete(ctx context.Context, id string) error {
	resp, err := s.do(ctx, http.MethodDelete, id, nil, nil, 0, nil)
	if err != nil {
		return err
	}
	if err := checkResponse(resp); err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Quota walks the bucket to total up object sizes. S3 exposes no quota API, so
// "total" is reported as unknown and the UI shows usage only.
func (s *s3) Quota(ctx context.Context) (Quota, error) {
	var used int64
	token := ""
	for pages := 0; pages < 50; pages++ { // bounded: a huge bucket must not hang the phone
		q := url.Values{"list-type": {"2"}, "max-keys": {"1000"}}
		if s.prefix != "" {
			q.Set("prefix", s.prefix)
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := s.do(ctx, http.MethodGet, "", q, nil, 0, nil)
		if err != nil {
			return Quota{}, err
		}
		if err := checkResponse(resp); err != nil {
			return Quota{}, err
		}
		var res listBucketResult
		err = xml.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		if err != nil {
			return Quota{}, err
		}
		for _, c := range res.Contents {
			used += c.Size
		}
		if !res.IsTruncated || res.NextContinueTk == "" {
			break
		}
		token = res.NextContinueTk
	}
	return Quota{Used: used, Total: 0}, nil
}

func lastSegment(p string) string {
	p = strings.TrimSuffix(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
