package forge

// Wire canonicalization + fixture derivation for the -update live-capture path
// (leg 2 of the forge integration-testing record,
// docs/designs/product/compass-forge-integration-testing/design.md §T2; RIG-2229).
//
// This file is UNTAGGED on purpose: the canonicalization logic and its
// invariants run in the normal credential-free `go test ./internal/forge/`
// battery, so a regression in the sentinel table or a break in the load-bearing
// golden invariants is caught without live credentials. The //go:build livegithub
// suite (livegithub_test.go) drives the live capture and calls the helpers here.
//
// The design (Matt's 2026-08-22 ruling): canonicalize on write, reusing the
// oracle's volatileFields as the single source of truth. Two tables express that
// tie:
//   - wireVolatile maps a PROVIDER wire JSON key to a fixed, type-appropriate
//     sentinel; canonicalizeWire recursively substitutes those values so a
//     pure-volatile per-run change canonicalizes to identical output while a real
//     shape change (a new/renamed/retyped field) survives and shows in the diff.
//   - domainToWire maps every DOMAIN volatile key (volatileFields) to the wire
//     key(s) it decodes from; TestUpdateCanonicalizeCoversVolatileFields asserts
//     the keyset EQUALS volatileFields, so adding a domain volatile without a
//     wire mapping fails the untagged battery.
//
// The load-bearing invariant, held BY CONSTRUCTION: TestGoldenFixtures replays
// every committed fixture and asserts BOTH the emitted request (method/path/
// query/body) AND the decoded value (Want) against the fixture. deriveFixtureHalves
// produces both halves from ONE invoke() replay over the canonicalized responses
// and canonicalized coordinates — the identical code path golden replay runs — so
// the committed request matches what replay emits, and marshal(decode(Body))==Want,
// no matter which keys are canonicalized. That is why string ids CAN be
// canonicalized (a Linear teamId/issueId resolved from the canonicalized prelude
// stays consistent with the request derived from that same prelude): the request
// is never recorded from the live wire, it is re-derived.
//
// context here flows through invoke() (which roots context.Background() as the
// test root — the sanctioned F-ttsr exemption, mirroring golden_test.go).

