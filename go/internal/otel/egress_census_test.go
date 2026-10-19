package otel

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type egressClass int

const (
	traced egressClass = iota
	noProvider
	testHelper
)

type egressSite struct {
	class  egressClass
	count  int
	reason string
}

var egressSites = map[string]egressSite{
	"cmd/compass-app/bridge_service.go:probe:compassv1connect.NewCompassServiceClient":                {class: noProvider, count: 1, reason: "the app installs no provider"},
	"cmd/compass-runner/main.go:runnerHTTPClient:http.DefaultClient":                                  {class: traced, count: 1, reason: "runner.Dial wraps it with the Connect interceptor"},
	"cmd/compass/client.go:httpClientFor:http.DefaultClient":                                          {class: noProvider, count: 1, reason: "the CLI has no provider; tracing is disabled by design"},
	"cmd/compass/client.go:newAgentRepositoryClient:compassv1connect.NewAgentRepositoryServiceClient": {class: noProvider, count: 1, reason: "the CLI installs no provider"},
	"cmd/compass/client.go:newClient:compassv1connect.NewCompassServiceClient":                        {class: noProvider, count: 1, reason: "the CLI installs no provider"},
	"cmd/compass/client.go:newCommsClient:compassv1connect.NewCommsServiceClient":                     {class: noProvider, count: 1, reason: "the CLI installs no provider"},
	"cmd/compass/client.go:newSecretsClient:compassv1connect.NewSecretsServiceClient":                 {class: noProvider, count: 1, reason: "the CLI installs no provider"},
	"e2e/clients.go:newAuthedClients:compassv1connect.NewCommsServiceClient":                          {class: testHelper, count: 1, reason: "e2e test helper uses a local client"},
	"e2e/clients.go:newAuthedClients:compassv1connect.NewCompassServiceClient":                        {class: testHelper, count: 1, reason: "e2e test helper uses a local client"},
	"e2e/clients.go:newUnauthedCompassClient:compassv1connect.NewCompassServiceClient":                {class: testHelper, count: 1, reason: "e2e test helper uses a local client"},
	"e2e/garage_fixture.go:garageS3Client:minio.New":                                                  {class: testHelper, count: 1, reason: "e2e fixture connects to local Garage"},
	"internal/auth/workload_identity.go:NewRunnerVerifier:http.DefaultClient":                         {class: traced, count: 1, reason: "the client is wrapped after clusterHTTPClient to preserve its TLS settings"},
	"internal/bridge/pump.go:NewUnixTarget:http.Client":                                               {class: noProvider, count: 1, reason: "client routes locally over a Unix socket"},
	"internal/bridge/tls_target.go:NewTLSTarget:http.Client":                                          {class: noProvider, count: 1, reason: "client runs in the app, which installs no provider"},
	"internal/embedded/embedded.go:WhoAmIOverUDS:compassv1connect.NewCompassServiceClient":            {class: noProvider, count: 1, reason: "embedded client runs in the app, which installs no provider"},
	"internal/embedded/embedded.go:WhoAmIOverUDS:http.Client":                                         {class: noProvider, count: 1, reason: "client routes locally over a Unix socket"},
	"internal/fabric/fabric.go:New:nats.Connect":                                                      {class: traced, count: 1, reason: "Fabric publishes and consumes traced messages"},
	"internal/forge/github.go:NewGitHub:http.Client":                                                  {class: testHelper, count: 1, reason: "nil-client fallback; production passes tracedHTTPClient"},
	"internal/forge/githubapp.go:newAppTokenSource:http.Client":                                       {class: testHelper, count: 1, reason: "nil-client fallback; production passes tracedHTTPClient"},
	"internal/forge/linear.go:NewLinear:http.Client":                                                  {class: testHelper, count: 1, reason: "nil-client fallback; production passes tracedHTTPClient"},
	"internal/forge/trunk.go:NewTrunk:http.Client":                                                    {class: testHelper, count: 1, reason: "nil-client fallback; production passes tracedHTTPClient"},
	"internal/linearagent/client.go:NewClient:http.DefaultClient":                                     {class: testHelper, count: 1, reason: "test-only fallback; production passes tracedHTTPClient"},
	"internal/linearagent/client.go:NewTokenSource:http.DefaultClient":                                {class: testHelper, count: 1, reason: "test-only fallback; production passes tracedHTTPClient"},
	"internal/pgshare/pgshare.go:IsolatedDatabase:pgx.Connect":                                        {class: testHelper, count: 2, reason: "test-only database harness"},
	"internal/pgshare/pgshare.go:IsolatedSchemaDSN:pgx.Connect":                                       {class: testHelper, count: 2, reason: "test-only database harness"},
	"internal/pgshare/pgshare.go:waitReady:pgx.Connect":                                               {class: testHelper, count: 1, reason: "test-only database readiness probe"},
	"internal/pgshare/pgshare.go:waitSuiteReady:pgx.Connect":                                          {class: testHelper, count: 1, reason: "test-only database readiness probe"},
	"internal/runner/runner.go:Dial:compassv1internalconnect.NewRunnerServiceClient":                  {class: traced, count: 1, reason: "Connect interceptor records client spans"},
	"internal/runner/runner.go:Dial:http.DefaultClient":                                               {class: traced, count: 1, reason: "the Connect interceptor carries the trace"},
	"internal/runner/tls.go:newTLSTransportClient:http.Client":                                        {class: noProvider, count: 1, reason: "runner client can be used without an installed provider"},
	"internal/runnertest/socket.go:DialAgentSocket:compassv1internalconnect.NewAgentGatewayClient":    {class: testHelper, count: 1, reason: "test-only generated client helper"},
	"internal/runnertest/socket.go:DialAgentSocket:http.Client":                                       {class: testHelper, count: 1, reason: "test-only local socket helper"},
	"internal/runnertest/socket.go:H2CClient:http.Client":                                             {class: testHelper, count: 1, reason: "test-only local socket helper"},
	"internal/runtime/microvm/dial.go:GuestClient:compassv1internalconnect.NewGuestControlClient":     {class: traced, count: 1, reason: "compass-runner passes its otelconnect interceptor; VM.Health stays bare by design"},
	"internal/runtime/microvm/dial.go:GuestClient:http.Client":                                        {class: traced, count: 1, reason: "transport under the traced guest Connect client"},
	"internal/stack/adapters/collector_container.go:NewCollectorContainer:http.Client":                {class: noProvider, count: 1, reason: "stack process installs no provider"},
	"internal/stack/adapters/dbprobe.go:ProbeDB:pgx.ConnectConfig":                                    {class: noProvider, count: 1, reason: "stack process installs no provider"},
	"internal/stack/adapters/gateway_container.go:NewGatewayContainer:http.Client":                    {class: noProvider, count: 1, reason: "stack process installs no provider"},
	"internal/stack/adapters/health.go:Probe:compassv1connect.NewCompassServiceClient":                {class: noProvider, count: 1, reason: "stack process installs no provider"},
	"internal/stack/adapters/health.go:Probe:http.Client":                                             {class: noProvider, count: 1, reason: "stack process installs no provider"},
	"internal/stack/adapters/nats_container.go:NewNatsContainer:http.Client":                          {class: noProvider, count: 1, reason: "stack process installs no provider"},
	"internal/stack/materialize.go:materializeGuestArtifact:http.Client":                              {class: noProvider, count: 1, reason: "stack process installs no provider"},
	"internal/store/objectstore.go:NewS3ObjectStore:minio.New":                                        {class: traced, count: 1, reason: "server passes otelx.HTTPTransport"},
	"internal/store/open_traced_pgtest.go:OpenTraced:pgxpool.NewWithConfig":                           {class: testHelper, count: 1, reason: "pgtest-only traced store"},
	"internal/store/store.go:Open:pgxpool.NewWithConfig":                                              {class: traced, count: 1, reason: "store pool config installs the pgx span tracer"},
	"server/serve.go:forgeHTTPClient:http.Client":                                                     {class: traced, count: 1, reason: "custom-CA client wraps its transport with otelx.HTTPTransport"},
	"server/serve.go:tracedHTTPClient:http.Client":                                                    {class: traced, count: 1, reason: "client transport uses otelx.HTTPTransport"},
}

