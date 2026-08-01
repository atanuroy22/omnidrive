package com.omnidrive.app;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.net.ConnectivityManager;
import android.net.LinkProperties;
import android.net.Network;
import android.os.Build;
import android.os.IBinder;
import android.os.PowerManager;
import android.util.Log;

import java.io.BufferedReader;
import java.io.File;
import java.io.InputStreamReader;
import java.net.HttpURLConnection;
import java.net.InetAddress;
import java.net.URL;
import java.util.ArrayDeque;
import java.util.ArrayList;
import java.util.Deque;
import java.util.List;

/**
 * Runs the OmniDrive server as a child process and keeps it alive while the
 * user is in other apps.
 *
 * The server binary ships inside the APK as {@code libomnidrive.so}. That
 * naming is not cosmetic: Android only extracts files matching {@code lib*.so}
 * into the app's native library directory, and since Android 10 that directory
 * is the only place an app is permitted to execute a binary from. Copying the
 * same file into {@code getFilesDir()} and running it there fails with
 * "Permission denied" on every modern device.
 */
public class ServerService extends Service {

    public static final String ACTION_STOP = "com.omnidrive.app.STOP";
    public static final int PORT = 8787;
    public static final String BASE_URL = "http://127.0.0.1:" + PORT;

    private static final String TAG = "OmniDrive";
    private static final String CHANNEL_ID = "omnidrive_server";
    private static final int NOTIFICATION_ID = 1;
    private static final int MAX_LOG_LINES = 400;

    /** Log lines shared with the UI. Guarded by its own monitor. */
    private static final Deque<String> LOG = new ArrayDeque<>();

    /** Notified whenever a line is appended, so the log view can follow along. */
    public interface LogListener {
        void onLogLine(String line);
    }

    private static volatile LogListener listener;
    private static volatile boolean running;

    public static void setLogListener(LogListener l) {
        listener = l;
    }

    public static boolean isRunning() {
        return running;
    }

    public static List<String> logSnapshot() {
        synchronized (LOG) {
            return new ArrayList<>(LOG);
        }
    }

    private static void appendLog(String line) {
        synchronized (LOG) {
            LOG.addLast(line);
            while (LOG.size() > MAX_LOG_LINES) {
                LOG.removeFirst();
            }
        }
        LogListener l = listener;
        if (l != null) {
            l.onLogLine(line);
        }
        Log.i(TAG, line);
    }

    private Process process;
    private Thread reader;
    private PowerManager.WakeLock wakeLock;

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        if (intent != null && ACTION_STOP.equals(intent.getAction())) {
            stopSelf();
            return START_NOT_STICKY;
        }
        createChannel();
        startForeground(NOTIFICATION_ID, buildNotification());

