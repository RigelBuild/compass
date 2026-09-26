package forge

// The GitHub GraphQL leg of the pull-request read. Review threads (with their
// resolution) and per-PR required-context status are GraphQL-only, so this file
// adds one POST path beside the REST client. It rides doJSONOn against the
// GraphQL rate bucket, sharing the TokenSource and the HTTP error mapping.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

// errMalformedRepo is returned (wrapped) when a GitHub repo coordinate is not
// exactly "owner/name"; GraphQL takes the two halves as separate variables.
var errMalformedRepo = errors.New("forge: malformed github repo, want owner/name")

// ghGraphQLRateLimited is the errors[].type GitHub returns, on HTTP 200, when
// the GraphQL budget is spent.
const ghGraphQLRateLimited = "RATE_LIMITED"

// ghTypeBot is the GraphQL __typename of a bot actor.
const ghTypeBot = "Bot"

// pullCoord is a pull request's GraphQL coordinate. repo is the REST form the
// caller passed; owner and name are its validated halves.
type pullCoord struct {
	repo   string
	owner  string
	name   string
	number uint64
}

// newPullCoord splits repo into owner and name. The number must fit GraphQL's
// 32-bit Int, or the query would be rejected (or address a different PR).
func newPullCoord(repo string, number uint64) (pullCoord, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return pullCoord{}, fmt.Errorf("%w: %q", errMalformedRepo, repo)
	}
	if number > math.MaxInt32 {
		return pullCoord{}, fmt.Errorf("forge: github pull request number %d exceeds the GraphQL Int range", number)
	}
	return pullCoord{repo: repo, owner: owner, name: name, number: number}, nil
}

// graphQLRequest is the POST body of one GraphQL call.
type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// graphQLEnvelope is a typed GraphQL response: data plus any errors. GitHub
// answers a failed query with HTTP 200 and a non-empty errors array.
type graphQLEnvelope[T any] struct {
	Data   T              `json:"data"`
	Errors []graphQLError `json:"errors"`
}

// graphQL posts one query through doJSONOn on the GraphQL rate bucket and
// decodes its data. A non-empty errors array is an error even on HTTP 200, so a
// failed query never reads as an empty result; a RATE_LIMITED entry is the
// budget skip (*RateLimitError), the same as a REST 403/429.
func graphQL[T any](ctx context.Context, g *GitHub, query string, vars map[string]any) (T, error) {
	var zero T
	var env graphQLEnvelope[T]
	if err := g.doJSONOn(ctx, resourceGraphQL, http.MethodPost, g.graphQLURL(), graphQLRequest{Query: query, Variables: vars}, &env); err != nil {
		return zero, err
	}
	if len(env.Errors) == 0 {
		return env.Data, nil
	}
	for _, e := range env.Errors {
		if e.Type == ghGraphQLRateLimited {
			return zero, fmt.Errorf("forge: github graphql: %s: %w", joinErrors(env.Errors), g.graphQLRateLimited())
		}
	}
	return zero, fmt.Errorf("forge: github graphql: %s", joinErrors(env.Errors))
}

// graphQLRateLimited arms the GraphQL gate after a RATE_LIMITED error and
// returns the skip with its retry hint. doJSONOn has already recorded the
// response's reset header, so an armed gate carries the hint; else arm the
// bounded default skip so the next call still fails fast.
func (g *GitHub) graphQLRateLimited() *RateLimitError {
	if hint, blocked := g.gateBlocked(resourceGraphQL); blocked {
		return &RateLimitError{RetryAfter: hint}
	}
	g.mu.Lock()
	g.armGate(resourceGraphQL, time.Time{})
	g.mu.Unlock()
	return &RateLimitError{}
}

// ghPageInfo is a GraphQL connection's cursor state.
type ghPageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

// ghGQLActor is a comment author. It is null for a deleted account ("ghost").
type ghGQLActor struct {
	Login    string `json:"login"`
	Typename string `json:"__typename"`
}

