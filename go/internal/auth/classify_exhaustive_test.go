package auth

// This test delivers the guarantee the //sumtype:decl on privilege cannot:
// gochecksumtype polices type-switches over the privilege sum, but
// classifyProcedure switches over a procedure string, which no compile-time tool
// can see. Instead of trusting a hand-maintained switch to stay in step with the
// generated RPC set, this ranges the proto service descriptors — the same source
// the connect procedure constants are generated from — and fails if any generated
// procedure is not explicitly classified. A newly added RPC therefore reddens CI
// until it is placed in a privilege class, which is the network-door invariant:
// a new procedure can never be served unclassified (silently fail-closed to
// adminOnly) without a maintainer noticing.

import (
	"testing"

	compassv1connect "github.com/sealedsecurity/compass/go/gen/compass/v1/compassv1connect"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
)

// procedurePath reconstructs the connect procedure path for a method descriptor.
// connect generates its procedure constants as "/<service full name>/<method
// name>" (e.g. "/compass.v1.CompassService/StartAgentSession"); anchorProcedurePath
// below guards that this reconstruction matches the generated constants.
func procedurePath(svc protoreflect.ServiceDescriptor, m protoreflect.MethodDescriptor) string {
	return "/" + string(svc.FullName()) + "/" + string(m.Name())
}

// gatedFileDescriptors is the explicit set of proto file descriptors whose
// services classifyProcedure must cover — the single source both the
// exhaustiveness test and the omitted-service guard read. Kept explicit (rather
// than derived wholesale from the registry) so the covered set stays
// self-documenting and intentional; TestClassificationGateCoversEveryRegisteredCompassService
// guards that this list can't silently fall behind a newly registered service file.
func gatedFileDescriptors() []protoreflect.FileDescriptor {
	return []protoreflect.FileDescriptor{
		compassv1.File_compass_v1_compass_proto,
		compassv1.File_compass_v1_comms_proto,
	}
}

// TestClassifyProcedureCoversEveryGeneratedProcedure fails if any generated
// CompassService or CommsService procedure is not explicitly classified by
// classifyProcedure. This is the build-time gate the doc comment promises: a new
// RPC added to the proto contract reddens this test until it is classified.
func TestClassifyProcedureCoversEveryGeneratedProcedure(t *testing.T) {
	files := gatedFileDescriptors()

	seen := 0
	for _, file := range files {
		services := file.Services()
		for si := range services.Len() {
			svc := services.Get(si)
			methods := svc.Methods()
			for mi := range methods.Len() {
				m := methods.Get(mi)
				path := procedurePath(svc, m)
				if _, ok := classifyProcedure(path); !ok {
					t.Errorf("generated procedure %q is not classified by classifyProcedure — add it to a privilege class (adminOnly or authenticatedOpen); an unclassified procedure silently fail-closes to adminOnly on the network door", path)
				}
				seen++
			}
		}
	}

	if seen == 0 {
		t.Fatal("ranged zero generated procedures — proto file descriptors did not expose any methods, so this exhaustiveness gate is vacuous")
	}
}

// TestAnchorProcedurePathMatchesConnectConstants guards the procedurePath
// reconstruction: if connect ever changes its procedure-path format, this test
// fails before the exhaustiveness test above silently starts matching nothing.
func TestAnchorProcedurePathMatchesConnectConstants(t *testing.T) {
	compass := compassv1.File_compass_v1_compass_proto.Services().ByName("CompassService")
	if compass == nil {
		t.Fatal("CompassService not found in proto file descriptor")
	}
	comms := compassv1.File_compass_v1_comms_proto.Services().ByName("CommsService")
	if comms == nil {
		t.Fatal("CommsService not found in proto file descriptor")
	}

	tests := []struct {
		name string
		svc  protoreflect.ServiceDescriptor
		meth protoreflect.Name
		want string
	}{
		{"compass StartAgentSession", compass, "StartAgentSession", compassv1connect.CompassServiceStartAgentSessionProcedure},
		{"compass GetServerInfo", compass, "GetServerInfo", compassv1connect.CompassServiceGetServerInfoProcedure},
		{"comms CreateUser", comms, "CreateUser", compassv1connect.CommsServiceCreateUserProcedure},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.svc.Methods().ByName(tc.meth)
			if m == nil {
				t.Fatalf("method %q not found on %s", tc.meth, tc.svc.FullName())
			}
			got := procedurePath(tc.svc, m)
			if got != tc.want {
				t.Errorf("procedurePath = %q, want connect constant %q — connect's procedure-path format changed, update procedurePath", got, tc.want)
			}
		})
	}
}

// TestClassificationGateCoversEveryRegisteredCompassService guards the
// hand-maintained gatedFileDescriptors slice against silently falling behind the
// proto contract. TestClassifyProcedureCoversEveryGeneratedProcedure above only
// checks the files that are IN the slice, so a whole new service added in a new
// .proto file (a new File_..._proto) that a maintainer forgets to add to the
// slice would never be coverage-checked — its RPCs would silently fail-closed to
// adminOnly on the network door with no red CI. This ranges the global proto
// registry (every generated file self-registers on import) for the same proto
// package(s) the gate already covers, and fails on any registered service not
// reachable from the slice.
//
// The covered packages are derived from the slice itself, never hard-coded, so
// the registry filter stays correct even if the compass proto package is renamed.
func TestClassificationGateCoversEveryRegisteredCompassService(t *testing.T) {
	// The service full names the gate's slice covers and the proto packages those
	// files live in — both derived from gatedFileDescriptors so nothing is guessed.
	covered := map[protoreflect.FullName]bool{}
	packages := map[protoreflect.FullName]bool{}
	for _, file := range gatedFileDescriptors() {
		packages[file.Package()] = true
		services := file.Services()
		for si := range services.Len() {
			covered[services.Get(si).FullName()] = true
		}
	}
	if len(packages) == 0 {
		t.Fatal("gatedFileDescriptors is empty — the classification gate covers nothing")
	}

	checked := 0
	for pkg := range packages {
		protoregistry.GlobalFiles.RangeFilesByPackage(pkg, func(file protoreflect.FileDescriptor) bool {
			services := file.Services()
			for si := range services.Len() {
				svc := services.Get(si)
				checked++
				if !covered[svc.FullName()] {
					t.Errorf("service %q is registered in proto package %q but its file is not in gatedFileDescriptors — add its File_..._proto to the slice so classifyProcedure's exhaustiveness gate covers its RPCs (otherwise they silently fail-closed to adminOnly on the network door)", svc.FullName(), pkg)
				}
			}
			return true
		})
	}

	if checked == 0 {
		t.Fatal("ranged zero registered services for the gated proto package(s) — the registry filter matched nothing, so this guard is vacuous")
	}
}
