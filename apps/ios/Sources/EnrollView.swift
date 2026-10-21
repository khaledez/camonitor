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
            if cameraBlocked {
                Text("Camera access needed. Enable it in Settings.")
                    .multilineTextAlignment(.center)
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                QRScannerView { raw in
                    Task {
                        await model.handleScanned(raw)
                        if model.phase == .needsEnrollment { scanAttempt += 1 }
                    }
                }
                .id(scanAttempt)
                .clipShape(RoundedRectangle(cornerRadius: 16))
            }
            if let error = model.enrollError {
                Text(error).foregroundStyle(.red)
            }
            Text("Scan the invite QR code. Ask the owner for a new invite if it fails.")
                .font(.footnote)
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
        }
        .padding()
    }
}
