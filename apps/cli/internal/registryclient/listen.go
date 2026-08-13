package registryclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/shareclient"
)

type ListenFilters struct {
	Organization string
	Space        string
	Capability   string
}

type ListenCapability struct {
	Name        string  `json:"name"`
	Version     string  `json:"version"`
	DisplayName *string `json:"displayName"`
}

type ListenScope struct {
	Organization string             `json:"organization"`
	Space        string             `json:"space"`
	DisplayName  *string            `json:"displayName"`
	Capabilities []ListenCapability `json:"capabilities"`
}

type ListenShareItem struct {
	Share               shareclient.Share `json:"share"`
	HasRequest          bool              `json:"hasRequest"`
	AwaitingFulfillment bool              `json:"awaitingFulfillment"`
}

type ListenInbox struct {
	Scopes   []ListenScope     `json:"scopes"`
	Shares   []ListenShareItem `json:"shares"`
	Requests []ListenRequest   `json:"requests"`
}

type ListenRequest struct {
	Summary execution.RequestRecord `json:"-"`
	Raw     map[string]any          `json:"-"`
	Status  string                  `json:"status"`
}

func (c *Client) ListenInbox(ctx context.Context, token string, filters ListenFilters) (*ListenInbox, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("a valid login token is required")
	}
	query := url.Values{}
	if filters.Organization != "" {
		query.Set("organization", filters.Organization)
	}
	if filters.Space != "" {
		query.Set("space", filters.Space)
	}
	if filters.Capability != "" {
		query.Set("capability", filters.Capability)
	}
	endpoint := "/api/v1/listen/inbox"
	if encoded := query.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	var response struct {
		Scopes   []ListenScope     `json:"scopes"`
		Shares   []ListenShareItem `json:"shares"`
		Requests []map[string]any  `json:"requests"`
	}
	if err := c.getLoose(ctx, token, endpoint, &response); err != nil {
		return nil, err
	}
	if response.Scopes == nil {
		response.Scopes = []ListenScope{}
	}
	if response.Shares == nil {
		response.Shares = []ListenShareItem{}
	}
	requests := make([]ListenRequest, 0, len(response.Requests))
	for _, raw := range response.Requests {
		record, err := decodeListenRequest(raw)
		if err != nil {
			return nil, fmt.Errorf("decode listen Request: %w", err)
		}
		status, _ := raw["status"].(string)
		requests = append(requests, ListenRequest{Summary: record, Raw: raw, Status: status})
	}
	return &ListenInbox{
		Scopes:   response.Scopes,
		Shares:   response.Shares,
		Requests: requests,
	}, nil
}

func (c *Client) DecideRequest(ctx context.Context, token, organization, space, requestID, decision string) (execution.RequestRecord, error) {
	if organization == "" || space == "" || requestID == "" || decision == "" {
		return execution.RequestRecord{}, fmt.Errorf("organization, space, request id, and decision are required")
	}
	endpoint := "/api/v1/organizations/" + url.PathEscape(organization) +
		"/spaces/" + url.PathEscape(space) +
		"/requests/" + url.PathEscape(requestID) +
		"/decide"
	var response map[string]any
	if err := c.postAuthorizedJSON(ctx, token, endpoint, map[string]string{"decision": decision}, http.StatusOK, &response); err != nil {
		return execution.RequestRecord{}, err
	}
	if inner, ok := response["request"].(map[string]any); ok {
		return decodeListenRequest(inner)
	}
	return decodeListenRequest(response)
}

func (c *Client) postAuthorizedJSON(ctx context.Context, token, endpoint string, body any, wantStatus int, output any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.resolve(endpoint), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "doppels-cli/v1alpha1")
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer response.Body.Close()
	if response.StatusCode != wantStatus {
		return responseError("POST "+endpoint, response.StatusCode, response.Body)
	}
	if output != nil {
		if err := decodeJSONLoose(response.Body, output); err != nil {
			return fmt.Errorf("decode %s response: %w", endpoint, err)
		}
	}
	return nil
}

func decodeListenRequest(raw map[string]any) (execution.RequestRecord, error) {
	data, err := json.Marshal(raw)
	if err != nil {
		return execution.RequestRecord{}, err
	}
	var record execution.RequestRecord
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&record); err != nil {
		return execution.RequestRecord{}, err
	}
	return record, nil
}