func TestCompareEgressSitesDetectsCountChanges(t *testing.T) {
	t.Parallel()

	const fixturePath = "fixture.go"
	const oneClient = `package fixture
import api "example.com/gen/servicev1connect"
func dial() { api.NewServiceClient(nil, "") }
`
	const twoClients = `package fixture
import api "example.com/gen/servicev1connect"
func dial() { api.NewServiceClient(nil, ""); api.NewServiceClient(nil, "") }
`
	const noClients = `package fixture
import api "example.com/gen/servicev1connect"
func dial() {}
`
	const key = "fixture.go:dial:api.NewServiceClient"
	baselineFile, err := parser.ParseFile(token.NewFileSet(), fixturePath, oneClient, 0)
	if err != nil {
		t.Fatal(err)
	}
	baseline := collectFileEgress(fixturePath, baselineFile)
	if baseline[key] != 1 {
		t.Fatalf("baseline count for %s = %d, want 1", key, baseline[key])
	}
	tests := []struct {
		name     string
		source   string
		wantDiff string
	}{
		{name: "one extra constructor", source: twoClients, wantDiff: "count = 2, want 1"},
		{name: "one fewer constructor", source: noClients, wantDiff: "is missing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			file, err := parser.ParseFile(token.NewFileSet(), fixturePath, tt.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			got := collectFileEgress(fixturePath, file)
			want := map[string]egressSite{
				key: {class: traced, count: 1, reason: "fixture traced client"},
			}
			diff := compareEgressSites(got, want)
			if len(diff) != 1 || !strings.Contains(diff[0], key) || !strings.Contains(diff[0], tt.wantDiff) {
				t.Fatalf("compareEgressSites() = %v, want one diagnostic for %s containing %q", diff, key, tt.wantDiff)
			}
		})
	}
}

