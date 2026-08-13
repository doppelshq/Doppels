package manifest

import (
	"testing"
)

func TestPublishedSchemaDigestsMatchRepository(t *testing.T) {
	resources, err := embeddedSchemaResources()
	if err != nil {
		t.Fatal(err)
	}
	for id, expected := range map[string]string{
		CapabilitySchemaID: CapabilitySchemaSHA256,
		RecipeSchemaID:     RecipeSchemaSHA256,
	} {
		actual, err := schemaBundleSHA256(id, resources)
		if err != nil {
			t.Fatal(err)
		}
		if actual != expected {
			t.Fatalf("%s bundle digest = %s, update the binary contract from %s", id, actual, expected)
		}
	}
}
