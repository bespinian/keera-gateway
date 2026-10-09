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

      keera = pkgs.buildGo127Module rec {
        pname = "keera";
        # A flake cannot see the git tag, so this says "devel", as `make build`
        # does. The revision stamped in below says which commit it is.
        version = "devel";
        src = ./.;
        # vendor/ is not committed, so a flake cannot see it: Nix fetches the
        # modules itself and checks them against this hash. When go.mod
        # changes, set it to pkgs.lib.fakeHash, build, and copy the hash Nix
        # prints.
        vendorHash = "sha256-rz1fFVtLBjqx5HtSXukW88QAeeiUp89iQhfo4R2S8co=";
        subPackages = [
          "cmd/keera-gateway"
          "cmd/keera"
        ];
        env.CGO_ENABLED = 0;
        # The source has no .git, so the version is stamped in, as the
        # Containerfile and `make dist` do.
        ldflags = [
          "-s"
          "-w"
          "-X github.com/bespinian/keera-gateway/internal/version.release=${version}"
          "-X github.com/bespinian/keera-gateway/internal/version.revision=${self.shortRev or self.dirtyShortRev or ""}"
        ];
        meta.mainProgram = "keera";
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
