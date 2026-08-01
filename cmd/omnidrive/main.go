// Command omnidrive runs the OmniDrive server: one static binary that serves a
// web UI on localhost and talks to every connected cloud account directly.
//
// Typical use on a phone, inside Termux:
//
//	omnidrive            # start, then open http://127.0.0.1:8787
//	omnidrive pair       # hand this whole setup to another device
//	omnidrive accounts   # list what is connected
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"omnidrive/internal/androidnet"
	"omnidrive/internal/portable"
	"omnidrive/internal/provider"
	"omnidrive/internal/server"
	"omnidrive/internal/store"
)

// version is overridden at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "omnidrive: "+err.Error())
		os.Exit(1)
	}
}

type options struct {
	dataDir string
	addr    string
	port    int
	// passphrase encrypts portable bundles. It is deliberately distinct from
	// devicePass: mixing the two means one wrong entry locks you out of the
	// local state as well as the backup.
	passphrase string
	devicePass string
	replace    bool
	noOpen     bool
}

func run() error {
	var opt options
	fs := flag.NewFlagSet("omnidrive", flag.ContinueOnError)
	fs.StringVar(&opt.dataDir, "data", defaultDataDir(), "directory for encrypted state")
	fs.StringVar(&opt.addr, "addr", "127.0.0.1", "address to bind (use 0.0.0.0 to reach it from other devices)")
	fs.IntVar(&opt.port, "port", 8787, "port to listen on")
	fs.StringVar(&opt.passphrase, "passphrase", os.Getenv("OMNIDRIVE_PASSPHRASE"), "passphrase for a portable bundle (export/import/push/pull)")
	fs.StringVar(&opt.devicePass, "device-passphrase", os.Getenv("OMNIDRIVE_DEVICE_PASSPHRASE"), "optional passphrase protecting this device's local state")
	fs.BoolVar(&opt.replace, "replace", false, "on import: discard local accounts instead of merging")
	fs.BoolVar(&opt.noOpen, "no-open", false, "do not open a browser on start (desktop only)")
	fs.Usage = usage

	// -v/-h are conventional enough to handle before any parsing.
	for _, a := range os.Args[1:] {
		switch a {
		case "-v", "--version", "version":
			fmt.Printf("omnidrive %s (%s/%s, go %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
			return nil
		case "-h", "--help", "help":
			usage()
			return nil
		}
	}

	// Split flags from positional arguments so the subcommand can appear
	// anywhere: `omnidrive export out -passphrase x` and
	// `omnidrive -data /path export out` both work.
	flagArgs, positionalArgs := split(os.Args[1:])

	cmd := "serve"
	if len(positionalArgs) > 0 && isCommand(positionalArgs[0]) {
		cmd, positionalArgs = positionalArgs[0], positionalArgs[1:]
	}

	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	positional := positionalArgs

	// Fix DNS before anything opens a socket.
	androidnet.Install()

	st, err := store.Open(opt.dataDir, opt.devicePass)
	if err != nil {
		return err
	}

	switch cmd {
	case "serve", "run", "start":
		return serve(opt, st)
	case "accounts":
		return listAccounts(st)
	case "export":
		return exportBundle(opt, st, positional)
	case "import":
		return importBundle(opt, st, positional)
	case "pair":
		return pairServe(opt, st)
	case "join":
		return pairJoin(opt, st, positional)
	case "push":
		return cloudPush(opt, st, positional)
	case "pull":
		return cloudPull(opt, st, positional)
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// commands is the set of recognised subcommands.
var commands = map[string]bool{
	"serve": true, "run": true, "start": true, "accounts": true,
	"pair": true, "join": true, "export": true, "import": true,
	"push": true, "pull": true, "version": true, "help": true,
	"-v": true, "--version": true, "-h": true, "--help": true,
}

func isCommand(s string) bool { return commands[s] }

// boolFlags are the flags that take no value, which permute needs to know.
var boolFlags = map[string]bool{"replace": true}

// split separates flags (with their values) from positional arguments. Go's
// flag package stops parsing at the first positional, so without this
// `omnidrive export out.file -passphrase x` would silently ignore the
// passphrase, and the subcommand would have to come first.
func split(args []string) (flags, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i+1:]...)
			break
		}
		if len(a) > 1 && strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			// A bool flag or an inline -flag=value consumes nothing more.
			if strings.Contains(name, "=") || boolFlags[name] {
				continue
			}
			if i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		rest = append(rest, a)
	}
	return flags, rest
}