// ghGQLComment is one review-thread comment.
type ghGQLComment struct {
	Author *ghGQLActor `json:"author"`
	Body   string      `json:"body"`
}

// ghGQLComments is one page of a thread's comments.
type ghGQLComments struct {
	PageInfo ghPageInfo     `json:"pageInfo"`
	Nodes    []ghGQLComment `json:"nodes"`
}

// ghGQLThread is one review thread. ID lets a thread with more than one page of
// comments be re-queried for the rest.
type ghGQLThread struct {
	ID         string        `json:"id"`
	IsResolved bool          `json:"isResolved"`
	Path       string        `json:"path"`
	Comments   ghGQLComments `json:"comments"`
}

// ghGQLThreads is one page of a pull request's review threads.
type ghGQLThreads struct {
	PageInfo ghPageInfo    `json:"pageInfo"`
	Nodes    []ghGQLThread `json:"nodes"`
}

// ghGQLContext is one status-check-rollup context: a CheckRun carries name, a
// legacy StatusContext carries context. Both carry isRequired for the PR.
type ghGQLContext struct {
	Typename   string `json:"__typename"`
	Name       string `json:"name"`
	Context    string `json:"context"`
	IsRequired bool   `json:"isRequired"`
}

// ghGQLContexts is one page of a commit's status-check-rollup contexts.
type ghGQLContexts struct {
	PageInfo ghPageInfo     `json:"pageInfo"`
	Nodes    []ghGQLContext `json:"nodes"`
}

// ghGQLRollup is a commit's status check rollup; null when the commit has none.
type ghGQLRollup struct {
	Contexts ghGQLContexts `json:"contexts"`
}

// ghGQLPullCommits is the PR's last commit (commits(last: 1)).
type ghGQLPullCommits struct {
	Nodes []struct {
		Commit struct {
			StatusCheckRollup *ghGQLRollup `json:"statusCheckRollup"`
		} `json:"commit"`
	} `json:"nodes"`
}

// ghGQLPull is the pull request half; each connection is absent when excluded.
type ghGQLPull struct {
	ReviewThreads *ghGQLThreads     `json:"reviewThreads"`
	Commits       *ghGQLPullCommits `json:"commits"`
}

// ghGQLRepo is the repository root of pullReadQuery.
type ghGQLRepo struct {
	PullRequest *ghGQLPull `json:"pullRequest"`
}

// ghGQLPullData is the data of pullReadQuery.
type ghGQLPullData struct {
	Repository *ghGQLRepo `json:"repository"`
}

// ghGQLThreadNode is the data of threadCommentsQuery.
type ghGQLThreadNode struct {
	Node *struct {
		Comments ghGQLComments `json:"comments"`
	} `json:"node"`
}

// pullReadQuery fetches review threads and required contexts in one call. The
// @include flags let a later page fetch only the connection still paging. The
// contexts are read through the pull request (commits(last: 1)), which needs
// only Pull requests: read, not the Contents access a git-object read would.
// Required-ness depends on the context name and the base-branch rules, not on
// the commit, so the last commit need not equal the REST head SHA.
const pullReadQuery = `query($owner: String!, $name: String!, $number: Int!, $threads: Boolean!, $threadsAfter: String, $contexts: Boolean!, $contextsAfter: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $threadsAfter) @include(if: $threads) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          isResolved
          path
          comments(first: 100) {
            pageInfo { hasNextPage endCursor }
            nodes { author { login __typename } body }
          }
        }
      }
      commits(last: 1) @include(if: $contexts) {
        nodes {
          commit {
            statusCheckRollup {
              contexts(first: 100, after: $contextsAfter) {
                pageInfo { hasNextPage endCursor }
                nodes {
                  __typename
                  ... on CheckRun { name isRequired(pullRequestNumber: $number) }
                  ... on StatusContext { context isRequired(pullRequestNumber: $number) }
                }
              }
            }
          }
        }
      }
    }
  }
}`

