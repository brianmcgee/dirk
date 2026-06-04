{
  flake,
  pkgs,
  perSystem,
  ...
}:
pkgs.buildGo125Module (_finalAttrs: {
  pname = "dirk";

  # there's no good way of tying in the version to a git tag or branch
  # so for simplicity's sake we set the version as the commit revision hash
  # we remove the `-dirty` suffix to avoid a lot of unnecessary rebuilds in local dev
  version = pkgs.lib.removeSuffix "-dirty" (flake.shortRev or flake.dirtyShortRev);

  src =
    let
      fs = pkgs.lib.fileset;
    in
    fs.toSource {
      root = ../.;
      fileset = fs.unions [
        ../cmd
        ../core
        ../rules
        ../services
        ../testing
        ../util
        ../go.mod
        ../go.sum
        ../logging.go
        ../main.go
        ../metrics.go
        ../slashingprotection.go
        ../tracing.go
      ];
    };

  buildInputs = with perSystem.ethereum-nix; [
    mcl
    bls_1_86
  ];

  runVend = true;
  vendorHash = "sha256-Tjb5OGhGc5K+sE3V6FqPQcgR48jJ7I73ygRlZNoqyuI=";

  doCheck = true;

  meta = {
    description = "Distributed remote keymanager and signer for Ethereum validators";
    homepage = "https://github.com/attestantio/dirk";
    license = pkgs.lib.licenses.asl20;
    mainProgram = "dirk";
    platforms = [
      "x86_64-linux"
      "aarch64-darwin"
      "aarch64-linux"
    ];
  };
})
