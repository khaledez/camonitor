import AVFoundation
import SwiftUI

/// Live, muted video for one camera. Stream URLs are signed and expire after
/// 15 minutes, so the player fetches a fresh one when playback fails and
/// proactively just before expiry.
struct LiveVideo: View {
    @ObservedObject var model: AppModel
    let camera: String
    var fill = true

    @State private var player = AVPlayer()
    @State private var isPlaying = false
    @Environment(\.scenePhase) private var scenePhase

    var body: some View {
        PlayerLayerView(player: player, fill: fill)
            .background(Theme.videoBackground)
            .overlay {
                if !isPlaying {
                    ProgressView().tint(.white.opacity(0.7))
                }
            }
            .onReceive(player.publisher(for: \.timeControlStatus)) { isPlaying = $0 == .playing }
            // iOS pauses video in the background and never resumes it, so the
            // loop restarts (with a fresh URL) each time the app becomes active.
            .task(id: PlaybackKey(camera: camera, isActive: scenePhase == .active)) {
                guard scenePhase == .active else { player.pause(); return }
                await keepPlaying()
            }
            .onDisappear { player.pause() }
            .accessibilityLabel(Text(LocalizedStringKey(Camera.all.first { $0.id == camera }?.name ?? "")))
    }

    private func keepPlaying() async {
        player.isMuted = true
        while !Task.isCancelled {
            guard let url = await model.streamURL(camera: camera) else {
                try? await Task.sleep(for: .seconds(5))
                continue
            }
            let item = AVPlayerItem(url: url)
            player.replaceCurrentItem(with: item)
            player.play()
            await untilRenewalNeeded(item)
            if !Task.isCancelled { try? await Task.sleep(for: .seconds(2)) }
        }
        player.replaceCurrentItem(with: nil)
    }

    /// Returns when the item fails, stalls, or its signed URL is about to expire.
    private func untilRenewalNeeded(_ item: AVPlayerItem) async {
        await withTaskGroup(of: Void.self) { group in
            group.addTask { try? await Task.sleep(for: .seconds(14 * 60)) }
            group.addTask {
                for await status in item.publisher(for: \.status).values where status == .failed { return }
            }
            group.addTask {
                let failures = NotificationCenter.default.notifications(named: AVPlayerItem.failedToPlayToEndTimeNotification, object: item)
                for await _ in failures { return }
            }
            group.addTask {
                let stalls = NotificationCenter.default.notifications(named: AVPlayerItem.playbackStalledNotification, object: item)
                for await _ in stalls { return }
            }
            await group.next()
            group.cancelAll()
        }
    }
}

private struct PlaybackKey: Equatable {
    let camera: String
    let isActive: Bool
}

private struct PlayerLayerView: UIViewRepresentable {
    let player: AVPlayer
    let fill: Bool

    func makeUIView(context: Context) -> PlayerUIView {
        let view = PlayerUIView()
        view.playerLayer.player = player
        return view
    }

    func updateUIView(_ view: PlayerUIView, context: Context) {
        view.playerLayer.player = player
        view.playerLayer.videoGravity = fill ? .resizeAspectFill : .resizeAspect
    }

    final class PlayerUIView: UIView {
        override class var layerClass: AnyClass { AVPlayerLayer.self }
        var playerLayer: AVPlayerLayer { layer as! AVPlayerLayer }
    }
}

/// The red "Live" pill shown over video.
struct LiveBadge: View {
    var body: some View {
        HStack(spacing: 6) {
            Circle().fill(Theme.live).frame(width: 7, height: 7)
            Text("Live")
        }
        .font(.caption.bold())
        .foregroundStyle(.white)
        .padding(.horizontal, 9)
        .padding(.vertical, 4)
        .background(.black.opacity(0.5), in: RoundedRectangle(cornerRadius: 7))
    }
}
