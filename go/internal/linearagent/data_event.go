package linearagent

import (
	"encoding/json"
	"fmt"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// dataEvent models Linear's data-change webhook envelope (type Issue/Comment,
// action create/update/remove, with a `data` object and, on update, an
// `updatedFrom` object of previous values). It is the sibling of SessionEvent
// (webhook.go) — the AgentSession arm is untouched; this is a new data arm.
// json tags mirror Linear's camelCase payload keys.
type dataEvent struct {
	Type        string          `json:"type"`
	Action      string          `json:"action"`
	Data        dataPayload     `json:"data"`
	UpdatedFrom json.RawMessage `json:"updatedFrom"`
}

// dataPayload is the union of Issue/Comment data fields this arm reads.
type dataPayload struct {
	// Issue fields.
	Number     uint64      `json:"number"`
	URL        string      `json:"url"`
	Identifier string      `json:"identifier"`
	Team       dataTeam    `json:"team"`
	State      dataState   `json:"state"`
	ProjectID  string      `json:"projectId"`
	Project    dataProject `json:"project"`

	// Comment fields.
	ID    string     `json:"id"`
	Body  string     `json:"body"`
	User  dataUser   `json:"user"`
	Issue *dataIssue `json:"issue"`
}

type dataTeam struct {
	Key string `json:"key"`
}

type dataState struct {
	Type string `json:"type"`
}

type dataProject struct {
	ID string `json:"id"`
}

type dataUser struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

// dataIssue is the issue a comment is attached to.
type dataIssue struct {
	Number    uint64      `json:"number"`
	URL       string      `json:"url"`
	Team      dataTeam    `json:"team"`
	ProjectID string      `json:"projectId"`
	Project   dataProject `json:"project"`
}

// dataUpdatedFrom carries only the fields this arm inspects to discriminate a
// STATE change from a plain UPDATE.
type dataUpdatedFrom struct {
	StateID *string `json:"stateId"`
}

// ParseLinearDataEvent maps a raw Linear data-change webhook body to a
// normalized forge.ForgeEvent, or ok=false for a payload this arm ignores
// (counted-and-dropped by the caller, never an error). Mapping (design.md
// 660-666): Issue create->OPENED; Issue update->STATE iff updatedFrom shows a
// workflow-state change, else UPDATE; Comment create->COMMENT. `remove`
// actions are counted-and-dropped (no notification kind models deletion).
func ParseLinearDataEvent(raw []byte) (ev forge.ForgeEvent, ok bool, err error) {
	var de dataEvent
	if uerr := json.Unmarshal(raw, &de); uerr != nil {
		return forge.ForgeEvent{}, false, fmt.Errorf("linearagent: parse data event: %w", uerr)
	}

	switch de.Type {
	case "Issue":
		return parseLinearIssue(de)
	case "Comment":
		return parseLinearComment(de)
	default:
		return forge.ForgeEvent{}, false, nil
	}
}

func parseLinearIssue(de dataEvent) (forge.ForgeEvent, bool, error) {
	base := forge.ForgeEvent{
		Provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR,
		Host:     "linear.app",
		Repo:     de.Data.Team.Key,
		Kind:     compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE,
		Number:   de.Data.Number,
		Project:  linearProjectID(de.Data.ProjectID, de.Data.Project),
		URL:      de.Data.URL,
	}

	switch de.Action {
	case "create":
		base.Change = compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_OPENED
		return base, true, nil
	case "update":
		if linearStateChanged(de.UpdatedFrom) {
			base.Change = compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_STATE
			base.State = forge.MapLinearState(de.Data.State.Type)
		} else {
			base.Change = compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_UPDATE
		}
		return base, true, nil
	default:
		// remove (and any other action) is counted-and-dropped.
		return forge.ForgeEvent{}, false, nil
	}
}

func parseLinearComment(de dataEvent) (forge.ForgeEvent, bool, error) {
	if de.Action != "create" || de.Data.Issue == nil {
		return forge.ForgeEvent{}, false, nil
	}
	iss := de.Data.Issue
	base := forge.ForgeEvent{
		Provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR,
		Host:     "linear.app",
		Repo:     iss.Team.Key,
		Kind:     compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE,
		Number:   iss.Number,
		Project:  linearProjectID(iss.ProjectID, iss.Project),
		URL:      iss.URL,
		Change:   compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_COMMENT,
	}
	base.Comment = linearCommentRef(de.Data)
	return base, true, nil
}

// linearProjectID prefers the flat projectId, falling back to the nested
// project object's id.
func linearProjectID(projectID string, project dataProject) string {
	if projectID != "" {
		return projectID
	}
	return project.ID
}

// linearStateChanged reports whether an update's updatedFrom names a prior
// stateId — i.e. the workflow state changed (design.md:660-662).
func linearStateChanged(updatedFrom json.RawMessage) bool {
	if len(updatedFrom) == 0 {
		return false
	}
	var uf dataUpdatedFrom
	if err := json.Unmarshal(updatedFrom, &uf); err != nil {
		return false
	}
	return uf.StateID != nil
}

// linearCommentRef builds a CommentRef from a Linear comment, running
// forge.StripOwner over the body (the one strip point) and surfacing the agent
// claim only for a single well-formed header.
func linearCommentRef(d dataPayload) *compassv1internal.CommentRef {
	clean, author, ok := forge.StripOwner(d.Body)
	ref := &compassv1internal.CommentRef{
		Url:          d.Issue.URL,
		CommentKey:   d.ID,
		Body:         clean,
		ForgeAccount: linearAccount(d.User),
	}
	if ok {
		ref.Agent = &compassv1.AgentAttribution{AgentHandle: author.AgentHandle}
	}
	return ref
}

// linearAccount renders the commenter's display login, preferring displayName.
func linearAccount(u dataUser) string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Name
}