func usage() {
	fmt.Fprint(os.Stderr, `omnidrive — one lightweight binary for all your cloud drives

Usage:
  omnidrive [flags]                 start the server (default)
  omnidrive accounts                list connected drives
  omnidrive pair                    share this setup with another device
  omnidrive join <pairing-link>     receive a setup from another device
  omnidrive export <file>           write an encrypted backup bundle
  omnidrive import <file>           read an encrypted backup bundle
  omnidrive push <account-id>       save the setup into a connected drive
  omnidrive pull <account-id>       restore the setup from a connected drive
  omnidrive version

Flags:
  -data string               directory for encrypted state
  -addr string               bind address (default 127.0.0.1)
  -port int                  port (default 8787)
  -passphrase string         passphrase for a portable bundle
                             (or $OMNIDRIVE_PASSPHRASE)
  -device-passphrase string  optional passphrase protecting local state
                             (or $OMNIDRIVE_DEVICE_PASSPHRASE)
  -replace                   on import, discard local accounts instead of merging
  -no-open                   do not open a browser on start (desktop only)

Examples:
  omnidrive -port 9000
  omnidrive export ~/storage/downloads/my-drives.omnibundle -passphrase hunter2two
  omnidrive join http://192.168.1.42:41235/pair/9f3a K3M9P2QX
`)
}

// defaultDataDir picks a writable location per platform. On Termux this
// resolves inside the app's private home, which is the only place guaranteed
// to be both writable and persistent.
func defaultDataDir() string {
	if v := os.Getenv("OMNIDRIVE_DATA"); v != "" {
		return v
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".omnidrive")
	}
	return ".omnidrive"
}

func serve(opt options, st *store.Store) error {
	addr := net.JoinHostPort(opt.addr, fmt.Sprint(opt.port))

	// A bind that is reachable from the network needs a shared secret; a
	// loopback bind does not, because reaching it already implies local access.
	token := ""
	if !isLoopback(opt.addr) {
		token = randomToken()
	}

	srv := server.New(server.Options{
		Addr: addr, Token: token, Version: version, Store: st,
	})

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 20 * time.Second,
		// No write timeout: uploads and downloads legitimately run for hours.
		IdleTimeout: 120 * time.Second,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w (is another copy already running?)", addr, err)
	}

	url := fmt.Sprintf("http://%s:%d", displayHost(opt.addr), opt.port)
	if token != "" {
		url += "/?t=" + token
	}
	printBanner(url, st, opt.dataDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Repair setups that accumulated duplicate drives before connecting became
	// idempotent.
	if merged := srv.DedupeAccounts(); len(merged) > 0 {
		log.Printf("merged %d duplicate drive(s): %s", len(merged), strings.Join(merged, ", "))
	}

	// On a phone, the device's own storage should just be there as a drive.
	srv.EnsureLocalStorage()

	// On a desktop, running the binary should show you the app. Otherwise it
	// looks like nothing happened: a console window and no visible result.
	if !opt.noOpen {
		go func() {
			if err := server.OpenBrowser(url); err != nil {
				log.Printf("could not open a browser automatically: %v", err)
			}
		}()
	}

	// Refresh quotas at startup and on a timer so the allocation strategy has
	// something current to work with.
	go func() {
		c, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		srv.RefreshQuotas(c)
	}()
	go quotaLoop(ctx, srv, st)

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		fmt.Println("\nshutting down…")
		shutCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutCtx)
	}
}

func quotaLoop(ctx context.Context, srv *server.Server, st *store.Store) {
	for {
		interval := time.Duration(st.Settings().SyncIntervalM) * time.Minute
		if interval <= 0 {
			interval = 30 * time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			c, cancel := context.WithTimeout(ctx, 3*time.Minute)
			srv.RefreshQuotas(c)
			cancel()
		}
	}
}

func printBanner(url string, st *store.Store, dataDir string) {
	accounts := st.Accounts()
	fmt.Println()
	fmt.Println("  OmniDrive " + version)
	fmt.Println("  ─────────────────────────────────────────")
	fmt.Printf("  Open      %s\n", url)
	fmt.Printf("  Drives    %d connected\n", len(accounts))
	fmt.Printf("  Data      %s\n", dataDir)
	if androidnet.Patched() {
		servers := androidnet.Servers()
		if len(servers) > 2 {
			servers = servers[:2]
		}
		fmt.Printf("  DNS       Android fix active (%s)\n", strings.Join(servers, ", "))
	}
	if len(accounts) == 0 {
		fmt.Println()
		fmt.Println("  No drives yet — this device keeps its own separate setup,")
		fmt.Println("  so drives connected on your phone do not appear here.")
		fmt.Println()
		fmt.Println("  Either connect one in the Drives tab, or copy an existing")
		fmt.Println("  setup over:  on the phone open Move -> Show pairing code,")
		fmt.Println("  then run:    omnidrive join <url> <code>")
	}
	fmt.Println()
	fmt.Println("  Press Ctrl+C to stop.")
	fmt.Println()
}

