import SwiftUI
import UIKit

enum Route: Hashable {
    case camera(String)
    case access
}

struct HomeView: View {
    @ObservedObject var model: AppModel

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 10) {
                HStack {
                    Text("Cameras").sectionLabel()
                    Spacer()
                    LiveBadge()
                }
                LazyVGrid(columns: [GridItem(.flexible(), spacing: 12), GridItem(.flexible())], spacing: 12) {
                    ForEach(Camera.all) { camera in
                        NavigationLink(value: Route.camera(camera.id)) {
                            CameraTile(model: model, camera: camera)
                        }
                    }
                }
                .padding(.bottom, 16)

                Text("Open").sectionLabel()
                DoorButtons(model: model, compact: false)

                NavigationLink(value: Route.access) {
                    HStack(spacing: 4) {
                        Text("Access granted by Khaled")
                            .foregroundStyle(Theme.secondaryText)
                        Text("· Manage").bold()
                    }
                    .font(.footnote)
                    .frame(maxWidth: .infinity, minHeight: 44)
                }
                .padding(.top, 24)
            }
            .padding(20)
        }
        .background(Theme.background)
        .navigationTitle("Abu Khaled's Home")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            NavigationLink(value: Route.access) {
                Image(systemName: "gearshape")
            }
            .accessibilityLabel(Text("Access settings"))
        }
        .navigationDestination(for: Route.self) { route in
            switch route {
            case .camera(let id): CameraView(model: model, selected: id)
            case .access: AccessView(model: model)
            }
        }
    }
}

private struct CameraTile: View {
    @ObservedObject var model: AppModel
    let camera: Camera

    var body: some View {
        LiveVideo(model: model, camera: camera.id)
            .aspectRatio(4 / 3, contentMode: .fit)
            .overlay(alignment: .bottomLeading) {
                Text(LocalizedStringKey(camera.name))
                    .font(.caption2.weight(.semibold))
                    .foregroundStyle(.white)
                    .padding(.horizontal, 7)
                    .padding(.vertical, 3)
                    .background(.black.opacity(0.6), in: RoundedRectangle(cornerRadius: 6))
                    .padding(8)
            }
            .clipShape(RoundedRectangle(cornerRadius: 14))
    }
}

/// Gate and outer-door buttons plus the result line under them.
/// `compact` is the side-by-side layout used on the camera screen.
struct DoorButtons: View {
    @ObservedObject var model: AppModel
    let compact: Bool

    var body: some View {
        VStack(spacing: 12) {
            let layout = compact ? AnyLayout(HStackLayout(spacing: 12)) : AnyLayout(VStackLayout(spacing: 14))
            layout {
                doorButton(compact ? "Gate" : "Open gate", icon: "door.garage.closed", filled: true) {
                    await model.openGate()
                }
                doorButton(compact ? "Outer door" : "Open outer door", icon: "door.left.hand.closed", filled: false) {
                    await model.openOutdoor()
                }
            }
            statusLine
        }
        // Success is a transient checkmark; failures stay until the next tap.
        .task(id: model.actionStatus) {
            guard model.actionSucceeded, model.actionStatus != nil else { return }
            try? await Task.sleep(for: .seconds(2))
            if !Task.isCancelled { model.actionStatus = nil }
        }
    }

    @ViewBuilder private var statusLine: some View {
        if model.isOpening {
            ProgressView()
        } else if let status = model.actionStatus {
            Label(status, systemImage: model.actionSucceeded ? "checkmark.circle.fill" : "exclamationmark.triangle.fill")
                .font(.subheadline.weight(.semibold))
                .foregroundStyle(model.actionSucceeded ? Theme.success : Theme.danger)
                .multilineTextAlignment(.center)
                .padding(10)
                .frame(maxWidth: .infinity)
                .background(.white, in: RoundedRectangle(cornerRadius: 12))
        }
    }

    private func doorButton(_ title: LocalizedStringKey, icon: String, filled: Bool, action: @escaping () async -> Void) -> some View {
        Button {
            model.actionStatus = nil
            Task {
                await action()
                if model.actionSucceeded && model.actionStatus != nil {
                    UINotificationFeedbackGenerator().notificationOccurred(.success)
                }
            }
        } label: {
            Label(title, systemImage: icon)
                .font(compact ? .body.bold() : .title3.bold())
                .frame(maxWidth: .infinity, minHeight: compact ? 60 : 78)
                .foregroundStyle(filled ? .white : Theme.accent)
                .background(filled ? Theme.accent : .white, in: RoundedRectangle(cornerRadius: 18))
                .overlay {
                    RoundedRectangle(cornerRadius: 18).strokeBorder(Theme.accent, lineWidth: filled ? 0 : 2)
                }
        }
        .buttonStyle(.plain)
        .disabled(model.isOpening)
    }
}

extension Text {
    func sectionLabel() -> some View {
        font(.footnote.weight(.semibold)).foregroundStyle(Theme.secondaryText)
    }
}
