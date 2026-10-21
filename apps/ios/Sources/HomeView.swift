import SwiftUI
import UIKit

struct HomeView: View {
    @ObservedObject var model: AppModel

    var body: some View {
        VStack(spacing: 20) {
            doorButton("Open Gate") { await model.openGate() }
            doorButton("Open Out-door") { await model.openOutdoor() }
            if model.isOpening {
                ProgressView()
            } else if let status = model.actionStatus {
                Label(status, systemImage: model.actionSucceeded ? "checkmark.circle.fill" : "exclamationmark.triangle.fill")
                    .foregroundStyle(model.actionSucceeded ? .green : .red)
            }
        }
        .padding()
    }

    private func doorButton(_ title: String, action: @escaping () async -> Void) -> some View {
        Button {
            model.actionStatus = nil
            Task {
                await action()
                if model.actionSucceeded && model.actionStatus != nil {
                    UINotificationFeedbackGenerator().notificationOccurred(.success)
                }
            }
        } label: {
            Text(title).font(.title2.bold()).frame(maxWidth: .infinity, minHeight: 90)
        }
        .buttonStyle(.borderedProminent)
        .disabled(model.isOpening)
    }
}
