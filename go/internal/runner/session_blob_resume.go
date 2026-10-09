//go:build unix

package runner

import (
	"context"

	"connectrpc.com/connect"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

func (h *agentHost) resumeBlobs(ctx context.Context, containerName, sessionID, resumeBody string, alreadyStarted bool) (string, []runtime.AgentFile, error) {
	refs := blobRefsNewestFirst(resumeBody)
	if len(refs) == 0 {
		return resumeBody, nil, nil
	}
	fetch, err := h.link.FetchSessionBlobs(ctx, containerName, sessionID, refs)
	var absent map[string]struct{}
	switch {
	case err == nil:
		absent = fetch.Absent
	case connect.CodeOf(err) == connect.CodeFailedPrecondition:
		absent = make(map[string]struct{}, len(refs))
		for _, ref := range refs {
			absent[ref] = struct{}{}
		}
	default:
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
		h.log.Warn("session blob fetch failed; starting with local references", "container", containerName, "session_id", sessionID, "error", err)
		return resumeBody, nil, nil
	}
	files := make([]runtime.AgentFile, 0, len(fetch.Blobs))
	for _, blob := range fetch.Blobs {
		files = append(files, runtime.AgentFile{Name: blob.SHA256, Data: blob.Data})
	}
	if alreadyStarted {
		return resumeBody, files, nil
	}
	marked, n := markAbsentBlobs(resumeBody, absent)
	if n > 0 {
		h.log.Info("marked absent session images unavailable", "container", containerName, "session_id", sessionID, "images", n)
	}
	return marked, files, nil
}
