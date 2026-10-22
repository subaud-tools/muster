package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ListTasksIn selects a bounded set of task threads.
type ListTasksIn struct {
	Project  string   `json:"project,omitempty" jsonschema:"optional project touched by a current participant or durable origin_project"`
	Statuses []string `json:"statuses,omitempty" jsonschema:"optional exact statuses; defaults to open, claimed, needs_info, and blocked"`
	ToKind   string   `json:"to_kind,omitempty" jsonschema:"optional exact target kind: agent, role, or broadcast"`
	ToTarget string   `json:"to_target,omitempty" jsonschema:"optional exact target; requires to_kind"`
	From     string   `json:"from,omitempty" jsonschema:"optional exact creator alias"`
	Limit    *int     `json:"limit,omitempty" jsonschema:"maximum tasks to return; defaults to 100 and must be between 1 and 500"`
}

// ListTasksOut contains matching tasks and reports whether more were omitted.
type ListTasksOut struct {
	Tasks     []ThreadView `json:"tasks" jsonschema:"matching tasks ordered by recent update then id"`
	Truncated bool         `json:"truncated" jsonschema:"true when more matching tasks exist beyond the limit"`
}

// TaskCreateIn is the input to task_create.
type TaskCreateIn struct {
	From     string `json:"from" jsonschema:"the requesting agent's alias"`
	ToKind   string `json:"to_kind" jsonschema:"agent, role, or broadcast"`
	ToTarget string `json:"to_target" jsonschema:"the assignee: an alias, a 'project:label' pair, or a bare label of an agent in your own project (resolved in that order) — or a role with to_kind=role; for broadcast: empty for every agent, or a project name for that project's agents only"`
	Subject  string `json:"subject" jsonschema:"a short task title"`
	Ref      string `json:"ref,omitempty" jsonschema:"optional pointer to the work (repo/branch/endpoint/file)"`
	Body     string `json:"body" jsonschema:"task details"`
	Intent   string `json:"intent,omitempty" jsonschema:"fyi | reply-requested | action-requested; mark FYIs so recipients' drains stay cheap — an FYI doesn't demand a reply. Left empty, a task is still treated as action-requested (a task is inherently a request for action)."`
}

// TaskClaimIn is the input to task_claim.
type TaskClaimIn struct {
	ThreadID int64  `json:"thread_id" jsonschema:"the task thread to claim"`
	By       string `json:"by" jsonschema:"the alias of the agent claiming the task"`
}

// TaskTransitionIn is the input to task_transition.
type TaskTransitionIn struct {
	ThreadID int64  `json:"thread_id" jsonschema:"the task thread to update"`
	By       string `json:"by" jsonschema:"the alias making the change"`
	Status   string `json:"status" jsonschema:"new status: open, claimed, needs_info, blocked, completed, declined, or cancelled"`
	Note     string `json:"note,omitempty" jsonschema:"optional note recorded with the status change"`
}

func listTasksHandler(_ context.Context, _ *mcp.CallToolRequest, in ListTasksIn) (*mcp.CallToolResult, ListTasksOut, error) {
	args := map[string]any{}
	if in.Project != "" {
		args["project"] = in.Project
	}
	if len(in.Statuses) > 0 {
		args["statuses"] = in.Statuses
	}
	if in.ToKind != "" {
		args["to_kind"] = in.ToKind
	}
	if in.ToTarget != "" {
		args["to_target"] = in.ToTarget
	}
	if in.From != "" {
		args["from"] = in.From
	}
	if in.Limit != nil {
		args["limit"] = *in.Limit
	}
	raw, err := callDaemon("list_tasks", args)
	if err != nil {
		return nil, ListTasksOut{}, err
	}
	var out ListTasksOut
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, ListTasksOut{}, err
	}
	if out.Tasks == nil {
		out.Tasks = []ThreadView{}
	}
	return nil, out, nil
}

func taskCreateHandler(_ context.Context, _ *mcp.CallToolRequest, in TaskCreateIn) (*mcp.CallToolResult, ThreadIDOut, error) {
	if err := requireRegisteredFrom(in.From); err != nil {
		return nil, ThreadIDOut{}, err
	}
	raw, err := callDaemon("task_create", map[string]any{
		"from": in.From, "to_kind": in.ToKind, "to_target": in.ToTarget,
		"subject": in.Subject, "ref": in.Ref, "body": in.Body, "intent": in.Intent,
	})
	if err != nil {
		return nil, ThreadIDOut{}, err
	}
	var out ThreadIDOut
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, ThreadIDOut{}, err
	}
	return nil, out, nil
}

func taskClaimHandler(_ context.Context, _ *mcp.CallToolRequest, in TaskClaimIn) (*mcp.CallToolResult, OKOut, error) {
	if _, err := callDaemon("task_claim", map[string]any{"thread_id": in.ThreadID, "by": in.By}); err != nil {
		return nil, OKOut{}, err
	}
	return nil, OKOut{OK: true, Detail: "claimed"}, nil
}

func taskTransitionHandler(_ context.Context, _ *mcp.CallToolRequest, in TaskTransitionIn) (*mcp.CallToolResult, OKOut, error) {
	if _, err := callDaemon("task_transition", map[string]any{
		"thread_id": in.ThreadID, "by": in.By, "status": in.Status, "note": in.Note,
	}); err != nil {
		return nil, OKOut{}, err
	}
	return nil, OKOut{OK: true, Detail: in.Status}, nil
}

// registerTaskTools registers the task tools on srv.
func registerTaskTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "list_tasks", Description: "Discover task threads across the bus with exact composable filters. Defaults to nonterminal tasks and 100 results; maximum 500. Results are ordered by most recent update and report truncated when more match."}, listTasksHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "task_create", Description: "Create a task addressed to an agent or role. The assignee(s) can claim and work it. Optional intent (fyi/reply-requested/action-requested) defaults to action-requested — a task is inherently a request for action. A reply-requested or action-requested send (every task without intent fyi) to an agent that has ended is refused with the reason; send fyi to leave it waiting for that alias to return."}, taskCreateHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "task_claim", Description: "Claim an open task. Only the first claimer succeeds; a second claim fails."}, taskClaimHandler)
	mcp.AddTool(srv, &mcp.Tool{Name: "task_transition", Description: "Move a task to a new status (claimed, needs_info, blocked, completed, declined, cancelled) with an optional note."}, taskTransitionHandler)
}
