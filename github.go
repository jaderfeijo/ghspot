package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

type Label struct {
	Name  string
	Color string
}

type Comment struct {
	Author    string
	Body      string
	CreatedAt time.Time
}

type Item struct {
	IsPR           bool
	Number         int
	Title          string
	URL            string
	State          string // OPEN, CLOSED, MERGED
	StateReason    string // COMPLETED, NOT_PLANNED, REOPENED (issues only)
	IsDraft        bool
	Body           string
	Author         string
	Repo           string
	Labels         []Label
	Assignees      []string
	Comments       int
	CreatedAt      time.Time
	UpdatedAt      time.Time
	HeadRef        string
	BaseRef        string
	Additions      int
	Deletions      int
	ReviewDecision string
}

func (it *Item) Key() string { return fmt.Sprintf("%s#%d", it.Repo, it.Number) }

const itemFields = `number title url state body createdAt updatedAt
author{login} repository{nameWithOwner}
labels(first:10){nodes{name color}}
assignees(first:5){nodes{login}}
comments{totalCount}`

const prFields = `isDraft headRefName baseRefName additions deletions`

const searchQuery = `query($q:String!,$after:String,$first:Int!){
  search(query:$q,type:ISSUE,first:$first,after:$after){
    issueCount
    pageInfo{hasNextPage endCursor}
    nodes{
      __typename
      ... on Issue{` + itemFields + ` stateReason}
      ... on PullRequest{` + itemFields + ` ` + prFields + `}
    }
  }
}`

const commentFields = `commentList: comments(last:50){nodes{author{login} body createdAt}}`

const detailQuery = `query($owner:String!,$name:String!,$n:Int!){
  repository(owner:$owner,name:$name){
    issueOrPullRequest(number:$n){
      __typename
      ... on Issue{` + itemFields + ` stateReason ` + commentFields + `}
      ... on PullRequest{` + itemFields + ` ` + prFields + ` reviewDecision ` + commentFields + `}
    }
  }
}`

type login struct{ Login string }

type rawComment struct {
	Author    *login
	Body      string
	CreatedAt time.Time
}

type rawNode struct {
	Typename    string `json:"__typename"`
	Number      int
	Title       string
	URL         string
	State       string
	StateReason string
	Body        string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Author      *login
	Repository  struct{ NameWithOwner string }
	Labels      struct{ Nodes []Label }
	Assignees   struct{ Nodes []login }
	Comments    struct{ TotalCount int }
	CommentList *struct{ Nodes []rawComment } `json:"commentList"`

	IsDraft        bool
	HeadRefName    string
	BaseRefName    string
	Additions      int
	Deletions      int
	ReviewDecision string
}

func authorName(l *login) string {
	if l == nil {
		return "ghost"
	}
	return l.Login
}

func (n rawNode) item() *Item {
	it := &Item{
		IsPR:           n.Typename == "PullRequest",
		Number:         n.Number,
		Title:          n.Title,
		URL:            n.URL,
		State:          n.State,
		StateReason:    n.StateReason,
		IsDraft:        n.IsDraft,
		Body:           n.Body,
		Author:         authorName(n.Author),
		Repo:           n.Repository.NameWithOwner,
		Labels:         n.Labels.Nodes,
		Comments:       n.Comments.TotalCount,
		CreatedAt:      n.CreatedAt,
		UpdatedAt:      n.UpdatedAt,
		HeadRef:        n.HeadRefName,
		BaseRef:        n.BaseRefName,
		Additions:      n.Additions,
		Deletions:      n.Deletions,
		ReviewDecision: n.ReviewDecision,
	}
	for _, a := range n.Assignees.Nodes {
		it.Assignees = append(it.Assignees, a.Login)
	}
	return it
}

type searchResult struct {
	items  []*Item
	cursor string
	more   bool
	total  int
}

func searchPage(c *api.GraphQLClient, q, after string, first int) (searchResult, error) {
	var resp struct {
		Search struct {
			IssueCount int
			PageInfo   struct {
				HasNextPage bool
				EndCursor   string
			}
			Nodes []rawNode
		}
	}
	vars := map[string]interface{}{"q": q, "first": first, "after": nil}
	if after != "" {
		vars["after"] = after
	}
	if err := c.Do(searchQuery, vars, &resp); err != nil {
		return searchResult{}, err
	}
	res := searchResult{
		cursor: resp.Search.PageInfo.EndCursor,
		more:   resp.Search.PageInfo.HasNextPage,
		total:  resp.Search.IssueCount,
	}
	for _, n := range resp.Search.Nodes {
		if n.Number == 0 {
			continue
		}
		res.items = append(res.items, n.item())
	}
	return res, nil
}

// fetchDetail loads a single issue or PR by number, including its comments.
// It returns a nil item when the number doesn't exist in the repo.
func fetchDetail(c *api.GraphQLClient, repo string, number int) (*Item, []Comment, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return nil, nil, fmt.Errorf("bad repo %q", repo)
	}
	var resp struct {
		Repository struct {
			IssueOrPullRequest *rawNode
		}
	}
	vars := map[string]interface{}{"owner": owner, "name": name, "n": number}
	if err := c.Do(detailQuery, vars, &resp); err != nil {
		// A missing number comes back as a GraphQL "not found" error.
		if strings.Contains(err.Error(), "Could not resolve") {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	n := resp.Repository.IssueOrPullRequest
	if n == nil {
		return nil, nil, nil
	}
	var comments []Comment
	if n.CommentList != nil {
		for _, rc := range n.CommentList.Nodes {
			comments = append(comments, Comment{Author: authorName(rc.Author), Body: rc.Body, CreatedAt: rc.CreatedAt})
		}
	}
	return n.item(), comments, nil
}
