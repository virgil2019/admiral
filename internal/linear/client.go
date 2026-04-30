// Package linear is admiral's Linear GraphQL client + webhook receiver,
// scoped to the v0.3 happy path of the Linear Agent SDK: receive an
// AgentSessionEvent, fetch issue context, post agent activities back.
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// TokenRefresherInterface is the interface for token refreshers.
type TokenRefresherInterface interface {
	RefreshAndRetry(ctx context.Context) (string, error)
}

// Client is admiral's Linear GraphQL client + webhook receiver.
type Client struct {
	httpClient     *http.Client
	endpoint       string
	apiToken       string
	tokenRefresher TokenRefresherInterface
}

func NewClient(endpoint, apiToken string) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		endpoint:   endpoint,
		apiToken:   apiToken,
	}
}

// SetTokenRefresher wires in the token refresher so that API calls that
// receive a 401 can attempt an automatic token refresh + one-shot retry.
func (c *Client) SetTokenRefresher(tr TokenRefresherInterface) {
	c.tokenRefresher = tr
}

// Issue is the subset of Linear issue fields admiral needs for prompt
// context. Mirror of `agent.ts` issueToContext.
type Issue struct {
	ID          string
	Identifier  string
	Title       string
	Description string
	URL         string
	StateName   string
	Priority    int
	AssigneeID  string
	TeamID      string
	ProjectID   string
	Labels      []string
	Comments    []Comment
}

// WorkflowState represents a Linear workflow state.
type WorkflowState struct {
	ID       string
	Name     string
	Type     string // "triage" / "backlog" / "unstarted" / "started" / "completed" / "canceled"
	Position float64
}

type Comment struct {
	UserName  string
	Body      string
	CreatedAt string
}

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type graphQLError struct {
	Message string `json:"message"`
}

type graphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []graphQLError  `json:"errors"`
}

// retryHTTP wraps c.httpClient.Do with classification + backoff.
// Caller should still inspect resp.StatusCode for HTTP-level errors;
// retryHTTP only retries transient ones automatically.
func (c *Client) retryHTTP(req *http.Request) (*http.Response, error) {
	delays := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
	// Read and clone body so each attempt gets a fresh reader.
	var bodyBytes []byte
	if req.Body != nil {
		bodyBytes, _ = io.ReadAll(req.Body)
		req.Body.Close()
	}
	var lastErr error
	for attempt := 0; attempt <= len(delays); attempt++ {
		if len(bodyBytes) > 0 {
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			if !isTransientNetErr(err) || attempt == len(delays) {
				return nil, err
			}
		} else {
			if !isTransientHTTPStatus(resp.StatusCode) {
				return resp, nil
			}
			resp.Body.Close()
			lastErr = fmt.Errorf("transient http %d", resp.StatusCode)
			if attempt == len(delays) {
				return nil, lastErr
			}
		}
		time.Sleep(delays[attempt])
	}
	return nil, lastErr
}

// isTransientHTTPStatus returns true for 5xx, 408, 429.
func isTransientHTTPStatus(s int) bool {
	return s >= 500 || s == http.StatusRequestTimeout || s == http.StatusTooManyRequests
}

// isTransientNetErr returns true for temporary network errors.
func isTransientNetErr(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Temporary() || ne.Timeout()
	}
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}

func (c *Client) do(ctx context.Context, req graphQLRequest, out any) error {
	return c.doWithToken(ctx, c.apiToken, &req, out)
}

