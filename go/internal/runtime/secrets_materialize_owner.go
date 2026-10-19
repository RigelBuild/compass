package runtime

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/RigelBuild/compass/go/internal/forgecred"
)

// GHOwnerTokensScript atomically writes owner credentials and the last owner
// marker, then removes files absent from the refreshed set.
func GHOwnerTokensScript(homeDir string, creds []GHCredentials) (string, error) {
	root := filepath.Join(homeDir, ".compass", "forge")
	owners := make([]string, 0, len(creds))
	active := make(map[string]struct{}, len(creds)*2)
	tokens := make(map[string]string, len(creds))
	defaults := make(map[string]string)
	for _, credential := range creds {
		host := credential.Host
		if !isValidHost(host) {
			return "", &InvalidHostError{Host: host}
		}
		owner := strings.ToLower(credential.Owner)
		if owner == "" {
			continue
		}
		if !forgecred.ValidOwner(owner) || strings.Contains(owner, "--") {
			return "", fmt.Errorf("invalid forge owner %q", credential.Owner)
		}
		path := forgecred.TokenPath(homeDir, host, owner)
		if _, exists := tokens[path]; !exists {
			owners = append(owners, path)
		}
		tokens[path] = credential.Token
		active[path] = struct{}{}
		defaults[host] = owner
	}

	hosts := make([]string, 0, len(defaults))
	for host := range defaults {
		hosts = append(hosts, host)
		active[forgecred.DefaultOwnerPath(homeDir, host)] = struct{}{}
	}
	sort.Strings(hosts)

	var b strings.Builder
	b.WriteString("set -eu\numask 077\n")
	b.WriteString("root=" + shellSingleQuote(root) + "\n")
	if len(creds) == 0 {
		b.WriteString(`rm -rf -- "$root"` + "\n")
		return b.String(), nil
	}
	b.WriteString(`mkdir -p "$root"` + "\n")
	for _, path := range owners {
		b.WriteString("d=" + shellSingleQuote(filepath.Dir(path)) + "\n")
		b.WriteString(`mkdir -p "$d"` + "\n")
		writeOwnerFile(&b, path, tokens[path])
	}
	for _, host := range hosts {
		writeOwnerFile(&b, forgecred.DefaultOwnerPath(homeDir, host), defaults[host]+"\n")
	}
	b.WriteString(`for f in "$root"/*/*; do` + "\n")
	b.WriteString(`  [ -f "$f" ] || continue` + "\n")
	writeCleanupCase(&b, active)
	b.WriteString("done\n")
	b.WriteString(`for f in "$root"/*/.default-owner; do` + "\n")
	b.WriteString(`  [ -f "$f" ] || continue` + "\n")
	writeCleanupCase(&b, defaultMarkerPaths(homeDir, hosts))
	b.WriteString("done\n")
	return b.String(), nil
}

func defaultMarkerPaths(home string, hosts []string) map[string]struct{} {
	paths := make(map[string]struct{}, len(hosts))
	for _, host := range hosts {
		paths[forgecred.DefaultOwnerPath(home, host)] = struct{}{}
	}
	return paths
}

func writeCleanupCase(b *strings.Builder, active map[string]struct{}) {
	paths := make([]string, 0, len(active))
	for path := range active {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		b.WriteString(`  rm -f -- "$f"` + "\n")
		return
	}
	b.WriteString(`  case "$f" in` + "\n")
	for _, path := range paths {
		b.WriteString("    " + shellSingleQuote(path) + ") ;;\n")
	}
	b.WriteString(`    *) rm -f -- "$f" ;;` + "\n")
	b.WriteString("  esac\n")
}

func writeOwnerFile(b *strings.Builder, path, value string) {
	encoded := base64.StdEncoding.EncodeToString([]byte(value))
	b.WriteString("f=" + shellSingleQuote(path) + "\n")
	b.WriteString(`t="$f.tmp.$$"` + "\n")
	b.WriteString(`(set -C; : > "$t")` + "\n")
	b.WriteString("printf %s '" + encoded + "' | base64 -d > \"$t\"\n")
	b.WriteString(`chmod 600 "$t"` + "\n")
	b.WriteString(`mv -f "$t" "$f"` + "\n")
}
