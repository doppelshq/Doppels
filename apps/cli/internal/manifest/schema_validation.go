package manifest

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

const (
	CommonSchemaID = "https://doppels.so/schemas/v1alpha1/common.schema.json"
	SpaceSchemaID  = "https://doppels.so/schemas/v1alpha1/space.schema.json"
)

var embeddedSchemaFiles = map[string]string{
	"common.schema.json":     CommonSchemaID,
	"space.schema.json":      SpaceSchemaID,
	"capability.schema.json": CapabilitySchemaID,
	"recipe.schema.json":     RecipeSchemaID,
}

//go:embed schemas/*.schema.json
var embeddedSchemaFS embed.FS

var (
	resourcesOnce sync.Once
	resources     map[string]*schemaResource
	resourcesErr  error
	compiledOnce  sync.Once
	compiled      map[string]*jsonschema.Schema
	compiledErr   error
)

func embeddedSchemaResources() (map[string]*schemaResource, error) {
	resourcesOnce.Do(func() {
		resources = make(map[string]*schemaResource, len(embeddedSchemaFiles))
		filenames := make([]string, 0, len(embeddedSchemaFiles))
		for filename := range embeddedSchemaFiles {
			filenames = append(filenames, filename)
		}
		sort.Strings(filenames)
		for _, filename := range filenames {
			source, err := embeddedSchemaFS.ReadFile("schemas/" + filename)
			if err != nil {
				resourcesErr = err
				return
			}
			resource, err := parseSchemaResource(source)
			if err != nil {
				resourcesErr = fmt.Errorf("parse embedded %s: %w", filename, err)
				return
			}
			if resource.ID != embeddedSchemaFiles[filename] {
				resourcesErr = fmt.Errorf("embedded %s has $id %q, want %q", filename, resource.ID, embeddedSchemaFiles[filename])
				return
			}
			if _, duplicate := resources[resource.ID]; duplicate {
				resourcesErr = fmt.Errorf("duplicate embedded schema $id %q", resource.ID)
				return
			}
			resources[resource.ID] = resource
		}
	})
	return resources, resourcesErr
}

func publishedSchemas() (map[string]*jsonschema.Schema, error) {
	compiledOnce.Do(func() {
		allResources, err := embeddedSchemaResources()
		if err != nil {
			compiledErr = err
			return
		}
		compiled = make(map[string]*jsonschema.Schema, 3)
		for kind, rootID := range map[string]string{
			"Capability": CapabilitySchemaID,
			"Recipe":     RecipeSchemaID,
			"Space":      SpaceSchemaID,
		} {
			bundle, err := schemaBundle(rootID, allResources)
			if err != nil {
				compiledErr = fmt.Errorf("resolve %s schema bundle: %w", kind, err)
				return
			}
			compiler := jsonschema.NewCompiler()
			compiler.DefaultDraft(jsonschema.Draft2020)
			compiler.AssertFormat()
			compiler.UseLoader(rejectExternalSchemaLoader{})
			for _, resource := range bundle {
				document, err := jsonschema.UnmarshalJSON(bytes.NewReader(resource.Source))
				if err != nil {
					compiledErr = fmt.Errorf("parse %s from fixed bundle: %w", resource.ID, err)
					return
				}
				if err := compiler.AddResource(resource.ID, document); err != nil {
					compiledErr = fmt.Errorf("add %s to fixed bundle: %w", resource.ID, err)
					return
				}
			}
			compiled[kind], err = compiler.Compile(rootID)
			if err != nil {
				compiledErr = fmt.Errorf("compile %s schema by $id: %w", kind, err)
				return
			}
		}
	})
	return compiled, compiledErr
}

type rejectExternalSchemaLoader struct{}

func (rejectExternalSchemaLoader) Load(schemaURL string) (any, error) {
	return nil, fmt.Errorf("schema URL %q is outside the fixed bundle", schemaURL)
}

func validatePublishedSchema(data []byte, kind string) error {
	schemas, err := publishedSchemas()
	if err != nil {
		return err
	}
	schema, ok := schemas[kind]
	if !ok {
		return fmt.Errorf("no published schema for kind %q", kind)
	}
	instance, err := yamlJSONInstance(data)
	if err != nil {
		return err
	}
	if err := schema.Validate(instance); err != nil {
		return fmt.Errorf("does not match %s: %w", schema.Location, err)
	}
	return nil
}

