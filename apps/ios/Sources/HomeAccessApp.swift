import SwiftUI

@main
struct HomeAccessApp: App {
    @StateObject private var model = AppModel()

    var body: some Scene {
        WindowGroup {
            Group {
                switch model.phase {
                case .needsEnrollment: EnrollView(model: model)
                case .enrolled: NavigationStack { HomeView(model: model) }
                }
            }
            .tint(Theme.accent)
            // The design is light-only; the camera screen draws its own dark colours.
            .preferredColorScheme(.light)
        }
    }
}