// threadCommentsQuery fetches the comments after the first page of one thread.
const threadCommentsQuery = `query($id: ID!, $after: String) {
  node(id: $id) {
    ... on PullRequestReviewThread {
      comments(first: 100, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes { author { login __typename } body }
      }
    }
  }
}`

// graphQLURL derives the GraphQL endpoint from the configured host, next to
// apiBase: api.github.com/graphql, or https://<host>/api/graphql on GHES.
func (g *GitHub) graphQLURL() string {
	if g.host == "" || g.host == hostGitHub {
		return "https://api.github.com/graphql"
	}
	return "https://" + g.host + "/api/graphql"
}

// checksForPull is the one checks path for a pull request (GetPullRequest and
// Checks): the REST roll-up from checksForSHA, with Required set from the PR's
// required contexts. withThreads also returns the review threads from the same
// GraphQL leg, so GetPullRequest pays one leg, not two.
func (g *GitHub) checksForPull(ctx context.Context, c pullCoord, sha string, withThreads bool) (Checks, []ReviewThread, error) {
	checks, err := g.checksForSHA(ctx, c.repo, sha)
	if err != nil {
		return Checks{}, nil, err
	}
	threads, required, err := g.pullGraphQL(ctx, c, withThreads)
	if err != nil {
		return Checks{}, nil, fmt.Errorf("forge: github graphql for %q#%d: %w", c.repo, c.number, err)
	}
	for i := range checks.Checks {
		if _, ok := required[checks.Checks[i].Name]; ok {
			checks.Checks[i].Required = true
		}
	}
	return checks, threads, nil
}

// pullGraphQLWalk is the cursor state of one pullGraphQL walk. A connection
// that has finished drops out of later queries through its @include flag.
type pullGraphQLWalk struct {
	threads                     []ReviewThread
	required                    map[string]struct{}
	threadsAfter, contextsAfter any // nil sends JSON null: the first page
	moreThreads, moreContexts   bool
}

// pullGraphQL walks pullReadQuery to completion: every review-thread page (when
// withThreads) and every context page. It returns the threads in forge order and
// the set of required context names (a CheckRun's name, a StatusContext's
// context), which match the REST check names.
func (g *GitHub) pullGraphQL(ctx context.Context, c pullCoord, withThreads bool) ([]ReviewThread, map[string]struct{}, error) {
	w := pullGraphQLWalk{required: map[string]struct{}{}, moreThreads: withThreads, moreContexts: true}
	for w.moreThreads || w.moreContexts {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		data, err := graphQL[ghGQLPullData](ctx, g, pullReadQuery, map[string]any{
			"owner": c.owner, "name": c.name, "number": c.number,
			"threads": w.moreThreads, "threadsAfter": w.threadsAfter,
			"contexts": w.moreContexts, "contextsAfter": w.contextsAfter,
		})
		if err != nil {
			return nil, nil, err
		}
		if data.Repository == nil {
			return nil, nil, fmt.Errorf("forge: github graphql: repository %q not found", c.repo)
		}
		pr := data.Repository.PullRequest
		if pr == nil {
			return nil, nil, fmt.Errorf("forge: github graphql: pull request %q#%d not found", c.repo, c.number)
		}
		if w.moreThreads {
			if err := g.foldThreads(ctx, &w, c, pr.ReviewThreads); err != nil {
				return nil, nil, err
			}
		}
		if w.moreContexts {
			if err := foldContexts(&w, c, pr.Commits); err != nil {
				return nil, nil, err
			}
		}
	}
	return w.threads, w.required, nil
}

