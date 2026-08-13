package shareclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"doppels.so/cli/internal/execution"
)

const MaxArtifactBytes int64 = 25 * 1024 * 1024
const maxResponseBytes int64 = 4 << 20

type Client struct {
	server            *url.URL
	httpClient        *http.Client
	dial              DialFunc
	heartbeatInterval time.Duration
	reconnectMin      time.Duration
	reconnectMax      time.Duration
	now               func() time.Time
}

type Options struct {
	Server            string
	HTTPClient        *http.Client
	Dial              DialFunc
	HeartbeatInterval time.Duration
	ReconnectMin      time.Duration
	ReconnectMax      time.Duration
	Now               func() time.Time
}

func New(options Options) (*Client, error) {
	server := strings.TrimRight(options.Server, "/")
	if server == "" {
		server = "https://doppels.so"
	}
	parsed, err := url.Parse(server)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid Doppels server URL %q", server)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("Doppels server URL cannot contain credentials, a query, or a fragment")
	}
	if parsed.Scheme == "http" && !loopbackHost(parsed.Hostname()) {
		return nil, errors.New("remote Doppels server URLs must use https")
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	dial := options.Dial
	if dial == nil {
		dial = coderDial(httpClient)
	}
	heartbeat := options.HeartbeatInterval
	if heartbeat == 0 {
		heartbeat = 30 * time.Second
	}
	reconnectMin := options.ReconnectMin
	if reconnectMin == 0 {
		reconnectMin = 250 * time.Millisecond
	}
	reconnectMax := options.ReconnectMax
	if reconnectMax == 0 {
		reconnectMax = 5 * time.Second
	}
	if heartbeat < 0 || reconnectMin < 0 || reconnectMax < reconnectMin {
		return nil, errors.New("invalid share client timing options")
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Client{server: parsed, httpClient: httpClient, dial: dial, heartbeatInterval: heartbeat, reconnectMin: reconnectMin, reconnectMax: reconnectMax, now: now}, nil
}

func (client *Client) Create(ctx context.Context, apiToken string, request CreateShareRequest) (*ShareCreated, error) {
	apiToken = strings.TrimSpace(apiToken)
	if apiToken != "" && containsControl(apiToken) {
		return nil, errors.New("DOPPELS_API_TOKEN contains invalid control characters")
	}
	if request.Capability == nil {
		return nil, errors.New("Capability snapshot is required")
	}
	if !request.ExpiresAt.After(client.now()) {
		return nil, errors.New("Share expiry must be in the future")
	}
	var response ShareCreated
	if err := client.doJSON(ctx, http.MethodPost, "/api/v1/shares", apiToken, request, http.StatusCreated, &response); err != nil {
		return nil, err
	}
	if response.Kind != "ShareCreated" || !validUUID(response.Share.ID) || len(response.RunnerToken) < 32 || containsControl(response.RunnerToken) || response.PublicURL == "" {
		return nil, errors.New("Cloud returned an incomplete ShareCreated response")
	}
	if response.APIVersion != APIVersion || response.Share.APIVersion != APIVersion || response.Share.Kind != "Share" || response.Share.RequestLimit != 1 {
		return nil, errors.New("Cloud returned an invalid Share contract")
	}
	if !validShareOwner(response.Share.SharedBy) {
		return nil, errors.New("Cloud returned an invalid Share owner")
	}
	if response.Share.CapabilityRevision != request.CapabilityRevision || !jsonValuesEqual(response.Share.Capability, request.Capability) || !definitionReferencesEqual(response.Share.Recipe, request.Recipe) {
		return nil, errors.New("Cloud changed the shared Capability or definition references")
	}
	if !response.Share.ExpiresAt.Equal(request.ExpiresAt) || !response.Share.ExpiresAt.After(response.Share.CreatedAt) {
		return nil, errors.New("Cloud returned an invalid Share lifetime")
	}
	publicURL, err := url.Parse(response.PublicURL)
	if err != nil || publicURL.Host == "" || (publicURL.Scheme != "http" && publicURL.Scheme != "https") {
		return nil, errors.New("Cloud returned an invalid public Share URL")
	}
	if publicURL.User != nil || publicURL.Fragment != "" || publicURL.RawQuery != "" ||
		(publicURL.Scheme == "http" && !loopbackHost(publicURL.Hostname())) {
		return nil, errors.New("Cloud returned an insecure public Share URL")
	}
	return &response, nil
}

func (client *Client) Inbox(ctx context.Context, apiToken string) ([]InboxItem, error) {
	if strings.TrimSpace(apiToken) == "" {
		return nil, errors.New("API token is required")
	}
	if containsControl(apiToken) {
		return nil, errors.New("API token contains invalid control characters")
	}
	var response struct {
		Shares []InboxItem `json:"shares"`
	}
	if err := client.doJSON(ctx, http.MethodGet, "/api/v1/shares/inbox", apiToken, nil, http.StatusOK, &response); err != nil {
		return nil, err
	}
	return response.Shares, nil
}

func (client *Client) Pending(ctx context.Context, shareID, runnerToken string) (*PendingState, error) {
	if runnerToken == "" || containsControl(runnerToken) {
		return nil, errors.New("valid runner token is required")
	}
	var response PendingState
	endpoint := "/api/v1/shares/" + url.PathEscape(shareID) + "/pending"
	if err := client.doJSON(ctx, http.MethodGet, endpoint, runnerToken, nil, http.StatusOK, &response); err != nil {
		return nil, err
	}
	if response.Share.ID != shareID {
		return nil, errors.New("Cloud returned pending work for another Share")
	}
	return &response, nil
}

func (client *Client) UploadArtifact(ctx context.Context, shareID, runnerToken string, upload ArtifactUpload) (*execution.ArtifactReference, error) {
	if runnerToken == "" || containsControl(runnerToken) {
		return nil, errors.New("valid runner token is required")
	}
	if shareID == "" || upload.RunID == "" || upload.ArtifactID == "" {
		return nil, errors.New("share, run, and artifact ids are required")
	}
	file, err := os.Open(upload.Path)
	if err != nil {
		return nil, fmt.Errorf("open artifact: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect artifact: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("artifact must be a regular file")
	}
	if info.Size() > MaxArtifactBytes {
		return nil, fmt.Errorf("artifact exceeds %d byte limit", MaxArtifactBytes)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return nil, fmt.Errorf("hash artifact: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind artifact: %w", err)
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	filename := upload.Filename
	if filename == "" {
		filename = path.Base(filepathToSlash(upload.Path))
	}
	if err := validateArtifactFilename(filename); err != nil {
		return nil, err
	}
	mediaType := upload.MediaType
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	if _, _, err := mime.ParseMediaType(mediaType); err != nil || containsControl(mediaType) {
		return nil, fmt.Errorf("invalid artifact media type %q", mediaType)
	}

	endpoint := "/api/v1/shares/" + url.PathEscape(shareID) + "/runs/" + url.PathEscape(upload.RunID) + "/artifacts/" + url.PathEscape(upload.ArtifactID)
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, client.resolve(endpoint), file)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", bearer(runnerToken))
	request.Header.Set("Content-Type", mediaType)
	request.Header.Set("X-Doppel-Filename", filename)
	request.Header.Set("X-Doppel-Sha256", digest)
	request.Header.Set("User-Agent", "doppels-cli/v1alpha1")
	request.ContentLength = info.Size()
	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("upload artifact: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return nil, statusError("upload artifact", response.StatusCode)
	}
	var decoded ArtifactUploadResponse
	if err := decodeJSON(response.Body, &decoded); err != nil {
		return nil, fmt.Errorf("decode artifact response: %w", err)
	}
	if decoded.Artifact.ID != upload.ArtifactID || decoded.Artifact.SHA256 != digest || decoded.Artifact.SizeBytes != info.Size() || decoded.Artifact.Filename != filename || decoded.Artifact.MediaType != mediaType {
		return nil, errors.New("Cloud returned artifact metadata that does not match the uploaded file")
	}
	return &decoded.Artifact, nil
}

func (client *Client) doJSON(ctx context.Context, method, endpoint, token string, body any, expected int, output any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, client.resolve(endpoint), reader)
	if err != nil {
		return err
	}
	if strings.TrimSpace(token) != "" {
		request.Header.Set("Authorization", bearer(token))
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "doppels-cli/v1alpha1")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, endpoint, err)
	}
	defer response.Body.Close()
	if response.StatusCode != expected {
		return statusError(method+" "+endpoint, response.StatusCode)
	}
	if output == nil {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}
	if err := decodeJSON(response.Body, output); err != nil {
		return fmt.Errorf("decode %s response: %w", endpoint, err)
	}
	return nil
}

