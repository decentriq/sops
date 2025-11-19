{
  description = "A basic gomod2nix flake";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  inputs.flake-utils.url = "github:numtide/flake-utils";
  inputs.gomod2nix.url = "github:nix-community/gomod2nix";
  inputs.gomod2nix.inputs.nixpkgs.follows = "nixpkgs";
  inputs.gomod2nix.inputs.flake-utils.follows = "flake-utils";

  outputs = {
    self,
    flake-utils,
    ...
  } @ inputs:
    flake-utils.lib.eachDefaultSystem (system: let
      pkgs = import inputs.nixpkgs {
        inherit system;
        overlays = [(inputs.gomod2nix.overlays.default)];
      };

      crossPkgs = import inputs.nixpkgs {
        inherit system;
        overlays = [(inputs.gomod2nix.overlays.default)];

        crossSystem = {
          config = "aarch64-unknown-linux-gnu";
          #config = "arm64-apple-darwin";
        };
      };

      sops = pkgs.callPackage ./sops.nix {};
    in {
      packages.default = sops;
      packages.sops = sops;

      #devShells.cross = pkgs.mkShell {
      #  buildInputs = [crossPkgs.gcc pkgs.go_1_24];
      #};
      devShells.cross = crossPkgs.mkShell {
        buildInputs = with crossPkgs; [gcc pkgs.go_1_24];
      };
      devShells.default = pkgs.mkShell {
        packages = with pkgs; [
          #(pkgs.mkGoEnv {pwd = ./.;})
          #libgcc
          clang
          #gcc_multi

          go_1_24
          pkgs.gomod2nix
        ];
      };
    });
}