import (
	"encoding/json"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Canonical sentinels. The coordinate sentinels (canonTitle/canonBody/canonRef
// and the numeric 42) MUST equal the wire sentinels for the same domain field,
// so a request derived from canonicalized coordinates and a Want derived from a
// canonicalized response agree (e.g. request head == response head.ref).
const (
	canonAccount   = "octocat"                           // login, displayName -> ForgeAccount/Author
	canonURL       = "https://example.invalid/canonical" // html_url, url, target_url -> URL
	canonIDString  = "canonical-id"                      // a string/UUID id (Linear) -> ID / resolve coordinate
	canonUpdatedAt = "2026-08-01T12:30:00Z"              // updated_at, updatedAt -> UpdatedAt
	canonSHA       = "canonicalsha"                      // sha -> HeadSHA
	canonRef       = "canonical-ref"                     // ref -> HeadRef/BaseRef
	canonTitle     = "canonical title"                   // title -> Title
	canonBody      = "canonical body"                    // body, description -> Body
)

// canonNumber is the numeric sentinel (number, numeric id) as a float64 — the
// type encoding/json decodes every JSON number to.
var canonNumber = float64(42)

// volatileFields names the forge DOMAIN JSON keys (domain types marshal to their
// Go field names — no json tags) that are NOT asserted against the committed
// fixture, because the forge assigns them per run/identity or a hygiene-unique
// artifact name perturbs them. It is the single source of truth the oracle's
// stripVolatile (livegithub_test.go) strips against AND that the -update capture
// path canonicalizes against (via domainToWire). It lives in this untagged file
// so BOTH the untagged canonicalization battery and the //go:build livegithub
// oracle share one definition (a tagged build compiles untagged files too).
//
//   - Forge-assigned per run: Number (issue/PR number), ID (comment/review id),
//     URL (canonical web url), UpdatedAt (server timestamp), HeadSHA (commit),
//     HeadRef / BaseRef (branch names).
//   - Forge-assigned per identity: ForgeAccount / Author (the live bot account
//     differs from the fixture's captured account).
//   - Run-supplied uniquely: Title / Body — hygiene requires run-id-suffixed
//     artifact names, so a live create's title/body never equals the fixture's
//     captured input; the state fold, label passthrough, verdict mapping, draft
//     flag and structural shape (everything NOT listed here) ARE asserted.
//
// Rate-limit headers are volatile too but never reach a decoded domain value,
// so there is nothing to strip for them here (they live on the HTTP response,
// which the oracle does not compare).
var volatileFields = map[string]struct{}{
	"Number":       {},
	"ID":           {},
	"URL":          {},
	"UpdatedAt":    {},
	"HeadSHA":      {},
	"HeadRef":      {},
	"BaseRef":      {},
	"ForgeAccount": {},
	"Author":       {},
	"Title":        {},
	"Body":         {},
}

// wireVolatile maps a provider wire JSON key to the sentinel that replaces its
// value during canonicalization. Each entry is a func of the current node so a
// key whose JSON type varies (id: numeric on GitHub, a UUID string on Linear)
// picks a type-appropriate sentinel; the fixed-value keys ignore the node. The
// keyset is the union of the wire keys every domainToWire entry references
// (asserted coherent in TestUpdateCanonicalizeCoversVolatileFields).
var wireVolatile = map[string]func(node any) any{
	"number":      fixedSentinel(canonNumber),
	"id":          canonID,
	"html_url":    fixedSentinel(canonURL),
	"url":         fixedSentinel(canonURL),
	"target_url":  fixedSentinel(canonURL),
	"updated_at":  fixedSentinel(canonUpdatedAt),
	"updatedAt":   fixedSentinel(canonUpdatedAt),
	"login":       fixedSentinel(canonAccount),
	"displayName": fixedSentinel(canonAccount),
	"sha":         fixedSentinel(canonSHA),
	"ref":         fixedSentinel(canonRef),
	"title":       fixedSentinel(canonTitle),
	"body":        fixedSentinel(canonBody),
	"description": fixedSentinel(canonBody),
}

// domainToWire maps each forge DOMAIN volatile key (the Go field names in
// volatileFields, which the domain types marshal to verbatim — no json tags) to
// the provider wire key(s) it decodes from. It is the explicit, reviewed
// correspondence that ties the canonicalization table back to the oracle's
// allowlist; its keyset MUST equal volatileFields (asserted below).
//
// Grounded against github.go / linear.go toX funcs:
//   - Number   <- number       (ghIssue/ghPull/ghPullDetail.Number, linearIssue.Number)
//   - ID       <- id           (ghComment.ID numeric; Linear comment id is a UUID string)
//   - URL      <- html_url,url,target_url (GitHub HTMLURL + ghStatus.TargetURL -> Check.URL; Linear URL)
//   - UpdatedAt<- updated_at,updatedAt (GitHub updated_at; Linear updatedAt)
//   - HeadSHA  <- sha           (ghPullDetail.Head.SHA)
//   - HeadRef  <- ref           (ghPull/ghPullDetail.Head.Ref)
//   - BaseRef  <- ref           (ghPull/ghPullDetail.Base.Ref) — same wire key as HeadRef
//   - ForgeAccount <- login,displayName (GitHub user.login; Linear creator/user displayName)
//   - Author   <- login        (ghReviewRow.User.Login on the reviews leg)
//   - Title    <- title
//   - Body     <- body,description (GitHub body; Linear description)
var domainToWire = map[string][]string{
	"Number":       {"number"},
	"ID":           {"id"},
	"URL":          {"html_url", "url", "target_url"},
	"UpdatedAt":    {"updated_at", "updatedAt"},
	"HeadSHA":      {"sha"},
	"HeadRef":      {"ref"},
	"BaseRef":      {"ref"},
	"ForgeAccount": {"login", "displayName"},
	"Author":       {"login"},
	"Title":        {"title"},
	"Body":         {"body", "description"},
}

// fixedSentinel returns a sentinel func that ignores the node and yields v.
func fixedSentinel(v any) func(any) any {
	return func(any) any { return v }
}

// canonID sentinels a numeric id (GitHub comment/review id) to canonNumber and a
// string/UUID id (Linear) to canonIDString, by JSON type. Canonicalizing string
// ids is SAFE here because the request half is re-derived from the canonicalized
// prelude (deriveFixtureHalves), not recorded from the live wire: a Linear
// resolveTeamID/resolveIssueID reads the canonicalized prelude id -> canonIDString
// and emits it as the request coordinate, which golden replay re-derives
// identically. Recording the live request (the discarded first cut) is what would
// have desynced a canonicalized id from a verbatim request coordinate.
func canonID(node any) any {
	switch node.(type) {
	case float64:
		return canonNumber
	case string:
		return canonIDString
	default:
		return node
	}
}

// canonicalizeWire recursively rewrites a wire-JSON tree, substituting a fixed
// sentinel for the value of every key in wireVolatile (matched by name at any
// depth) while preserving key, structure, and type. A non-JSON or empty input is
// returned unchanged. It is a pure function: same input -> same output, so a
// per-run-only change canonicalizes to identical bytes and a genuine shape change
// survives.
func canonicalizeWire(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return raw // not JSON (or malformed) — leave verbatim for the reviewer to see
	}
	out, err := json.Marshal(canonNode(tree))
	if err != nil {
		return raw
	}
	return out
}

// canonNode walks one decoded JSON node, substituting sentinels for volatile keys
// and recursing into every non-volatile child. A volatile key's value is replaced
// wholesale (the wire-volatile values are scalars), never recursed.
func canonNode(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if sentinel, vol := wireVolatile[k]; vol {
				x[k] = sentinel(child)
				continue
			}
			x[k] = canonNode(child)
		}
		return x
	case []any:
		for i, child := range x {
			x[i] = canonNode(child)
		}
		return x
	default:
		return v
	}
}

