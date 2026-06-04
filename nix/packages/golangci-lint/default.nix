# golangci-lint built with the Attestant `attgo` module plugin compiled in.
#
# golangci-lint module plugins (see .golangci.yml `linters.settings.custom`)
# cannot be loaded at runtime: Go has no dynamic plugin loading here, so the
# plugin's code must be linked into the binary. Upstream's `golangci-lint custom`
# command does this by cloning golangci-lint, blank-importing each plugin into
# cmd/golangci-lint/plugins.go, adding the module to go.mod, and rebuilding --
# all of which needs network access at build time.
#
# add-attgo-plugin.patch reproduces exactly those edits (for the plugins pinned
# in .custom-gcl.yml: attgo-linter v0.2.0) so the build is reproducible inside
# the Nix sandbox. Keep the pinned versions here in sync with .custom-gcl.yml.
{ pkgs, ... }:
# Build with the Go toolchain the project pins (go.mod / .github workflows) so
# the linter's type checking matches the code under test.
pkgs.buildGo125Module (finalAttrs: {
  pname = "golangci-lint";
  version = "2.8.0";

  src = pkgs.fetchFromGitHub {
    owner = "golangci";
    repo = "golangci-lint";
    tag = "v${finalAttrs.version}";
    hash = "sha256-w6MAOirj8rPHYbKrW4gJeemXCS64fNtteV6IioqIQTQ=";
  };

  # Inject the attgo plugin: blank import in plugins.go plus the matching
  # go.mod / go.sum requirement. Applied to both the vendor and build phases.
  patches = [ ./add-attgo-plugin.patch ];

  vendorHash = "sha256-LYc+lRB+mbg75Au1ok0Az8sFL+HtgPknpFPF0Zgz0EE=";

  subPackages = [ "cmd/golangci-lint" ];

  nativeBuildInputs = [ pkgs.installShellFiles ];

  # Mirror upstream's goreleaser ldflags so `golangci-lint version` is sane;
  # the -custom-gcl suffix signals the plugin-augmented build.
  ldflags = [
    "-s"
    "-w"
    "-X main.version=${finalAttrs.version}-custom-gcl"
    "-X main.commit=v${finalAttrs.version}"
    "-X main.date=1970-01-01T00:00:00Z"
  ];

  postInstall = ''
    installShellCompletion --cmd golangci-lint \
      --bash <($out/bin/golangci-lint completion bash) \
      --fish <($out/bin/golangci-lint completion fish) \
      --zsh <($out/bin/golangci-lint completion zsh)
  '';

  meta = {
    description = "Fast linters runner for Go, with the Attestant attgo plugin compiled in";
    homepage = "https://golangci-lint.run/";
    changelog = "https://github.com/golangci/golangci-lint/blob/v${finalAttrs.version}/CHANGELOG.md";
    mainProgram = "golangci-lint";
    license = pkgs.lib.licenses.gpl3Plus;
  };
})
