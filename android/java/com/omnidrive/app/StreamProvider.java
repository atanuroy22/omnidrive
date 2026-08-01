package com.omnidrive.app;

import android.content.ContentProvider;
import android.content.ContentValues;
import android.database.Cursor;
import android.database.MatrixCursor;
import android.net.Uri;
import android.os.Handler;
import android.os.HandlerThread;
import android.os.ParcelFileDescriptor;
import android.os.ProxyFileDescriptorCallback;
import android.os.storage.StorageManager;
import android.provider.OpenableColumns;
import android.system.ErrnoException;
import android.system.OsConstants;
import android.util.Base64;

import java.io.IOException;
import java.io.InputStream;
import java.net.HttpURLConnection;
import java.net.URL;

/**
 * Exposes files from the local server as {@code content://} URIs.
 *
 * This is what makes "Open with" show your actual apps. An ACTION_VIEW intent
 * carrying an {@code http://} URL matches on the *scheme* first, so only
 * browsers are offered however precise the MIME type is — video players,
 * document readers and image viewers register for content:// and file://, not
 * for http. Handing them a content URI puts OmniDrive on the same footing as
 * any file manager.
 *
 * Reads are served by {@link StorageManager#openProxyFileDescriptor}, so the
 * receiving app gets a *seekable* descriptor backed by HTTP range requests
 * against the local server. A player can jump to the middle of a film without
 * anything being downloaded first.
 */
public class StreamProvider extends ContentProvider {

    public static final String AUTHORITY = "com.omnidrive.app.stream";

    private static final String PARAM_URL = "u";
    private static final String PARAM_NAME = "n";
    private static final String PARAM_SIZE = "s";
    private static final String PARAM_TYPE = "t";

    private HandlerThread thread;
    private Handler handler;

    /**
     * Builds the content URI for a file served by the local HTTP server.
     * The upstream URL is base64-encoded so its own query string survives
     * intact.
     */
    public static Uri uriFor(String httpURL, String name, long size, String mime) {
        return new Uri.Builder()
                .scheme("content")
                .authority(AUTHORITY)
                .appendPath("file")
                // A trailing path segment with the real name: some apps derive
                // the file type from the URI's last segment rather than asking.
                .appendPath(name == null ? "file" : name)
                .appendQueryParameter(PARAM_URL,
                        Base64.encodeToString(httpURL.getBytes(), Base64.URL_SAFE | Base64.NO_WRAP))
                .appendQueryParameter(PARAM_NAME, name == null ? "file" : name)
                .appendQueryParameter(PARAM_SIZE, String.valueOf(size))
                .appendQueryParameter(PARAM_TYPE, mime == null ? "*/*" : mime)
                .build();
    }

    @Override
    public boolean onCreate() {
        thread = new HandlerThread("omnidrive-stream");
        thread.start();
        handler = new Handler(thread.getLooper());
        return true;
    }

    private static String upstream(Uri uri) {
        String encoded = uri.getQueryParameter(PARAM_URL);
        if (encoded == null) return null;
        return new String(Base64.decode(encoded, Base64.URL_SAFE | Base64.NO_WRAP));
    }

    @Override
    public String getType(Uri uri) {
        String t = uri.getQueryParameter(PARAM_TYPE);
        return t == null ? "*/*" : t;
    }

    /**
     * Apps ask for the display name and size before opening; without these
     * many show "unknown file" or refuse outright.
     */
    @Override
    public Cursor query(Uri uri, String[] projection, String selection,
                        String[] selectionArgs, String sortOrder) {
        String name = uri.getQueryParameter(PARAM_NAME);
        long size = parseSize(uri);

        String[] cols = projection != null ? projection
                : new String[]{OpenableColumns.DISPLAY_NAME, OpenableColumns.SIZE};
        MatrixCursor cursor = new MatrixCursor(cols, 1);
        Object[] row = new Object[cols.length];
        for (int i = 0; i < cols.length; i++) {
            if (OpenableColumns.DISPLAY_NAME.equals(cols[i])) row[i] = name;
            else if (OpenableColumns.SIZE.equals(cols[i])) row[i] = size;
        }
        cursor.addRow(row);
        return cursor;
    }

    private static long parseSize(Uri uri) {
        try {
            return Long.parseLong(uri.getQueryParameter(PARAM_SIZE));
        } catch (Exception e) {
            return 0;
        }
    }

    @Override
    public ParcelFileDescriptor openFile(Uri uri, String mode) throws java.io.FileNotFoundException {
        final String url = upstream(uri);
        if (url == null) throw new java.io.FileNotFoundException("no upstream URL");
        final long size = parseSize(uri);

        StorageManager sm = getContext().getSystemService(StorageManager.class);
        if (sm == null) throw new java.io.FileNotFoundException("no StorageManager");

        try {
            return sm.openProxyFileDescriptor(ParcelFileDescriptor.MODE_READ_ONLY,
                    new RangeCallback(url, size), handler);
        } catch (IOException e) {
            throw new java.io.FileNotFoundException("cannot open stream: " + e.getMessage());
        }
    }

    /** Serves reads by asking the local server for that byte range. */
    private static final class RangeCallback extends ProxyFileDescriptorCallback {
        private final String url;
        private final long size;

        RangeCallback(String url, long size) {
            this.url = url;
            this.size = size;
        }

        @Override
        public long onGetSize() {
            return size;
        }

        @Override
        public int onRead(long offset, int count, byte[] data) throws ErrnoException {
            if (size > 0 && offset >= size) return 0;

            HttpURLConnection conn = null;
            try {
                conn = (HttpURLConnection) new URL(url).openConnection();
                conn.setConnectTimeout(15_000);
                conn.setReadTimeout(60_000);
                conn.setRequestProperty("Range", "bytes=" + offset + "-" + (offset + count - 1));

                int status = conn.getResponseCode();
                if (status != HttpURLConnection.HTTP_PARTIAL && status != HttpURLConnection.HTTP_OK) {
                    throw new ErrnoException("onRead", OsConstants.EIO);
                }
                try (InputStream in = conn.getInputStream()) {
                    int total = 0;
                    while (total < count) {
                        int n = in.read(data, total, count - total);
                        if (n < 0) break;
                        total += n;
                    }
                    return total;
                }
            } catch (ErrnoException e) {
                throw e;
            } catch (Exception e) {
                throw new ErrnoException("onRead", OsConstants.EIO);
            } finally {
                if (conn != null) conn.disconnect();
            }
        }

        @Override
        public void onRelease() {
            // Nothing to clean up: each read owns its own connection.
        }
    }

    // This provider is read-only; the rest of the contract is unused.

    @Override
    public Uri insert(Uri uri, ContentValues values) {
        throw new UnsupportedOperationException("read-only");
    }

    @Override
    public int delete(Uri uri, String selection, String[] selectionArgs) {
        throw new UnsupportedOperationException("read-only");
    }

    @Override
    public int update(Uri uri, ContentValues values, String selection, String[] selectionArgs) {
        throw new UnsupportedOperationException("read-only");
    }
}
