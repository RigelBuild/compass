{ lib }:
let
  platforms = [ "linux/amd64" "linux/arm64" ];
  indexMediaType = "application/vnd.oci.image.index.v1+json";
  manifestMediaType = "application/vnd.oci.image.manifest.v1+json";
  layerMediaType = "application/vnd.oci.image.layer.v1.tar+gzip";
  repo = "ghcr.io/rigelbuild/compass-agent";
  isSha256 = value: builtins.isString value && builtins.match "sha256:[0-9a-f]{64}" value != null;
  isObject = value: builtins.isAttrs value;
  platformFor = system:
    {
      "x86_64-linux" = "linux/amd64";
      "aarch64-linux" = "linux/arm64";
    }.${system} or (throw "guest-image: no agent image member for ${system}; agent-oci.lock pins linux/amd64 and linux/arm64 only");
  checkLock = lock:
    let
      platformData = if isObject lock then lock.platforms or null else null;
      platformKeys = if isObject platformData then builtins.attrNames platformData else [ ];
      validMember = member:
        isObject member
        && isSha256 (member.manifest or null)
        && builtins.isList (member.layers or null)
        && member.layers != [ ]
        && builtins.all isSha256 member.layers;
      bad =
        if !isObject lock then "lock must be a JSON object"
        else if (lock.repo or null) != repo then "repo must be ${repo}"
        else if !builtins.isString (lock.tag or null) || builtins.match "git-[0-9a-f]{12}" lock.tag == null then "tag must match git-<sha12>"
        else if !isSha256 (lock.digest or null) then "digest must be sha256:<64 hex>"
        else if !isObject platformData then "platforms must be an object"
        else if builtins.sort builtins.lessThan platformKeys != builtins.sort builtins.lessThan platforms then "platforms must contain exactly linux/amd64 and linux/arm64"
        else if !builtins.all (platform: validMember (platformData.${platform} or null)) platforms then "each platform needs a manifest digest and non-empty sha256 layers"
        else null;
    in
    if bad == null then lock
    else throw "guest-image: agent-oci.lock is not a valid pin: ${bad}";
  descriptorPlatform = descriptor:
    let
      value = if isObject descriptor then descriptor.platform or null else null;
      os = if isObject value then value.os or null else null;
      architecture = if isObject value then value.architecture or null else null;
    in
    if os == "linux" && architecture == "amd64" then "linux/amd64"
    else if os == "linux" && architecture == "arm64" then "linux/arm64"
    else throw "guest-image: OCI index member has unsupported platform ${toString os}/${toString architecture}";
  memberDigest = { lock, index, platform }:
    let
      checked = checkLock lock;
      descriptors = if isObject index && (index.mediaType or null) == indexMediaType && builtins.isList (index.manifests or null) then index.manifests
        else throw "guest-image: agent image index is not an OCI image index";
      memberAt = descriptor:
        let
          platform = descriptorPlatform descriptor;
        in
        {
          inherit platform;
          digest = if isObject descriptor then descriptor.digest or null else null;
          mediaType = if isObject descriptor then descriptor.mediaType or null else null;
        };
      members = map memberAt descriptors;
      actualPlatforms = builtins.sort builtins.lessThan (map (member: member.platform) members);
      selected = builtins.filter (member: member.platform == platform) members;
      lockMember = checked.platforms.${platform} or (throw "guest-image: lock has no member for ${platform}");
      descriptor = if builtins.length selected == 1 then builtins.head selected else throw "guest-image: OCI index has no unique ${platform} member";
    in
    if !(builtins.elem platform platforms) then throw "guest-image: unsupported pinned platform ${toString platform}"
    else if actualPlatforms != platforms then throw "guest-image: OCI index platform set must contain exactly linux/amd64 and linux/arm64"
    else if !builtins.all (member: member.mediaType == manifestMediaType && isSha256 member.digest) members then throw "guest-image: OCI index member is not a valid image manifest descriptor"
    else if descriptor.digest != lockMember.manifest then throw "guest-image: ${platform} index member digest does not match agent-oci.lock"
    else descriptor.digest;
  memberLayers = { lock, manifest, platform }:
    let
      checked = checkLock lock;
      lockMember = checked.platforms.${platform} or (throw "guest-image: lock has no member for ${toString platform}");
      validManifest = isObject manifest && (manifest.mediaType or null) == manifestMediaType && builtins.isList (manifest.layers or null) && manifest.layers != [ ];
      layers = if validManifest then map (layer: if isObject layer then layer.digest or null else null) manifest.layers else [ ];
      layerTypesValid = validManifest && builtins.all (layer: isObject layer && (layer.mediaType or null) == layerMediaType && isSha256 (layer.digest or null)) manifest.layers;
    in
    if !(builtins.elem platform platforms) then throw "guest-image: unsupported pinned platform ${toString platform}"
    else if !validManifest || !layerTypesValid then throw "guest-image: ${toString platform} member manifest has invalid layers"
    else if layers != lockMember.layers then throw "guest-image: agent-oci.lock layers do not match the manifest it pins (${lockMember.manifest}). Rewrite it with `bun tools/guest-image/pin-agent-image.ts --relock`, never by hand."
    else lockMember.layers;
in
{
  inherit platformFor checkLock memberDigest memberLayers;
}