// doWithToken performs the GraphQL request with the given token. On HTTP 401
// and a configured TokenRefresher, it attempts one token refresh + retry
// before failing.
func (c *Client) doWithToken(ctx context.Context, token string, graphqlReq *graphQLRequest, out any) error {
	body, err := json.Marshal(graphqlReq)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", bearer(token))
	resp, err := c.retryHTTP(httpReq)
	if err != nil {
		return fmt.Errorf("post graphql: %w", err)
	}
	if resp == nil {
		return fmt.Errorf("post graphql: retryHTTP returned nil resp with err=%v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		// HTTP 401 — attempt one token refresh if available.
		if resp.StatusCode == http.StatusUnauthorized && c.tokenRefresher != nil {
			newToken, refreshErr := c.tokenRefresher.RefreshAndRetry(ctx)
			if refreshErr != nil {
				return fmt.Errorf("linear http 401 (token refresh failed): %w", refreshErr)
			}
			// Retry once with the new token.
			return c.doWithToken(ctx, newToken, graphqlReq, out)
		}
		return fmt.Errorf("linear http %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}
	var env graphQLResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("unmarshal envelope: %w", err)
	}
	if len(env.Errors) > 0 {
		return fmt.Errorf("linear graphql: %s", env.Errors[0].Message)
	}
	if out != nil {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("unmarshal data: %w", err)
		}
	}
	return nil
}

func bearer(token string) string {
	if strings.HasPrefix(token, "Bearer ") {
		return token
	}
	return "Bearer " + token
}

const issueQuery = `query Issue($id: String!) {
  issue(id: $id) {
    id
    identifier
    title
    description
    url
    priority
    state { name }
    assignee { id }
    team { id }
    project { id }
    labels { nodes { name } }
    comments(first: 20) { nodes { body createdAt user { name } } }
  }
}`

func (c *Client) GetIssue(ctx context.Context, id string) (*Issue, error) {
	var data struct {
		Issue *struct {
			ID          string `json:"id"`
			Identifier  string `json:"identifier"`
			Title       string `json:"title"`
			Description string `json:"description"`
			URL         string `json:"url"`
			Priority    int    `json:"priority"`
			State       *struct {
				Name string `json:"name"`
			} `json:"state"`
			Assignee *struct {
				ID string `json:"id"`
			} `json:"assignee"`
			Team *struct {
				ID string `json:"id"`
			} `json:"team"`
			Project *struct {
				ID string `json:"id"`
			} `json:"project"`
			Labels struct {
				Nodes []struct {
					Name string `json:"name"`
				} `json:"nodes"`
			} `json:"labels"`
			Comments struct {
				Nodes []struct {
					Body      string `json:"body"`
					CreatedAt string `json:"createdAt"`
					User      *struct {
						Name string `json:"name"`
					} `json:"user"`
				} `json:"nodes"`
			} `json:"comments"`
		} `json:"issue"`
	}
	if err := c.do(ctx, graphQLRequest{
		Query:     issueQuery,
		Variables: map[string]any{"id": id},
	}, &data); err != nil {
		return nil, err
	}
	if data.Issue == nil || data.Issue.ID == "" {
		return nil, fmt.Errorf("issue %s not found", id)
	}
	out := &Issue{
		ID:          data.Issue.ID,
		Identifier:  data.Issue.Identifier,
		Title:       data.Issue.Title,
		Description: data.Issue.Description,
		URL:         data.Issue.URL,
		Priority:    data.Issue.Priority,
	}
	if data.Issue.State != nil {
		out.StateName = data.Issue.State.Name
	}
	if data.Issue.Assignee != nil {
		out.AssigneeID = data.Issue.Assignee.ID
	}
	if data.Issue.Team != nil {
		out.TeamID = data.Issue.Team.ID
	}
	if data.Issue.Project != nil {
		out.ProjectID = data.Issue.Project.ID
	}
	for _, l := range data.Issue.Labels.Nodes {
		out.Labels = append(out.Labels, l.Name)
	}
	for _, c := range data.Issue.Comments.Nodes {
		cm := Comment{Body: c.Body, CreatedAt: c.CreatedAt}
		if c.User != nil {
			cm.UserName = c.User.Name
		}
		out.Comments = append(out.Comments, cm)
	}
	return out, nil
}

// AgentActivityType is the discriminator for the AgentActivityContent input.
// Linear's schema names: "thought" (italic, ephemeral-friendly), "action"
// (with action+parameter+optional result), "response" (the agent's answer
// — usually one terminal post per session), "error" (visible failure),
// "elicitation" (asking the user a question — surfaces a reply input).
type AgentActivityType string

const (
	ActivityThought     AgentActivityType = "thought"
	ActivityAction      AgentActivityType = "action"
	ActivityResponse    AgentActivityType = "response"
	ActivityError       AgentActivityType = "error"
	ActivityElicitation AgentActivityType = "elicitation"
)

// AgentActivity is the input for agentActivityCreate. Use the typed
// constructors below to keep field requirements straight per type — the
// schema rejects e.g. an "action" without `action` + `parameter`.
type AgentActivity struct {
	Type      AgentActivityType `json:"type"`
	Body      string            `json:"body,omitempty"`
	Action    string            `json:"action,omitempty"`
	Parameter string            `json:"parameter,omitempty"`
	Result    string            `json:"result,omitempty"`
	Ephemeral bool              `json:"ephemeral,omitempty"`
}

func Thought(body string, ephemeral bool) AgentActivity {
	return AgentActivity{Type: ActivityThought, Body: body, Ephemeral: ephemeral}
}

func Action(action, parameter, result string) AgentActivity {
	return AgentActivity{Type: ActivityAction, Action: action, Parameter: parameter, Result: result}
}

func Response(body string) AgentActivity {
	return AgentActivity{Type: ActivityResponse, Body: body}
}

func ErrorActivity(body string) AgentActivity {
	return AgentActivity{Type: ActivityError, Body: body}
}

func Elicitation(body string) AgentActivity {
	return AgentActivity{Type: ActivityElicitation, Body: body}
}

const agentActivityCreateMutation = `mutation CreateAgentActivity($input: AgentActivityCreateInput!) {
  agentActivityCreate(input: $input) {
    success
    agentActivity { id }
  }
}`

// PostAgentActivity posts one activity into an agent session thread. The
// session is durable: subsequent activities to the same sessionID land in
// the same thread.
func (c *Client) PostAgentActivity(ctx context.Context, sessionID string, a AgentActivity) error {
	input := map[string]any{
		"agentSessionId": sessionID,
		"content":        a,
	}
	var data struct {
		AgentActivityCreate struct {
			Success bool `json:"success"`
		} `json:"agentActivityCreate"`
	}
	if err := c.do(ctx, graphQLRequest{
		Query:     agentActivityCreateMutation,
		Variables: map[string]any{"input": input},
	}, &data); err != nil {
		return err
	}
	if !data.AgentActivityCreate.Success {
		return fmt.Errorf("agentActivityCreate returned success=false")
	}
	return nil
}

const workflowStatesQuery = `query WorkflowStates($teamID: String!) {
  workflowStates(filter: {team: {id: {eq: $teamID}}}) {
    nodes { id name type position }
  }
}`

// GetWorkflowStates returns all workflow states for a given team.
func (c *Client) GetWorkflowStates(ctx context.Context, teamID string) ([]WorkflowState, error) {
	var data struct {
		WorkflowStates struct {
			Nodes []struct {
				ID       string  `json:"id"`
				Name     string  `json:"name"`
				Type     string  `json:"type"`
				Position float64 `json:"position"`
			} `json:"nodes"`
		} `json:"workflowStates"`
	}
	if err := c.do(ctx, graphQLRequest{
		Query:     workflowStatesQuery,
		Variables: map[string]any{"teamID": teamID},
	}, &data); err != nil {
		return nil, err
	}
	states := make([]WorkflowState, 0, len(data.WorkflowStates.Nodes))
	for _, n := range data.WorkflowStates.Nodes {
		states = append(states, WorkflowState{
			ID:       n.ID,
			Name:     n.Name,
			Type:     n.Type,
			Position: n.Position,
		})
	}
	return states, nil
}

const issueUpdateMutation = `mutation IssueUpdate($id: String!, $input: IssueUpdateInput!) {
  issueUpdate(id: $id, input: $input) { success }
}`

// IssueUpdate sets the workflow state of an issue.
func (c *Client) IssueUpdate(ctx context.Context, issueID, stateID string) error {
	input := map[string]any{"stateId": stateID}
	var data struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	if err := c.do(ctx, graphQLRequest{
		Query:     issueUpdateMutation,
		Variables: map[string]any{"id": issueID, "input": input},
	}, &data); err != nil {
		return err
	}
	if !data.IssueUpdate.Success {
		return fmt.Errorf("issueUpdate returned success=false")
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
