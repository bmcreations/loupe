package androidx.inspection;
public abstract class InspectorFactory<T extends Inspector> {
  private final String id;
  public InspectorFactory(String inspectorId) { this.id = inspectorId; }
  public final String getInspectorId() { return id; }
  public abstract T createInspector(Connection connection, InspectorEnvironment environment);
}