func isLoopback(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func displayHost(bind string) string {
	if bind == "0.0.0.0" || bind == "::" || bind == "" {
		return portable.LANAddress()
	}
	return bind
}

func randomToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Falling back to a fixed token would be worse than failing loudly.
		panic("cannot read random bytes: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func listAccounts(st *store.Store) error {
	accounts := st.Accounts()
	if len(accounts) == 0 {
		fmt.Println("No drives connected.")
		return nil
	}
	fmt.Printf("%-18s  %-11s  %-28s  %s\n", "ID", "KIND", "LABEL", "USED / TOTAL")
	for _, a := range accounts {
		used := humanBytes(a.QuotaUsed)
		total := "unknown"
		if a.QuotaTotal > 0 {
			total = humanBytes(a.QuotaTotal)
		}
		label := a.Label
		if !a.Enabled {
			label += " (disabled)"
		}
		fmt.Printf("%-18s  %-11s  %-28s  %s / %s\n", a.ID, a.Kind, truncate(label, 28), used, total)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func humanBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

func exportBundle(opt options, st *store.Store, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: omnidrive export <file> -passphrase <pass>")
	}
	pass, err := requirePassphrase(opt.passphrase)
	if err != nil {
		return err
	}
	blob, err := portable.Export(st, pass)
	if err != nil {
		return err
	}
	if err := os.WriteFile(args[0], blob, 0o600); err != nil {
		return err
	}
	fmt.Printf("Wrote %s (%s, %d drives).\n", args[0], humanBytes(int64(len(blob))), len(st.Accounts()))
	fmt.Println("Keep the passphrase safe — without it the bundle cannot be opened.")
	return nil
}

func importBundle(opt options, st *store.Store, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: omnidrive import <file> -passphrase <pass>")
	}
	pass, err := requirePassphrase(opt.passphrase)
	if err != nil {
		return err
	}
	blob, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	summary, added, updated, err := portable.Import(st, blob, pass, opt.replace)
	if err != nil {
		return err
	}
	reportImport(summary, added, updated)
	return nil
}

func reportImport(summary portable.Summary, added, updated int) {
	fmt.Printf("Imported a bundle from %s (%s), created %s.\n",
		summary.Device, summary.Platform, summary.CreatedAt.Local().Format(time.RFC1123))
	fmt.Printf("%d new drive(s), %d updated.\n", added, updated)
	for _, a := range summary.Accounts {
		fmt.Println("  · " + a)
	}
}

func pairServe(opt options, st *store.Store) error {
	pairer := portable.NewPairer()

	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return fmt.Errorf("open a pairing port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	offer, err := pairer.Create(st, port)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /pair/{token}", func(w http.ResponseWriter, r *http.Request) {
		blob, err := pairer.Fetch(r.PathValue("token"), r.URL.Query().Get("code"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(blob)
		fmt.Println("\n  Setup collected. You can stop this command now.")
	})

	fmt.Println()
	fmt.Printf("  Pairing %d drive(s). Paste this one link on the other device —\n", offer.Accounts)
	fmt.Println("  into the Move tab, or on a command line:")
	fmt.Println()
	fmt.Printf("      omnidrive join \"%s\"\n", offer.Link)
	fmt.Println()
	fmt.Println("  Single use · expires " + offer.ExpiresAt.Local().Format(time.Kitchen))
	fmt.Println()

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	// Stop automatically when the offer expires so the port never lingers.
	go func() {
		time.Sleep(time.Until(offer.ExpiresAt))
		stop()
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func pairJoin(opt options, st *store.Store, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: omnidrive join <pairing-link>")
	}
	link := args[0]
	// The link carries the code, but accept a separately typed one as well.
	code := ""
	if len(args) > 1 {
		code = args[1]
	} else {
		code = portable.CodeFromLink(link)
	}
	if code == "" {
		return errors.New("that link carries no pairing code — copy the whole link, " +
			"including everything after the '?', and quote it if your shell splits on '&'")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	blob, err := portable.Join(ctx, link, code)
	if err != nil {
		return err
	}
	summary, added, updated, err := portable.Import(st, blob, portable.NormalizeCode(code), opt.replace)
	if err != nil {
		return err
	}
	reportImport(summary, added, updated)
	fmt.Println("\nStart the server with:  omnidrive")
	return nil
}

func cloudPush(opt options, st *store.Store, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: omnidrive push <account-id> -passphrase <pass>\n(run `omnidrive accounts` for IDs)")
	}
	pass, err := requirePassphrase(opt.passphrase)
	if err != nil {
		return err
	}
	srv := server.New(server.Options{Version: version, Store: st})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := srv.PushConfigTo(ctx, args[0], pass); err != nil {
		return err
	}
	fmt.Printf("Saved the setup to %s/%s in that drive.\n", portable.ConfigFolder, portable.BundleName)
	return nil
}

func cloudPull(opt options, st *store.Store, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: omnidrive pull <account-id> -passphrase <pass>")
	}
	pass, err := requirePassphrase(opt.passphrase)
	if err != nil {
		return err
	}
	srv := server.New(server.Options{Version: version, Store: st})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	summary, added, updated, err := srv.PullConfigFrom(ctx, args[0], pass, opt.replace)
	if err != nil {
		return err
	}
	reportImport(summary, added, updated)
	return nil
}

func requirePassphrase(p string) (string, error) {
	if len(p) < 8 {
		return "", errors.New("a passphrase of at least 8 characters is required: pass -passphrase or set OMNIDRIVE_PASSPHRASE")
	}
	return p, nil
}

// Keep the provider package linked into CLI-only builds so `omnidrive
// accounts` can render kinds without the server being constructed.
var _ = provider.KindGoogleDrive