        if (process == null) {
            startServer();
        }
        // START_STICKY: if Android reclaims memory under pressure, bring the
        // server back rather than leaving the UI pointing at a dead port.
        return START_STICKY;
    }

    private void startServer() {
        File binary = new File(getApplicationInfo().nativeLibraryDir, "libomnidrive.so");
        if (!binary.exists()) {
            appendLog("FATAL: server binary missing at " + binary);
            appendLog("This APK was built without a binary for this CPU (" + Build.SUPPORTED_ABIS[0] + ").");
            return;
        }

        File dataDir = new File(getFilesDir(), "omnidrive");
        //noinspection ResultOfMethodCallIgnored
        dataDir.mkdirs();

        List<String> cmd = new ArrayList<>();
        cmd.add(binary.getAbsolutePath());
        cmd.add("-data");
        cmd.add(dataDir.getAbsolutePath());
        cmd.add("-addr");
        cmd.add("127.0.0.1");   // loopback only; pairing opens its own port on demand
        cmd.add("-port");
        cmd.add(String.valueOf(PORT));

        try {
            ProcessBuilder pb = new ProcessBuilder(cmd);
            pb.redirectErrorStream(true);
            // HOME decides where the server would fall back to; point it at our
            // private storage so nothing is ever written outside the sandbox.
            pb.environment().put("HOME", getFilesDir().getAbsolutePath());
            pb.environment().put("TMPDIR", getCacheDir().getAbsolutePath());
            // Android has no /etc/resolv.conf, and a Go binary cannot ask
            // `getprop` for the nameservers: os/exec probes with faccessat2(2),
            // which Android's seccomp policy answers with SIGSYS and kills the
            // process. Look them up here, where there is a proper API for it.
            String dns = detectDnsServers();
            if (!dns.isEmpty()) {
                pb.environment().put("OMNIDRIVE_DNS", dns);
                appendLog("dns servers: " + dns);
            }
            process = pb.start();
            running = true;
            appendLog("started " + binary.getName() + " on port " + PORT);
        } catch (Exception e) {
            appendLog("FATAL: could not start the server: " + e);
            return;
        }

        acquireWakeLock();

        final Process p = process;
        reader = new Thread(() -> {
            try (BufferedReader in = new BufferedReader(new InputStreamReader(p.getInputStream()))) {
                String line;
                while ((line = in.readLine()) != null) {
                    appendLog(line);
                }
            } catch (Exception e) {
                appendLog("log stream ended: " + e);
            }
            try {
                int code = p.waitFor();
                running = false;
                appendLog("server exited with code " + code);
            } catch (InterruptedException ignored) {
                Thread.currentThread().interrupt();
            }
        }, "omnidrive-log");
        reader.setDaemon(true);
        reader.start();
    }

    /**
     * Returns the active network's DNS servers as a comma-separated list.
     * Requires only ACCESS_NETWORK_STATE, which is not a runtime permission.
     */
    private String detectDnsServers() {
        StringBuilder sb = new StringBuilder();
        try {
            ConnectivityManager cm = getSystemService(ConnectivityManager.class);
            if (cm == null) return "";
            Network active = cm.getActiveNetwork();
            if (active == null) return "";
            LinkProperties props = cm.getLinkProperties(active);
            if (props == null) return "";
            for (InetAddress addr : props.getDnsServers()) {
                String ip = addr.getHostAddress();
                if (ip == null || ip.isEmpty()) continue;
                if (sb.length() > 0) sb.append(',');
                sb.append(ip);
            }
        } catch (Exception e) {
            // Not fatal: the server falls back to public resolvers.
            appendLog("could not read system DNS: " + e);
        }
        return sb.toString();
    }

    /**
     * A partial wake lock keeps the CPU running so a large upload continues
     * with the screen off. Without it Doze suspends the process mid-transfer.
     */
    private void acquireWakeLock() {
        try {
            PowerManager pm = (PowerManager) getSystemService(Context.POWER_SERVICE);
            if (pm == null) return;
            wakeLock = pm.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "omnidrive:server");
            wakeLock.setReferenceCounted(false);
            wakeLock.acquire();
        } catch (Exception e) {
            appendLog("wake lock unavailable: " + e);
        }
    }

    @Override
    public void onDestroy() {
        running = false;
        if (wakeLock != null && wakeLock.isHeld()) {
            wakeLock.release();
        }
        if (process != null) {
            // Ask the server to shut down cleanly so an in-flight state write
            // completes; destroy() sends SIGTERM, which it handles.
            process.destroy();
            try {
                process.waitFor();
            } catch (InterruptedException ignored) {
                Thread.currentThread().interrupt();
            }
            process = null;
        }
        appendLog("service stopped");
        super.onDestroy();
    }

    private void createChannel() {
        NotificationManager nm = getSystemService(NotificationManager.class);
        if (nm == null) return;
        NotificationChannel channel = new NotificationChannel(
                CHANNEL_ID, getString(R.string.notif_channel), NotificationManager.IMPORTANCE_LOW);
        channel.setShowBadge(false);
        channel.setDescription(getString(R.string.notif_text));
        nm.createNotificationChannel(channel);
    }

    private Notification buildNotification() {
        Intent open = new Intent(this, MainActivity.class)
                .setFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP);
        PendingIntent openPI = PendingIntent.getActivity(
                this, 0, open, PendingIntent.FLAG_IMMUTABLE);

        Intent stop = new Intent(this, ServerService.class).setAction(ACTION_STOP);
        PendingIntent stopPI = PendingIntent.getService(
                this, 1, stop, PendingIntent.FLAG_IMMUTABLE);

        return new Notification.Builder(this, CHANNEL_ID)
                .setContentTitle(getString(R.string.notif_title))
                .setContentText(getString(R.string.notif_text))
                .setSmallIcon(android.R.drawable.stat_sys_upload_done)
                .setContentIntent(openPI)
                .addAction(new Notification.Action.Builder(
                        null, getString(R.string.action_stop), stopPI).build())
                .setOngoing(true)
                .setShowWhen(false)
                .build();
    }

    /**
     * Polls the server's health endpoint until it answers. The first launch
     * has to derive an encryption key, so "ready" is a moment or two after
     * "process started".
     */
    public static boolean waitUntilReady(int timeoutMs) {
        long deadline = System.currentTimeMillis() + timeoutMs;
        while (System.currentTimeMillis() < deadline) {
            try {
                HttpURLConnection c = (HttpURLConnection) new URL(BASE_URL + "/api/health").openConnection();
                c.setConnectTimeout(700);
                c.setReadTimeout(700);
                int code = c.getResponseCode();
                c.disconnect();
                if (code == 200) return true;
            } catch (Exception ignored) {
                // Not up yet; keep waiting.
            }
            try {
                Thread.sleep(150);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                return false;
            }
        }
        return false;
    }
}
