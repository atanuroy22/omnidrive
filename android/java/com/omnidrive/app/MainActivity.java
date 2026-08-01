package com.omnidrive.app;

import android.Manifest;
import android.app.Activity;
import android.content.ContentValues;
import android.content.Intent;
import android.content.SharedPreferences;
import android.content.pm.PackageManager;
import android.graphics.Color;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.os.Environment;
import android.os.Handler;
import android.os.Looper;
import android.provider.MediaStore;
import android.provider.Settings;
import android.util.TypedValue;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.webkit.CookieManager;
import android.webkit.DownloadListener;
import android.webkit.URLUtil;
import android.webkit.ValueCallback;
import android.webkit.WebChromeClient;
import android.webkit.WebResourceRequest;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.Button;
import android.widget.FrameLayout;
import android.widget.LinearLayout;
import android.widget.ScrollView;
import android.widget.TextView;
import android.widget.Toast;

import java.io.InputStream;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

/**
 * The whole app: a WebView pointed at the local server, plus a startup screen
 * that doubles as a log viewer when something goes wrong.
 */
public class MainActivity extends Activity implements ServerService.LogListener {

    private FrameLayout root;
    private WebView web;
    private LinearLayout splash;
    private TextView logView;
    private ScrollView logScroll;
    private Button logToggle;

    /** The sign-in page overlay, non-null only while one is open. */
    private ViewGroup loginOverlay;
    private TextView loginStatus;
    private String loginKind;
    /** The ndus value currently being checked, so it is not submitted twice. */
    private String loginTriedSession;
    private boolean loginSubmitting;

    private ValueCallback<Uri[]> filePicker;
    private boolean returningFromBrowser;
    private boolean loaded;

    private final Handler ui = new Handler(Looper.getMainLooper());
    private final ExecutorService io = Executors.newCachedThreadPool();

