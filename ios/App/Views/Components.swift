import SwiftUI
import UIKit

extension Machine.Way {
    /// How packets reach the machine, in the words of `gst status`.
    var text: String {
        switch self {
        case .here:
            String(localized: "This machine")
        case .direct:
            String(localized: "Direct")
        case .relay(let via) where via.isEmpty:
            String(localized: "Relay")
        case .relay(let via):
            String(localized: "Relay via \(via.joined(separator: ", "))")
        case .offline:
            String(localized: "Offline")
        }
    }

    var color: Color {
        switch self {
        case .here, .direct: .green
        case .relay: .teal
        case .offline: .secondary
        }
    }
}

/// A dot that says whether packets reach a machine: filled while they do.
struct StatusDot: View {
    let way: Machine.Way

    var body: some View {
        Group {
            if case .offline = way {
                Circle().strokeBorder(.tertiary, lineWidth: 1.5)
            } else {
                Circle().fill(way.color)
            }
        }
        .frame(width: 9, height: 9)
        .accessibilityHidden(true)
    }
}

/// Copies a text, and shows for a moment that it did.
struct CopyButton: View {
    let text: String
    var title: LocalizedStringKey = "Copy"
    @State private var isCopied = false

    var body: some View {
        Button {
            UIPasteboard.general.string = text
            isCopied = true
            Task {
                try? await Task.sleep(for: .seconds(1.5))
                isCopied = false
            }
        } label: {
            Label(title, systemImage: isCopied ? "checkmark" : "doc.on.doc")
                .contentTransition(.symbolEffect(.replace))
        }
        .sensoryFeedback(.success, trigger: isCopied) { _, copied in copied }
    }
}

/// A value that a person may want elsewhere, with a button that copies it.
struct CopyableValue: View {
    let title: LocalizedStringKey
    let value: String

    var body: some View {
        LabeledContent(title) {
            HStack(spacing: 10) {
                Text(value)
                    .monospaced()
                    .textSelection(.enabled)
                CopyButton(text: value)
                    .labelStyle(.iconOnly)
                    .buttonStyle(.borderless)
            }
        }
    }
}

/// A text set apart for a person to compare or copy: a key, a
/// fingerprint, a command.
struct CodeBlock: View {
    let text: String
    var lineLimit: Int? = nil

    var body: some View {
        Text(text)
            .font(.footnote.monospaced())
            .lineLimit(lineLimit)
            .truncationMode(.middle)
            .textSelection(.enabled)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(14)
            .background(.fill.tertiary, in: .rect(cornerRadius: 14))
    }
}

/// The soft light behind the network: colored while gst is on, and
/// fading into the background below.
struct Aura: View {
    let isOn: Bool

    var body: some View {
        MeshGradient(
            width: 3,
            height: 3,
            points: [
                [0, 0], [0.5, 0], [1, 0],
                [0, 0.5], [0.6, 0.4], [1, 0.5],
                [0, 1], [0.5, 1], [1, 1],
            ],
            colors: isOn
                ? [.teal, .mint, .cyan, .green, .teal, .blue, .mint, .cyan, .teal]
                : Array(repeating: Color.gray, count: 9)
        )
        .opacity(isOn ? 0.35 : 0.12)
        .mask(LinearGradient(colors: [.black, .black.opacity(0)], startPoint: .top, endPoint: .center))
        .background(Color(uiColor: .systemBackground))
        .ignoresSafeArea()
        .animation(.smooth(duration: 1.2), value: isOn)
    }
}

extension String {
    /// The text as a sentence starts: gst's own words, which the app
    /// shows as they are, start small, as errors do.
    var sentence: String {
        prefix(1).uppercased() + dropFirst()
    }

    /// The text for `Text` to show, with the commands that it quotes in
    /// backticks set as code, as `Text` does for a key.
    var withCode: LocalizedStringKey {
        LocalizedStringKey(self)
    }
}

extension Date {
    /// How long ago it was, as "3 hours ago".
    var ago: String {
        formatted(.relative(presentation: .numeric))
    }
}

extension UInt64 {
    /// A number of bytes, as "1.2 MB".
    var bytes: String {
        Int64(clamping: self).formatted(.byteCount(style: .binary))
    }
}
