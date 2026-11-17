{
  description = "Persistent SSH port forwards for Linux and macOS";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  # .github/workflows/nix.yml builds gst on every push and pushes it here.
  nixConfig = {
    extra-substituters = [ "https://tau.cachix.org" ];
    extra-trusted-public-keys = [ "tau.cachix.org-1:tqAThmpvqthNsfMUwRlljsDKPW8147I/+yzRVZnxKsA=" ];
  };

  outputs = { self, nixpkgs }:
    let
      systems = [
        "aarch64-linux"
        "x86_64-linux"
        "aarch64-darwin"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forAllSystems (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = self.packages.${system}.gst;
          gst = (pkgs.buildGo126Module {
            pname = "gst";
            # The release is internal/version/version.go's; see AGENTS.md.
            version = builtins.head (builtins.match ".*const Release = \"([^\"]+)\".*"
              (builtins.readFile ./internal/version/version.go));
            src = pkgs.lib.cleanSource ./.;
            vendorHash = "sha256-zwAyWDXDhSUMT+4/aL/hVOTk2X42qbBqF2wfQWL9HZE=";
            subPackages = [ "cmd/gst" ];
            # cleanSource drops .git, so Go cannot stamp the commit.
            ldflags =
              let
                d = self.lastModifiedDate or "";
                at = pkgs.lib.optionalString (d != "") (with builtins;
                  "${substring 0 4 d}-${substring 4 2 d}-${substring 6 2 d}T${substring 8 2 d}:${substring 10 2 d}:${substring 12 2 d}Z");
              in [
                "-X github.com/yangtau/ssh-tunnel/internal/version.commit=${self.rev or self.dirtyRev or ""}"
                "-X github.com/yangtau/ssh-tunnel/internal/version.committed=${at}"
              ];

            nativeBuildInputs = [ pkgs.installShellFiles ]
              ++ pkgs.lib.optionals pkgs.stdenv.hostPlatform.isDarwin [ pkgs.darwin.cctools ];
            nativeCheckInputs = [ pkgs.openssh ];
            checkPhase = ''
              runHook preCheck
              go test ./...
              go vet ./...
              go vet -tags integration ./integration
              runHook postCheck
            '';

            postInstall = ''
              installShellCompletion --cmd gst \
                --bash <($out/bin/gst completion bash) \
                --zsh <($out/bin/gst completion zsh) \
                --fish <($out/bin/gst completion fish)
            '';

            postFixup = pkgs.lib.optionalString pkgs.stdenv.hostPlatform.isDarwin ''
              otool -l "$out/bin/gst" | grep -A 2 LC_UUID | grep 'uuid '
            '';

            meta = {
              description = "Keep SSH port forwards running as user services";
              homepage = "https://github.com/yangtau/ssh-tunnel";
              mainProgram = "gst";
              platforms = systems;
            };
          }).overrideAttrs (old: pkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isDarwin {
            # macOS network privacy needs LC_UUID, which Go derives from its build ID.
            ldflags = builtins.filter (flag: flag != "-buildid=") old.ldflags;
          });
        });

      checks = forAllSystems (system: {
        default = self.packages.${system}.gst;
      });

      devShells = forAllSystems (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          # NoCC: nixpkgs' C compiler brings its own xcrun and macOS SDK,
          # which hide Xcode's iOS SDKs from ios/build-core.sh.
          default = pkgs.mkShellNoCC {
            packages = [ pkgs.go_1_26 pkgs.gopls pkgs.gnumake pkgs.openssh ]
              ++ pkgs.lib.optionals pkgs.stdenv.hostPlatform.isDarwin [ pkgs.xcodegen ];
          };
          # The Android SDK is Google's, under its own license, which
          # using this shell accepts; its NDK runs only on x86_64 Linux
          # and macOS. The versions are the app's: compileSdk in
          # android/app/build.gradle.kts, the build tools that its Android
          # Gradle plugin asks for, and the NDK of android/README.md.
          android =
            let
              unfree = import nixpkgs {
                inherit system;
                config = { allowUnfree = true; android_sdk.accept_license = true; };
              };
              sdk = (unfree.androidenv.composeAndroidPackages {
                platformVersions = [ "37.0" ];
                buildToolsVersions = [ "36.0.0" ];
                includeNDK = true;
                ndkVersions = [ "29.0.14206865" ];
                includeCmake = false;
              }).androidsdk;
            in
            pkgs.mkShellNoCC {
              inputsFrom = [ default ];
              packages = [ sdk pkgs.jdk21 ];
              ANDROID_HOME = "${sdk}/libexec/android-sdk";
            };
        in
        { inherit default; } // pkgs.lib.optionalAttrs (system != "aarch64-linux") { inherit android; });
    };
}
