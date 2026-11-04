package dev.loupe.a11y;

import android.accessibilityservice.AccessibilityServiceInfo;
import android.app.UiAutomation;
import android.graphics.Rect;
import android.os.HandlerThread;
import android.os.Looper;
import android.view.accessibility.AccessibilityNodeInfo;
import android.view.accessibility.AccessibilityWindowInfo;
import java.lang.reflect.Constructor;
import java.util.List;

/**
 * Prints the screen's accessibility tree as uiautomator dump's XML, on one
 * line, with two attributes uiautomator leaves out: whether a scrollable node
 * offers scroll-backward and scroll-forward. A list at its top offers no
 * backward action, so a reader can tell the edge without swiping into it.
 *
 * Run as {@code CLASSPATH=<dex> app_process / dev.loupe.a11y.Dump}, the way
 * uiautomator itself starts. It holds Android's one UiAutomation connection
 * for the ~30ms of the walk. When another client (uiautomator, Maestro, an
 * instrumentation test) holds it, it prints "busy" and exits 3 instead of
 * being killed by the uncaught exception.
 */
public final class Dump {
    static final int EXIT_BUSY = 3;

    public static void main(String[] args) throws Exception {
        // AccessibilityInteractionClient makes a Handler on the main looper.
        Looper.prepareMainLooper();
        HandlerThread thread = new HandlerThread("loupe-a11y");
        thread.start();
        UiAutomation ua = newUiAutomation(thread.getLooper());
        try {
            UiAutomation.class.getMethod("connect").invoke(ua);
        } catch (java.lang.reflect.InvocationTargetException e) {
            if (e.getCause() instanceof IllegalStateException) {
                System.out.println("busy");
                System.out.flush();
                System.exit(EXIT_BUSY);
            }
            throw e;
        }
        AccessibilityServiceInfo info = ua.getServiceInfo();
        info.flags |= AccessibilityServiceInfo.FLAG_RETRIEVE_INTERACTIVE_WINDOWS
            | AccessibilityServiceInfo.FLAG_INCLUDE_NOT_IMPORTANT_VIEWS
            | AccessibilityServiceInfo.FLAG_REPORT_VIEW_IDS;
        ua.setServiceInfo(info);
        try {
            // uiautomator waits for idle too, but with a 10s cap; a list
            // mid-fling is read as it stands rather than waited out.
            ua.waitForIdle(100, 500);
        } catch (java.util.concurrent.TimeoutException ignored) {
        }

        StringBuilder out = new StringBuilder(64 * 1024);
        out.append("<?xml version='1.0' encoding='UTF-8' standalone='yes' ?><hierarchy rotation=\"")
            .append(rotation(ua)).append("\">");
        List<AccessibilityWindowInfo> windows = ua.getWindows();
        boolean any = false;
        // getWindows is top-most first; uiautomator writes the bottom one first.
        for (int i = windows.size() - 1; i >= 0; i--) {
            // uiautomator leaves out the status and navigation bars and the
            // keyboard.
            int type = windows.get(i).getType();
            if (type == AccessibilityWindowInfo.TYPE_SYSTEM
                || type == AccessibilityWindowInfo.TYPE_ACCESSIBILITY_OVERLAY
                || type == AccessibilityWindowInfo.TYPE_INPUT_METHOD) continue;
            AccessibilityNodeInfo root = windows.get(i).getRoot();
            if (root != null) {
                node(out, root, 0);
                any = true;
            }
        }
        if (!any) {
            AccessibilityNodeInfo root = ua.getRootInActiveWindow();
            if (root != null) node(out, root, 0);
        }
        out.append("</hierarchy>");
        System.out.println(out);
        System.out.flush();
        System.exit(0);
    }

    static UiAutomation newUiAutomation(Looper looper) throws Exception {
        Object conn = Class.forName("android.app.UiAutomationConnection").getDeclaredConstructor().newInstance();
        Class<?> ic = Class.forName("android.app.IUiAutomationConnection");
        // Deprecated from API 34 in favour of a Context one, but still there
        // and still what uiautomator's shell wrapper uses.
        Constructor<UiAutomation> c = UiAutomation.class.getDeclaredConstructor(Looper.class, ic);
        c.setAccessible(true);
        return c.newInstance(looper, conn);
    }

    static int rotation(UiAutomation ua) {
        try {
            Object dm = Class.forName("android.hardware.display.DisplayManagerGlobal").getMethod("getInstance").invoke(null);
            Object di = dm.getClass().getMethod("getDisplayInfo", int.class).invoke(dm, 0);
            return di.getClass().getField("rotation").getInt(di);
        } catch (Exception e) {
            return 0;
        }
    }

    static void node(StringBuilder out, AccessibilityNodeInfo n, int index) {
        Rect r = new Rect();
        n.getBoundsInScreen(r);
        out.append("<node index=\"").append(index).append('"');
        attr(out, "text", n.getText());
        attr(out, "resource-id", n.getViewIdResourceName());
        attr(out, "class", n.getClassName());
        attr(out, "package", n.getPackageName());
        attr(out, "content-desc", n.getContentDescription());
        flag(out, "checkable", n.isCheckable());
        flag(out, "checked", n.isChecked());
        flag(out, "clickable", n.isClickable());
        flag(out, "enabled", n.isEnabled());
        flag(out, "focusable", n.isFocusable());
        flag(out, "focused", n.isFocused());
        flag(out, "scrollable", n.isScrollable());
        flag(out, "long-clickable", n.isLongClickable());
        flag(out, "password", n.isPassword());
        flag(out, "selected", n.isSelected());
        if (n.isScrollable()) {
            boolean back = false, fwd = false;
            for (AccessibilityNodeInfo.AccessibilityAction a : n.getActionList()) {
                if (a.getId() == AccessibilityNodeInfo.ACTION_SCROLL_BACKWARD) back = true;
                if (a.getId() == AccessibilityNodeInfo.ACTION_SCROLL_FORWARD) fwd = true;
            }
            flag(out, "scroll-backward", back);
            flag(out, "scroll-forward", fwd);
        }
        out.append(" bounds=\"[").append(r.left).append(',').append(r.top).append("][")
            .append(r.right).append(',').append(r.bottom).append("]\"");
        int kids = n.getChildCount();
        if (kids == 0) {
            out.append(" />");
            return;
        }
        out.append('>');
        int shown = 0;
        for (int i = 0; i < kids; i++) {
            AccessibilityNodeInfo c = n.getChild(i);
            // uiautomator's dumper leaves out what the user cannot see.
            if (c == null || !c.isVisibleToUser()) continue;
            node(out, c, shown++);
        }
        out.append("</node>");
    }

    static void flag(StringBuilder out, String name, boolean v) {
        out.append(' ').append(name).append("=\"").append(v).append('"');
    }

    static void attr(StringBuilder out, String name, CharSequence v) {
        out.append(' ').append(name).append("=\"");
        if (v != null) {
            for (int i = 0; i < v.length(); i++) {
                char ch = v.charAt(i);
                switch (ch) {
                    case '&': out.append("&amp;"); break;
                    case '<': out.append("&lt;"); break;
                    case '>': out.append("&gt;"); break;
                    case '"': out.append("&quot;"); break;
                    case '\n': out.append("&#10;"); break;
                    case '\r': out.append("&#13;"); break;
                    default:
                        if (ch < 0x20 && ch != '\t') continue;
                        out.append(ch);
                }
            }
        }
        out.append('"');
    }
}
