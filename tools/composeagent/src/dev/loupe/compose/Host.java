package dev.loupe.compose;

import android.content.Context;
import android.content.pm.ApplicationInfo;
import android.net.LocalServerSocket;
import android.net.LocalSocket;
import android.os.Build;
import android.os.Process;
import android.util.Log;
import android.view.View;
import android.view.inspector.WindowInspector;
import androidx.inspection.ArtTooling;
import androidx.inspection.Connection;
import androidx.inspection.Inspector;
import androidx.inspection.InspectorEnvironment;
import dalvik.system.DexClassLoader;
import java.io.ByteArrayOutputStream;
import java.io.DataInputStream;
import java.io.DataOutputStream;
import java.io.File;
import java.io.FileInputStream;
import java.io.FileOutputStream;
import java.io.InputStream;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.TimeUnit;
import java.util.zip.ZipEntry;
import java.util.zip.ZipFile;

/**
 * Hosts Google's Compose inspector (from the app's own ui-android version)
 * behind stub ArtTooling, and answers loupe over localabstract
 * "loupe-compose-<pid>". Request: op u8, len u32 BE, payload. Response:
 * status u8 (0 ok, 1 error), len u32 BE, payload.
 */
public class Host {
  static final String TAG = "LoupeCompose";
  static final String VERSION_RESOURCE = "META-INF/androidx.compose.ui_ui.version";

  static Context ctx;
  static Inspector inspector;
  static String loadedJar;
  static long generation;
  // The inspector checks that every call comes from the thread it was
  // created on ("cachedNodes should be accessed by the inspector thread"),
  // and loupe opens a fresh connection per request, so every op runs here.
  static final ExecutorService inspectorThread =
      Executors.newSingleThreadExecutor(r -> {
        Thread t = new Thread(r, "loupe-compose-inspector");
        t.setDaemon(true);
        return t;
      });

  public static void serve(String dex, Object application) throws Throwable {
    ctx = (Context) application;
    LocalServerSocket server = new LocalServerSocket("loupe-compose-" + Process.myPid());
    while (true) {
      final LocalSocket s = server.accept();
      Thread t = new Thread(() -> session(s), "loupe-compose-conn");
      t.setDaemon(true);
      t.start();
    }
  }

  static void session(LocalSocket s) {
    try (LocalSocket sock = s) {
      DataInputStream in = new DataInputStream(sock.getInputStream());
      DataOutputStream out = new DataOutputStream(sock.getOutputStream());
      while (true) {
        int op = in.read();
        if (op < 0) return;
        byte[] payload = new byte[in.readInt()];
        in.readFully(payload);
        byte status = 0;
        byte[] body;
        try {
          body = inspectorThread.submit(() -> handle(op, payload)).get();
        } catch (ExecutionException e) {
          status = 1;
          body = String.valueOf(e.getCause()).getBytes(StandardCharsets.UTF_8);
        } catch (Throwable e) {
          status = 1;
          body = String.valueOf(e).getBytes(StandardCharsets.UTF_8);
        }
        out.write(status);
        out.writeInt(body.length);
        out.write(body);
        out.flush();
      }
    } catch (Throwable e) {
      Log.w(TAG, "connection ended", e);
    }
  }

  static byte[] handle(int op, byte[] payload) throws Exception {
    switch (op) {
      case 'H':
        return ("{\"pid\":" + Process.myPid() + ",\"composeVersion\":\"" + composeVersion() + "\"}")
            .getBytes(StandardCharsets.UTF_8);
      case 'L':
        load(new String(payload, StandardCharsets.UTF_8));
        return new byte[0];
      case 'C':
        return composables();
      default:
        throw new IllegalArgumentException("unknown op " + op);
    }
  }

  // The version resource sits at the APK root, so read it from the APKs
  // directly rather than trusting the class loader to serve META-INF.
  static String composeVersion() throws Exception {
    ApplicationInfo ai = ctx.getApplicationInfo();
    List<String> apks = new ArrayList<>();
    apks.add(ai.sourceDir);
    if (ai.splitSourceDirs != null) for (String p : ai.splitSourceDirs) apks.add(p);
    for (String apk : apks) {
      try (ZipFile z = new ZipFile(apk)) {
        ZipEntry e = z.getEntry(VERSION_RESOURCE);
        if (e == null) continue;
        try (InputStream in = z.getInputStream(e)) {
          return new String(readAll(in), StandardCharsets.UTF_8).trim();
        }
      }
    }
    return "";
  }

