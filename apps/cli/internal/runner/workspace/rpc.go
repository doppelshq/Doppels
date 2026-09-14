package workspace

import (
	"encoding/json"
	"errors"
	"sort"

	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runner/server"
)

// RPCServer is the subset of server.Server used to expose workspace methods.
// Keeping it narrow makes persistence-before-notification independently
// testable without opening a socket.
type RPCServer interface {
	Handle(method string, handler server.Handler)
	EmitNodeEvent(event proto.NodeEvent)
}

// RegisterRPC attaches the PR5 method surface to the Runner server.
func RegisterRPC(target RPCServer, service *Service) {
	target.Handle("v1/listWorkspaces", func(params []byte) (any, *proto.Error) {
		if err := decodeRPCParams(params, &struct{}{}); err != nil {
			return nil, err
		}
		return service.ListWorkspaces(), nil
	})
	target.Handle("v1/addWorkspace", func(params []byte) (any, *proto.Error) {
		var request struct {
			Root string `json:"root"`
		}
		if err := decodeRPCParams(params, &request); err != nil {
			return nil, err
		}
		if request.Root == "" {
			return nil, invalidParams("root is required")
		}
		summary, added, err := service.AddWorkspace(request.Root)
		if err != nil {
			return nil, domainError(err)
		}
		if added {
			// Registry.Add synchronously commits before returning, so observers
			// can always reload the root by the time they receive this event.
			target.EmitNodeEvent(proto.NodeEvent{Kind: proto.NodeEventWorkspaceAdded, Payload: summary})
		}
		return summary, nil
	})
	target.Handle("v1/removeWorkspace", func(params []byte) (any, *proto.Error) {
		var request struct {
			Root string `json:"root"`
		}
		if err := decodeRPCParams(params, &request); err != nil {
			return nil, err
		}
		if request.Root == "" {
			return nil, invalidParams("root is required")
		}
		root, err := service.RemoveWorkspace(request.Root)
		if err != nil {
			return nil, domainError(err)
		}
		// Registry.Remove has already atomically persisted the new set and
		// never deletes workspace files.
		target.EmitNodeEvent(proto.NodeEvent{
			Kind:    proto.NodeEventWorkspaceRemoved,
			Payload: map[string]string{"root": root},
		})
		return map[string]any{}, nil
	})
	target.Handle("v1/listCapabilities", func(params []byte) (any, *proto.Error) {
		var request struct {
			Workspace string `json:"workspace"`
		}
		if err := decodeRPCParams(params, &request); err != nil {
			return nil, err
		}
		result, err := service.ListCapabilities(request.Workspace)
		if err != nil {
			return nil, domainError(err)
		}
		return result, nil
	})
	target.Handle("v1/getCapability", func(params []byte) (any, *proto.Error) {
		var request struct {
			Workspace string `json:"workspace"`
			Name      string `json:"name"`
			Version   string `json:"version"`
		}
		if err := decodeRPCParams(params, &request); err != nil {
			return nil, err
		}
		if request.Workspace == "" {
			return nil, invalidParams("workspace is required")
		}
		if request.Name == "" {
			return nil, invalidParams("name is required")
		}
		result, err := service.GetCapability(request.Workspace, request.Name, request.Version)
		if err == nil {
			return result, nil
		}
		if errors.Is(err, ErrCapabilityAmbiguous) {
			versions := capabilityVersions(service, request.Workspace, request.Name)
			return nil, &proto.Error{
				Code:    proto.CodeCapabilityNotFound,
				Message: "capability version is ambiguous; include version",
				Data:    map[string]any{"versions": versions},
			}
		}
		return nil, domainError(err)
	})
}

func decodeRPCParams(params []byte, out any) *proto.Error {
	if len(params) == 0 {
		return nil
	}
	if err := json.Unmarshal(params, out); err != nil {
		return invalidParams(err.Error())
	}
	return nil
}

func invalidParams(message string) *proto.Error {
	return &proto.Error{Code: proto.CodeInvalidParams, Message: message}
}

func domainError(err error) *proto.Error {
	switch {
	case errors.Is(err, ErrWorkspaceNotFound):
		return &proto.Error{Code: proto.CodeWorkspaceNotFound, Message: err.Error()}
	case errors.Is(err, ErrCapabilityNotFound), errors.Is(err, ErrCapabilityAmbiguous):
		return &proto.Error{Code: proto.CodeCapabilityNotFound, Message: err.Error()}
	default:
		return &proto.Error{Code: proto.CodeInternal, Message: err.Error()}
	}
}

func capabilityVersions(service *Service, workspace, name string) []string {
	capabilities, err := service.ListCapabilities(workspace)
	if err != nil {
		return []string{}
	}
	versions := make([]string, 0)
	for _, capability := range capabilities {
		if capability.Name == name {
			versions = append(versions, capability.Version)
		}
	}
	sort.Strings(versions)
	return versions
}
