{
  inputs = {
    nixpkgs.url = "https://channels.nixos.org/nixpkgs-unstable/nixexprs.tar.xz";
  };

  outputs = { self, nixpkgs, ... }:
    let
      inherit (nixpkgs) lib;
      inherit (lib.attrsets) genAttrs;

      pkgsFor = pkgs: system: import pkgs { inherit system; };

      allSystems = lib.systems.flakeExposed;

      forAllSystems = f: genAttrs allSystems
        (system: f {
          inherit system lib;
          pkgs = pkgsFor nixpkgs system;
          localPkgs = self.packages.${system};
        });
    in
    {
      packages = forAllSystems ({ ... }:
        let
        in
        {
        });

      formatter = forAllSystems ({ pkgs, ... }: pkgs.treefmt);

      devShells = forAllSystems
        ({ lib, pkgs, ... }:
          let
          in {
            default = pkgs.mkShell {
              packages = lib.unique [
                # Formatter
                # keep-sorted start
                pkgs.gofumpt
                pkgs.golangci-lint
                pkgs.gotools
                pkgs.keep-sorted
                pkgs.nixf-diagnose
                pkgs.nixpkgs-fmt
                pkgs.shellcheck
                pkgs.treefmt
                pkgs.typos
                # keep-sorted end

                # Go tools
                # keep-sorted start
                pkgs.go-tools
                pkgs.go
                pkgs.gomodifytags
                pkgs.gopls
                pkgs.gotests
                pkgs.gotools
                # keep-sorted end

                # Random tools
                # keep-sorted start
                pkgs.jq
                pkgs.atproto-goat
                # keep-sorted end
              ];
            };
          });
    };
}
