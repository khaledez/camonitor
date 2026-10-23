import SwiftUI

struct AccessView: View {
    @ObservedObject var model: AppModel
    @State private var confirmingRemoval = false

    private var isDemo: Bool { model.session?.isDemo ?? false }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 10) {
                statusCard.padding(.bottom, 8)

                Text("What you can do").sectionLabel()
                VStack(spacing: 0) {
                    permissionRow("Open the gate", icon: "door.garage.closed", available: !isDemo)
                    Divider()
                    permissionRow("Open the outer door", icon: "door.left.hand.closed", available: !isDemo)
                    Divider()
                    permissionRow("Watch both cameras", icon: "video", available: true)
                }
                .background(Theme.card, in: RoundedRectangle(cornerRadius: 16))
                .overlay { RoundedRectangle(cornerRadius: 16).strokeBorder(Theme.border) }

                removeButton.padding(.top, 32)
            }
            .padding(20)
        }
        .background(Theme.background)
        .navigationTitle("Access")
        .navigationBarTitleDisplayMode(.inline)
        .confirmationDialog("Remove access from this phone?", isPresented: $confirmingRemoval, titleVisibility: .visible) {
            Button("Remove access", role: .destructive) {
                Task { await model.forget() }
            }
        } message: {
            Text("You will need a new invite from Khaled to get back in.")
        }
    }

    private var statusCard: some View {
        VStack(spacing: 4) {
            Image(systemName: "checkmark")
                .font(.title2.bold())
                .foregroundStyle(Theme.success)
                .frame(width: 52, height: 52)
                .background(Theme.success.opacity(0.12), in: Circle())
                .padding(.bottom, 8)
            Text(isDemo ? "This phone has a demo pass" : "This phone has access")
                .font(.headline)
            if let name = model.session?.label, !isDemo {
                Text("Registered as \(name) · granted by Khaled")
                    .font(.subheadline)
                    .foregroundStyle(Theme.secondaryText)
            } else {
                Text("Granted by Khaled")
                    .font(.subheadline)
                    .foregroundStyle(Theme.secondaryText)
            }
            if let since = model.session?.created {
                Text("Since \(since.formatted(date: .long, time: .omitted))")
                    .font(.caption)
                    .foregroundStyle(Theme.secondaryText)
            }
        }
        .multilineTextAlignment(.center)
        .frame(maxWidth: .infinity)
        .padding(20)
        .background(Theme.card, in: RoundedRectangle(cornerRadius: 16))
        .overlay { RoundedRectangle(cornerRadius: 16).strokeBorder(Theme.border) }
    }

    private func permissionRow(_ title: LocalizedStringKey, icon: String, available: Bool) -> some View {
        HStack(spacing: 12) {
            Image(systemName: icon)
                .foregroundStyle(available ? Theme.accent : Theme.secondaryText)
                .frame(width: 24)
            Text(title)
                .strikethrough(!available)
                .foregroundStyle(available ? Theme.text : Theme.secondaryText)
            Spacer()
            if !available {
                Text("Not on demo pass")
                    .font(.caption)
                    .foregroundStyle(Theme.secondaryText)
            }
        }
        .padding(.horizontal, 16)
        .frame(minHeight: 50)
    }

    private var removeButton: some View {
        VStack(spacing: 12) {
            Button(role: .destructive) { confirmingRemoval = true } label: {
                Label("Remove access from this phone", systemImage: "trash")
                    .font(.body.bold())
                    .frame(maxWidth: .infinity, minHeight: 60)
                    .foregroundStyle(Theme.danger)
                    .background(Theme.card, in: RoundedRectangle(cornerRadius: 16))
                    .overlay { RoundedRectangle(cornerRadius: 16).strokeBorder(Theme.danger.opacity(0.35), lineWidth: 2) }
            }
            .buttonStyle(.plain)
            Text("This deletes your key from this phone. You will need a new invite from Khaled to get back in.")
                .font(.caption)
                .foregroundStyle(Theme.secondaryText)
                .multilineTextAlignment(.center)
                .frame(maxWidth: .infinity)
        }
    }
}
