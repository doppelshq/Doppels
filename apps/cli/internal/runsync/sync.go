// Package runsync drains the local Run outbox into the control plane.
package runsync

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/registryclient"
	"doppels.so/cli/internal/runindex"
	"doppels.so/cli/internal/runstate"
	"doppels.so/cli/internal/shareclient"
)

type Client interface {
	IngestRun(ctx context.Context, token, organization, space string, body registryclient.IngestPayload) error
}

// Flush pushes pending local Runs to the Space selected by Context.
// Failures bump attempts and leave rows pending (Offline-First).
func Flush(ctx context.Context, root string, client Client, token, organization, space string) (int, error) {
	if organization == "" || space == "" {
		return 0, nil
	}
	idx, err := runindex.Open(root)
	if err != nil {
		return 0, err
	}
	defer idx.Close()
	items, err := idx.ListOutbox()
	if err != nil {
		return 0, err
	}
	synced := 0
	for _, item := range items {
		payload, err := buildPayload(root, item.RunID, organization, space)
		if err != nil {
			_ = idx.BumpOutboxAttempt(item.ID)
			continue
		}
		if err := client.IngestRun(ctx, token, organization, space, payload); err != nil {
			_ = idx.BumpOutboxAttempt(item.ID)
			continue
		}
		if err := idx.AckOutbox(item.ID, item.RunID); err != nil {
			return synced, err
		}
		synced++
	}
	return synced, nil
}

func buildPayload(root, runID, organization, space string) (registryclient.IngestPayload, error) {
	detail, err := runstate.Load(root, runID)
	if err != nil {
		return registryclient.IngestPayload{}, err
	}
	request := requestWire(detail.Request, organization, space)
	run := runWire(detail.Run)
	events := make([]map[string]any, 0, len(detail.Events))
	for _, event := range detail.Events {
		sanitized := shareclient.SanitizeRunEvent(event)
		events = append(events, eventWire(sanitized))
	}
	if len(events) == 0 {
		return registryclient.IngestPayload{}, fmt.Errorf("run %s has no events", runID)
	}
	return registryclient.IngestPayload{Request: request, Run: run, Events: events}, nil
}

func requestWire(request execution.RequestRecord, organization, space string) map[string]any {
	wire := map[string]any{
		"apiVersion":     request.APIVersion,
		"kind":           request.Kind,
		"id":             request.ID,
		"idempotencyKey": request.IdempotencyKey,
		"origin":         "cli",
		"capability":     definitionWire(request.Capability),
		"inputs":         request.Inputs,
		"requestedBy":    map[string]any{"kind": request.RequestedBy.Kind, "id": request.RequestedBy.ID},
		"organization":   organization,
		"space":          space,
	}
	if request.Origin == "api" {
		wire["origin"] = "api"
	}
	return wire
}

func runWire(run execution.RunRecord) map[string]any {
	wire := map[string]any{
		"apiVersion":          run.APIVersion,
		"kind":                run.Kind,
		"id":                  run.ID,
		"requestId":           run.RequestID,
		"createdAt":           run.CreatedAt.UTC().Format(time.RFC3339Nano),
		"capability":          definitionWire(run.Capability),
		"inputs":              run.Inputs,
		"executor":            map[string]any{"kind": run.Executor.Kind, "id": run.Executor.ID},
		"submissionMessageId": newMessageID(),
	}
	if run.Recipe != nil {
		wire["recipe"] = definitionWire(*run.Recipe)
	}
	if run.NodeID != "" {
		wire["nodeId"] = run.NodeID
	}
	return wire
}

func eventWire(event execution.RunEvent) map[string]any {
	wire := map[string]any{
		"apiVersion":          event.APIVersion,
		"kind":                event.Kind,
		"runId":               event.RunID,
		"sequence":            event.Sequence,
		"occurredAt":          event.OccurredAt.UTC().Format(time.RFC3339Nano),
		"type":                event.Type,
		"submissionMessageId": newMessageID(),
	}
	if event.StepID != "" {
		wire["stepId"] = event.StepID
	}
	if len(event.Data) > 0 {
		wire["data"] = event.Data
	} else {
		wire["data"] = map[string]any{}
	}
	return wire
}

func definitionWire(ref execution.DefinitionReference) map[string]any {
	return map[string]any{
		"name": ref.Name, "version": ref.Version, "manifestSha256": ref.ManifestSHA256,
		"schema": map[string]any{"id": ref.Schema.ID, "sha256": ref.Schema.SHA256},
	}
}

func newMessageID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("00000000-0000-4000-8000-%012x", time.Now().UnixNano())
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	hex := fmt.Sprintf("%x", value)
	return strings.Join([]string{hex[0:8], hex[8:12], hex[12:16], hex[16:20], hex[20:32]}, "-")
}
