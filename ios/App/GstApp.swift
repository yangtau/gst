import SwiftUI

@main
struct GstApp: App {
    @State private var model = GstApp.makeModel()

    private static func makeModel() -> AppModel {
        #if DEBUG
        if let demo = Demo.current {
            return demo.model
        }
        #endif
        return AppModel()
    }

    var body: some Scene {
        WindowGroup {
            RootView()
                .environment(model)
        }
    }
}

/// Opens gst, and then shows the network.
struct RootView: View {
    @Environment(AppModel.self) private var model
    @Environment(\.scenePhase) private var scenePhase
    @State private var isStarted = false

    var body: some View {
        Group {
            if let failure = model.failure {
                ContentUnavailableView("gst cannot start", systemImage: "exclamationmark.triangle", description: Text(failure))
            } else {
                #if DEBUG
                HomeView(sheet: Demo.current?.sheet, addStep: Demo.current?.addStep ?? .form)
                    .preferredColorScheme(Demo.isDark ? .dark : nil)
                #else
                HomeView()
                #endif
            }
        }
        .task {
            await model.start()
            isStarted = true
        }
        // What the daemon reports changes by itself, as links connect
        // and machines come and go, so the app asks while it is in view.
        .task(id: RefreshKey(isStarted: isStarted, isActive: scenePhase == .active, state: model.tunnel.state)) {
            guard isStarted, scenePhase == .active else { return }
            while !Task.isCancelled {
                await model.refresh()
                try? await Task.sleep(for: .seconds(2))
            }
        }
    }

    private struct RefreshKey: Equatable {
        var isStarted: Bool
        var isActive: Bool
        var state: Tunnel.State
    }
}
