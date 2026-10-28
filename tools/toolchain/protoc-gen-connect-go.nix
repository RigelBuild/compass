{ pkgs, goToolchain }:
(pkgs.buildGoModule.override { go = goToolchain; }) {
  pname = "protoc-gen-connect-go";
  version = "go-mod"; # go.mod pins this plugin through the connect/v2 tool module.
  src = builtins.path { path = ../../go; name = "compass-go-src"; };
  proxyVendor = true;
  vendorHash = "sha256-qpXrjwm1eNNIc6MLBZbXDyKNwxudyIoPeyD/W63bQfA=";
  doCheck = false; # src is all of compass go/; its tests run elsewhere.
  buildPhase = ''
    runHook preBuild
    go install -ldflags=-buildid= connectrpc.com/connect/v2/cmd/protoc-gen-connect-go
    runHook postBuild
  '';
}