  static void load(String jarPath) throws Exception {
    if (jarPath.equals(loadedJar)) return;
    if (inspector != null) throw new IllegalStateException("an inspector is already loaded from " + loadedJar);
    File dir = new File(ctx.getCodeCacheDir(), "loupe-inspector");
    File lib = new File(dir, "lib");
    lib.mkdirs();
    try (ZipFile z = new ZipFile(jarPath)) {
      ZipEntry so = null;
      for (String abi : Build.SUPPORTED_ABIS) {
        so = z.getEntry("lib/" + abi + "/libcompose_inspection_jni.so");
        if (so != null) break;
      }
      if (so == null) throw new IllegalStateException("inspector.jar has no native library for this device");
      try (InputStream in = z.getInputStream(so)) {
        write(new File(lib, "libcompose_inspection_jni.so"), readAll(in));
      }
    }
    // ART refuses to load a dex from a writable file.
    File ro = new File(dir, "inspector.jar");
    ro.delete();
    try (InputStream in = new FileInputStream(jarPath)) {
      write(ro, readAll(in));
    }
    ro.setWritable(false);

    DexClassLoader loader = new DexClassLoader(ro.getPath(), null, lib.getPath(), Host.class.getClassLoader());
    Class<?> fc = loader.loadClass("androidx.compose.ui.inspection.ComposeLayoutInspectorFactory");
    Object factory = fc.getConstructor().newInstance();
    // Stubs: hooks only feed recomposition counts, and findInstances is only
    // used for XR. The composable tree doesn't need either.
    ArtTooling tooling = new ArtTooling() {
      public <X> List<X> findInstances(Class<X> c) { return new ArrayList<>(); }
      public void registerEntryHook(Class<?> c, String m, EntryHook h) {}
      public <X> void registerExitHook(Class<?> c, String m, ExitHook<X> h) {}
    };
    InspectorEnvironment env = () -> tooling;
    Connection conn = new Connection() {};
    inspector = (Inspector) fc.getMethod("createInspector", Connection.class, InspectorEnvironment.class)
        .invoke(factory, conn, env);
    loadedJar = jarPath;
  }

  static byte[] composables() throws Exception {
    if (inspector == null) throw new IllegalStateException("no inspector loaded");
    List<View> roots = WindowInspector.getGlobalWindowViews();
    ByteArrayOutputStream out = new ByteArrayOutputStream();
    DataOutputStream d = new DataOutputStream(out);
    d.writeInt(roots.size());
    generation++;
    for (View root : roots) {
      CountDownLatch got = new CountDownLatch(1);
      byte[][] resp = new byte[1][];
      inspector.onReceiveCommand(getComposables(root.getUniqueDrawingId(), generation), r -> {
        resp[0] = r;
        got.countDown();
      });
      got.await(10, TimeUnit.SECONDS);
      byte[] b = resp[0] == null ? new byte[0] : resp[0];
      d.writeInt(b.length);
      d.write(b);
    }
    return out.toByteArray();
  }

  // Command { get_composables_command(1) { root_view_id(1), skip_system_composables(2), generation(3) } }
  static byte[] getComposables(long root, long gen) {
    ByteArrayOutputStream inner = new ByteArrayOutputStream();
    inner.write(0x08);
    varint(inner, root);
    inner.write(0x10);
    inner.write(1);
    inner.write(0x18);
    varint(inner, gen);
    byte[] in = inner.toByteArray();
    ByteArrayOutputStream outer = new ByteArrayOutputStream();
    outer.write(0x0a);
    varint(outer, in.length);
    outer.write(in, 0, in.length);
    return outer.toByteArray();
  }

  static void varint(ByteArrayOutputStream o, long v) {
    while ((v & ~0x7FL) != 0) {
      o.write((int) ((v & 0x7F) | 0x80));
      v >>>= 7;
    }
    o.write((int) v);
  }

  static byte[] readAll(InputStream in) throws Exception {
    ByteArrayOutputStream out = new ByteArrayOutputStream();
    byte[] buf = new byte[65536];
    int n;
    while ((n = in.read(buf)) > 0) out.write(buf, 0, n);
    return out.toByteArray();
  }

  static void write(File f, byte[] b) throws Exception {
    try (FileOutputStream out = new FileOutputStream(f)) {
      out.write(b);
    }
  }
}
