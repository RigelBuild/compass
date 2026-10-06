{ pkgs, lib }:
# /etc/compass-agent/source-fingerprint: sha256 over the sorted sha256sum-format
# lines (`<sha256>  <relpath>\n`) of the bundled source. The e2e fixture
# recomputes it from the tree (agentSourceFingerprint in go/e2e) and refuses a
# stale local compass-agent:latest; the two must stay byte-identical.
let
  root = ../packages/compass-agent;
  relPath = f: lib.removePrefix (toString root + "/") (toString f);
  files = lib.sort (a: b: relPath a < relPath b) (
    lib.fileset.toList (import ./agent-source-files.nix { inherit lib; })
  );
  fingerprint = builtins.hashString "sha256" (
    lib.concatMapStrings (f: "${builtins.hashFile "sha256" f}  ${relPath f}\n") files
  );
in
pkgs.writeTextDir "etc/compass-agent/source-fingerprint" fingerprint