// yamlJSONInstance retains object-member presence, explicit nulls and empty
// strings before decoding into Go structs, while rejecting YAML values that
// cannot be represented by the JSON data model used by JSON Schema.
func yamlJSONInstance(data []byte) (any, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var root yaml.Node
	if err := decoder.Decode(&root); err != nil {
		return nil, fmt.Errorf("decode YAML for schema validation: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return nil, errors.New("a manifest file must contain exactly one YAML document")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode trailing YAML for schema validation: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) != 1 {
		return nil, errors.New("manifest must contain one non-empty YAML document")
	}
	if err := validateJSONCompatibleYAML(root.Content[0], "$", false); err != nil {
		return nil, err
	}
	var value any
	if err := root.Content[0].Decode(&value); err != nil {
		return nil, fmt.Errorf("decode validated YAML document: %w", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("manifest is not JSON-compatible YAML: %w", err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("decode JSON-compatible manifest: %w", err)
	}
	return instance, nil
}

var (
	jsonIntegerLexeme     = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)$`)
	jsonNumberLexeme      = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)
	ambiguousPlainLiteral = regexp.MustCompile(`(?i)^(?:y|yes|n|no|on|off|null|~|\.nan|[+-]?\.inf)$`)
	ambiguousTimestamp    = regexp.MustCompile(`^[0-9]{4}-[0-9]{1,2}-[0-9]{1,2}(?:$|[Tt \t])`)
	ambiguousSexagesimal  = regexp.MustCompile(`^[+-]?[0-9][0-9_]*(?::[0-5]?[0-9])+(?:\.[0-9_]*)?$`)
)

func validateJSONCompatibleYAML(node *yaml.Node, path string, mappingKey bool) error {
	if node == nil {
		return fmt.Errorf("%s: YAML node is missing", path)
	}
	if node.Style&yaml.TaggedStyle != 0 {
		return yamlSubsetError(node, path, "explicit YAML tags are not supported")
	}
	if node.Anchor != "" || node.Kind == yaml.AliasNode || node.Alias != nil {
		return yamlSubsetError(node, path, "YAML anchors and aliases are not supported")
	}
	switch node.Kind {
	case yaml.MappingNode:
		if mappingKey {
			return yamlSubsetError(node, path, "mapping keys must be strings")
		}
		if len(node.Content)%2 != 0 {
			return yamlSubsetError(node, path, "mapping must contain key/value pairs")
		}
		seen := map[string]struct{}{}
		for index := 0; index < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			keyPath := path + ".<key>"
			if key.Tag == "!!merge" || key.Value == "<<" && key.Style == 0 {
				return yamlSubsetError(key, keyPath, "YAML merge keys are not supported")
			}
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return yamlSubsetError(key, keyPath, "mapping keys must be strings")
			}
			if err := validateJSONCompatibleYAML(key, keyPath, true); err != nil {
				return err
			}
			if _, duplicate := seen[key.Value]; duplicate {
				return yamlSubsetError(key, path+"."+key.Value, "duplicate mapping key")
			}
			seen[key.Value] = struct{}{}
			if err := validateJSONCompatibleYAML(value, path+"."+key.Value, false); err != nil {
				return err
			}
		}
		return nil
	case yaml.SequenceNode:
		if mappingKey {
			return yamlSubsetError(node, path, "mapping keys must be strings")
		}
		for index, item := range node.Content {
			if err := validateJSONCompatibleYAML(item, fmt.Sprintf("%s[%d]", path, index), false); err != nil {
				return err
			}
		}
		return nil
	case yaml.ScalarNode:
		return validateJSONCompatibleScalar(node, path, mappingKey)
	default:
		return yamlSubsetError(node, path, "unsupported YAML node")
	}
}

func validateJSONCompatibleScalar(node *yaml.Node, path string, mappingKey bool) error {
	if mappingKey && node.Tag != "!!str" {
		return yamlSubsetError(node, path, "mapping keys must be strings")
	}
	switch node.Tag {
	case "!!str":
		if node.Style == 0 && (ambiguousPlainLiteral.MatchString(node.Value) || ambiguousTimestamp.MatchString(node.Value) || ambiguousSexagesimal.MatchString(node.Value)) {
			return yamlSubsetError(node, path, fmt.Sprintf("ambiguous plain scalar %q must be quoted", node.Value))
		}
	case "!!bool":
		if node.Value != "true" && node.Value != "false" {
			return yamlSubsetError(node, path, "boolean scalars must use lowercase true or false")
		}
	case "!!null":
		if node.Value != "null" {
			return yamlSubsetError(node, path, "null scalars must use lowercase null")
		}
	case "!!int":
		if !jsonIntegerLexeme.MatchString(node.Value) {
			return yamlSubsetError(node, path, fmt.Sprintf("integer %q must use JSON number grammar", node.Value))
		}
	case "!!float":
		if !jsonNumberLexeme.MatchString(node.Value) {
			return yamlSubsetError(node, path, fmt.Sprintf("number %q must use JSON number grammar", node.Value))
		}
	default:
		return yamlSubsetError(node, path, fmt.Sprintf("YAML tag %q is outside the JSON-compatible subset", node.Tag))
	}
	return nil
}

func yamlSubsetError(node *yaml.Node, path, message string) error {
	return fmt.Errorf("%s at YAML line %d, column %d: %s", path, node.Line, node.Column, message)
}