// canonicalizeCoordinates canonicalizes the run-supplied request coordinates
// (Number and the create-op inputs) to the same sentinels the wire canonicalizer
// uses, so a request derived from these coordinates agrees with a Want derived
// from a canonicalized response. It copies Input so a caller's live coordinates
// are untouched. Repo, Filter, Labels, and Draft are NOT volatile (they are
// asserted or stable), so they pass through.
func canonicalizeCoordinates(req fixtureRequest) fixtureRequest {
	if req.Number != 0 {
		req.Number = 42
	}
	if req.Input != nil {
		in := *req.Input // copy: never mutate the caller's coordinates
		if in.Title != "" {
			in.Title = canonTitle
		}
		if in.Body != "" {
			in.Body = canonBody
		}
		if in.HeadRef != "" {
			in.HeadRef = canonRef
		}
		if in.BaseRef != "" {
			in.BaseRef = canonRef
		}
		req.Input = &in
	}
	return req
}

// capturedResponse is one scripted response the live capture recorded, in wire
// order: the status and the raw provider body canonicalizeWire rewrites before a
// fixture stores it. The request is deliberately NOT captured — it is re-derived
// (deriveFixtureHalves) so the committed request matches golden replay by
// construction.
type capturedResponse struct {
	status int
	body   json.RawMessage
}

// assembleFixture builds a candidate fixture from a recording: the captured
// responses (canonicalized, in wire order) become Prelude/Body/Extra with the
// asserted response at index prelude, the coordinates are canonicalized, and
// BOTH request halves and Want are derived by replay (deriveFixtureHalves) so the
// load-bearing golden invariants hold by construction.
func assembleFixture(t *testing.T, provider, name string, prelude int, coords fixtureRequest, responses []capturedResponse) fixture {
	t.Helper()
	if len(responses) <= prelude {
		t.Fatalf("%s/%s: captured %d responses, need at least %d (prelude probes) + 1 (asserted)",
			provider, name, len(responses), prelude)
	}
	f := fixture{Name: name, Request: canonicalizeCoordinates(coords)}

	asserted := responses[prelude]
	f.Response.Status = asserted.status
	f.Response.Body = canonicalizeWire(asserted.body)
	for _, r := range responses[:prelude] {
		f.Response.Prelude = append(f.Response.Prelude, fixtureStep{Status: r.status, Body: canonicalizeWire(r.body)})
	}
	for _, r := range responses[prelude+1:] {
		f.Response.Extra = append(f.Response.Extra, fixtureStep{Status: r.status, Body: canonicalizeWire(r.body)})
	}
	return deriveFixtureHalves(t, provider, f)
}

