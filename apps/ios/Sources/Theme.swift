import SwiftUI

/// Colours from the "Guest App" design canvas.
enum Theme {
    static let accent = Color(red: 0x2d / 255, green: 0x7d / 255, blue: 0x8e / 255)
    static let background = Color(red: 0xf1 / 255, green: 0xf5 / 255, blue: 0xf6 / 255)
    static let card = Color.white
    static let border = Color(red: 0xe0 / 255, green: 0xe8 / 255, blue: 0xea / 255)
    static let text = Color(red: 0x16 / 255, green: 0x28 / 255, blue: 0x2d / 255)
    static let secondaryText = Color(red: 0x44 / 255, green: 0x58 / 255, blue: 0x5e / 255)
    static let videoBackground = Color(red: 0x0f / 255, green: 0x16 / 255, blue: 0x19 / 255)
    static let cameraScreen = Color(red: 0x0d / 255, green: 0x13 / 255, blue: 0x16 / 255)
    static let live = Color(red: 0xe8 / 255, green: 0x51 / 255, blue: 0x3f / 255)
    static let danger = Color(red: 0xb2 / 255, green: 0x3b / 255, blue: 0x2e / 255)
    static let success = Color(red: 0x1f / 255, green: 0x8a / 255, blue: 0x5b / 255)
}

/// The two cameras guests may watch. Ids match the server's `guest.cameras` keys.
struct Camera: Identifiable, Hashable {
    let id: String
    let name: String  // localisation key

    static let all = [
        Camera(id: "10", name: "Front camera"),
        Camera(id: "15", name: "Courtyard"),
    ]
}
