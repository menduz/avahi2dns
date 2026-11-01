{
  lib,
  buildGoModule,
}:

buildGoModule {
  pname = "avahi2dns";
  version = "0.2.1-unstable";

  # Only the files of the Go build: a change in nix/ or in the README does not
  # rebuild the package.
  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      ../.mockery.yml
      (lib.fileset.fileFilter (file: file.hasExt "go") ../.)
    ];
  };

  vendorHash = "sha256-G1lF4Vy9wzGkDWIcFJksuTfIwyxymauYZIFAys6M/9g=";

  # The tests use the mocks of mockery (a tool in go.mod), which are not in git.
  preCheck = ''
    go generate ./...
  '';

  meta = {
    description = "Small DNS server which interfaces with avahi";
    homepage = "https://github.com/LouisBrunner/avahi2dns";
    license = lib.licenses.mit;
    platforms = lib.platforms.linux;
    mainProgram = "avahi2dns";
  };
}
