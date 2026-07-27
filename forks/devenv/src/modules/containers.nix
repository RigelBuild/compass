{ pkgs, config, lib, self, ... }:

let
  projectName = name:
    if config.name == null
    then throw ''You need to set `name = "myproject";` or `containers.${name}.name = "mycontainer"; to be able to generate a container.''
    else config.name;
  types = lib.types;
  projectRoot = builtins.path { path = self; name = "source"; };

  requiredInputs = config.lib.getInputs [
    {
      name = "nix2container";
      url = "github:nlewo/nix2container";
      attribute = "containers";
      follows = [ "nixpkgs" ];
    }
    {
      name = "mk-shell-bin";
      url = "github:rrbutani/nix-mk-shell-bin";
      attribute = "containers";
    }
  ];
  nix2container = requiredInputs.nix2container.packages.${pkgs.stdenv.system};
  mk-shell-bin = requiredInputs.mk-shell-bin;
  shell = mk-shell-bin.lib.mkShellBin { drv = config.shell; nixpkgs = pkgs; };
  bash = "${pkgs.bashInteractive}/bin/bash";
  mkEntrypoint = cfg: pkgs.writeScript "entrypoint" ''
    #!${bash}

    export PATH=/bin

    source ${shell.envScript}

    # expand any envvars before exec
    cmd="`echo "$@"|${pkgs.envsubst}/bin/envsubst`"

    ${bash} -c "$cmd"
  '';
  # Default container identity. Per-container overridable via the `user` /
  # `group` / `homeDir` options below — the uid/gid stay module-wide because
  # nix2container's `nixUid`/`nixGid` and the initialized Nix DB are built around
  # a single numeric owner, and nothing has needed to move it.
  #
  # These remain the DEFAULTS, and the two additions that would otherwise fire on
  # the default path — staging the container user's $HOME and its `perms` entry —
  # are guarded on the home actually moving. So a container that sets none of
  # these options resolves to the SAME `mkEtc` store path as upstream, with an
  # identical image config and no extra `perms` entry — held by construction:
  # every parameterized script body keeps upstream's bytes verbatim, line
  # breaking included, since a comment inside `runCommand`'s text is part of the
  # build command (see the `mkEtc` note below, and `mkHome` above).
  defaultUser = "user";
  defaultGroup = "user";
  uid = "1000";
  gid = "1000";
  defaultHomeDir = "/env";

  # Resolve a container's identity from its config, falling back to the defaults.
  # Everything that bakes identity into the image (the passwd/group/shadow rows,
  # the $HOME skeleton, file ownership, the image config's User/HOME/USER) reads
  # through these so a single option set moves all of them together.
  cfgUser = cfg: cfg.user;
  cfgGroup = cfg: cfg.group;
  cfgHomeDir = cfg: cfg.homeDir;

  # The homeDir of the container currently being built, for the module-scope
  # devenv.root/dotfile overrides below. The container being built is found from
  # config only: the backend re-evaluates the module tree once per container with
  # that container's `isBuilding` forced true
  # (devenv-nix-backend/bootstrap/bootstrapLib.nix, `mkContainerBuilds`), and that
  # `mkForce` is the single source of truth — nothing else ever sets `isBuilding`,
  # so at most one container carries it and this lookup is deterministic. Falls
  # back to the default when no container is building, so evaluation never depends
  # on a container that does not exist.
  buildingContainer =
    lib.findFirst (cfg: cfg.isBuilding) null (lib.attrValues config.containers);
  buildingHomeDir =
    if buildingContainer == null
    then defaultHomeDir
    else buildingContainer.homeDir;

  mkHome = cfg: path: (pkgs.runCommand "devenv-container-home" { } ''
    mkdir -p $out${cfgHomeDir cfg}
    if [ -d ${path} ]; then
      # Copy the directory's contents into the working directory so that, e.g.,
      # the project root ends up directly under ${cfgHomeDir cfg} rather than in a
      # hash-prefixed subdirectory.
      cp -rP ${path}/. $out${cfgHomeDir cfg}/
    else
      # Copy a single file using its original name, dropping the store hash.
      # Preserve symlinks (-P) rather than following them: paths produced by the
      # `files` option are symlinks into the store, and their targets are not part
      # of this source path's closure, so dereferencing would fail to stat them.
      # Keeping the symlink lets Nix's output scan pull the target into the
      # closure so it ends up in the image.
      cp -P ${path} "$out${cfgHomeDir cfg}/${baseNameOf path}"
    fi
  '');

  mkMultiHome = cfg: paths: map (mkHome cfg) paths;

  homeRoots = cfg: (
    if (builtins.typeOf cfg.copyToRoot == "list")
    then cfg.copyToRoot
    else [ cfg.copyToRoot ]
  );

  mkTmp = (pkgs.runCommand "devenv-container-tmp" { } ''
    mkdir -p $out/tmp
  '');

  # The container user's /etc scaffold: passwd/shadow/group rows, a permissive
  # pam stack, and — for a container that moves its home off the default — the
  # home directory itself.
  #
  # The $HOME staging is conditional on purpose. nix, direnv and devenv all write
  # into $HOME, and a root-owned or absent home makes nix fall back to the passwd
  # home with "$HOME is not owned by you" (ownership comes from the buildImage
  # `perms` entry below). But upstream never created this directory, so staging it
  # unconditionally would add an empty `/env` to every default-configured
  # container's output and move its derivation hash. The guard — and keeping this
  # explanation OUT of the script body, since a comment inside `runCommand`'s text
  # is itself part of the build command and would shift the hash on its own —
  # keeps the default path byte-identical to upstream. The optional fragment
  # carries its own leading `\n\n` as an explicit escape rather than as blank
  # lines inside an indented string, so the statement separation cannot be lost
  # to a whitespace-trimming edit.
  mkEtc = cfg: (pkgs.runCommand "devenv-container-etc" { } ''
    mkdir -p $out/etc/pam.d

    echo "root:x:0:0:System administrator:/root:${bash}" > \
          $out/etc/passwd
    echo "${cfgUser cfg}:x:${uid}:${gid}::${cfgHomeDir cfg}:${bash}" >> \
          $out/etc/passwd

    echo "root:!x:::::::" > $out/etc/shadow
    echo "${cfgUser cfg}:!x:::::::" >> $out/etc/shadow

    echo "root:x:0:" > $out/etc/group
    echo "${cfgGroup cfg}:x:${gid}:" >> $out/etc/group

    cat > $out/etc/pam.d/other <<EOF
    account sufficient pam_unix.so
    auth sufficient pam_rootok.so
    password requisite pam_unix.so nullok sha512
    session required pam_unix.so
    EOF

    touch $out/etc/login.defs${lib.optionalString (cfgHomeDir cfg != defaultHomeDir)
      "\n\nmkdir -p \"$out${cfgHomeDir cfg}\""}
  '');

  mkPerm = cfg: derivation:
    {
      path = derivation;
      mode = "0744";
      uid = lib.toInt uid;
      gid = lib.toInt gid;
      uname = cfgUser cfg;
      gname = cfgGroup cfg;
    };

  # The env baked into the image config, with devenv's own orchestration vars
  # dropped.
  #
  # `config.env` is the MERGED env of the whole devenv, and top-level.nix assigns
  # DEVENV_PROFILE/STATE/RUNTIME/DOTFILE/ROOT into it unconditionally (top-level.nix:
  # 328-332). Those are build-host coordinates — the builder's project directory,
  # its `/run/user/<uid>` runtime dir, its `.devenv` dotfile tree — and none of
  # them exists inside the image. Serializing them is worse than omitting them:
  # DEVENV_ROOT being set is precisely the sentinel devenv's shell hook reads to
  # conclude a devenv shell is already active (devenv/hooks/hook.posix.sh:24-35),
  # so an image carrying it silently refuses to activate anything. It also makes
  # the image config differ per build host.
  #
  # This has to happen HERE, at serialization. A project setting `env = { }` cannot
  # suppress them, because the merge that adds them happens upstream in
  # top-level.nix on the same attrset. A container's entrypoint still gets correct
  # in-image values from the sourced shell env script.
  imageEnv = lib.filterAttrs (name: _: !(lib.hasPrefix "DEVENV_" name)) config.env;


  mkDerivation = cfg: nix2container.nix2container.buildImage ({
    name = cfg.name;
    tag = cfg.version;
    initializeNixDatabase = true;
    nixUid = lib.toInt uid;
    nixGid = lib.toInt gid;

    copyToRoot = [
      (pkgs.buildEnv {
        name = "devenv-container-root";
        paths = [
          pkgs.coreutils-full
          pkgs.bashInteractive
          pkgs.su
          pkgs.sudo
          pkgs.dockerTools.usrBinEnv
        ];
        pathsToLink = [ "/bin" "/usr/bin" ];
      })
      (mkEtc cfg)
      mkTmp
    ];

    maxLayers = cfg.maxLayers;

    layers =
      if cfg.enableLayerDeduplication
      then
        builtins.foldl'
          (layers: layer:
            layers ++ [
              (nix2container.nix2container.buildLayer (layer // { inherit layers; }))
            ]
          )
          [ ]
          cfg.layers
      else builtins.map (layer: nix2container.nix2container.buildLayer layer) cfg.layers
    ;

    perms = [
      {
        path = mkTmp;
        regex = "/tmp";
        mode = "1777";
        uid = 0;
        gid = 0;
        uname = "root";
        gname = "root";
      }
    ]
    # The container user's $HOME, staged by mkEtc. Without this the directory
    # arrives root-owned and read-only, and every tool that writes under $HOME
    # (nix's cache, direnv, devenv) fails or silently falls back.
    #
    # Added only alongside the staged directory above, so a default-configured
    # container's perms list is unchanged.
    #
    # The regex matches the SOURCE path — nix2container tests it against
    # `srcPath`, the store path, BEFORE `rewrite` strips the store prefix
    # (nix/tar.go:104-106) — so it is anchored on the staging derivation's own
    # store path rather than on the in-image `$HOME`. Anchoring on the image
    # path (`^/home/agent`) silently matches nothing and the home stays
    # root-owned; leaving it bare would match any staged path merely
    # CONTAINING the home's name, since `re.Match` is unanchored.
    #
    # Both halves are regex-escaped: `homeDir` is a free-form `types.str`, so a
    # path holding regex syntax (`+`, `(`, `.`) would otherwise reach
    # `regexp.MustCompile` as a pattern — panicking the build on invalid syntax,
    # or silently matching paths the operator never named.
    ++ lib.optional (cfgHomeDir cfg != defaultHomeDir) {
      path = mkEtc cfg;
      regex = "^${lib.strings.escapeRegex "${mkEtc cfg}${cfgHomeDir cfg}"}";
      mode = "0755";
      uid = lib.toInt uid;
      gid = lib.toInt gid;
      uname = cfgUser cfg;
      gname = cfgGroup cfg;
    };

    config = {
      Entrypoint = cfg.entrypoint;
      User = "${cfgUser cfg}";
      WorkingDir = cfg.workingDir;
      Env = lib.mapAttrsToList
        (name: value:
          "${name}=${toString value}"
        )
        imageEnv ++ [ "HOME=${cfgHomeDir cfg}" "USER=${cfgUser cfg}" ];
      Cmd =
        if builtins.isList cfg.startupCommand
        then cfg.startupCommand
        else [ cfg.startupCommand ];
    };
  } // lib.optionalAttrs (cfg.fromImage != null) {
    fromImage = cfg.fromImage;
  });

  # <container> <registry> <args>
  mkCopyScript = cfg: pkgs.writeShellScript "copy-container" ''
    set -e -o pipefail

    container=$1
    shift

    if [[ "$1" == false ]]; then
      registry="${cfg.registry}"
    else
      registry="$1"
    fi
    shift

    dest="''${registry}${cfg.name}:${cfg.version}"

    if [[ $# == 0 ]]; then
      args=(${if cfg.defaultCopyArgs == [] then "" else toString cfg.defaultCopyArgs})
    else
      args=("$@")
    fi

    echo
    echo "Copying container $container to $dest"
    echo

    ${nix2container.skopeo-nix2container}/bin/skopeo --insecure-policy copy "nix:$container" "$dest" ''${args[@]}
  '';
  containerOptions = types.submodule ({ name, config, ... }: {
    options = {
      name = lib.mkOption {
        type = types.nullOr types.str;
        description = "Name of the container.";
        defaultText = "top-level name or containers.mycontainer.name";
        default = "${projectName name}-${name}";
      };

      fromImage = lib.mkOption {
        type = types.nullOr types.package;
        description = "An existing OCI base image to build on top of, built with nix2container's pullImage.";
        default = null;
      };

      version = lib.mkOption {
        type = types.nullOr types.str;
        description = "Version/tag of the container.";
        default = "latest";
      };

      copyToRoot = lib.mkOption {
        type = types.either types.path (types.listOf types.path);
        description = "Add a path to the container. Defaults to the whole git repo.";
        default = projectRoot;
        defaultText = lib.literalExpression "self";
      };

      startupCommand = lib.mkOption {
        type = types.nullOr (types.oneOf [ types.str types.package (types.listOf types.str) ]);
        description = ''
          Command to run in the container.

          Can be a string, a package, or a list of strings for individual arguments.
          Use a list when your entrypoint expects separate arguments, e.g.:
          `startupCommand = [ "-f" "/var/lib/haproxy/haproxy.cfg" ];`
        '';
        default = null;
      };

      entrypoint = lib.mkOption {
        type = types.listOf types.anything;
        description = "Entrypoint of the container.";
        default = [ (mkEntrypoint config) ];
        defaultText = lib.literalExpression "[ entrypoint ]";
      };

      user = lib.mkOption {
        type = types.str;
        description = ''
          Unix user name baked into the container's passwd/shadow entry, its
          image config `User`, and `$USER`. The uid stays 1000 regardless — it is
          what nix2container's `nixUid` and the initialized Nix DB are built
          around — so this renames the account, it does not renumber it.
        '';
        default = defaultUser;
      };

      group = lib.mkOption {
        type = types.str;
        description = "Unix group name baked into the container's group entry. The gid stays 1000.";
        default = defaultGroup;
      };

      homeDir = lib.mkOption {
        type = types.str;
        description = ''
          The container user's `$HOME`: its passwd home field, the `HOME` in the
          image config, the staged (and uid-owned) home directory, and the
          default `workingDir`. Set this when the image must match a home path
          an external supervisor execs with — a mismatch makes nix, direnv and
          devenv fall back to the passwd home ("$HOME is not owned by you").
        '';
        default = defaultHomeDir;
      };

      workingDir = lib.mkOption {
        type = types.str;
        description = "Working directory of the container.";
        default = config.homeDir;
      };

      defaultCopyArgs = lib.mkOption {
        type = types.listOf types.str;
        description =
          ''
            Default arguments to pass to `skopeo copy`.
            You can override them by passing arguments to the script.
          '';
        default = [ ];
      };

      registry = lib.mkOption {
        type = types.nullOr types.str;
        description = "Registry to push the container to.";
        default = "docker-daemon:";
      };

      maxLayers = lib.mkOption {
        type = types.nullOr types.int;
        description = "Maximum number of container layers created.";
        default = 1;
      };

      enableLayerDeduplication = (lib.mkEnableOption ''
        layer deduplication using the approach described at https://blog.eigenvalue.net/2023-nix2container-everything-once/
      '') // { default = true; };

      layers = lib.mkOption {
        type = types.listOf (types.submoduleWith {
          modules = [
            {
              options = {
                deps = lib.mkOption {
                  type = types.listOf types.package;
                  description = "A list of store paths to include in the layer.";
                  default = [ ];
                };
                copyToRoot = lib.mkOption {
                  type = types.listOf types.package;
                  description = ''
                    A list of derivations copied to the image root directory.

                    Store path prefixes ``/nix/store/hash-path`` are removed in order to relocate them to the image ``/``.
                  '';
                  default = [ ];
                };
                reproducible = lib.mkOption {
                  type = types.bool;
                  description = "Whether the layer should be reproducible.";
                  default = true;
                };
                maxLayers = lib.mkOption {
                  type = types.int;
                  description = "The maximum number of layers to create.";
                  default = 1;
                };
                perms = lib.mkOption {
                  description = ''
                    A list of file permissions which are set when the tar layer is created.

                    These permissions are not written to the Nix store.
                  '';
                  default = [ ];
                  type = types.listOf (types.submoduleWith {
                    modules = [
                      {
                        options = {
                          path = lib.mkOption {
                            type = types.pathInStore;
                            description = "A store path.";
                          };
                          regex = lib.mkOption {
                            type = types.nullOr types.str;
                            description = "A regex pattern to select files or directories to apply the ``mode`` to.";
                            example = ".*";
                            default = null;
                          };
                          mode = lib.mkOption {
                            type = types.nullOr types.str;
                            description = "The numeric permissions mode to apply to all of the files matched by the ``regex``.";
                            example = "644";
                            default = null;
                          };
                          gid = lib.mkOption {
                            type = types.nullOr types.int;
                            description = "The group ID to apply to all of the files matched by the ``regex``.";
                            example = "1000";
                            default = null;
                          };
                          uid = lib.mkOption {
                            type = types.nullOr types.int;
                            description = "The user ID to apply to all of the files matched by the ``regex``.";
                            example = "1000";
                            default = null;
                          };
                          uname = lib.mkOption {
                            type = types.nullOr types.str;
                            description = "The user name to apply to all of the files matched by the ``regex``.";
                            example = "root";
                            default = null;
                          };
                          gname = lib.mkOption {
                            type = types.nullOr types.str;
                            description = "The group name to apply to all of the files matched by the ``regex``.";
                            example = "root";
                            default = null;
                          };
                        };
                      }
                    ];
                  });
                };
                ignore = lib.mkOption {
                  type = types.nullOr types.pathInStore;
                  default = null;
                  description = ''
                    A store path to ignore when building the layer. This is mainly useful to ignore the configuration file from the container layer.
                  '';
                };
              };
            }
          ];
        });
        description = "The layers to create.";
        default = [ ];
      };

      isBuilding = lib.mkOption {
        type = types.bool;
        default = false;
        description = "Set to true when the environment is building this container.";
      };

      derivation = lib.mkOption {
        type = types.package;
        internal = true;
        default = mkDerivation config;
      };

      copyScript = lib.mkOption {
        type = types.package;
        internal = true;
        default = mkCopyScript config;
      };

      dockerRun = lib.mkOption {
        type = types.package;
        internal = true;
        default = pkgs.writeShellScript "docker-run" ''
          if [ -t 0 ]; then
            ${pkgs.docker-client}/bin/docker run -it ${config.name}:${config.version} "$@"
          else
            ${pkgs.docker-client}/bin/docker run -i ${config.name}:${config.version} "$@"
          fi
        '';
      };
    };

    config.layers = [
      {
        perms = map (mkPerm config) (mkMultiHome config (homeRoots config));
        copyToRoot = mkMultiHome config (homeRoots config);
      }
    ];
  });
in
{
  options = {
    containers = lib.mkOption {
      type = types.attrsOf containerOptions;
      default = { };
      description = "Container specifications that can be built, copied and ran using `devenv container`.";
    };

    container = {
      isBuilding = lib.mkOption {
        type = types.bool;
        default = false;
        description = ''
          Devenv set it to true when the environment is a container.

          Example:
          ```nix
          { pkgs, config, lib, ... }:
          {
            packages = [ pkgs.openssl ]
            ++ lib.optionals (!config.container.isBuilding) [ pkgs.git ];
          }
          ```
        '';
      };
    };
  };

  config = lib.mkMerge [
    {
      containers.shell = {
        name = lib.mkDefault "shell";
        startupCommand = lib.mkDefault bash;
      };

      containers.processes = {
        name = lib.mkDefault "processes";
        startupCommand = lib.mkDefault config.procfileScript;
      };
    }
    (lib.mkIf config.container.isBuilding {
      devenv.tmpdir = lib.mkOverride (lib.modules.defaultOverridePriority - 1) "/tmp";
      devenv.runtime = lib.mkOverride (lib.modules.defaultOverridePriority - 1) "${config.devenv.tmpdir}/devenv";
      # The building container's own home, not the module default: `homeDir` is
      # per-container now, and `buildingHomeDir` reads it off the one container the
      # backend forced `isBuilding` on.
      devenv.root = lib.mkForce buildingHomeDir;
      devenv.dotfile = lib.mkOverride 49 "${buildingHomeDir}/.devenv";
    })
    {
      tasks."devenv:container:copy" = {
        exec = ''
          copy_script=$(${pkgs.jq}/bin/jq -r '.copy_script' <<< "$DEVENV_TASK_INPUT")
          spec=$(${pkgs.jq}/bin/jq -r '.spec' <<< "$DEVENV_TASK_INPUT")
          registry=$(${pkgs.jq}/bin/jq -r '.registry' <<< "$DEVENV_TASK_INPUT")
          readarray -t copy_args < <(${pkgs.jq}/bin/jq -r '.copy_args[]' <<< "$DEVENV_TASK_INPUT")

          "$copy_script" "$spec" "$registry" "''${copy_args[@]}"
        '';
        showOutput = true;
      };
    }
  ];
}