func decodeJSON(reader io.Reader, output any) error {
	data, err := io.ReadAll(io.LimitReader(reader, maxResponseBytes+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > maxResponseBytes {
		return fmt.Errorf("response exceeds %d byte limit", maxResponseBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("response contains trailing JSON")
	}
	return nil
}

func statusError(operation string, status int) error {
	return HTTPError{Operation: operation, StatusCode: status}
}

type HTTPError struct {
	Operation  string
	StatusCode int
}

func (problem HTTPError) Error() string {
	return fmt.Sprintf("%s: Cloud returned HTTP %s", problem.Operation, strconv.Itoa(problem.StatusCode))
}

func (problem HTTPError) Permanent() bool {
	return problem.StatusCode >= 400 && problem.StatusCode < 500 && problem.StatusCode != http.StatusRequestTimeout && problem.StatusCode != http.StatusTooManyRequests
}

func bearer(token string) string { return "Bearer " + token }

func validShareOwner(owner execution.ActorReference) bool {
	if owner.ID == "" {
		return false
	}
	switch owner.Kind {
	case "identity", "agent", "service", "anonymous":
		return true
	default:
		return false
	}
}

func (client *Client) resolve(endpoint string) string {
	copy := *client.server
	copy.Path = strings.TrimRight(copy.Path, "/") + endpoint
	return copy.String()
}

func filepathToSlash(value string) string { return strings.ReplaceAll(value, "\\", "/") }

func validateArtifactFilename(filename string) error {
	if filename == "" || len(filename) > 255 || filename == "." || filename == ".." || strings.ContainsAny(filename, "/\\") || containsControl(filename) {
		return fmt.Errorf("invalid artifact filename %q", filename)
	}
	return nil
}

func containsControl(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

func definitionReferencesEqual(left, right *execution.DefinitionReference) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
