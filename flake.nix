{
  description = "Keera Gateway - LLM gateway and control plane";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs =
    {
      self,
      nixpkgs,
      ...
    }:
    let
      system = "x86_64-linux";
      pkgs = import nixpkgs {
        inherit system;
        # nixpkgs refuses to build software under a licence that is not free
        # in its sense. This allows Keera and nothing else.
        config.allowUnfreePredicate = pkg: nixpkgs.lib.getName pkg == "keera";
      };

      keera = pkgs.buildGoModule {
        pname = "keera";
        version = "0.1.0";
        src = ./.;
        # Dependencies are vendored, which is what makes this build work
        # offline - and what makes an air-gapped customer possible later.
        vendorHash = null;
        subPackages = [
          "cmd/keera-gateway"
          "cmd/keera"
        ];
        env.CGO_ENABLED = 0;
        ldflags = [
          "-s"
          "-w"
        ];
        meta.mainProgram = "keera-gateway";
        # Source-available: production use by a company needs a commercial
        # agreement, so this is not free software in the nixpkgs sense.
        meta.license = {
          fullName = "Keera Community Licence v1.0";
          url = "https://github.com/bespinian/keera-gateway/blob/main/LICENSE";
          free = false;
          redistributable = true;
        };
      };
    in
    {
      packages.${system} = {
        inherit keera;
        default = keera;
      };

      formatter.${system} = pkgs.nixfmt-tree;
    };
}
