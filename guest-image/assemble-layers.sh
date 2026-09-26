# Unpacks OCI layer tarballs into a staged rootfs tree. Shared by the rootfs
# derivation and its fixture tests so both exercise the same code. Bash, not
# TypeScript: it runs inside a nix runCommand builder, which has no bun.
#   unpack ROOT LAYER...                 header allowlist, whiteouts, extract
#   restore-dir-modes ROOT LAYER...      re-apply published directory modes
#   check-contract ROOT IMAGE PATH...    each PATH resolves to an executable
set -euo pipefail

# Only regular files, directories and links that stay inside the tree. Device
# nodes, FIFOs and escaping links have no business in a guest userland.
check_headers() {
  local layer=$1 bad
  # Escaping spaces in names leaves the raw " -> " / " link to " delimiter
  # unambiguous; numeric owners keep the five-field prefix fixed; -P lists
  # targets as published rather than with tar's leading-/ stripped.
  if ! bad=$(tar -P --quoting-style=escape --quote-chars=' ' --numeric-owner -tvzf "$layer" | awk '
    function climbs(name, target,   n, p, i, depth, m, q) {
      n = split(name, p, "/")
      depth = 0
      for (i = 1; i < n; i++) if (p[i] != "" && p[i] != ".") depth++
      m = split(target, q, "/")
      for (i = 1; i <= m; i++) {
        if (q[i] == "..") { if (--depth < 0) return 1 }
        else if (q[i] != "" && q[i] != ".") depth++
      }
      return 0
    }
    {
      t = substr($0, 1, 1)
      line = $0
      sub(/^([^ ]+ +){5}/, "", line)
      # restore-dir-modes feeds names to tar -T, which reads a leading "-" as an
      # option: "--directory=.." would retarget every later directory.
      if (substr(line, 1, 1) == "-") { print "  option-like member name: " $0; bad = 1; next }
      if (t == "-" || t == "d") next
      if (t != "l" && t != "h") { print "  unsupported entry type: " $0; bad = 1; next }
      sep = (t == "l") ? " -> " : " link to "
      i = index(line, sep)
      if (i == 0) { print "  unparseable link entry: " $0; bad = 1; next }
      name = substr(line, 1, i - 1)
      target = substr(line, i + length(sep))
      if (t == "h") {
        if (target ~ /^\// || target ~ /(^|\/)\.\.(\/|$)/) { print "  hardlink leaves the archive: " $0; bad = 1 }
      } else if (target ~ /^\//) {
        if (target !~ /^\/nix\/store\// || target ~ /(^|\/)\.\.(\/|$)/) { print "  symlink to a host path: " $0; bad = 1 }
      } else if (climbs(name, target)) {
        print "  symlink climbs out of the archive root: " $0; bad = 1
      }
    }
    END { exit bad }
  '); then
    echo "guest-image: BUILD-BREAK — layer $layer has entries outside the allowlist:" >&2
    printf '%s\n' "$bad" >&2
    exit 1
  fi
}

unpack() {
  local root=$1 layer members d p c marker dir base
  shift
  # Whiteouts in a layer hide only the layers BELOW it, so each layer's markers
  # are applied before its own content is extracted -- an opaque marker applied
  # afterwards would delete the sibling files that same layer adds.
  for layer in "$@"; do
    check_headers "$layer"
    members=$(tar -tzf "$layer" | sed 's|^\./||')

    # Archive names drive the rm -rf and tar writes below, so a `..` component
    # fails the build rather than being sanitized: tar would follow it out.
    if printf '%s\n' "$members" | grep -qE '(^|/)\.\.(/|$)'; then
      echo "guest-image: BUILD-BREAK — layer $layer has a '..' member path." >&2
      exit 1
    fi

    # A layer replacing a lower symlink with a real directory is legitimate OCI,
    # but tar would write THROUGH the stale symlink. Drop such parents first.
    for d in $(printf '%s\n' "$members" | sed -n 's|/[^/]*$||p' | sort -u); do
      p=""
      IFS=/
      for c in $d; do
        [ -n "$c" ] || continue
        p="$p/$c"
        if [ -L "$root$p" ]; then rm -f "$root$p"; fi
      done
      unset IFS
    done

    # A marker is a `.wh.`-prefixed final COMPONENT; a path merely containing
    # `.wh.` is ordinary content, not a delete order.
    printf '%s\n' "$members" | { grep -E '(^|/)\.wh\.' || true; } | while read -r marker; do
      dir=$(dirname "$marker")
      base=$(basename "$marker")
      case "$base" in
        .wh.*) ;;
        *) continue ;;
      esac
      if [ "$base" = ".wh..wh..opq" ]; then
        # A marker naming a path the lower layers never created means the stack
        # is not what this build thinks it is.
        if [ ! -d "$root/$dir" ]; then
          echo "guest-image: BUILD-BREAK — opaque marker names a missing directory $dir." >&2
          exit 1
        fi
        find "$root/$dir" -mindepth 1 -maxdepth 1 -exec rm -rf {} +
      else
        rm -rf "$root/$dir/${base#.wh.}"
      fi
    done

    # -p keeps each member's published mode rather than the builder's umask;
    # staging write access is re-opened on DIRECTORIES only.
    tar -xzpf "$layer" -C "$root" --overwrite \
      --exclude='.wh.*' --exclude='*/.wh.*'
    find "$root" -type d -exec chmod u+w {} +
  done
}

restore_dir_modes() {
  local root=$1 layer dirs
  shift
  dirs=$(mktemp)
  for layer in "$@"; do
    # Select directories by tar's type flag: these layers list directories with
    # no trailing slash. The name is everything after the five-field prefix, in
    # tar's escape quoting, which -T unquotes.
    if tar --quoting-style=escape --numeric-owner -tvzf "$layer" \
      | awk '$1 ~ /^d/ { sub(/^([^ ]+ +){5}/, ""); print }' > "$dirs"; then
      tar -xzpf "$layer" -C "$root" --overwrite --no-recursion -T "$dirs"
    fi
  done
  rm -f "$dirs"
  # Set / outright: a boot-layer `cp -a` stamps the store's 0555 onto it.
  chmod 0755 "$root"
}

check_contract() {
  local root=$1 image=$2 p resolved rest hops comp tail link target
  shift 2
  # EVERY component resolves inside $root, as the kernel will after
  # switch_root; following only the final symlink would resolve an intermediate
  # /bin -> /nix/store/... against the BUILDER's store.
  for p in "$@"; do
    resolved=""
    rest="/$p"
    hops=0
    while [ -n "$rest" ]; do
      comp=${rest#/}
      comp=${comp%%/*}
      tail=${rest#/"$comp"}
      case "$comp" in
        . | "")
          rest="$tail"
          continue
          ;;
        ..)
          resolved=$(dirname "$resolved")
          [ "$resolved" = "/" ] || [ "$resolved" = "." ] && resolved=""
          rest="$tail"
          continue
          ;;
      esac
      resolved="$resolved/$comp"
      if [ -L "$root$resolved" ]; then
        # Count symlink traversals, not components; Linux's own ELOOP is 40.
        hops=$((hops + 1))
        if [ "$hops" -gt 40 ]; then
          echo "guest-image: BUILD-BREAK — /$p is a symlink loop in the image." >&2
          exit 1
        fi
        link=$(readlink "$root$resolved")
        case "$link" in
          /*)
            resolved=""
            rest="$link$tail"
            ;;
          *)
            resolved=$(dirname "$resolved")
            [ "$resolved" = "/" ] || [ "$resolved" = "." ] && resolved=""
            rest="/$link$tail"
            ;;
        esac
      else
        rest="$tail"
      fi
    done
    target="$resolved"
    if [ ! -f "$root$target" ] || [ ! -x "$root$target" ]; then
      echo "guest-image: BUILD-BREAK — /$p is not an executable in the rootfs." >&2
      echo "  It resolves to $target, which the image does not carry as one." >&2
      echo "  The guest userland comes from the pinned agent image ($image)." >&2
      echo "  Either that image stopped shipping it, or a layer failed to" >&2
      echo "  unpack. /bin/sh missing is a TOTAL backend outage: every" >&2
      echo "  microVM Start shells out to arm egress." >&2
      exit 1
    fi
  done
}

cmd=${1:?usage: assemble-layers.sh unpack|restore-dir-modes|check-contract ROOT ...}
shift
case "$cmd" in
  unpack) unpack "$@" ;;
  restore-dir-modes) restore_dir_modes "$@" ;;
  check-contract) check_contract "$@" ;;
  *)
    echo "assemble-layers.sh: unknown command $cmd" >&2
    exit 2
    ;;
esac
