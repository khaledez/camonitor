import SwiftUI

@main
struct HomeAccessApp: App {
    @StateObject private var model = AppModel()

    var body: some Scene {
        WindowGroup {
            switch model.phase {
            case .needsEnrollment: EnrollView(model: model)
            case .enrolled: HomeView(model: model)
            }
        }
    }
}
