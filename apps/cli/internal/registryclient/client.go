// Package registryclient speaks the durable control-plane registry API used by
// doppels preview and doppels apply. It has no knowledge of local discovery or CLI
// configuration, keeping those commands testable without a Cloud process.
package registryclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"doppels.so/cli/internal/execution"
)

const maxResponseBytes int64 = 4 << 20

type Client struct {
	server *url.URL
	http   *http.Client
}

func New(server string, httpClient *http.Client) (*Client, error) {
	parsed, err := ParseServer(server)
	if err != nil {
		return nil, err
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{server: parsed, http: httpClient}, nil
}

func ParseServer(server string) (*url.URL, error) {
	server = strings.TrimRight(strings.TrimSpace(server), "/")
	parsed, err := url.Parse(server)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid Doppels server URL %q", server)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("Doppels server URL cannot contain credentials, a query, or a fragment")
	}
	if parsed.Scheme == "http" && !loopbackHost(parsed.Hostname()) {
		return nil, errors.New("remote Doppels server URLs must use https")
	}
	return parsed, nil
}

type ScopeMetadata struct {
	Name        string            `json:"name"`
	DisplayName string            `json:"displayName,omitempty"`
	Summary     string            `json:"summary,omitempty"`
	Description string            `json:"description,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type SpaceConfiguration struct {
	SourceAuthority string        `json:"sourceAuthority"`
	Metadata        ScopeMetadata `json:"metadata"`
}

type Resource struct {
	Kind            string                        `json:"kind"`
	SourceAuthority string                        `json:"sourceAuthority"`
	Revision        execution.DefinitionReference `json:"revision"`
	Manifest        any                           `json:"manifest"`
	ManifestSource  string                        `json:"manifestSource,omitempty"`
}

type ReconcileRequest struct {
	Space     *SpaceConfiguration `json:"space,omitempty"`
	Resources []Resource          `json:"resources"`
}

type Change struct {
	Action  string `json:"action"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

type PreviewResponse struct {
	Changes    []Change `json:"changes"`
	Applicable bool     `json:"applicable"`
}

type ApplyResponse struct {
	Changes []Change `json:"changes"`
	Applied bool     `json:"applied"`
}

// KeepEntry identifies a Capability or Recipe still present in the local
// Project. prune unregisters manifest-managed Space registrations whose
// kind/name is absent from this set; it never inspects revisions.
type KeepEntry struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type PrunePlanResponse struct {
	Changes    []Change `json:"changes"`
	Applicable bool     `json:"applicable"`
}

type PruneResponse struct {
	Changes []Change `json:"changes"`
	Pruned  bool     `json:"pruned"`
}

type ActorReference struct {
	Kind        string  `json:"kind"`
	ID          string  `json:"id"`
	Email       *string `json:"email,omitempty"`
	DisplayName *string `json:"displayName,omitempty"`
}

type SessionResponse struct {
	APIVersion    string         `json:"apiVersion"`
	Identity      ActorReference `json:"identity"`
	Personal      *PersonalScope `json:"personal,omitempty"`
	Organizations []Organization `json:"organizations,omitempty"`
	CSRFToken     string         `json:"csrfToken,omitempty"`
}

type PersonalScope struct {
	Organization string `json:"organization"`
	Space        string `json:"space"`
}

type Organization struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	DisplayName *string `json:"displayName"`
	CreatedAt   string  `json:"createdAt"`
	UpdatedAt   string  `json:"updatedAt"`
}

type Space struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	DisplayName     *string           `json:"displayName"`
	Summary         *string           `json:"summary"`
	Description     *string           `json:"description"`
	Labels          map[string]string `json:"labels"`
	Annotations     map[string]string `json:"annotations"`
	SourceAuthority string            `json:"sourceAuthority,omitempty"`
	CreatedAt       string            `json:"createdAt"`
	UpdatedAt       string            `json:"updatedAt"`
}

type DeviceLoginStart struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type DeviceLoginPoll struct {
	Status   string         `json:"status"`
	Token    string         `json:"token,omitempty"`
	Identity ActorReference `json:"identity"`
	Personal *PersonalScope `json:"personal,omitempty"`
	Error    string         `json:"error,omitempty"`
}

