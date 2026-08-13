package execution

import "doppels.so/cli/internal/manifest"

func ReferenceCapability(definition manifest.CapabilityDefinition) DefinitionReference {
	return DefinitionReference{
		Name: definition.Value.Metadata.Name, Version: definition.Value.Metadata.Version,
		ManifestSHA256: definition.Source.SHA256,
		Schema:         SchemaReference{ID: manifest.CapabilitySchemaID, SHA256: manifest.CapabilitySchemaSHA256},
	}
}

func ReferenceRecipe(definition manifest.RecipeDefinition) DefinitionReference {
	return DefinitionReference{
		Name: definition.Value.Metadata.Name, Version: definition.Value.Metadata.Version,
		ManifestSHA256: definition.Source.SHA256,
		Schema:         SchemaReference{ID: manifest.RecipeSchemaID, SHA256: manifest.RecipeSchemaSHA256},
	}
}
