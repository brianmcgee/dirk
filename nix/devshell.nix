{ pkgs, perSystem, ... }:
let
  go = pkgs.go_1_25;
in
pkgs.mkShell {

  GOROOT = "${go}/share/go";

  packages = [
    go
    perSystem.self.golangci-lint
  ];
}