    private static final int REQ_FILE = 100;
    private static final int REQ_NOTIFICATIONS = 101;

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);

        root = new FrameLayout(this);
        root.setBackgroundColor(getColor(R.color.bg));

        web = buildWebView();
        root.addView(web, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));

        splash = buildSplash();
        root.addView(splash, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));

        setContentView(root);

        requestNotificationPermission();
        requestAllFilesAccessOnFirstRun();
        registerBackHandler();

        ServerService.setLogListener(this);
        startForegroundService(new Intent(this, ServerService.class));
        waitForServer();
    }

    // --- UI construction (no layout XML: it is one screen) ---

    private WebView buildWebView() {
        WebView w = new WebView(this);
        w.setBackgroundColor(getColor(R.color.bg));

        WebSettings s = w.getSettings();
        s.setJavaScriptEnabled(true);
        s.setDomStorageEnabled(true);
        // The page is served from our own process; there is no third party to
        // isolate from, and this keeps EventSource and fetch working normally.
        s.setMediaPlaybackRequiresUserGesture(false);
        s.setSupportZoom(false);
        s.setUseWideViewPort(true);
        s.setLoadWithOverviewMode(true);
        CookieManager.getInstance().setAcceptCookie(true);

        w.setWebViewClient(new WebViewClient() {
            @Override
            public boolean shouldOverrideUrlLoading(WebView view, WebResourceRequest req) {
                Uri url = req.getUrl();
                if (isLocal(url)) {
                    return false;
                }
                // Google and Microsoft both refuse OAuth inside an embedded
                // WebView ("disallowed_useragent"), so sign-in has to happen in
                // the real browser. The redirect lands back on our loopback
                // server, which the browser can reach on the same device.
                openExternally(url);
                return true;
            }
        });

        w.setWebChromeClient(new WebChromeClient() {
            @Override
            public boolean onShowFileChooser(WebView view, ValueCallback<Uri[]> cb,
                                             FileChooserParams params) {
                if (filePicker != null) {
                    filePicker.onReceiveValue(null);
                }
                filePicker = cb;
                try {
                    Intent pick = new Intent(Intent.ACTION_OPEN_DOCUMENT)
                            .addCategory(Intent.CATEGORY_OPENABLE)
                            .setType("*/*")
                            .putExtra(Intent.EXTRA_ALLOW_MULTIPLE, true);
                    startActivityForResult(pick, REQ_FILE);
                    return true;
                } catch (Exception e) {
                    filePicker = null;
                    toast("No file picker available");
                    return false;
                }
            }
        });

        // The page is served from our own process over loopback, so exposing a
        // few platform calls to it is safe: nothing remote can ever reach this
        // interface, because non-local URLs are handed to the browser instead.
        w.addJavascriptInterface(new AndroidBridge(), "OmniDriveAndroid");

        w.setDownloadListener(new DownloadListener() {
            @Override
            public void onDownloadStart(String url, String userAgent, String disposition,
                                        String mimeType, long length) {
                // Nothing in here may throw: this callback runs on the UI
                // thread with no framework handler, so an exception takes the
                // whole app down. A blob: URL did exactly that — it is an
                // opaque URI, and getQueryParameter throws on those.
                String name = null;
                try {
                    // Prefer the name the server states, then the one in the
                    // URL. guessFileName is the last resort because it invents
                    // ".bin" whenever the type is application/octet-stream.
                    name = filenameFromDisposition(disposition);
                    if (name == null) {
                        Uri parsed = Uri.parse(url);
                        if (parsed.isHierarchical()) {
                            name = parsed.getQueryParameter("name");
                        }
                    }
                    if (name == null || name.isEmpty()) {
                        name = URLUtil.guessFileName(url, disposition, mimeType);
                    }
                } catch (Exception e) {
                    name = "download";
                }
                if (!url.startsWith("http://") && !url.startsWith("https://")) {
                    // Only the local server can be fetched; a blob: or data:
                    // URL belongs to the page and cannot be re-requested.
                    toast("Could not start that download");
                    return;
                }
                // The system DownloadManager runs in another process and cannot
                // be relied on to reach our loopback server, so we fetch it
                // ourselves and hand the bytes to MediaStore.
                saveDownload(url, name, mimeType);
            }
        });
        return w;
    }

    private LinearLayout buildSplash() {
        LinearLayout box = new LinearLayout(this);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setGravity(Gravity.CENTER);
        box.setBackgroundColor(getColor(R.color.bg));
        int pad = dp(24);
        box.setPadding(pad, pad, pad, pad);

        TextView title = new TextView(this);
        title.setText(R.string.starting);
        title.setTextColor(getColor(R.color.text));
        title.setTextSize(TypedValue.COMPLEX_UNIT_SP, 17);
        title.setGravity(Gravity.CENTER);
        box.addView(title);

        logToggle = new Button(this);
        logToggle.setText(R.string.show_log);
        logToggle.setAllCaps(false);
        logToggle.setBackgroundColor(getColor(R.color.surface));
        logToggle.setTextColor(getColor(R.color.muted));
        logToggle.setOnClickListener(v -> {
            boolean visible = logScroll.getVisibility() == View.VISIBLE;
            logScroll.setVisibility(visible ? View.GONE : View.VISIBLE);
        });
        LinearLayout.LayoutParams tp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        tp.topMargin = dp(16);
        box.addView(logToggle, tp);

        logView = new TextView(this);
        logView.setTextColor(getColor(R.color.muted));
        logView.setTextSize(TypedValue.COMPLEX_UNIT_SP, 11);
        logView.setTypeface(android.graphics.Typeface.MONOSPACE);
        logView.setTextIsSelectable(true);

        logScroll = new ScrollView(this);
        logScroll.addView(logView);
        logScroll.setVisibility(View.GONE);
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, dp(260));
        lp.topMargin = dp(12);
        box.addView(logScroll, lp);

        renderLog(String.join("\n", ServerService.logSnapshot()));
        return box;
    }

    private int dp(int value) {
        return Math.round(value * getResources().getDisplayMetrics().density);
    }

    // --- server startup ---

    private void waitForServer() {
        io.execute(() -> {
            final boolean ready = ServerService.waitUntilReady(30_000);
            ui.post(() -> {
                if (ready) {
                    loaded = true;
                    web.loadUrl(ServerService.BASE_URL + "/");
                    splash.setVisibility(View.GONE);
                    maybeAddDeviceStorage();
                } else {
                    // Surface the log rather than leaving a blank screen: the
                    // reason is almost always in the server's own output.
                    logScroll.setVisibility(View.VISIBLE);
                    toast("OmniDrive did not start — see the log");
                }
            });
        });
    }

    @Override
    public void onLogLine(String line) {
        ui.post(() -> {
            renderLog(logView.getText() + "\n" + line);
            logScroll.post(() -> logScroll.fullScroll(View.FOCUS_DOWN));
        });
    }

    private void renderLog(String text) {
        logView.setText(text.trim());
    }

    // --- navigation ---

    private boolean isLocal(Uri url) {
        String host = url.getHost();
        return host != null && (host.equals("127.0.0.1") || host.equals("localhost"));
    }

    private void openExternally(Uri url) {
        try {
            returningFromBrowser = true;
            startActivity(new Intent(Intent.ACTION_VIEW, url)
                    .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK));
        } catch (Exception e) {
            returningFromBrowser = false;
            toast("No browser app found");
        }
    }

    /**
     * Browsing the device is the point of a file manager, so ask for the
     * permission that enables it as soon as the app opens — but only once.
     * Android offers no in-app dialog for this one: it must be a visit to a
     * system settings screen.
     */
    private void requestAllFilesAccessOnFirstRun() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.R) return;
        if (Environment.isExternalStorageManager()) return;

        SharedPreferences prefs = getSharedPreferences("omnidrive", MODE_PRIVATE);
        if (prefs.getBoolean("asked_all_files", false)) return;
        prefs.edit().putBoolean("asked_all_files", true).apply();

        // A moment's delay so the app is visibly open first; being thrown
        // straight into system settings from a blank screen reads as a crash.
        ui.postDelayed(() -> {
            toast("Allow storage access so OmniDrive can see your files");
            new AndroidBridge().requestAllFilesAccess();
        }, 900);
    }

    @Override
    protected void onResume() {
        super.onResume();
        if (returningFromBrowser && loaded) {
            // The user may have just finished an OAuth sign-in in the browser,
            // in which case there is a new drive the page does not know about.
            returningFromBrowser = false;
            web.reload();
        }
        // Coming back from the permission screen: connect the device storage
        // now that it is actually readable.
        maybeAddDeviceStorage();
    }

    private boolean storageAdded;

    private void maybeAddDeviceStorage() {
        if (storageAdded || !loaded) return;
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R
                && !Environment.isExternalStorageManager()) {
            return;
        }
        storageAdded = true;
        io.execute(() -> {
            HttpURLConnection conn = null;
            try {
                conn = (HttpURLConnection) new URL(
                        ServerService.BASE_URL + "/api/storage/add").openConnection();
                conn.setRequestMethod("POST");
                conn.setConnectTimeout(5_000);
                conn.setReadTimeout(15_000);
                conn.setDoOutput(true);
                conn.getOutputStream().close();

                if (conn.getResponseCode() == 200 && web != null) {
                    ui.post(() -> {
                        if (loaded) web.reload();
                    });
                }
            } catch (Exception e) {
                // Not fatal: the drive can still be added from the Drives tab.
                storageAdded = false;
            } finally {
                if (conn != null) conn.disconnect();
            }
        });
    }

    /**
     * Android 16 (target SDK 36) enables predictive back, which stops calling
     * onBackPressed(). Register the modern callback where it exists and fall
     * back to the old path everywhere else.
     */
    private void registerBackHandler() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            getOnBackInvokedDispatcher().registerOnBackInvokedCallback(
                    android.window.OnBackInvokedDispatcher.PRIORITY_DEFAULT,
                    this::handleBack);
        }
    }

    /**
     * Back navigation.
     *
     * The page is asked directly whether it has somewhere to go, rather than
     * inferring it from WebView.canGoBack(). That method reflects the
     * WebBackForwardList, which does not dependably include history.pushState
     * entries across WebView versions — so a single-page app can be sitting
     * several screens deep while canGoBack() reports false, and back closes
     * the whole app. Asking the page removes the guesswork entirely.
     */
    private void handleBack() {
        // A sign-in page is a screen of its own: back closes it rather than
        // navigating the app underneath, which the user cannot even see.
        if (loginOverlay != null) {
            finishWebLogin(false, getString(R.string.sign_in_cancelled));
            return;
        }
        if (web == null || !loaded) {
            finish();
            return;
        }
        web.evaluateJavascript("(function(){try{return !!(window.omniBack&&window.omniBack())}catch(e){return false}})()",
                value -> {
                    if ("true".equals(value)) {
                        return; // the page navigated itself
                    }
                    if (web.canGoBack()) {
                        web.goBack();
                    } else {
                        finish();
                    }
                });
    }

    /**
     * The legacy back path, still reached on older devices and on any build
     * where the predictive-back callback is not delivered.
     *
     * This deliberately never calls super: doing so finishes the activity, and
     * if the modern callback is not firing — which is what happens when
     * android:enableOnBackInvokedCallback is missing from the manifest — back
     * would close the whole app from any screen instead of navigating up.
     */
    @Override
    @SuppressWarnings("deprecation")
    public void onBackPressed() {
        handleBack();
    }

    // --- file picker result ---

    @Override
    protected void onActivityResult(int requestCode, int resultCode, Intent data) {
        super.onActivityResult(requestCode, resultCode, data);
        if (requestCode != REQ_FILE) return;

        if (filePicker == null) return;
        Uri[] result = null;

        if (resultCode == RESULT_OK && data != null) {
            if (data.getClipData() != null) {
                int n = data.getClipData().getItemCount();
                result = new Uri[n];
                for (int i = 0; i < n; i++) {
                    result[i] = data.getClipData().getItemAt(i).getUri();
                }
            } else if (data.getData() != null) {
                result = new Uri[]{data.getData()};
            }
        }
        filePicker.onReceiveValue(result);
        filePicker = null;
    }

    // --- downloads ---

    /**
     * Streams a file from the local server into the public Downloads folder.
     * MediaStore means no storage permission is needed on Android 10+.
     */
    /** Extracts a filename from a Content-Disposition header, or null. */
    private static String filenameFromDisposition(String disposition) {
        if (disposition == null) return null;
        // RFC 5987 form first: it carries the unmangled UTF-8 name.
        java.util.regex.Matcher m = java.util.regex.Pattern
                .compile("filename\\*=UTF-8''([^;]+)", java.util.regex.Pattern.CASE_INSENSITIVE)
                .matcher(disposition);
        if (m.find()) {
            try {
                return java.net.URLDecoder.decode(m.group(1), "UTF-8");
            } catch (Exception ignored) {
                // Fall through to the plain form.
            }
        }
        m = java.util.regex.Pattern
                .compile("filename=\"?([^\";]+)\"?", java.util.regex.Pattern.CASE_INSENSITIVE)
                .matcher(disposition);
        return m.find() ? m.group(1) : null;
    }

    private void saveDownload(String url, String name, String mimeType) {
        toast("Downloading " + name);
        io.execute(() -> {
            HttpURLConnection conn = null;
            try {
                conn = (HttpURLConnection) new URL(url).openConnection();
                conn.setConnectTimeout(15_000);
                conn.setReadTimeout(0); // large files can take a while to arrive
                if (conn.getResponseCode() / 100 != 2) {
                    throw new IllegalStateException("server returned HTTP " + conn.getResponseCode());
                }

                ContentValues values = new ContentValues();
                values.put(MediaStore.Downloads.DISPLAY_NAME, name);
                if (mimeType != null && !mimeType.isEmpty()) {
                    values.put(MediaStore.Downloads.MIME_TYPE, mimeType);
                }
                values.put(MediaStore.Downloads.RELATIVE_PATH, Environment.DIRECTORY_DOWNLOADS);
                values.put(MediaStore.Downloads.IS_PENDING, 1);

                Uri target = getContentResolver().insert(
                        MediaStore.Downloads.EXTERNAL_CONTENT_URI, values);
                if (target == null) {
                    throw new IllegalStateException("could not create the download entry");
                }

                try (InputStream in = conn.getInputStream();
                     OutputStream out = getContentResolver().openOutputStream(target)) {
                    if (out == null) {
                        throw new IllegalStateException("could not open the download for writing");
                    }
                    byte[] buf = new byte[64 * 1024];
                    int n;
                    while ((n = in.read(buf)) > 0) {
                        out.write(buf, 0, n);
                    }
                }

                // Clearing IS_PENDING publishes the file to other apps.
                values.clear();
                values.put(MediaStore.Downloads.IS_PENDING, 0);
                getContentResolver().update(target, values, null, null);

                ui.post(() -> toast("Saved to Downloads: " + name));
            } catch (Exception e) {
                ui.post(() -> toast("Download failed: " + e.getMessage()));
            } finally {
                if (conn != null) conn.disconnect();
            }
        });
    }

    // --- provider sign-in pages ---

    /**
     * Sign-in for providers that offer no OAuth at all.
     *
     * TeraBox is the case this exists for: its 1 TB accounts authenticate with
     * an ordinary session cookie, exactly as its own app does, and there is no
     * developer console or client ID anywhere in the flow. The page below is
     * TeraBox's own, opened in a WebView we own so that the cookie it sets can
     * be read and handed to the local server — the user never sees it.
     *
     * This is the one place the app deliberately embeds a third-party page.
     * OAuth sign-ins go to the real browser instead (see buildWebView), because
     * Google and Microsoft refuse to serve them here; a cookie login has no
     * redirect to come back on, so there is nothing for a browser to hand over.
     */
    private static final String TERABOX_LOGIN = "https://www.1024terabox.com/login";

    /**
     * The sign-in page is loaded as a desktop browser, on purpose.
     *
     * TeraBox redirects every mobile User-Agent to /wap, which is an
     * advertisement for its own Android app and offers no way to sign in. The
     * identical request from a desktop browser gets the real login form. So the
     * usual trick of taking Android's WebView string and removing the "; wv"
     * marker is not enough here — the whole string has to be a desktop one.
     */
    private static final String TERABOX_LOGIN_UA =
            "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
                    + "(KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36";

    /**
     * Interchangeable TeraBox front-ends; the session may land on any of them.
     *
     * Order matters: TeraBox has moved to www.1024terabox.com and the original
     * www.terabox.com no longer resolves on many networks, so leading with it
     * would mean a timeout on the way to a page that does load. It stays in the
     * list because the cookie has to be cleared from wherever it was set.
     */
    private static final String[] TERABOX_DOMAINS = {
            "https://www.1024terabox.com",
            "https://www.1024tera.com",
            "https://www.terabox.app",
            "https://www.terabox.com",
            "https://www.4funbox.com",
            "https://www.mirrobox.com",
    };

    private void startWebLogin(String kind, String label) {
        if (loginOverlay != null) return; // one at a time
        loginKind = (kind == null || kind.isEmpty()) ? "terabox" : kind;
        loginTriedSession = null;
        loginSubmitting = false;

        LinearLayout box = new LinearLayout(this);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setBackgroundColor(getColor(R.color.bg));
        // Consume taps: without this they fall through to the page underneath.
        box.setClickable(true);

        LinearLayout bar = new LinearLayout(this);
        bar.setOrientation(LinearLayout.HORIZONTAL);
        bar.setGravity(Gravity.CENTER_VERTICAL);
        bar.setBackgroundColor(getColor(R.color.surface));
        int pad = dp(12);
        bar.setPadding(pad, pad, pad, pad);

        TextView title = new TextView(this);
        title.setText(getString(R.string.sign_in_to,
                (label == null || label.isEmpty()) ? "your drive" : label));
        title.setTextColor(getColor(R.color.text));
        title.setTextSize(TypedValue.COMPLEX_UNIT_SP, 15);
        bar.addView(title, new LinearLayout.LayoutParams(
                0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));

        Button cancel = new Button(this);
        cancel.setText(R.string.cancel);
        cancel.setAllCaps(false);
        cancel.setBackgroundColor(getColor(R.color.surface));
        cancel.setTextColor(getColor(R.color.muted));
        cancel.setOnClickListener(v -> finishWebLogin(false, getString(R.string.sign_in_cancelled)));
        bar.addView(cancel);

        box.addView(bar, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));

        // A running commentary, because everything this screen waits for is
        // invisible: a cookie appearing, and a server accepting it. Without it a
        // refusal looks like nothing happening.
        loginStatus = new TextView(this);
        loginStatus.setText(R.string.sign_in_waiting);
        loginStatus.setTextColor(getColor(R.color.muted));
        loginStatus.setTextSize(TypedValue.COMPLEX_UNIT_SP, 12);
        loginStatus.setBackgroundColor(getColor(R.color.surface));
        loginStatus.setPadding(pad, 0, pad, dp(8));
        box.addView(loginStatus, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));

        WebView login = new WebView(this);
        login.setBackgroundColor(getColor(R.color.bg));
        WebSettings s = login.getSettings();
        s.setJavaScriptEnabled(true);
        s.setDomStorageEnabled(true);
        // A desktop page in a phone-width WebView: scale it to fit, and leave
        // pinch-zoom available rather than trapping the user at that scale.
        s.setUseWideViewPort(true);
        s.setLoadWithOverviewMode(true);
        s.setSupportZoom(true);
        s.setBuiltInZoomControls(true);
        s.setDisplayZoomControls(false);
        // See TERABOX_LOGIN_UA: a mobile string lands on an app advertisement
        // instead of a login form. A desktop string also drops the "; wv"
        // marker that identity providers use to refuse sign-in inside an app.
        // Signing in with an email and password always works; a Google or Apple
        // button may still be declined, which is why the guide says so.
        s.setUserAgentString(TERABOX_LOGIN_UA);

        CookieManager cookies = CookieManager.getInstance();
        cookies.setAcceptCookie(true);
        cookies.setAcceptThirdPartyCookies(login, true);

        login.setWebChromeClient(new WebChromeClient());
        login.setWebViewClient(new WebViewClient() {
            @Override
            public void onPageFinished(WebView view, String url) {
                captureSession();
            }

            @Override
            public void doUpdateVisitedHistory(WebView view, String url, boolean isReload) {
                // A single-page login never fires onPageFinished again after the
                // sign-in call, so the history change is the only signal that
                // anything happened.
                captureSession();
            }
        });
        login.loadUrl(TERABOX_LOGIN);

        box.addView(login, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, 0, 1f));

        loginOverlay = box;
        root.addView(box, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
        pollForSession();
    }

    /**
     * Watches for a finished session.
     *
     * "ndus" is the cookie TeraBox sets when sign-in succeeds; there is no
     * callback URL to wait for, so its appearance is the only signal available.
     * The awkward part is that a cookie by that name can also be present before
     * anyone has signed in, and it can land on a different one of TeraBox's
     * interchangeable hosts than the page is being served from. So rather than
     * trusting the first thing that looks right and closing:
     *
     *   - every known host is checked, not just the first with a match;
     *   - values too short to be a session are ignored;
     *   - a value already refused by the server is not sent again;
     *   - a refusal leaves the page open, because the usual reason for one is
     *     that the user has not finished signing in yet.
     *
     * The earlier version did the opposite of all four and reported "your
     * sign-in expired" to people who had just signed in.
     */
    private void captureSession() {
        if (loginOverlay == null || loginSubmitting) return;

        CookieManager cookies = CookieManager.getInstance();
        for (String domain : TERABOX_DOMAINS) {
            String jar = cookies.getCookie(domain);
            String session = sessionValue(jar);
            if (session == null || session.equals(loginTriedSession)) continue;

            loginTriedSession = session;
            loginSubmitting = true;
            cookies.flush();
            submitSession(jar, domain);
            return;
        }
    }

    /** The ndus value in a cookie jar, or null if there is no usable one. */
    private static String sessionValue(String jar) {
        if (jar == null) return null;
        for (String pair : jar.split(";")) {
            String[] kv = pair.split("=", 2);
            if (kv.length != 2 || !"ndus".equals(kv[0].trim())) continue;
            String v = kv[1].trim();
            // A signed-out visitor can hold a stub by the same name. Real
            // values are opaque and around 40 characters.
            return v.length() >= 16 ? v : null;
        }
        return null;
    }

    /**
     * Keeps checking while the page is open. A login page that signs in without
     * navigating sets the cookie with no event for us to hang off, so polling is
     * the only thing that catches every case.
     */
    private void pollForSession() {
        if (loginOverlay == null) return;
        captureSession();
        ui.postDelayed(this::pollForSession, 1200);
    }

    private void setLoginStatus(String text) {
        if (loginStatus != null) loginStatus.setText(text);
    }

    /** Hands the captured session to the local server, which validates it. */
    private void submitSession(String jar, String domain) {
        setLoginStatus(getString(R.string.signing_in));
        final String body = "{\"kind\":\"" + jsonEscape(loginKind) + "\",\"fields\":{"
                + "\"cookie\":\"" + jsonEscape(jar) + "\","
                + "\"domain\":\"" + jsonEscape(domain) + "\"}}";

        io.execute(() -> {
            HttpURLConnection conn = null;
            String error = null;
            try {
                conn = (HttpURLConnection) new URL(
                        ServerService.BASE_URL + "/api/connect/direct").openConnection();
                conn.setRequestMethod("POST");
                conn.setRequestProperty("Content-Type", "application/json");
                conn.setConnectTimeout(10_000);
                // Connecting makes a real call to the provider before it saves
                // anything, so this is slower than a local request looks.
                conn.setReadTimeout(60_000);
                conn.setDoOutput(true);
                try (OutputStream out = conn.getOutputStream()) {
                    out.write(body.getBytes("UTF-8"));
                }
                if (conn.getResponseCode() / 100 != 2) {
                    error = readErrorBody(conn);
                }
            } catch (Exception e) {
                error = e.getMessage();
            } finally {
                if (conn != null) conn.disconnect();
            }

            final String failure = error;
            ui.post(() -> {
                loginSubmitting = false;
                if (failure == null) {
                    finishWebLogin(true, null);
                    return;
                }
                // Do not close. The overwhelmingly common reason the server
                // refuses is that sign-in has not finished yet, and closing the
                // page makes that unrecoverable — the user would have to start
                // again and hit exactly the same thing.
                setLoginStatus(getString(R.string.sign_in_retry, failure));
            });
        });
    }

    /** Reads the server's error text, which says what the provider objected to. */
    private static String readErrorBody(HttpURLConnection conn) {
        try (InputStream in = conn.getErrorStream()) {
            if (in == null) return "sign-in failed (HTTP " + conn.getResponseCode() + ")";
            java.io.ByteArrayOutputStream buf = new java.io.ByteArrayOutputStream();
            byte[] chunk = new byte[4096];
            int n;
            while ((n = in.read(chunk)) > 0 && buf.size() < 8192) {
                buf.write(chunk, 0, n);
            }
            String text = buf.toString("UTF-8");
            // The server answers {"error":"..."}; show that rather than JSON.
            java.util.regex.Matcher m = java.util.regex.Pattern
                    .compile("\"error\"\\s*:\\s*\"(.*?)\"", java.util.regex.Pattern.DOTALL)
                    .matcher(text);
            return m.find() ? m.group(1).replace("\\\"", "\"") : text;
        } catch (Exception e) {
            return "sign-in failed";
        }
    }

    /** Tears the overlay down and tells the page how it went. */
    private void finishWebLogin(boolean ok, String message) {
        ViewGroup overlay = loginOverlay;
        loginOverlay = null; // also stops pollForSession
        loginStatus = null;
        loginSubmitting = false;
        if (overlay != null) {
            WebView inner = null;
            for (int i = 0; i < overlay.getChildCount(); i++) {
                if (overlay.getChildAt(i) instanceof WebView) {
                    inner = (WebView) overlay.getChildAt(i);
                }
            }
            root.removeView(overlay);
            if (inner != null) {
                // Detach before destroying: a WebView destroyed while still in a
                // view hierarchy takes the process with it.
                overlay.removeView(inner);
                inner.destroy();
            }
        }
        if (web == null || !loaded) return;
        web.evaluateJavascript(
                "window.omniWebLoginDone && window.omniWebLoginDone("
                        + jsString(loginKind) + "," + ok + "," + jsString(message) + ")", null);
    }

    private static String jsonEscape(String s) {
        if (s == null) return "";
        StringBuilder out = new StringBuilder(s.length() + 16);
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            switch (c) {
                case '"': out.append("\\\""); break;
                case '\\': out.append("\\\\"); break;
                case '\n': out.append("\\n"); break;
                case '\r': out.append("\\r"); break;
                case '\t': out.append("\\t"); break;
                default:
                    if (c < 0x20) out.append(String.format("\\u%04x", (int) c));
                    else out.append(c);
            }
        }
        return out.toString();
    }

    /** A JSON string is also a valid JavaScript string literal. */
    private static String jsString(String s) {
        return s == null ? "null" : "\"" + jsonEscape(s) + "\"";
    }

    // --- bridge to the platform ---

    /**
     * The small set of things the web UI cannot do for itself. Kept
     * deliberately tiny: every method here is reachable by the page.
     */
    public class AndroidBridge {

        /** True once the user has granted "All files access". */
        @android.webkit.JavascriptInterface
        public boolean hasAllFilesAccess() {
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                return Environment.isExternalStorageManager();
            }
            return true; // pre-Android 11 the normal storage permission suffices
        }

        /**
         * Opens the system screen that grants it. Android deliberately offers
         * no in-app dialog for this permission — it must be a settings visit.
         */
        @android.webkit.JavascriptInterface
        public void requestAllFilesAccess() {
            ui.post(() -> {
                if (Build.VERSION.SDK_INT < Build.VERSION_CODES.R) return;
                try {
                    startActivity(new Intent(
                            Settings.ACTION_MANAGE_APP_ALL_FILES_ACCESS_PERMISSION,
                            Uri.parse("package:" + getPackageName())));
                } catch (Exception e) {
                    // Some OEM builds omit the per-app screen; fall back to the
                    // global list.
                    try {
                        startActivity(new Intent(Settings.ACTION_MANAGE_ALL_FILES_ACCESS_PERMISSION));
                    } catch (Exception ignored) {
                        toast("Could not open the storage permission screen");
                    }
                }
            });
        }

        @android.webkit.JavascriptInterface
        public String platform() {
            return "android-" + Build.VERSION.SDK_INT;
        }

        /**
         * Opens a web page in the real browser.
         *
         * The setup guides link out to developer consoles and sign-up pages,
         * and those are ordinary web links — not files. They used to be handed
         * to {@link #openExternal}, which wraps its argument in a content:// URI
         * from StreamProvider and expects four arguments; called with two it
         * matched no method at all, so tapping "Open this page" did nothing.
         */
        @android.webkit.JavascriptInterface
        public void openUrl(String url) {
            ui.post(() -> {
                if (url == null || !(url.startsWith("http://") || url.startsWith("https://"))) {
                    toast("That link cannot be opened");
                    return;
                }
                openExternally(Uri.parse(url));
            });
        }

        /**
         * Opens a provider's own sign-in page for the drives that have no OAuth
         * — TeraBox today. The page reads this method's existence to decide
         * whether to offer in-app sign-in or ask for a pasted cookie.
         */
        @android.webkit.JavascriptInterface
        public void webLogin(String kind, String label) {
            ui.post(() -> startWebLogin(kind, label));
        }

        /**
         * Signs out of such a provider by clearing the cookies its sign-in page
         * left behind. Without this, disconnecting a drive and connecting it
         * again would silently reuse the same session — so "sign out" would not
         * have signed anything out.
         */
        @android.webkit.JavascriptInterface
        public void clearWebLogin(String kind) {
            ui.post(() -> {
                CookieManager cookies = CookieManager.getInstance();
                for (String domain : TERABOX_DOMAINS) {
                    String jar = cookies.getCookie(domain);
                    if (jar == null) continue;
                    // Android offers no per-site clear, so each cookie is
                    // expired by name. Clearing the whole jar would also drop
                    // any other site the user has signed in to.
                    for (String pair : jar.split(";")) {
                        String name = pair.split("=", 2)[0].trim();
                        if (!name.isEmpty()) {
                            cookies.setCookie(domain, name + "=; Max-Age=0; Path=/");
                        }
                    }
                }
                cookies.flush();
            });
        }

        /**
         * Hands a stream URL to a real player. VLC, MX Player and the rest open
         * http:// URLs directly, so a codec the WebView cannot decode still
         * plays — and still streams, rather than downloading first.
         */
        /**
         * Saves a file under its real name.
         *
         * The page knows the filename exactly; WebView's own DownloadListener
         * does not, and URLUtil.guessFileName falls back to inventing one from
         * the MIME type — which is how an APK arrived as "download.bin".
         */
        @android.webkit.JavascriptInterface
        public void download(String url, String name, String mime) {
            ui.post(() -> saveDownload(url, name, mime));
        }

        /**
         * Opens a file with any app on the device.
         *
         * The URI handed out is content://, not the http:// address of the
         * local server. That distinction is the whole point: ACTION_VIEW
         * matches on scheme first, so an http URL only ever offers browsers,
         * however exact the MIME type. Players and viewers register for
         * content:// — the same URIs a file manager hands them.
         */
        /**
         * Shares files with any app that accepts them — Bluetooth, a chat app,
         * email, another cloud client.
         *
         * The URIs are content:// from {@link StreamProvider}, which is what
         * receivers expect; an http:// URL would only be treated as a link.
         * Each argument is a "url|name|size|mime" record, newline-separated.
         */
        /**
         * Passes a public download link to any app that takes text — a chat
         * app, email, a QR generator.
         *
         * This is deliberately plain text rather than a content:// URI: the
         * point of a share link is that the recipient fetches the file from the
         * cloud provider themselves, so nothing is transferred from this phone.
         */
        @android.webkit.JavascriptInterface
        public void shareText(String text, String subject) {
            ui.post(() -> {
                try {
                    Intent send = new Intent(Intent.ACTION_SEND);
                    send.setType("text/plain");
                    send.putExtra(Intent.EXTRA_TEXT, text);
                    if (subject != null && !subject.isEmpty()) {
                        send.putExtra(Intent.EXTRA_SUBJECT, subject);
                    }
                    Intent chooser = Intent.createChooser(send, "Share link");
                    chooser.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
                    startActivity(chooser);
                } catch (Exception e) {
                    toast("Could not share: " + e.getMessage());
                }
            });
        }

        @android.webkit.JavascriptInterface
        public void shareFiles(String records) {
            ui.post(() -> {
                try {
                    ArrayList<Uri> uris = new ArrayList<>();
                    String type = null;
                    for (String line : records.split("\n")) {
                        String[] parts = line.split("\\|", 4);
                        if (parts.length < 4) continue;
                        long size = 0;
                        try {
                            size = Long.parseLong(parts[2]);
                        } catch (Exception ignored) {
                            // Size is advisory for sharing.
                        }
                        uris.add(StreamProvider.uriFor(parts[0], parts[1], size, parts[3]));
                        // Mixed selections fall back to the generic type.
                        if (type == null) type = parts[3];
                        else if (!type.equals(parts[3])) type = "*/*";
                    }
                    if (uris.isEmpty()) {
                        toast("Nothing to share");
                        return;
                    }

                    Intent send;
                    if (uris.size() == 1) {
                        send = new Intent(Intent.ACTION_SEND);
                        send.putExtra(Intent.EXTRA_STREAM, uris.get(0));
                    } else {
                        send = new Intent(Intent.ACTION_SEND_MULTIPLE);
                        send.putParcelableArrayListExtra(Intent.EXTRA_STREAM, uris);
                    }
                    send.setType(type == null ? "*/*" : type);
                    send.addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION);

                    Intent chooser = Intent.createChooser(send, "Share");
                    chooser.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK
                            | Intent.FLAG_GRANT_READ_URI_PERMISSION);
                    startActivity(chooser);
                } catch (Exception e) {
                    toast("Could not share that");
                }
            });
        }

        @android.webkit.JavascriptInterface
        public void openExternal(String url, String name, String sizeText, String mime) {
            ui.post(() -> {
                try {
                    long size = 0;
                    try {
                        size = Long.parseLong(sizeText);
                    } catch (Exception ignored) {
                        // Unknown length: players cope, seeking is just limited.
                    }
                    String type = (mime == null || mime.isEmpty()) ? "*/*" : mime;
                    Uri content = StreamProvider.uriFor(url, name, size, type);

                    Intent view = new Intent(Intent.ACTION_VIEW);
                    view.setDataAndType(content, type);
                    view.addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION
                            | Intent.FLAG_ACTIVITY_NEW_TASK);

                    Intent chooser = Intent.createChooser(view, "Open with");
                    chooser.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK
                            | Intent.FLAG_GRANT_READ_URI_PERMISSION);
                    startActivity(chooser);
                } catch (Exception e) {
                    toast("No app available to open this");
                }
            });
        }
    }

    // --- misc ---

    private void requestNotificationPermission() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU
                && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS)
                != PackageManager.PERMISSION_GRANTED) {
            // Denial is survivable: the service still runs, the user just does
            // not get the ongoing notification with its Stop button.
            requestPermissions(new String[]{Manifest.permission.POST_NOTIFICATIONS}, REQ_NOTIFICATIONS);
        }
    }

    private void toast(String msg) {
        Toast.makeText(this, msg, Toast.LENGTH_SHORT).show();
    }

    @Override
    protected void onDestroy() {
        ServerService.setLogListener(null);
        io.shutdownNow();
        if (web != null) {
            web.destroy();
        }
        super.onDestroy();
    }
}