// foldThreads appends one page of review threads to w and advances its cursor.
func (g *GitHub) foldThreads(ctx context.Context, w *pullGraphQLWalk, c pullCoord, conn *ghGQLThreads) error {
	if conn == nil {
		return fmt.Errorf("forge: github graphql: pull request %q#%d has no review threads connection", c.repo, c.number)
	}
	for _, t := range conn.Nodes {
		comments, err := g.threadComments(ctx, t)
		if err != nil {
			return err
		}
		w.threads = append(w.threads, ReviewThread{Path: t.Path, Resolved: t.IsResolved, Comments: comments})
	}
	var err error
	if w.moreThreads, w.threadsAfter, err = nextPage(conn.PageInfo, w.threadsAfter); err != nil {
		return fmt.Errorf("forge: github graphql review threads: %w", err)
	}
	return nil
}

// foldContexts adds one page of required context names to w and advances its
// cursor. A PR with no commits, or a last commit with no checks or statuses
// (a null rollup), has nothing required.
func foldContexts(w *pullGraphQLWalk, c pullCoord, commits *ghGQLPullCommits) error {
	if commits == nil {
		return fmt.Errorf("forge: github graphql: pull request %q#%d has no commits connection", c.repo, c.number)
	}
	if len(commits.Nodes) == 0 || commits.Nodes[0].Commit.StatusCheckRollup == nil {
		w.moreContexts = false
		return nil
	}
	contexts := commits.Nodes[0].Commit.StatusCheckRollup.Contexts
	for _, cx := range contexts.Nodes {
		if !cx.IsRequired {
			continue
		}
		name := cx.Name
		if cx.Typename == "StatusContext" {
			name = cx.Context
		}
		w.required[name] = struct{}{}
	}
	var err error
	if w.moreContexts, w.contextsAfter, err = nextPage(contexts.PageInfo, w.contextsAfter); err != nil {
		return fmt.Errorf("forge: github graphql status contexts: %w", err)
	}
	return nil
}

// threadComments maps a thread's comments in order, fetching every page past
// the first, so a long thread is never truncated.
func (g *GitHub) threadComments(ctx context.Context, t ghGQLThread) ([]ThreadComment, error) {
	out := make([]ThreadComment, 0, len(t.Comments.Nodes))
	page := t.Comments
	var after any // the first page came inline with the thread
	for {
		for _, n := range page.Nodes {
			out = append(out, toThreadComment(n))
		}
		more, next, err := nextPage(page.PageInfo, after)
		if err != nil {
			return nil, fmt.Errorf("forge: github graphql thread comments: %w", err)
		}
		if !more {
			return out, nil
		}
		after = next
		data, err := graphQL[ghGQLThreadNode](ctx, g, threadCommentsQuery, map[string]any{"id": t.ID, "after": after})
		if err != nil {
			return nil, err
		}
		if data.Node == nil {
			return nil, fmt.Errorf("forge: github graphql: review thread %q not found", t.ID)
		}
		page = data.Node.Comments
	}
}

// toThreadComment maps one wire comment. A null author (a deleted account) maps
// to an empty Author and IsBot false. GraphQL drops the "[bot]" suffix REST
// logins carry, so it is restored to keep one author spelling across both reads.
func toThreadComment(n ghGQLComment) ThreadComment {
	tc := ThreadComment{Body: n.Body}
	if n.Author != nil {
		tc.Author = n.Author.Login
		tc.IsBot = n.Author.Typename == ghTypeBot
		if tc.IsBot {
			tc.Author += "[bot]"
		}
	}
	return tc
}

// nextPage reads a connection's cursor state against the cursor that fetched
// the page (prev; nil for the first page). A next page with no cursor, or with
// the cursor already used, is an error: asking again would loop forever.
func nextPage(p ghPageInfo, prev any) (bool, any, error) {
	if !p.HasNextPage {
		return false, nil, nil
	}
	if p.EndCursor == "" {
		return false, nil, errors.New("hasNextPage without an endCursor")
	}
	if s, ok := prev.(string); ok && s == p.EndCursor {
		return false, nil, fmt.Errorf("endCursor %q repeats the previous cursor", p.EndCursor)
	}
	return true, p.EndCursor, nil
}
