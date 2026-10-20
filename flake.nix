{
  description = "Persistent SSH port forwards for Linux and macOS";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

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
          gst = pkgs.buildGo126Module {
            pname = "gst";
            version = "unstable";
            src = pkgs.lib.cleanSource ./.;
            vendorHash = "sha256-jAL/X2MEAliPJjhu66GrxeJs+ztrx5dp+mHOErsrxnA=";
            subPackages = [ "cmd/gst" ];

            nativeBuildInputs = [ pkgs.installShellFiles ];
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

            meta = {
              description = "Keep SSH port forwards running as user services";
              homepage = "https://github.com/yangtau/ssh-tunnel";
              mainProgram = "gst";
              platforms = systems;
            };
          };
        });

      checks = forAllSystems (system: {
        default = self.packages.${system}.gst;
      });

      devShells = forAllSystems (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.mkShell {
            packages = [ pkgs.go_1_26 pkgs.gopls pkgs.gnumake pkgs.openssh ];
          };
        });
    };
}