func TestCollectFileEgress(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		source string
		want   map[string]int
	}{
		{
			name: "generated wrapper and separate runtime constructor",
			source: `package fixture
import api "example.com/gen/servicev1connect"
import connect "connectrpc.com/connect/v2"
func dial() {
	api.NewServiceClient(connect.NewClient(nil))
	connect.NewClient(nil)
}
`,
			want: map[string]int{
				"fixture.go:dial:api.NewServiceClient": 1,
				"fixture.go:dial:connect.NewClient":    1,
			},
		},
		{
			name: "package-level initializers and aliased net/http selectors",
			source: `package fixture
import api "example.com/gen/servicev1connect"
import connect "connectrpc.com/connect/v2"
import web "net/http"
var (
	client = &web.Client{}
	service = api.NewServiceClient(connect.NewClient(nil))
	defaultClient = web.DefaultClient
)
`,
			want: map[string]int{
				"fixture.go:<package-level>:http.Client":          1,
				"fixture.go:<package-level>:api.NewServiceClient": 1,
				"fixture.go:<package-level>:http.DefaultClient":   1,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", tt.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			got := collectFileEgress("fixture.go", file)
			if !maps.Equal(got, tt.want) {
				t.Fatalf("collectFileEgress() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEgressCensus(t *testing.T) {
	t.Parallel()

	got, err := collectEgressSites(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if diff := compareEgressSites(got, egressSites); len(diff) != 0 {
		t.Fatalf("egress census mismatch:\n - %s", strings.Join(diff, "\n - "))
	}
}

func collectEgressSites(root string) (map[string]int, error) {
	sites := map[string]int{}
	err := filepath.WalkDir(root, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(filePath, ".go") || strings.HasSuffix(filePath, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filePath, nil, parser.ParseComments)
		if err != nil {
			return fmt.Errorf("parse %s: %w", filePath, err)
		}
		if ast.IsGenerated(file) {
			return nil
		}
		rel, err := filepath.Rel(root, filePath)
		if err != nil {
			return fmt.Errorf("relative path for %s: %w", filePath, err)
		}
		for key, count := range collectFileEgress(filepath.ToSlash(rel), file) {
			sites[key] += count
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk Go sources under %s: %w", root, err)
	}
	return sites, nil
}

func collectFileEgress(filePath string, file *ast.File) map[string]int {
	imports := make(map[string]string, len(file.Imports))
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := path.Base(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		} else {
			name = defaultImportName(importPath, name)
		}
		imports[name] = importPath
	}

	sites := map[string]int{}
	add := func(function, callee string) {
		sites[filePath+":"+function+":"+callee]++
	}
	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			if decl.Body != nil {
				collectFunctionEgress(decl.Body, decl.Name.Name, imports, add)
			}
		case *ast.GenDecl:
			if decl.Tok != token.VAR {
				continue
			}
			for _, spec := range decl.Specs {
				collectFunctionEgress(spec, "<package-level>", imports, add)
			}
		}
	}
	return sites
}

func collectFunctionEgress(node ast.Node, function string, imports map[string]string, add func(string, string)) {
	skipped := generatedConnectClientArguments(node, imports)
	ast.Inspect(node, func(node ast.Node) bool {
		if _, ok := httpClientLiteral(node, imports); ok {
			add(function, "http.Client")
		}
		selector, ok := node.(*ast.SelectorExpr)
		if ok && selector.Sel.Name == "DefaultClient" {
			if alias, isIdent := selector.X.(*ast.Ident); isIdent && imports[alias.Name] == "net/http" {
				add(function, "http.DefaultClient")
			}
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		alias, method, ok := importedMethod(call, imports)
		if !ok {
			return true
		}
		importPath := imports[alias]
		callee := alias + "." + method
		switch {
		case strings.HasPrefix(method, "New") && strings.HasSuffix(method, "Client") && isGeneratedConnectPackage(importPath):
			add(function, callee)
		case method == "NewClient" && isConnectRuntimePackage(importPath):
			if _, skip := skipped[call]; !skip {
				add(function, callee)
			}
		case importMatches(importPath, "minio") && method == "New":
			add(function, callee)
		case importMatches(importPath, "pgxpool") && method == "NewWithConfig":
			add(function, callee)
		case importMatches(importPath, "pgx") && (method == "Connect" || method == "ConnectConfig"):
			add(function, callee)
		case importMatches(importPath, "nats") && method == "Connect":
			add(function, callee)
		}
		return true
	})
}

func generatedConnectClientArguments(node ast.Node, imports map[string]string) map[*ast.CallExpr]struct{} {
	skipped := map[*ast.CallExpr]struct{}{}
	ast.Inspect(node, func(node ast.Node) bool {
		wrapper, ok := node.(*ast.CallExpr)
		if !ok || !generatedConnectClientCall(wrapper, imports) {
			return true
		}
		for _, arg := range wrapper.Args {
			clientCall, ok := arg.(*ast.CallExpr)
			if ok && connectNewClientCall(clientCall, imports) {
				skipped[clientCall] = struct{}{}
			}
		}
		return true
	})
	return skipped
}

func generatedConnectClientCall(call *ast.CallExpr, imports map[string]string) bool {
	alias, method, ok := importedMethod(call, imports)
	return ok && strings.HasPrefix(method, "New") && strings.HasSuffix(method, "Client") && isGeneratedConnectPackage(imports[alias])
}

func connectNewClientCall(call *ast.CallExpr, imports map[string]string) bool {
	alias, method, ok := importedMethod(call, imports)
	return ok && method == "NewClient" && isConnectRuntimePackage(imports[alias])
}

func importedMethod(call *ast.CallExpr, imports map[string]string) (string, string, bool) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", "", false
	}
	alias, ok := selector.X.(*ast.Ident)
	if !ok {
		return "", "", false
	}
	if _, ok := imports[alias.Name]; !ok {
		return "", "", false
	}
	return alias.Name, selector.Sel.Name, true
}

func httpClientLiteral(node ast.Node, imports map[string]string) (string, bool) {
	literal, ok := node.(*ast.CompositeLit)
	if !ok {
		return "", false
	}
	alias, ok := httpClientType(literal.Type)
	return "http", ok && imports[alias] == "net/http"
}

func isGeneratedConnectPackage(importPath string) bool {
	for part := range strings.SplitSeq(importPath, "/") {
		if strings.HasSuffix(part, "connect") && part != "connect" {
			return true
		}
	}
	return false
}

func isConnectRuntimePackage(importPath string) bool {
	return importPath == "connectrpc.com/connect" || importPath == "connectrpc.com/connect/v2"
}

func httpClientType(expr ast.Expr) (string, bool) {
	star, ok := expr.(*ast.StarExpr)
	if ok {
		expr = star.X
	}
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Client" {
		return "", false
	}
	alias, ok := selector.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	return alias.Name, true
}

func defaultImportName(importPath, base string) string {
	switch {
	case importPath == "connectrpc.com/connect/v2" || strings.HasSuffix(importPath, "/connect"):
		return "connect"
	case strings.HasSuffix(importPath, "/pgx/v5") || strings.HasSuffix(importPath, "/pgx/v4"):
		return "pgx"
	case strings.HasSuffix(importPath, "/nats.go"):
		return "nats"
	case strings.Contains(importPath, "/minio-go/"):
		return "minio"
	}
	return base
}

func importMatches(importPath, packageName string) bool {
	for part := range strings.SplitSeq(importPath, "/") {
		if part == packageName || strings.HasPrefix(part, packageName+".") {
			return true
		}
	}
	return false
}

func compareEgressSites(got map[string]int, want map[string]egressSite) []string {
	var diff []string
	for key, count := range got {
		expected, ok := want[key]
		if !ok {
			diff = append(diff, fmt.Sprintf("unclassified egress site %s (count %d)", key, count))
			continue
		}
		if expected.reason == "" {
			diff = append(diff, fmt.Sprintf("egress site %s has no classification reason", key))
		}
		switch expected.class {
		case traced, noProvider, testHelper:
		default:
			diff = append(diff, fmt.Sprintf("egress site %s has invalid class %d", key, expected.class))
		}
		if count != expected.count {
			diff = append(diff, fmt.Sprintf("egress site %s count = %d, want %d", key, count, expected.count))
		}
	}
	for key := range want {
		if _, ok := got[key]; !ok {
			diff = append(diff, fmt.Sprintf("egress site %s is missing", key))
		}
	}
	sort.Strings(diff)
	return diff
}
