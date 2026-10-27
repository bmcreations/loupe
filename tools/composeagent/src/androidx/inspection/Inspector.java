package androidx.inspection;
import java.util.concurrent.Executor;
public abstract class Inspector {
  private final Connection connection;
  public Inspector(Connection connection) { this.connection = connection; }
  protected final Connection getConnection() { return connection; }
  public void onDispose() {}
  public abstract void onReceiveCommand(byte[] data, CommandCallback callback);
  public interface CommandCallback {
    void reply(byte[] response);
    default void addCancellationListener(Executor executor, Runnable runnable) {}
  }
}
