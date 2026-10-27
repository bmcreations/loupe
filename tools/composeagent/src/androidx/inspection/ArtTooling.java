package androidx.inspection;
import java.util.List;
public interface ArtTooling {
  <T> List<T> findInstances(Class<T> clazz);
  void registerEntryHook(Class<?> originClass, String originMethod, EntryHook entryHook);
  <T> void registerExitHook(Class<?> originClass, String originMethod, ExitHook<T> exitHook);
  interface EntryHook { void onEntry(Object thisObject, List<Object> args); }
  interface ExitHook<T> { T onExit(T result); }
}