func (c *Client) StartDeviceLogin(ctx context.Context) (*DeviceLoginStart, error) {
	var result DeviceLoginStart
	if err := c.postJSON(ctx, "/api/v1/cli/auth/start", map[string]any{}, http.StatusOK, &result); err != nil {
		return nil, err
	}
	if result.DeviceCode == "" || result.UserCode == "" || result.VerificationURI == "" {
		return nil, errors.New("Cloud returned an incomplete device login challenge")
	}
	if result.Interval <= 0 {
		result.Interval = 2
	}
	if result.ExpiresIn <= 0 {
		result.ExpiresIn = 600
	}
	return &result, nil
}

func (c *Client) PollDeviceLogin(ctx context.Context, deviceCode string) (*DeviceLoginPoll, error) {
	var result DeviceLoginPoll
	status, err := c.postJSONStatus(ctx, "/api/v1/cli/auth/poll", map[string]any{"device_code": deviceCode}, &result)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		if result.Token == "" || result.Identity.ID == "" {
			return nil, errors.New("Cloud returned an incomplete device login approval")
		}
		result.Status = "approved"
		return &result, nil
	case http.StatusAccepted:
		result.Status = "authorization_pending"
		return &result, nil
	default:
		code := result.Error
		if code == "" {
			code = result.Status
		}
		if code == "" {
			code = "device_login_failed"
		}
		return nil, HTTPError{Operation: "device login poll", StatusCode: status, Code: code}
	}
}

func (c *Client) postJSON(ctx context.Context, endpoint string, body any, wantStatus int, output any) error {
	status, err := c.postJSONStatus(ctx, endpoint, body, output)
	if err != nil {
		return err
	}
	if status != wantStatus {
		return HTTPError{Operation: "POST " + endpoint, StatusCode: status}
	}
	return nil
}

func (c *Client) postJSONStatus(ctx context.Context, endpoint string, body any, output any) (int, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.resolve(endpoint), bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "doppels-cli/v1alpha1")
	response, err := c.http.Do(request)
	if err != nil {
		return 0, fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer response.Body.Close()
	if output != nil {
		if err := decodeJSONLoose(response.Body, output); err != nil {
			// Non-JSON error bodies are common on failure; keep the status code.
			if response.StatusCode == http.StatusOK || response.StatusCode == http.StatusAccepted {
				return response.StatusCode, fmt.Errorf("decode %s response: %w", endpoint, err)
			}
		}
	}
	return response.StatusCode, nil
}

func (c *Client) Organizations(ctx context.Context, token string) ([]Organization, error) {
	var response struct {
		Organizations []Organization `json:"organizations"`
	}
	if err := c.get(ctx, token, "/api/v1/organizations", &response); err != nil {
		return nil, err
	}
	if response.Organizations == nil {
		response.Organizations = []Organization{}
	}
	return response.Organizations, nil
}

func (c *Client) Spaces(ctx context.Context, token, organization string) ([]Space, error) {
	if organization == "" {
		return nil, errors.New("Organization is required")
	}
	var response struct {
		Spaces []Space `json:"spaces"`
	}
	endpoint := "/api/v1/organizations/" + url.PathEscape(organization) + "/spaces"
	if err := c.get(ctx, token, endpoint, &response); err != nil {
		return nil, err
	}
	if response.Spaces == nil {
		response.Spaces = []Space{}
	}
	return response.Spaces, nil
}

// SpaceRun is a control-plane Run summary for a Space-scoped Request.
type SpaceRun struct {
	ID         string `json:"id"`
	RequestID  string `json:"requestId"`
	CreatedAt  string `json:"createdAt"`
	Status     string `json:"status"`
	Source     string `json:"source"`
	Capability string `json:"capability"`
	Recipe     string `json:"recipe,omitempty"`
}

func (c *Client) ListRuns(ctx context.Context, token, organization, space string) ([]SpaceRun, error) {
	if organization == "" || space == "" {
		return nil, errors.New("Organization and Space are required")
	}
	endpoint := "/api/v1/organizations/" + url.PathEscape(organization) + "/spaces/" + url.PathEscape(space) + "/runs"
	var response struct {
		Runs []spaceRunPayload `json:"runs"`
	}
	if err := c.getLoose(ctx, token, endpoint, &response); err != nil {
		return nil, err
	}
	result := make([]SpaceRun, 0, len(response.Runs))
	for _, item := range response.Runs {
		source := item.Source
		if source == "" {
			source = "cloud"
		}
		result = append(result, SpaceRun{
			ID: item.ID, RequestID: item.RequestID, CreatedAt: item.CreatedAt,
			Status: item.Status, Source: source,
			Capability: definitionLabel(item.Capability),
			Recipe:     definitionLabel(item.Recipe),
		})
	}
	return result, nil
}

