//go:build unix

package runner

import (
	"strings"
	"testing"
)

func TestMarkAbsentBlobsRewritesOnlyAbsentContentImages(t *testing.T) {
	const absent = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const transient = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	body := "{\"type\":\"message\",\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"image\",\"data\":\"blob:sha256:" + absent + "\",\"mimeType\":\"image/png\"},{\"type\":\"image\",\"data\":\"blob:sha256:" + transient + "\"}]}}\n"

	got, n := markAbsentBlobs(body, map[string]struct{}{absent: {}})
	if n != 1 {
		t.Fatalf("markAbsentBlobs replacements = %d, want 1", n)
	}
	if !strings.Contains(got, imageUnavailableText) || !strings.Contains(got, "blob:sha256:"+transient) {
		t.Fatalf("marked body = %s, want absent marker and unchanged transient reference", got)
	}
}

func TestMarkAbsentBlobsPreservesUnchangedLinesAndLargeNumbers(t *testing.T) {
	const absent = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	const untouched = "{\"type\":\"message\",\"message\":{\"content\":\"plain\"}}  \n"
	large := "{\"sequence\":18446744073709551615,\"type\":\"message\",\"message\":{\"content\":[{\"type\":\"image\",\"data\":\"blob:sha256:" + absent + "\"}]}}\n"
	body := untouched + large

	got, n := markAbsentBlobs(body, map[string]struct{}{absent: {}})
	if n != 1 {
		t.Fatalf("markAbsentBlobs replacements = %d, want 1", n)
	}
	if !strings.HasPrefix(got, untouched) {
		t.Fatalf("unchanged line = %q, want byte-identical %q", strings.SplitN(got, large, 2)[0], untouched)
	}
	if !strings.Contains(got, "18446744073709551615") {
		t.Fatalf("rewritten line lost large integer: %s", got)
	}
}
