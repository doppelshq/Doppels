package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const schemaBundleDomain = "doppels.so/schema-bundle-sha256/v1\x00"

type schemaResource struct {
	ID       string
	Source   []byte
	Document any
}

func parseSchemaResource(source []byte) (*schemaResource, error) {
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(source))
	if err != nil {
		return nil, err
	}
	object, ok := document.(map[string]any)
	if !ok {
		return nil, errors.New("schema resource must be an object")
	}
	id, ok := object["$id"].(string)
	if !ok || id == "" {
		return nil, errors.New("schema resource must declare a non-empty $id")
	}
	parsed, err := url.Parse(id)
	if err != nil || !parsed.IsAbs() || parsed.Fragment != "" || parsed.String() != id {
		return nil, fmt.Errorf("schema resource has invalid $id %q", id)
	}
	return &schemaResource{ID: id, Source: append([]byte(nil), source...), Document: document}, nil
}

// schemaBundle resolves the complete external $ref closure for rootID. Every
// referenced resource must be supplied by the embedded, immutable schema set.
// External cycles are rejected so the bundle membership is always explicit.
func schemaBundle(rootID string, resources map[string]*schemaResource) ([]*schemaResource, error) {
	const (
		visiting = 1
		visited  = 2
	)
	state := make(map[string]int, len(resources))
	var bundle []*schemaResource
	var visit func(string) error
	visit = func(id string) error {
		resource, ok := resources[id]
		if !ok {
			return fmt.Errorf("schema bundle references unfixed external resource %q", id)
		}
		switch state[id] {
		case visiting:
			return fmt.Errorf("schema bundle contains an external $ref cycle at %q", id)
		case visited:
			return nil
		}
		state[id] = visiting
		references, err := externalSchemaReferences(resource)
		if err != nil {
			return err
		}
		for _, reference := range references {
			if err := visit(reference); err != nil {
				return err
			}
		}
		state[id] = visited
		bundle = append(bundle, resource)
		return nil
	}
	if err := visit(rootID); err != nil {
		return nil, err
	}
	sort.Slice(bundle, func(i, j int) bool { return bundle[i].ID < bundle[j].ID })
	return bundle, nil
}

func externalSchemaReferences(resource *schemaResource) ([]string, error) {
	base, err := url.Parse(resource.ID)
	if err != nil {
		return nil, err
	}
	references := map[string]struct{}{}
	var walk func(any, *url.URL) error
	walk = func(value any, currentBase *url.URL) error {
		switch typed := value.(type) {
		case map[string]any:
			nextBase := currentBase
			if rawID, present := typed["$id"]; present {
				id, ok := rawID.(string)
				if !ok || id == "" {
					return fmt.Errorf("schema %s contains a non-string or empty $id", resource.ID)
				}
				resolved, err := currentBase.Parse(id)
				if err != nil {
					return fmt.Errorf("schema %s contains invalid $id %q: %w", resource.ID, id, err)
				}
				nextBase = resolved
			}
			if rawReference, present := typed["$ref"]; present {
				reference, ok := rawReference.(string)
				if !ok || reference == "" {
					return fmt.Errorf("schema %s contains a non-string or empty $ref", resource.ID)
				}
				resolved, err := nextBase.Parse(reference)
				if err != nil {
					return fmt.Errorf("schema %s contains invalid $ref %q: %w", resource.ID, reference, err)
				}
				resolved.Fragment, resolved.RawFragment = "", ""
				dependency := resolved.String()
				if dependency != "" && dependency != resource.ID {
					references[dependency] = struct{}{}
				}
			}
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if err := walk(typed[key], nextBase); err != nil {
					return err
				}
			}
		case []any:
			for _, item := range typed {
				if err := walk(item, currentBase); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(resource.Document, base); err != nil {
		return nil, err
	}
	result := make([]string, 0, len(references))
	for reference := range references {
		result = append(result, reference)
	}
	sort.Strings(result)
	return result, nil
}

// schemaBundleSHA256 hashes schema identities and the digest of their exact
// source bytes. Length prefixes and a versioned domain separator make the
// byte stream unambiguous and prevent reuse in another hashing namespace.
func schemaBundleSHA256(rootID string, resources map[string]*schemaResource) (string, error) {
	bundle, err := schemaBundle(rootID, resources)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(schemaBundleDomain))
	writeBundleLength(hash, uint64(len(bundle)))
	for _, resource := range bundle {
		id := []byte(resource.ID)
		sourceDigest := sha256.Sum256(resource.Source)
		writeBundleLength(hash, uint64(len(id)))
		_, _ = hash.Write(id)
		writeBundleLength(hash, uint64(len(sourceDigest)))
		_, _ = hash.Write(sourceDigest[:])
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

type byteWriter interface {
	Write([]byte) (int, error)
}

func writeBundleLength(writer byteWriter, length uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], length)
	_, _ = writer.Write(encoded[:])
}

func schemaResourceIDs(resources []*schemaResource) string {
	ids := make([]string, len(resources))
	for index, resource := range resources {
		ids[index] = resource.ID
	}
	return strings.Join(ids, ",")
}
