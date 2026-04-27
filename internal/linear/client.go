// Package linear is admiral's Linear GraphQL client + webhook receiver.
// Scope is the v0.3 happy path: read an issue, set its workflow state,
// post one comment.
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	httpClient *http.Client
	endpoint   string
	apiToken   string
}

func NewClient(endpoint, apiToken string) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		endpoint:   endpoint,
		apiToken:   apiToken,
	}
}

type Issue struct {
	ID          string
	Identifier  string
	Title       string
	Description string
	URL         string
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

func (c *Client) do(ctx context.Context, req graphQLRequest, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", c.apiToken)
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("post graphql: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
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

const issueQuery = `query Issue($id: String!) {
  issue(id: $id) {
    id
    identifier
    title
    description
    url
  }
}`

func (c *Client) GetIssue(ctx context.Context, id string) (*Issue, error) {
	var data struct {
		Issue struct {
			ID          string `json:"id"`
			Identifier  string `json:"identifier"`
			Title       string `json:"title"`
			Description string `json:"description"`
			URL         string `json:"url"`
		} `json:"issue"`
	}
	if err := c.do(ctx, graphQLRequest{
		Query:     issueQuery,
		Variables: map[string]any{"id": id},
	}, &data); err != nil {
		return nil, err
	}
	if data.Issue.ID == "" {
		return nil, fmt.Errorf("issue %s not found", id)
	}
	return &Issue{
		ID:          data.Issue.ID,
		Identifier:  data.Issue.Identifier,
		Title:       data.Issue.Title,
		Description: data.Issue.Description,
		URL:         data.Issue.URL,
	}, nil
}

const issueUpdateMutation = `mutation IssueSetState($id: String!, $stateId: String!) {
  issueUpdate(id: $id, input: {stateId: $stateId}) {
    success
  }
}`

func (c *Client) SetIssueState(ctx context.Context, issueID, stateID string) error {
	var data struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	if err := c.do(ctx, graphQLRequest{
		Query:     issueUpdateMutation,
		Variables: map[string]any{"id": issueID, "stateId": stateID},
	}, &data); err != nil {
		return err
	}
	if !data.IssueUpdate.Success {
		return fmt.Errorf("issueUpdate returned success=false")
	}
	return nil
}

const commentCreateMutation = `mutation CommentCreate($issueId: String!, $body: String!) {
  commentCreate(input: {issueId: $issueId, body: $body}) {
    success
  }
}`

func (c *Client) PostComment(ctx context.Context, issueID, body string) error {
	var data struct {
		CommentCreate struct {
			Success bool `json:"success"`
		} `json:"commentCreate"`
	}
	if err := c.do(ctx, graphQLRequest{
		Query:     commentCreateMutation,
		Variables: map[string]any{"issueId": issueID, "body": body},
	}, &data); err != nil {
		return err
	}
	if !data.CommentCreate.Success {
		return fmt.Errorf("commentCreate returned success=false")
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
