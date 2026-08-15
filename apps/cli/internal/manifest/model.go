package manifest

import (
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

const APIVersion = "doppels.so/v1alpha1"

type Document interface {
	Type() TypeMeta
	Meta() Metadata
}

type TypeMeta struct {
	APIVersion string `yaml:"apiVersion" json:"apiVersion"`
	Kind       string `yaml:"kind" json:"kind"`
}

type Metadata struct {
	Name        string            `yaml:"name" json:"name"`
	Version     string            `yaml:"version" json:"version"`
	DisplayName string            `yaml:"displayName,omitempty" json:"displayName,omitempty"`
	Summary     string            `yaml:"summary,omitempty" json:"summary,omitempty"`
	Description string            `yaml:"description,omitempty" json:"description,omitempty"`
	Impact      string            `yaml:"impact,omitempty" json:"impact,omitempty"`
	Tags        []string          `yaml:"tags,omitempty" json:"tags,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty" json:"annotations,omitempty"`
}

// SpaceMetadata deliberately has no Version: a Space is mutable desired
// configuration, unlike immutable Capability and Recipe revisions.
type SpaceMetadata struct {
	Name        string            `yaml:"name" json:"name"`
	DisplayName string            `yaml:"displayName,omitempty" json:"displayName,omitempty"`
	Summary     string            `yaml:"summary,omitempty" json:"summary,omitempty"`
	Description string            `yaml:"description,omitempty" json:"description,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty" json:"annotations,omitempty"`
}

type SpaceDiscovery struct {
	Capabilities []string `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	Recipes      []string `yaml:"recipes,omitempty" json:"recipes,omitempty"`
}

type Space struct {
	TypeMeta  `yaml:",inline"`
	Metadata  SpaceMetadata   `yaml:"metadata" json:"metadata"`
	Discovery *SpaceDiscovery `yaml:"discovery,omitempty" json:"discovery,omitempty"`
}

func (s *Space) Type() TypeMeta { return s.TypeMeta }
func (s *Space) Meta() Metadata {
	return Metadata{
		Name: s.Metadata.Name, DisplayName: s.Metadata.DisplayName,
		Summary: s.Metadata.Summary, Description: s.Metadata.Description,
		Labels: s.Metadata.Labels, Annotations: s.Metadata.Annotations,
	}
}

type Documentation struct {
	Readme string `yaml:"readme" json:"readme"`
}

type Capability struct {
	TypeMeta      `yaml:",inline"`
	Metadata      Metadata                  `yaml:"metadata" json:"metadata"`
	Documentation *Documentation            `yaml:"documentation,omitempty" json:"documentation,omitempty"`
	Inputs        map[string]InputContract  `yaml:"inputs" json:"inputs"`
	Outputs       map[string]OutputContract `yaml:"outputs" json:"outputs"`
}

func (c *Capability) Type() TypeMeta { return c.TypeMeta }
func (c *Capability) Meta() Metadata { return c.Metadata }

type InputContract struct {
	Type        string `yaml:"type" json:"type"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Required    bool   `yaml:"required,omitempty" json:"required,omitempty"`
	Default     any    `yaml:"default,omitempty" json:"default,omitempty"`
	Enum        []any  `yaml:"enum,omitempty" json:"enum,omitempty"`
	DefaultSet  bool   `yaml:"-" json:"-"`
}

func (c *InputContract) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("input contract must be an object")
	}
	allowed := map[string]bool{"type": true, "description": true, "required": true, "default": true, "enum": true}
	for i := 0; i < len(node.Content); i += 2 {
		name := node.Content[i].Value
		if !allowed[name] {
			return fmt.Errorf("field %q not found in input contract", name)
		}
		if name == "default" {
			c.DefaultSet = true
		}
	}
	type raw InputContract
	return node.Decode((*raw)(c))
}

