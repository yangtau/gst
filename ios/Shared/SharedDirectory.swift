import Foundation

extension Core {
    /// The directory that the app and the extension share. gst keeps
    /// the machine's identity and its key there, so it stays out of
    /// backups: restored onto another phone, the two would be one machine
    /// to the network.
    static func sharedDirectory() throws -> URL {
        let group = Bundle.main.object(forInfoDictionaryKey: "GstAppGroup") as? String ?? ""
        guard let container = FileManager.default.containerURL(forSecurityApplicationGroupIdentifier: group) else {
            throw Failure(message: "The app group \(group) is missing from this build.")
        }
        var directory = container.appendingPathComponent("gst", isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        var values = URLResourceValues()
        values.isExcludedFromBackup = true
        try? directory.setResourceValues(values)
        return directory
    }
}
