#if DEBUG
import SwiftUI

// The screens on a made-up network (Demo), for Xcode's canvas.

#Preview("Network") {
    HomeView().environment(Demo.home.model)
}

#Preview("Problems") {
    HomeView().environment(Demo.problems.model)
}

#Preview("No network yet") {
    HomeView().environment(Demo.welcome.model)
}

#Preview("Off") {
    HomeView().environment(Demo.off.model)
}

#Preview("Host key") {
    AddMachineSheet(step: Demo.hostKey.addStep).environment(Demo.hostKey.model)
}

#Preview("Authorize") {
    AddMachineSheet(step: Demo.authorize.addStep).environment(Demo.authorize.model)
}

#Preview("Machine") {
    HomeView(sheet: Demo.machine.sheet).environment(Demo.machine.model)
}

#Preview("Settings") {
    SettingsSheet().environment(Demo.settings.model)
}
#endif
