import SwiftUI

// The UI test bundle needs a host app to install with. Nothing here runs
// during a session: the runner drives whatever is on screen.
@main
struct LoupeRunnerHost: App {
    var body: some Scene {
        WindowGroup { Text("Loupe runner") }
    }
}
