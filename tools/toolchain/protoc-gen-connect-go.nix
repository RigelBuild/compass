{ pkgs, goToolchain }:
(pkgs.buildGoModule.override { go = goToolchain; }) {
  pname = "protoc-gen-connect-go";
  version = "go-mod"; # go.mod pins this plugin through the connect/v2 tool module.
  src = builtins.path { path = ../../go; name = "compass-go-src"; };
  proxyVendor = true;
  vendorHash = "sha256-hTx1g3emTlQrjxI6WCb0SpeQUXg2HHRzA28vRc1KmGQ=";
  doCheck = false; # src is all of compass go/; its tests run elsewhere.
  buildPhase = ''
    runHook preBuild
    go install -ldflags=-buildid= connectrpc.com/connect/v2/cmd/protoc-gen-connect-go
    runHook postBuild
  '';
}
