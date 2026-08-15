package shareclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/manifest"
)

const APIVersion = manifest.APIVersion

type CreateShareRequest struct {
	CapabilityRevision    execution.DefinitionReference  `json:"capabilityRevision"`
	Capability            *manifest.Capability           `json:"capability"`
	Recipe                *execution.DefinitionReference `json:"recipe,omitempty"`
	ExpiresAt             time.Time                      `json:"expiresAt"`
	Inputs                map[string]any                 `json:"inputs,omitempty"`
	InputsLocked          bool                           `json:"inputsLocked,omitempty"`
	ArtifactRetentionDays int                            `json:"artifactRetentionDays,omitempty"`
}

type Share struct {
	APIVersion            string                         `json:"apiVersion"`
	Kind                  string                         `json:"kind"`
	ID                    string                         `json:"id"`
	CreatedAt             time.Time                      `json:"createdAt"`
	ExpiresAt             time.Time                      `json:"expiresAt"`
	CapabilityRevision    execution.DefinitionReference  `json:"capabilityRevision"`
	Capability            *manifest.Capability           `json:"capability"`
	Recipe                *execution.DefinitionReference `json:"recipe,omitempty"`
	SharedBy              execution.ActorReference       `json:"sharedBy"`
	RequestLimit          int                            `json:"requestLimit"`
	Inputs                map[string]any                 `json:"inputs,omitempty"`
	InputsLocked          bool                           `json:"inputsLocked,omitempty"`
	ArtifactRetentionDays int                            `json:"artifactRetentionDays,omitempty"`
}

type ShareCreated struct {
	APIVersion  string `json:"apiVersion"`
	Kind        string `json:"kind"`
	Share       Share  `json:"share"`
	PublicURL   string `json:"publicUrl"`
	RunnerToken string `json:"runnerToken"`
}

type PendingState struct {
	Share   Share                    `json:"share"`
	Request *execution.RequestRecord `json:"request"`
	Run     *execution.RunRecord     `json:"run"`
	Events  []execution.RunEvent     `json:"events"`
}

// InboxItem is a Share owned by the authenticated Identity for doppels listen.
type InboxItem struct {
	Share               Share `json:"share"`
	HasRequest          bool  `json:"hasRequest"`
	AwaitingFulfillment bool  `json:"awaitingFulfillment"`
}

type ShareMessage struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	MessageID  string          `json:"messageId"`
	ShareID    string          `json:"shareId"`
	OccurredAt time.Time       `json:"occurredAt"`
	Event      string          `json:"event"`
	Payload    json.RawMessage `json:"payload"`
}

func (message ShareMessage) Request() (*execution.RequestRecord, error) {
	var request execution.RequestRecord
	if err := decodeRawJSON(message.Payload, &request); err != nil {
		return nil, err
	}
	return &request, nil
}

func (message ShareMessage) Run() (*execution.RunRecord, error) {
	var run execution.RunRecord
	if err := decodeRawJSON(message.Payload, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

func (message ShareMessage) RunEvent() (*execution.RunEvent, error) {
	var event execution.RunEvent
	if err := decodeRawJSON(message.Payload, &event); err != nil {
		return nil, err
	}
	return &event, nil
}

func decodeRawJSON(data []byte, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	}
	return errors.New("payload contains trailing JSON")
}

type Update struct {
	Message  *ShareMessage
	Recovery *PendingState
}

type ArtifactUpload struct {
	RunID      string
	ArtifactID string
	Path       string
	Filename   string
	MediaType  string
}

type ArtifactUploadResponse struct {
	Artifact execution.ArtifactReference `json:"artifact"`
}
