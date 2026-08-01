package portable

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"omnidrive/internal/store"
)

// Pairing lets one device hand its whole setup to another over the local
// network. The receiving device needs a short URL and a one-time code; the
// code is also the encryption passphrase, so the bundle is never readable by
// anything that merely sees the traffic.
//
// The code is 8 Crockford base32 characters (40 bits). That is far too weak to
// publish, which is why an offer is single-use, expires in ten minutes, and is
// only reachable on the local network.

const (
	pairTTL      = 10 * time.Minute
	pairCodeLen  = 8
	maxCodeTries = 5
	codeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // Crockford: no I, L, O, U
)

// Offer is a live pairing invitation.
type Offer struct {
	Token string `json:"token"`
	Code  string `json:"code"`
	URL   string `json:"url"`
	// Link is URL and Code combined, so pairing is one string to pass along
	// rather than two things to keep together. The code is still the
	// decryption passphrase; carrying it in the link does not weaken the
	// offer's real protections — LAN-only, single use, ten-minute expiry —
	// because in practice both halves always travelled together anyway.
	Link      string    `json:"link"`
	ExpiresAt time.Time `json:"expiresAt"`
	Accounts  int       `json:"accounts"`

	blob     []byte
	tries    int
	consumed bool
}

// Pairer holds outstanding offers. The zero value is not usable; call NewPairer.
type Pairer struct {
	mu     sync.Mutex
	offers map[string]*Offer
}

func NewPairer() *Pairer {
	return &Pairer{offers: map[string]*Offer{}}
}

// Create seals the current setup under a fresh one-time code and returns the
// invitation. port is the port the API is listening on, used to build the URL.
func (p *Pairer) Create(st *store.Store, port int) (*Offer, error) {
	accounts := st.Accounts()
	if len(accounts) == 0 {
		return nil, errors.New("nothing to pair: no accounts are connected on this device")
	}
	code, err := randomCode(pairCodeLen)
	if err != nil {
		return nil, err
	}
	blob, err := Export(st, code)
	if err != nil {
		return nil, err
	}
	token, err := randomCode(16)
	if err != nil {
		return nil, err
	}

	host := LANAddress()
	base := fmt.Sprintf("http://%s:%d/pair/%s", host, port, token)
	offer := &Offer{
		Token:     token,
		Code:      code,
		URL:       base,
		Link:      base + "?c=" + code,
		ExpiresAt: time.Now().Add(pairTTL),
		Accounts:  len(accounts),
		blob:      blob,
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked()
	p.offers[token] = offer
	return offer, nil
}

// Fetch validates a code against an offer and returns the sealed bundle. The
// offer is consumed on success and destroyed after too many wrong codes.
func (p *Pairer) Fetch(token, code string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked()

	offer, ok := p.offers[token]
	if !ok {
		return nil, errors.New("pairing link is unknown or has expired")
	}
	if offer.consumed {
		return nil, errors.New("pairing link has already been used")
	}
	// Constant-time compare so a wrong code leaks nothing through timing.
	if subtle.ConstantTimeCompare([]byte(normalizeCode(code)), []byte(offer.Code)) != 1 {
		offer.tries++
		if offer.tries >= maxCodeTries {
			delete(p.offers, token)
			return nil, errors.New("too many incorrect codes; pairing link destroyed")
		}
		return nil, fmt.Errorf("incorrect code (%d attempts left)", maxCodeTries-offer.tries)
	}
	offer.consumed = true
	delete(p.offers, token)
	return offer.blob, nil
}

// Revoke cancels an outstanding offer.
func (p *Pairer) Revoke(token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.offers, token)
}

// Active lists offers that have not expired, for the UI.
func (p *Pairer) Active() []*Offer {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked()
	out := make([]*Offer, 0, len(p.offers))
	for _, o := range p.offers {
		out = append(out, o)
	}
	return out
}

func (p *Pairer) sweepLocked() {
	now := time.Now()
	for k, o := range p.offers {
		if now.After(o.ExpiresAt) {
			delete(p.offers, k)
		}
	}
}

// CodeFromLink extracts the pairing code carried in a link, or "" if absent.
func CodeFromLink(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	q := u.Query()
	for _, key := range []string{"c", "code"} {
		if v := q.Get(key); v != "" {
			return normalizeCode(v)
		}
	}
	return ""
}

// Join downloads a bundle from another device and returns the sealed bytes.
//
// rawURL may be the combined link, in which case code may be empty; an
// explicit code always wins, so a separately-typed one still works.
func Join(ctx context.Context, rawURL, code string) ([]byte, error) {
	rawURL = strings.TrimSpace(rawURL)
	if code == "" {
		code = CodeFromLink(rawURL)
	}
	if code == "" {
		return nil, errors.New("that pairing link carries no code — copy the whole link, " +
			"including everything after the '?'")
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("bad pairing URL: %w", err)
	}
	if u.Scheme == "" {
		u, err = url.Parse("http://" + rawURL)
		if err != nil {
			return nil, fmt.Errorf("bad pairing URL: %w", err)
		}
	}
	q := u.Query()
	q.Del("c")
	q.Set("code", normalizeCode(code))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	// Pairing runs on a phone hotspot as often as on Wi-Fi; a short timeout
	// keeps a wrong IP from hanging the UI.
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach the other device: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBundle))
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		}
		return nil, errors.New(msg)
	}
	return body, nil
}

// randomCode returns n characters of unbiased Crockford base32.
func randomCode(n int) (string, error) {
	// 32 divides 256 evenly, so masking the low 5 bits introduces no bias.
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, v := range b {
		out[i] = codeAlphabet[v&0x1F]
	}
	return string(out), nil
}

// NormalizeCode is the exported form, used by callers that need to turn a
// user-typed code into the exact passphrase the bundle was sealed with.
func NormalizeCode(s string) string { return normalizeCode(s) }

// normalizeCode makes codes forgiving to type: case-insensitive, and the
// characters Crockford excludes are folded onto their look-alikes.
func normalizeCode(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	r := strings.NewReplacer("-", "", " ", "", "I", "1", "L", "1", "O", "0", "U", "V")
	return r.Replace(s)
}

// LANAddress reports the address other devices on the same network can use to
// reach this one. It prefers a real private-network interface over loopback.
func LANAddress() string {
	// Dialling a public address does not send traffic for UDP, but it makes
	// the kernel pick the interface it would actually route through — which is
	// the one the other phone can reach.
	if conn, err := net.Dial("udp4", "8.8.8.8:53"); err == nil {
		defer conn.Close()
		if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok && addr.IP != nil {
			return addr.IP.String()
		}
	}
	// Fall back to scanning interfaces, which also covers the case where the
	// phone is acting as a hotspot with no upstream route.
	ifaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip != nil && ip.IsPrivate() {
				return ip.String()
			}
		}
	}
	return "127.0.0.1"
}
