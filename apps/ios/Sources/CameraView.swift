import SwiftUI

struct CameraView: View {
    @ObservedObject var model: AppModel
    @State var selected: String

    private var camera: Camera { Camera.all.first { $0.id == selected } ?? Camera.all[0] }

    var body: some View {
        VStack(spacing: 14) {
            LiveVideo(model: model, camera: selected, fill: false)
                .overlay(alignment: .topLeading) { LiveBadge().padding(12) }
                .overlay(alignment: .topTrailing) { clock.padding(12) }
                .clipShape(RoundedRectangle(cornerRadius: 18))
                .frame(maxHeight: .infinity)

            HStack(spacing: 10) {
                ForEach(Camera.all) { cam in
                    let isSelected = cam.id == selected
                    Button { selected = cam.id } label: {
                        Text(LocalizedStringKey(cam.name))
                            .font(.subheadline.weight(.semibold))
                            .frame(maxWidth: .infinity, minHeight: 44)
                            .foregroundStyle(isSelected ? Theme.cameraScreen : .white.opacity(0.85))
                            .background(isSelected ? Color.white : .white.opacity(0.08), in: RoundedRectangle(cornerRadius: 12))
                    }
                    .buttonStyle(.plain)
                    .accessibilityAddTraits(isSelected ? .isSelected : [])
                }
            }

            DoorButtons(model: model, compact: true)
        }
        .padding(16)
        .background(Theme.cameraScreen)
        .navigationTitle(Text(LocalizedStringKey(camera.name)))
        .navigationBarTitleDisplayMode(.inline)
        .toolbarBackground(Theme.cameraScreen, for: .navigationBar)
        .toolbarBackground(.visible, for: .navigationBar)
        .toolbarColorScheme(.dark, for: .navigationBar)
    }

    private var clock: some View {
        TimelineView(.periodic(from: .now, by: 1)) { context in
            Text(context.date, format: .dateTime.hour().minute())
                .font(.caption.monospacedDigit())
                .foregroundStyle(.white.opacity(0.85))
                .padding(.horizontal, 9)
                .padding(.vertical, 4)
                .background(.black.opacity(0.5), in: RoundedRectangle(cornerRadius: 7))
        }
    }
}
