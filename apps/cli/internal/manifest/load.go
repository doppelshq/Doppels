package manifest

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Loaded struct {
	Path      string
	Directory string
	SHA256    string
	Document  Document
}

func Load(path string) (Loaded, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Loaded{}, err
	}
	document, err := Decode(data)
	if err != nil {
		return Loaded{}, fmt.Errorf("%s: %w", filepath.Clean(path), err)
	}
	return Loaded{
		Path:      filepath.Clean(path),
		Directory: filepath.Dir(filepath.Clean(path)),
		SHA256:    fmt.Sprintf("%x", sha256.Sum256(data)),
		Document:  document,
	}, nil
}

func Decode(data []byte) (Document, error) {
	var header TypeMeta
	if err := yaml.Unmarshal(data, &header); err != nil {
		return nil, fmt.Errorf("decode YAML: %w", err)
	}

	var document Document
	switch header.Kind {
	case "Capability":
		document = &Capability{}
	case "Recipe":
		document = &Recipe{}
	case "Space":
		document = &Space{}
	case "":
		return nil, errors.New("missing required field kind")
	default:
		return nil, fmt.Errorf("unsupported MVP kind %q", header.Kind)
	}
	if err := validatePublishedSchema(data, header.Kind); err != nil {
		return nil, fmt.Errorf("validate %s schema: %w", header.Kind, err)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(document); err != nil {
		return nil, fmt.Errorf("decode %s: %w", header.Kind, err)
	}

	var extra any
	err := decoder.Decode(&extra)
	if err == nil {
		return nil, errors.New("a manifest file must contain exactly one YAML document")
	}
	if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode trailing YAML: %w", err)
	}
	return document, nil
}