type OutputContract struct {
	Type        string `yaml:"type" json:"type"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	MediaType   string `yaml:"mediaType,omitempty" json:"mediaType,omitempty"`
}

type Recipe struct {
	TypeMeta  `yaml:",inline"`
	Metadata  Metadata                    `yaml:"metadata" json:"metadata"`
	Provides  []string                    `yaml:"provides" json:"provides"`
	Runtime   string                      `yaml:"runtime" json:"runtime"`
	Requires  *Requirements               `yaml:"requires,omitempty" json:"requires,omitempty"`
	Env       map[string]EnvironmentValue `yaml:"env,omitempty" json:"env,omitempty"`
	Defaults  *Defaults                   `yaml:"defaults,omitempty" json:"defaults,omitempty"`
	Steps     []Step                      `yaml:"steps,omitempty" json:"steps,omitempty"`
	Procedure *Procedure                  `yaml:"procedure,omitempty" json:"procedure,omitempty"`
	Evidence  map[string]Evidence         `yaml:"evidence,omitempty" json:"evidence,omitempty"`
	Returns   map[string]string           `yaml:"returns,omitempty" json:"returns,omitempty"`
}

func (r *Recipe) Type() TypeMeta { return r.TypeMeta }
func (r *Recipe) Meta() Metadata { return r.Metadata }

type Requirements struct {
	Commands []CommandRequirement `yaml:"commands,omitempty" json:"commands,omitempty"`
	HostEnv  []string             `yaml:"hostEnv,omitempty" json:"hostEnv,omitempty"`
	Files    []string             `yaml:"files,omitempty" json:"files,omitempty"`
}

type CommandRequirement struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Short   bool   `json:"-"`
}

func (r *CommandRequirement) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag != "!!str" {
			return fmt.Errorf("command requirement must be a string or object")
		}
		r.Name = node.Value
		r.Short = true
		return nil
	case yaml.MappingNode:
		allowed := map[string]bool{"name": true, "version": true}
		for i := 0; i < len(node.Content); i += 2 {
			if !allowed[node.Content[i].Value] {
				return fmt.Errorf("field %q not found in command requirement", node.Content[i].Value)
			}
		}
		var value struct {
			Name    string `yaml:"name"`
			Version string `yaml:"version,omitempty"`
		}
		if err := node.Decode(&value); err != nil {
			return err
		}
		r.Name, r.Version = value.Name, value.Version
		return nil
	default:
		return fmt.Errorf("command requirement must be a string or object")
	}
}

func (r CommandRequirement) MarshalJSON() ([]byte, error) {
	if r.Short && r.Version == "" {
		return []byte(fmt.Sprintf("%q", r.Name)), nil
	}
	type object struct {
		Name    string `json:"name"`
		Version string `json:"version,omitempty"`
	}
	return json.Marshal(object{Name: r.Name, Version: r.Version})
}

type EnvironmentValue struct {
	Literal *string     `json:"-"`
	HostEnv *HostEnvRef `json:"-"`
}

type HostEnvRef struct {
	From string `yaml:"from" json:"from"`
	Name string `yaml:"name" json:"name"`
}

func (v *EnvironmentValue) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag != "!!str" {
			return fmt.Errorf("environment value must be a string or host_env reference")
		}
		value := node.Value
		v.Literal = &value
		return nil
	case yaml.MappingNode:
		allowed := map[string]bool{"from": true, "name": true}
		for i := 0; i < len(node.Content); i += 2 {
			if !allowed[node.Content[i].Value] {
				return fmt.Errorf("field %q not found in environment value", node.Content[i].Value)
			}
		}
		var ref HostEnvRef
		if err := node.Decode(&ref); err != nil {
			return err
		}
		v.HostEnv = &ref
		return nil
	default:
		return fmt.Errorf("environment value must be a string or host_env reference")
	}
}

func (v EnvironmentValue) MarshalJSON() ([]byte, error) {
	if v.Literal != nil {
		return json.Marshal(*v.Literal)
	}
	return json.Marshal(v.HostEnv)
}

type Defaults struct {
	Timeout               string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	Approval              string `yaml:"approval,omitempty" json:"approval,omitempty"`
	WorkingDirectory      string `yaml:"workingDirectory,omitempty" json:"workingDirectory,omitempty"`
	ArtifactRetentionDays *int   `yaml:"artifactRetentionDays,omitempty" json:"artifactRetentionDays,omitempty"`
}

// ArtifactRetentionDaysOrDefault returns Recipe defaults.artifactRetentionDays
// or 7 when unset. Values must be one of 1, 7, 14, 30 (enforced by schema).
func (r *Recipe) ArtifactRetentionDaysOrDefault() int {
	if r != nil && r.Defaults != nil && r.Defaults.ArtifactRetentionDays != nil {
		return *r.Defaults.ArtifactRetentionDays
	}
	return 7
}

type Procedure struct {
	Readme string `yaml:"readme" json:"readme"`
}

type Evidence struct {
	Type        string `yaml:"type" json:"type"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

type Step struct {
	ID               string                      `yaml:"id" json:"id"`
	Name             string                      `yaml:"name" json:"name"`
	Approval         string                      `yaml:"approval,omitempty" json:"approval,omitempty"`
	WorkingDirectory string                      `yaml:"workingDirectory,omitempty" json:"workingDirectory,omitempty"`
	Env              map[string]EnvironmentValue `yaml:"env,omitempty" json:"env,omitempty"`
	Run              *Run                        `yaml:"run" json:"run"`
	Produces         map[string]Product          `yaml:"produces,omitempty" json:"produces,omitempty"`
}

type Run struct {
	Shell   string `yaml:"shell" json:"shell"`
	Script  string `yaml:"script" json:"script"`
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

type Product struct {
	File string `yaml:"file,omitempty" json:"file,omitempty"`
	Env  string `yaml:"env,omitempty" json:"env,omitempty"`
}