type spaceRunPayload struct {
	ID         string         `json:"id"`
	RequestID  string         `json:"requestId"`
	CreatedAt  string         `json:"createdAt"`
	Status     string         `json:"status"`
	Source     string         `json:"source"`
	Capability map[string]any `json:"capability"`
	Recipe     map[string]any `json:"recipe"`
}

func definitionLabel(value map[string]any) string {
	if value == nil {
		return ""
	}
	name, _ := value["name"].(string)
	version, _ := value["version"].(string)
	if name == "" {
		return ""
	}
	if version == "" {
		return name
	}
	return name + "@" + version
}

func (c *Client) get(ctx context.Context, token, endpoint string, output any) error {
	return c.doGet(ctx, token, endpoint, output, true)
}

func (c *Client) getLoose(ctx context.Context, token, endpoint string, output any) error {
	return c.doGet(ctx, token, endpoint, output, false)
}

func (c *Client) doGet(ctx context.Context, token, endpoint string, output any, strict bool) error {
	if strings.TrimSpace(token) == "" || containsControl(token) {
		return errors.New("a valid login token is required")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.resolve(endpoint), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "doppels-cli/v1alpha1")
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("GET %s: %w", endpoint, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return responseError("GET "+endpoint, response.StatusCode, response.Body)
	}
	decode := decodeJSON
	if !strict {
		decode = decodeJSONLoose
	}
	if err := decode(response.Body, output); err != nil {
		return fmt.Errorf("decode %s response: %w", endpoint, err)
	}
	return nil
}

func (c *Client) Session(ctx context.Context, token string) (*SessionResponse, error) {
	var result SessionResponse
	if err := c.get(ctx, token, "/api/v1/session", &result); err != nil {
		return nil, err
	}
	if result.APIVersion != execution.APIVersion || result.Identity.ID == "" ||
		(result.Identity.Kind != "identity" && result.Identity.Kind != "agent" && result.Identity.Kind != "service") {
		return nil, errors.New("Cloud returned an incomplete session")
	}
	if result.Personal != nil {
		if result.Personal.Organization == "" || result.Personal.Space == "" {
			return nil, errors.New("Cloud returned an incomplete personal scope")
		}
	}
	return &result, nil
}

