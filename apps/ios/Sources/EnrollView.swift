import AVFoundation
import SwiftUI

struct EnrollView: View {
    @ObservedObject var model: AppModel
    // QRScannerView reports only one code per instance; a new id gives a fresh scanner.
    @State private var scanAttempt = 0

    private var cameraBlocked: Bool {
        let status = AVCaptureDevice.authorizationStatus(for: .video)
        return status == .denied || status == .restricted
    }

    var body: some View {
        VStack(spacing: 16) {
            VStack(spacing: 6) {
                Text("Abu Khaled's Home").font(.title2.bold())
                Text("Scan the invite QR code from Khaled to set up access on this phone.")
                    .font(.subheadline)
                    .foregroundStyle(Theme.secondaryText)
                    .multilineTextAlignment(.center)
            }

            scanner
                .clipShape(RoundedRectangle(cornerRadius: 18))

            if let error = model.enrollError {
                Label(error, systemImage: "exclamationmark.triangle.fill")
                    .font(.subheadline.weight(.semibold))
                    .foregroundStyle(Theme.danger)
                    .multilineTextAlignment(.center)
            }

            // For when the QR can't be pointed at (e.g. it's on this same phone): paste the invite link.
            VStack(spacing: 8) {
                Text("Have an invite link instead?")
                    .font(.footnote)
                    .foregroundStyle(Theme.secondaryText)
                PasteButton(payloadType: String.self) { strings in
                    Task { @MainActor in await submit(strings.first ?? "") }
                }
                .tint(Theme.accent)
                .labelStyle(.titleAndIcon)
            }
        }
        .padding(20)
        .background(Theme.background)
    }

    @ViewBuilder private var scanner: some View {
        if cameraBlocked {
            Text("Camera access is off. Turn it on in Settings, or paste the invite link below.")
                .multilineTextAlignment(.center)
                .foregroundStyle(Theme.secondaryText)
                .padding()
                .frame(maxWidth: .infinity, maxHeight: .infinity)
                .background(Theme.card)
        } else {
            QRScannerView { raw in
                Task { await submit(raw) }
            }
            .id(scanAttempt)
        }
    }

    private func submit(_ raw: String) async {
        await model.handleScanned(raw)
        if model.phase == .needsEnrollment { scanAttempt += 1 }
    }
}