// deriveFixtureHalves replays a fixture's scripted responses (Prelude, Body,
// Extra, in wire order) through the real client via invoke(), capturing BOTH the
// emitted asserted request (Method/Path/Query/Headers/Body) and the decoded value
// (Want). Because golden replayFixture runs the IDENTICAL invoke() over the
// identical scripted responses and coordinates, the committed request halves match
// what replay emits by construction, and marshal(decode(Body))==Want. It mirrors
// replayFixture's response scripting exactly.
func deriveFixtureHalves(t *testing.T, provider string, f fixture) fixture {
	t.Helper()
	responses := make([]scriptedResponse, 0, len(f.Response.Prelude)+1+len(f.Response.Extra))
	for _, s := range f.Response.Prelude {
		responses = append(responses, toScripted(s))
	}
	responses = append(responses, scriptedResponse{
		status:  f.Response.Status,
		body:    string(f.Response.Body),
		headers: f.Response.Headers,
	})
	for _, s := range f.Response.Extra {
		responses = append(responses, toScripted(s))
	}
	rt := &scriptedRoundTripper{responses: responses}
	ts := &fakeTokenSource{token: "test-token"}

	got := invoke(t, provider, rt, ts, f.Request)
	f.Response.Want = mustMarshal(t, got)

	// Guard that replay consumed EXACTLY every scripted response — the same
	// exact-count invariant golden replayFixture asserts. A response the client
	// never reaches (e.g. a later pagination page whose rel=next Link header the
	// recorder dropped, so HasNext stayed false and the loop stopped early) would
	// otherwise be written into a truncated fixture silently, surfacing only later
	// and confusingly as a golden count mismatch. Fail loudly AT CAPTURE instead.
	wantN := len(f.Response.Prelude) + 1 + len(f.Response.Extra)
	if got := len(rt.requests); got != wantN {
		t.Fatalf("derive %s/%s: replay emitted %d requests, want exactly %d — a captured response was not consumed (a dropped rel=next Link header truncating a paginated leg?); the fixture would be truncated",
			provider, f.Name, got, wantN)
	}
	idx := len(f.Response.Prelude)
	asserted := rt.requests[idx]
	f.Request.Method = asserted.Method
	f.Request.Path = asserted.URL.Path
	f.Request.Query = flattenQuery(asserted.URL.Query())

	// Pin the deterministic content-negotiation headers the client sets (never
	// Authorization — a fixture holds no token, and replay skips it).
	hdr := map[string]string{}
	for _, k := range []string{"Content-Type", "Accept"} {
		if v := asserted.Header.Get(k); v != "" {
			hdr[k] = v
		}
	}
	if len(hdr) > 0 {
		f.Request.Headers = hdr
	}
	if body := readReqBody(t, asserted); body != "" {
		f.Request.Body = json.RawMessage(body)
	}
	return f
}

// flattenQuery reduces a url.Values to the single-value map the fixture schema
// pins (fixtureRequest.Query), taking the first value per key; nil when empty so
// a query-less op omits the field.
func flattenQuery(q url.Values) map[string]string {
	if len(q) == 0 {
		return nil
	}
	out := make(map[string]string, len(q))
	for k, vs := range q {
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}
	return out
}

// TestUpdateCanonicalizeCoversVolatileFields is the "single source of truth" tie:
// domainToWire's keyset MUST equal the oracle's volatileFields, and the two tables
// MUST agree on the wire keys — every key domainToWire references has a
// wireVolatile sentinel, and every sentinel is referenced. A new domain volatile
// added to volatileFields without a wire mapping — or a wireVolatile key no domain
// field decodes from — fails here.
func TestUpdateCanonicalizeCoversVolatileFields(t *testing.T) {
	got := make(map[string]struct{}, len(domainToWire))
	for k := range domainToWire {
		got[k] = struct{}{}
	}
	if !reflect.DeepEqual(got, volatileFields) {
		t.Errorf("domainToWire keyset != volatileFields\n domainToWire: %v\n volatileFields: %v",
			sortedKeys(got), sortedKeys(volatileFields))
	}

	referenced := map[string]struct{}{}
	for domain, wires := range domainToWire {
		for _, w := range wires {
			referenced[w] = struct{}{}
			if _, ok := wireVolatile[w]; !ok {
				t.Errorf("domainToWire[%q] references wire key %q with no wireVolatile sentinel", domain, w)
			}
		}
	}
	for w := range wireVolatile {
		if _, ok := referenced[w]; !ok {
			t.Errorf("wireVolatile has key %q not referenced by any domainToWire entry", w)
		}
	}
}

