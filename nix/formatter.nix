{
  pkgs,
  flake,
  inputs,
  ...
}:
let
  mod = inputs.treefmt-nix.lib.evalModule pkgs {
    projectRootFile = ".git/config";

    programs = {
      gofmt.enable = true;
      nixfmt.enable = true;
      deadnix.enable = true;
      statix.enable = true;
    };

    settings = {
      formatter = {
        deadnix = {
          priority = 1;
        };

        statix = {
          priority = 2;
        };

        nixfmt = {
          priority = 3;
        };
      };
    };
  };

  wrapper = mod.config.build.wrapper // {
    passthru.tests.check = mod.config.build.check flake;
  };

  unsupported = pkgs.writeShellApplication {
    name = "unsupported-platform";
    text = ''
      echo "nix fmt is not supported on ${pkgs.stdenv.hostPlatform.system}";
    '';
  };
in
# nixfmt-rfc-style is based on Haskell, which is broke on RiscV currently
if pkgs.stdenv.hostPlatform.isRiscV then unsupported else wrapper
