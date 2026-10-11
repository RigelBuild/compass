{ lib }:
let
  pin = import ./agent-pin.nix { inherit lib; };
  digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
  amdManifest = "sha256:1111111111111111111111111111111111111111111111111111111111111111";
  armManifest = "sha256:2222222222222222222222222222222222222222222222222222222222222222";
  layerA = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
  layerB = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc";
  manifest = layers: {
    mediaType = "application/vnd.oci.image.manifest.v1+json";
    inherit layers;
  };
  layer = hash: {
    mediaType = "application/vnd.oci.image.layer.v1.tar+gzip";
    digest = hash;
  };
  lock = {
    repo = "ghcr.io/rigelbuild/compass-agent";
    tag = "git-0123456789ab";
    inherit digest;
    platforms = {
      "linux/amd64" = { manifest = amdManifest; layers = [ layerA layerB ]; };
      "linux/arm64" = { manifest = armManifest; layers = [ layerB layerA ]; };
    };
  };
  index = {
    mediaType = "application/vnd.oci.image.index.v1+json";
    manifests = [
      { mediaType = "application/vnd.oci.image.manifest.v1+json"; digest = amdManifest; platform = { os = "linux"; architecture = "amd64"; }; }
      { mediaType = "application/vnd.oci.image.manifest.v1+json"; digest = armManifest; platform = { os = "linux"; architecture = "arm64"; }; }
    ];
  };
  force = value: builtins.tryEval (builtins.deepSeq value value);
  succeeds = value: (force value).success;
  fails = value: !(force value).success;
  tests = {
    platformForAmd64 = { expr = succeeds (pin.platformFor "x86_64-linux"); expected = true; };
    platformForArm64 = { expr = succeeds (pin.platformFor "aarch64-linux"); expected = true; };
    unsupportedSystem = { expr = fails (pin.platformFor "x86_64-darwin"); expected = true; };
    checkLockPositive = { expr = succeeds (pin.checkLock lock); expected = true; };
    checkLockMissingArm = { expr = fails (pin.checkLock (lock // { platforms = builtins.removeAttrs lock.platforms [ "linux/arm64" ]; })); expected = true; };
    checkLockExtraPlatform = { expr = fails (pin.checkLock (lock // { platforms = lock.platforms // { "linux/s390x" = lock.platforms."linux/arm64"; }; })); expected = true; };
    checkLockV1 = { expr = fails (pin.checkLock (builtins.removeAttrs lock [ "platforms" ] // { layers = [ layerA ]; })); expected = true; };
    memberDigestAmd64 = { expr = succeeds (pin.memberDigest { inherit lock index; platform = "linux/amd64"; }); expected = true; };
    memberDigestArm64 = { expr = succeeds (pin.memberDigest { inherit lock index; platform = "linux/arm64"; }); expected = true; };
    memberDigestMissingPlatform = { expr = fails (pin.memberDigest { inherit lock; index = index // { manifests = [ (builtins.head index.manifests) ]; }; platform = "linux/amd64"; }); expected = true; };
    memberDigestDuplicatePlatform = { expr = fails (pin.memberDigest { inherit lock; index = index // { manifests = index.manifests ++ [ (builtins.head index.manifests) ]; }; platform = "linux/amd64"; }); expected = true; };
    memberDigestMismatch = { expr = fails (pin.memberDigest { inherit lock; index = index // { manifests = [ (builtins.head index.manifests // { digest = armManifest; }) (builtins.elemAt index.manifests 1) ]; }; platform = "linux/amd64"; }); expected = true; };
    memberLayersAmd64 = { expr = succeeds (pin.memberLayers { inherit lock; manifest = manifest [ (layer layerA) (layer layerB) ]; platform = "linux/amd64"; }); expected = true; };
    memberLayersArm64 = { expr = succeeds (pin.memberLayers { inherit lock; manifest = manifest [ (layer layerB) (layer layerA) ]; platform = "linux/arm64"; }); expected = true; };
    memberLayersMismatch = { expr = fails (pin.memberLayers { inherit lock; manifest = manifest [ (layer layerA) ]; platform = "linux/amd64"; }); expected = true; };
  };
  failures = lib.runTests tests;
in
if failures == [ ] then true
else throw "guest-image: agent pin tests failed: ${builtins.concatStringsSep ", " failures}"