func (c *Client) Preview(ctx context.Context, token, organization, space string, body ReconcileRequest) (*PreviewResponse, error) {
	var response PreviewResponse
	if err := c.reconcile(ctx, "preview", token, organization, space, body, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

func (c *Client) Apply(ctx context.Context, token, organization, space string, body ReconcileRequest) (*ApplyResponse, error) {
	var response ApplyResponse
	if err := c.reconcile(ctx, "apply", token, organization, space, body, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

// PrunePlan previews unregistering manifest-managed Space registrations
// absent from keep; it never mutates state.
func (c *Client) PrunePlan(ctx context.Context, token, organization, space string, keep []KeepEntry) (*PrunePlanResponse, error) {
	var response PrunePlanResponse
	if err := c.postPrune(ctx, token, organization, space, keep, false, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

// Prune executes the unregister plan previewed by PrunePlan.
func (c *Client) Prune(ctx context.Context, token, organization, space string, keep []KeepEntry) (*PruneResponse, error) {
	var response PruneResponse
	if err := c.postPrune(ctx, token, organization, space, keep, true, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

type pruneRequestBody struct {
	Keep  []KeepEntry `json:"keep"`
	Apply bool        `json:"apply,omitempty"`
}

func (c *Client) postPrune(ctx context.Context, token, organization, space string, keep []KeepEntry, apply bool, output any) error {
	if strings.TrimSpace(token) == "" || containsControl(token) {
		return errors.New("a valid login token is required")
	}
	if organization == "" || space == "" {
		return errors.New("Organization and Space are required")
	}
	if keep == nil {
		keep = []KeepEntry{}
	}
	endpoint := "/api/v1/organizations/" + url.PathEscape(organization) + "/spaces/" + url.PathEscape(space) + "/prune"
	data, err := json.Marshal(pruneRequestBody{Keep: keep, Apply: apply})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.resolve(endpoint), bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "doppels-cli/v1alpha1")
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("prune registry: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return responseError("POST "+endpoint, response.StatusCode, response.Body)
	}
	if err := decodeJSON(response.Body, output); err != nil {
		return fmt.Errorf("decode prune response: %w", err)
	}
	return nil
}

type RegistryDefinition struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	SourceAuthority string `json:"sourceAuthority"`
	Revision        struct {
		ID             string         `json:"id"`
		Version        string         `json:"version"`
		ManifestSHA256 string         `json:"manifestSha256"`
		Manifest       map[string]any `json:"manifest"`
		Schema         struct {
			ID     string `json:"id"`
			SHA256 string `json:"sha256"`
		} `json:"schema"`
	} `json:"revision"`
}

func (c *Client) Capabilities(ctx context.Context, token, organization, space string) ([]RegistryDefinition, error) {
	if organization == "" || space == "" {
		return nil, errors.New("Organization and Space are required")
	}
	endpoint := "/api/v1/organizations/" + url.PathEscape(organization) + "/spaces/" + url.PathEscape(space) + "/capabilities"
	var response struct {
		Capabilities []RegistryDefinition `json:"capabilities"`
	}
	if err := c.getLoose(ctx, token, endpoint, &response); err != nil {
		return nil, err
	}
	if response.Capabilities == nil {
		response.Capabilities = []RegistryDefinition{}
	}
	return response.Capabilities, nil
}

func (c *Client) Recipes(ctx context.Context, token, organization, space string) ([]RegistryDefinition, error) {
	if organization == "" || space == "" {
		return nil, errors.New("Organization and Space are required")
	}
	endpoint := "/api/v1/organizations/" + url.PathEscape(organization) + "/spaces/" + url.PathEscape(space) + "/recipes"
	var response struct {
		Recipes []RegistryDefinition `json:"recipes"`
	}
	if err := c.getLoose(ctx, token, endpoint, &response); err != nil {
		return nil, err
	}
	if response.Recipes == nil {
		response.Recipes = []RegistryDefinition{}
	}
	return response.Recipes, nil
}

type IngestPayload struct {
	Request map[string]any   `json:"request"`
	Run     map[string]any   `json:"run"`
	Events  []map[string]any `json:"events"`
}

func (c *Client) IngestRun(ctx context.Context, token, organization, space string, body IngestPayload) error {
	if organization == "" || space == "" {
		return errors.New("Organization and Space are required")
	}
	endpoint := "/api/v1/organizations/" + url.PathEscape(organization) + "/spaces/" + url.PathEscape(space) + "/runs/ingest"
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.resolve(endpoint), bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "doppels-cli/v1alpha1")
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("ingest run: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return responseError("POST "+endpoint, response.StatusCode, response.Body)
	}
	return nil
}

type HubListing struct {
	Organization      string `json:"organization"`
	CapabilityName    string `json:"capabilityName"`
	CapabilityVersion string `json:"capabilityVersion"`
	CapabilitySummary string `json:"capabilitySummary,omitempty"`
	CapabilitySource  string `json:"capabilitySource"`
	RecipeName        string `json:"recipeName"`
	RecipeVersion     string `json:"recipeVersion"`
	RecipeSource      string `json:"recipeSource"`
	Status            string `json:"status"`
	PublicPath        string `json:"publicPath"`
}

type PublishRequest struct {
	CapabilityName    string `json:"capabilityName"`
	CapabilityVersion string `json:"capabilityVersion"`
	CapabilitySummary string `json:"capabilitySummary,omitempty"`
	CapabilitySource  string `json:"capabilitySource"`
	RecipeName        string `json:"recipeName"`
	RecipeVersion     string `json:"recipeVersion"`
	RecipeSource      string `json:"recipeSource"`
}

func (c *Client) Publish(ctx context.Context, token, organization string, body PublishRequest) (*HubListing, error) {
	if strings.TrimSpace(token) == "" || containsControl(token) {
		return nil, errors.New("a valid login token is required")
	}
	if organization == "" {
		return nil, errors.New("Organization is required")
	}
	endpoint := "/api/v1/organizations/" + url.PathEscape(organization) + "/hub/publications"
	var listing HubListing
	if err := c.postAuthJSON(ctx, token, endpoint, http.StatusCreated, body, &listing); err != nil {
		return nil, err
	}
	return &listing, nil
}

func (c *Client) Unpublish(ctx context.Context, token, organization, capabilityName string) (*HubListing, error) {
	if strings.TrimSpace(token) == "" || containsControl(token) {
		return nil, errors.New("a valid login token is required")
	}
	if organization == "" || capabilityName == "" {
		return nil, errors.New("Organization and Capability name are required")
	}
	endpoint := "/api/v1/organizations/" + url.PathEscape(organization) + "/hub/publications/" + url.PathEscape(capabilityName) + "/unpublish"
	var listing HubListing
	if err := c.postAuthJSON(ctx, token, endpoint, http.StatusOK, map[string]any{}, &listing); err != nil {
		return nil, err
	}
	return &listing, nil
}

func (c *Client) GetHubListing(ctx context.Context, organization, capabilityName string) (*HubListing, error) {
	if organization == "" || capabilityName == "" {
		return nil, errors.New("Organization and Capability name are required")
	}
	endpoint := "/api/v1/hub/" + url.PathEscape(organization) + "/" + url.PathEscape(capabilityName)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.resolve(endpoint), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "doppels-cli/v1alpha1")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", endpoint, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, responseError("GET "+endpoint, response.StatusCode, response.Body)
	}
	var listing HubListing
	if err := decodeJSONLoose(response.Body, &listing); err != nil {
		return nil, fmt.Errorf("decode hub listing: %w", err)
	}
	if listing.CapabilityName == "" || listing.RecipeSource == "" {
		return nil, errors.New("Cloud returned an incomplete hub listing")
	}
	return &listing, nil
}

func (c *Client) postAuthJSON(ctx context.Context, token, endpoint string, wantStatus int, body, output any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.resolve(endpoint), bytes.NewReader(data))
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
	if output == nil {
		return nil
	}
	if err := decodeJSONLoose(response.Body, output); err != nil {
		return fmt.Errorf("decode %s response: %w", endpoint, err)
	}
	return nil
}

func (c *Client) reconcile(ctx context.Context, operation, token, organization, space string, body ReconcileRequest, output any) error {
	if strings.TrimSpace(token) == "" || containsControl(token) {
		return errors.New("a valid login token is required")
	}
	if organization == "" || space == "" {
		return errors.New("Organization and Space are required")
	}
	endpoint := "/api/v1/organizations/" + url.PathEscape(organization) + "/spaces/" + url.PathEscape(space) + "/" + operation
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.resolve(endpoint), bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "doppels-cli/v1alpha1")
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("%s registry: %w", operation, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		data, readErr := readLimited(response.Body)
		if readErr != nil {
			return HTTPError{Operation: operation, StatusCode: response.StatusCode}
		}
		if response.StatusCode == http.StatusConflict {
			var conflict struct {
				Error   string   `json:"error"`
				Applied bool     `json:"applied"`
				Changes []Change `json:"changes"`
			}
			if err := decodeJSON(bytes.NewReader(data), &conflict); err == nil && conflict.Error == "registry_conflict" && !conflict.Applied {
				return RegistryConflictError{Changes: conflict.Changes}
			}
		}
		return HTTPError{Operation: operation, StatusCode: response.StatusCode, Code: errorCode(data)}
	}
	if err := decodeJSON(response.Body, output); err != nil {
		return fmt.Errorf("decode %s response: %w", operation, err)
	}
	return nil
}

type HTTPError struct {
	Operation  string
	StatusCode int
	Code       string
}

type RegistryConflictError struct{ Changes []Change }

func (e RegistryConflictError) Error() string { return "registry contains conflicting changes" }

func (e HTTPError) Error() string {
	message := e.Operation + " registry: Cloud returned HTTP " + strconv.Itoa(e.StatusCode)
	if e.Code != "" {
		message += " (" + e.Code + ")"
	}
	return message
}

func (c *Client) resolve(endpoint string) string {
	ref, err := url.Parse(endpoint)
	if err != nil {
		copy := *c.server
		copy.Path = strings.TrimRight(copy.Path, "/") + endpoint
		return copy.String()
	}
	base := *c.server
	base.Path = strings.TrimRight(base.Path, "/") + ref.EscapedPath()
	base.RawQuery = ref.RawQuery
	base.Fragment = ref.Fragment
	return base.String()
}

func decodeJSON(reader io.Reader, output any) error {
	return decodeJSONMode(reader, output, true)
}

func decodeJSONLoose(reader io.Reader, output any) error {
	return decodeJSONMode(reader, output, false)
}

func decodeJSONMode(reader io.Reader, output any, strict bool) error {
	data, err := readLimited(reader)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("response contains trailing JSON")
	}
	return nil
}

func readLimited(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxResponseBytes {
		return nil, fmt.Errorf("response exceeds %d byte limit", maxResponseBytes)
	}
	return data, nil
}

func responseError(operation string, status int, body io.Reader) error {
	data, err := readLimited(body)
	if err != nil {
		return HTTPError{Operation: operation, StatusCode: status}
	}
	return HTTPError{Operation: operation, StatusCode: status, Code: errorCode(data)}
}

func errorCode(data []byte) string {
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &payload) != nil || containsControl(payload.Error) {
		return ""
	}
	return payload.Error
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func containsControl(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}
