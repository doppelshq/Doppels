package registryclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPlanSendsScopedAuthenticatedRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v1/organizations/acme/spaces/platform/plan" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		var body ReconcileRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Resources) != 1 || body.Resources[0].SourceAuthority != "manifest" {
			t.Fatalf("body = %#v", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"changes":[{"action":"create","kind":"Capability","name":"greet","version":"1.0.0"}],"applicable":true}`))
	}))
	defer server.Close()
	client, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Plan(context.Background(), "secret", "acme", "platform", ReconcileRequest{Resources: []Resource{{Kind: "Capability", SourceAuthority: "manifest"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !response.Applicable || len(response.Changes) != 1 {
		t.Fatalf("response = %#v", response)
	}
}

func TestApplyReturnsStructuredRegistryConflict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusConflict)
		_, _ = writer.Write([]byte(`{"error":"registry_conflict","applied":false,"changes":[{"action":"conflict","kind":"Capability","name":"greet","version":"1.0.0","reason":"immutable_revision_mismatch"}]}`))
	}))
	defer server.Close()
	client, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Apply(context.Background(), "secret", "acme", "platform", ReconcileRequest{Resources: []Resource{}})
	var conflict RegistryConflictError
	if !errors.As(err, &conflict) || len(conflict.Changes) != 1 || conflict.Changes[0].Reason != "immutable_revision_mismatch" {
		t.Fatalf("error = %#v", err)
	}
}

func TestPrunePlanAndPruneSendKeepAndApplyFlag(t *testing.T) {
	var applyFlags []bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v1/organizations/acme/spaces/platform/prune" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		var body struct {
			Keep  []KeepEntry `json:"keep"`
			Apply bool        `json:"apply"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Keep) != 1 || body.Keep[0] != (KeepEntry{Kind: "Capability", Name: "greet"}) {
			t.Fatalf("keep = %#v", body.Keep)
		}
		applyFlags = append(applyFlags, body.Apply)
		writer.Header().Set("Content-Type", "application/json")
		if body.Apply {
			_, _ = writer.Write([]byte(`{"changes":[{"action":"unregister","kind":"Recipe","name":"stale"}],"pruned":true}`))
		} else {
			_, _ = writer.Write([]byte(`{"changes":[{"action":"unregister","kind":"Recipe","name":"stale"}],"applicable":true}`))
		}
	}))
	defer server.Close()
	client, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	keep := []KeepEntry{{Kind: "Capability", Name: "greet"}}

	plan, err := client.PrunePlan(context.Background(), "secret", "acme", "platform", keep)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Applicable || len(plan.Changes) != 1 {
		t.Fatalf("plan = %#v", plan)
	}

	pruned, err := client.Prune(context.Background(), "secret", "acme", "platform", keep)
	if err != nil {
		t.Fatal(err)
	}
	if !pruned.Pruned || len(pruned.Changes) != 1 {
		t.Fatalf("pruned = %#v", pruned)
	}

	if len(applyFlags) != 2 || applyFlags[0] != false || applyFlags[1] != true {
		t.Fatalf("apply flags = %#v", applyFlags)
	}
}

func TestServerRejectsInsecureRemoteHTTP(t *testing.T) {
	if _, err := New("http://example.com", nil); err == nil {
		t.Fatal("expected insecure URL rejection")
	}
}

func TestHTTPErrorIncludesCloudErrorCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"error":"organization_not_found"}`))
	}))
	defer server.Close()
	client, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Spaces(context.Background(), "secret", "missing")
	var httpErr HTTPError
	if !errors.As(err, &httpErr) || httpErr.Code != "organization_not_found" {
		t.Fatalf("error = %#v", err)
	}
}

func TestListRunsDecodesSpaceSummaries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/api/v1/organizations/acme/spaces/platform/runs" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"runs":[{"apiVersion":"doppels.so/v1alpha1","kind":"Run","id":"run-1","requestId":"req-1","createdAt":"2026-08-02T10:00:00Z","capability":{"name":"greet","version":"1.0.0"},"inputs":{},"executor":{"kind":"identity","id":"yuri"},"status":"succeeded","source":"cloud"}]}`))
	}))
	defer server.Close()
	client, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	runs, err := client.ListRuns(context.Background(), "secret", "acme", "platform")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != "run-1" || runs[0].Capability != "greet@1.0.0" || runs[0].Source != "cloud" {
		t.Fatalf("runs = %#v", runs)
	}
}

func TestResolveKeepsQueryString(t *testing.T) {
	client, err := New("http://127.0.0.1:4000", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := client.resolve("/api/v1/listen/inbox?organization=acme&space=platform")
	want := "http://127.0.0.1:4000/api/v1/listen/inbox?organization=acme&space=platform"
	if got != want {
		t.Fatalf("resolve = %q, want %q", got, want)
	}
}

func TestListenInboxSendsOrganizationQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/api/v1/listen/inbox" {
			t.Fatalf("request = %s %s", request.Method, request.URL.RequestURI())
		}
		if request.URL.Query().Get("organization") != "acme" {
			t.Fatalf("query = %q", request.URL.RawQuery)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"scopes":[{"organization":"acme","space":"platform","capabilities":[]}],"shares":[],"requests":[]}`))
	}))
	defer server.Close()
	client, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := client.ListenInbox(context.Background(), "secret", ListenFilters{Organization: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox.Scopes) != 1 || inbox.Scopes[0].Organization != "acme" {
		t.Fatalf("inbox = %#v", inbox)
	}
}
