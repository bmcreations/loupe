package dev.loupe.compose;

import android.util.Log;
import dalvik.system.DexClassLoader;

/**
 * Entry point from the agent. Waits for the app's Application, then reloads
 * this dex under the app's class loader so Compose classes resolve, and hands
 * over to Host.
 */
public class Bootstrap {
  static final String STARTED = "loupe.compose.started";

  public static void run(final String dex) {
    // A second attach loads this dex under a fresh loader, so a static field
    // can't remember the first one; a system property can.
    synchronized (System.class) {
      if (System.getProperty(STARTED) != null) return;
      System.setProperty(STARTED, "1");
    }
    Thread t = new Thread(() -> {
      try {
        Class<?> at = Class.forName("android.app.ActivityThread");
        Object app = null;
        while (app == null) {
          app = at.getMethod("currentApplication").invoke(null);
          if (app == null) Thread.sleep(5);
        }
        DexClassLoader inner = new DexClassLoader(dex, null, null, app.getClass().getClassLoader());
        inner.loadClass("dev.loupe.compose.Host")
            .getMethod("serve", String.class, Object.class)
            .invoke(null, dex, app);
      } catch (Throwable e) {
        System.clearProperty(STARTED);
        Log.e(Host.TAG, "bootstrap failed", e);
      }
    }, "loupe-compose");
    t.setDaemon(true);
    t.start();
  }
}