// TestUpdateCanonicalizeStable is the RED->GREEN guard for the whole capture
// pipeline:
//
//  1. Completeness: every wire-volatile key (incl. displayName and target_url —
//     the two the read-path-only view misses) canonicalizes to its sentinel while
//     a non-volatile sibling survives.
//  2. GitHub full-pipeline: a synthetic create_issue "live capture" (run-id title,
//     real-ish number/login/url) runs the REAL assembleFixture -> replayFixture
//     passes. This is the guard for the request/response consistency invariant: if
//     the request half were taken from the live wire instead of derived, the run-id
//     title would survive in Request.Body and replay's assertRequest would go RED.
//  3. Linear full-pipeline: a synthetic create_issue with a live team UUID in the
//     prelude runs the pipeline -> replayFixture passes, the derived request's
//     teamId is the string-id sentinel (not the live UUID — the regen-stability
//     guard), and the live displayName does not leak into the canonicalized body.
//  4. Shape drift survives: a wire field with no sentinel is preserved.
func TestUpdateCanonicalizeStable(t *testing.T) {
	// (1) Completeness across every wire-volatile key.
	all := json.RawMessage(`{
		"number": 1, "id": 2, "html_url": "h", "url": "u", "target_url": "t",
		"updated_at": "a", "updatedAt": "b", "login": "l", "displayName": "d",
		"sha": "s", "ref": "r", "title": "ti", "body": "bo", "description": "de",
		"state": "open", "keep": "kept"
	}`)
	wantAll := json.RawMessage(`{
		"number": 42, "id": 42, "html_url": "https://example.invalid/canonical",
		"url": "https://example.invalid/canonical", "target_url": "https://example.invalid/canonical",
		"updated_at": "2026-08-01T12:30:00Z", "updatedAt": "2026-08-01T12:30:00Z",
		"login": "octocat", "displayName": "octocat", "sha": "canonicalsha",
		"ref": "canonical-ref", "title": "canonical title", "body": "canonical body",
		"description": "canonical body", "state": "open", "keep": "kept"
	}`)
	assertJSONEqual(t, "canonicalized every wire-volatile key", canonicalizeWire(all), wantAll)

	// (2) GitHub full pipeline: assembleFixture then a golden replay. A request
	// half taken from the live wire (title "live issue compass-abc123") would fail
	// replay's request-body compare against the canonicalized Request.Body.
	ghCoords := fixtureRequest{
		Op:    "create_issue",
		Repo:  "org/repo",
		Input: &fixtureInput{Title: "live issue compass-abc123", Body: "stamped body", Labels: []string{"bug", "p1"}},
	}
	ghResp := []capturedResponse{{
		status: 201,
		body: json.RawMessage(`{
			"number": 98765,
			"title": "live issue compass-abc123",
			"body": "stamped body",
			"state": "open",
			"html_url": "https://github.com/rig/throwaway/issues/98765",
			"user": { "login": "rig-bot" },
			"labels": [{ "name": "bug" }, { "name": "p1" }]
		}`),
	}}
	ghFixture := assembleFixture(t, providerGitHub, "create_issue_probe", 0, ghCoords, ghResp)
	replayFixture(t, providerGitHub, ghFixture)
	if strings.Contains(string(ghFixture.Request.Body), "compass-abc123") {
		t.Errorf("run-id title leaked into derived request body: %s (the request must be derived from canonicalized coordinates, not the live wire)", ghFixture.Request.Body)
	}

	// (3) Linear full pipeline: a live team UUID in the prelude, a live displayName
	// in the body. replayFixture proves consistency; the explicit checks prove the
	// live UUID and displayName are canonicalized (regen stability), which the
	// consistency-only replay cannot see.
	lnCoords := fixtureRequest{
		Op:    "create_issue",
		Repo:  "SEA",
		Input: &fixtureInput{Title: "live compass-xyz", Body: "stamped body"},
	}
	lnResp := []capturedResponse{
		{status: 200, body: json.RawMessage(`{"data":{"teams":{"nodes":[{"id":"live-team-uuid-abc"}]}}}`)},
		{status: 200, body: json.RawMessage(`{"data":{"viewer":{"app":true}}}`)},
		{status: 200, body: json.RawMessage(`{"data":{"issueCreate":{"issue":{
			"number": 555,
			"title": "live compass-xyz",
			"description": "stamped body",
			"url": "https://linear.app/x/issue/SEA-555",
			"state": { "name": "Todo", "type": "unstarted" },
			"labels": { "nodes": [] },
			"creator": { "displayName": "live-bot" },
			"updatedAt": "2026-08-22T09:00:00Z"
		}}}}`)},
	}
	lnFixture := assembleFixture(t, providerLinear, "create_issue_probe", 2, lnCoords, lnResp)
	replayFixture(t, providerLinear, lnFixture)

	var lnVars struct {
		Variables struct {
			Input struct {
				TeamID string `json:"teamId"`
			} `json:"input"`
		} `json:"variables"`
	}
	if err := json.Unmarshal(lnFixture.Request.Body, &lnVars); err != nil {
		t.Fatalf("unmarshal derived linear request body: %v (%s)", err, lnFixture.Request.Body)
	}
	if lnVars.Variables.Input.TeamID != canonIDString {
		t.Errorf("linear teamId not canonicalized in derived request: got %q, want %q (a string resolve id must canonicalize so regeneration is stable)",
			lnVars.Variables.Input.TeamID, canonIDString)
	}
	if strings.Contains(string(lnFixture.Response.Body), "live-bot") {
		t.Error("linear displayName leaked into the canonicalized response body (displayName must be a wire-volatile key)")
	}

	// (4) Shape drift survives: a wire field with no sentinel is preserved (value
	// and all) so a reviewer sees it in the regenerated diff, while volatile
	// siblings are still canonicalized.
	drift := json.RawMessage(`{
		"number": 98765,
		"title": "live",
		"new_wire_field": "survives-drift",
		"user": { "login": "rig-bot" }
	}`)
	var driftTree map[string]any
	if err := json.Unmarshal(canonicalizeWire(drift), &driftTree); err != nil {
		t.Fatalf("unmarshal canonicalized drift body: %v", err)
	}
	if got := driftTree["new_wire_field"]; got != "survives-drift" {
		t.Errorf("new wire field dropped by canonicalization: got %v, want %q (shape drift must survive)", got, "survives-drift")
	}
	if got := driftTree["number"]; got != canonNumber {
		t.Errorf("volatile key not canonicalized alongside a surviving new field: number = %v, want 42", got)
	}
	if user, ok := driftTree["user"].(map[string]any); !ok || user["login"] != canonAccount {
		t.Errorf("nested volatile key not canonicalized: user = %v, want login octocat", driftTree["user"])
	}
}

// TestUpdateCanonicalizeComposite is the credential-free guard for the single
// riskiest capture: get_pull_request, the only fixture with EXTRA legs. It runs
// a synthetic live capture — a PR detail GET (asserted) followed by the reviews,
// check_runs, and legacy statuses legs (Extra) — through the REAL assembleFixture
// -> replayFixture pipeline. This exercises what create_issue cannot: the
// responses[prelude+1:] Extra assembly, canonNode's []any recursion into an
// array of objects that themselves carry volatile keys (the two reviews, each an
// Author<-user.login), and volatile substitution across separate legs
// (target_url on the statuses leg, sha on the detail leg vs Checks.HeadSHA).
func TestUpdateCanonicalizeComposite(t *testing.T) {
	coords := fixtureRequest{Op: "get_pull_request", Repo: "org/repo", Number: 98765}
	responses := []capturedResponse{
		// asserted: PR detail with live-ish volatile values.
		{status: 200, body: json.RawMessage(`{
			"number": 98765, "title": "a feature", "body": "raw <!--owner--> pr body",
			"state": "closed", "html_url": "https://github.com/org/repo/pull/98765",
			"draft": false, "additions": 120, "deletions": 30, "changed_files": 4,
			"merged": true,
			"head": { "ref": "feature-live", "sha": "livesha123abc" },
			"base": { "ref": "main" },
			"user": { "login": "live-author" }
		}`)},
		// extra leg 1: reviews — an array of objects, each with a volatile Author.
		{status: 200, body: json.RawMessage(`[
			{ "body": "lgtm", "state": "APPROVED", "user": { "login": "botly-live", "type": "Bot" } },
			{ "body": "needs work", "state": "CHANGES_REQUESTED", "user": { "login": "carol-live", "type": "User" } }
		]`)},
		// extra leg 2: check_runs — html_url volatile.
		{status: 200, body: json.RawMessage(`{ "check_runs": [
			{ "name": "build", "status": "completed", "conclusion": "success", "html_url": "https://ci/live-build" }
		] }`)},
		// extra leg 3: legacy statuses — target_url volatile.
		{status: 200, body: json.RawMessage(`{ "statuses": [
			{ "context": "legacy-ci", "state": "success", "target_url": "https://ci/live-legacy" }
		] }`)},
	}

	// assembleFixture derives BOTH halves by replay; replayFixture then re-asserts
	// the emitted requests (all 4 legs, exact count) and decode(Body)==Want. A
	// regression in Extra-leg assembly or array-of-volatile-objects recursion
	// fails here credential-free instead of only at live -update time.
	f := assembleFixture(t, providerGitHub, "get_pull_request_probe", 0, coords, responses)
	replayFixture(t, providerGitHub, f)

	// Want is marshal(decode(Body)); domain types carry no json tags, so they
	// marshal to their Go field names verbatim. Decode into a local tagged struct
	// (the file's convention, cf. lnVars) so musttag is satisfied.
	var pr struct {
		Number  uint64 `json:"Number"`
		Reviews []struct {
			Author  string `json:"Author"`
			IsBot   bool   `json:"IsBot"`
			Verdict string `json:"Verdict"`
		} `json:"Reviews"`
		Checks struct {
			HeadSHA string `json:"HeadSHA"`
			Checks  []struct {
				Name string `json:"Name"`
				URL  string `json:"URL"`
			} `json:"Checks"`
		} `json:"Checks"`
	}
	if err := json.Unmarshal(f.Response.Want, &pr); err != nil {
		t.Fatalf("unmarshal derived get_pull_request Want: %v (%s)", err, f.Response.Want)
	}

	// (b) both review authors — volatile keys INSIDE an array of objects —
	// canonicalize; the non-volatile Verdict/IsBot survive distinctly per review.
	if len(pr.Reviews) != 2 {
		t.Fatalf("derived Reviews = %d, want 2", len(pr.Reviews))
	}
	for i, rv := range pr.Reviews {
		if rv.Author != canonAccount {
			t.Errorf("Reviews[%d].Author = %q, want %q (login inside a reviews array must canonicalize)", i, rv.Author, canonAccount)
		}
	}
	if pr.Reviews[0].Verdict != "approved" || !pr.Reviews[0].IsBot {
		t.Errorf("Reviews[0] non-volatile fields not preserved: verdict=%q isBot=%v, want approved/true", pr.Reviews[0].Verdict, pr.Reviews[0].IsBot)
	}
	if pr.Reviews[1].Verdict != "changes_requested" || pr.Reviews[1].IsBot {
		t.Errorf("Reviews[1] non-volatile fields not preserved: verdict=%q isBot=%v, want changes_requested/false", pr.Reviews[1].Verdict, pr.Reviews[1].IsBot)
	}

	// (c) the statuses leg's target_url and the checks leg's html_url both
	// canonicalize to canonURL (URL<-html_url,target_url).
	if len(pr.Checks.Checks) != 2 {
		t.Fatalf("derived Checks.Checks = %d, want 2 (check_runs + legacy statuses)", len(pr.Checks.Checks))
	}
	for _, c := range pr.Checks.Checks {
		if c.URL != canonURL {
			t.Errorf("Check %q URL = %q, want %q (both html_url and target_url are wire-volatile)", c.Name, c.URL, canonURL)
		}
	}

	// (d) head.sha canonicalizes consistently across the detail leg and the
	// rolled-up Checks.HeadSHA (both decode from the same canonicalized sha).
	if pr.Checks.HeadSHA != canonSHA {
		t.Errorf("Checks.HeadSHA = %q, want %q (head.sha must canonicalize consistently)", pr.Checks.HeadSHA, canonSHA)
	}
	if pr.Number != uint64(canonNumber) {
		t.Errorf("derived Number = %d, want %d (canonicalized)", pr.Number, uint64(canonNumber))
	}
}

// sortedKeys returns a set's keys in sorted order for a stable failure message.
func sortedKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
