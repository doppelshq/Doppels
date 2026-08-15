package manifest

// Published v1alpha1 schema identities are part of the binary contract. Each
// digest fixes the complete, versioned external-$ref bundle rather than only
// the root file, and never depends on a consumer checkout containing schemas.
const (
	CapabilitySchemaID     = "https://doppels.so/schemas/v1alpha1/capability.schema.json"
	CapabilitySchemaSHA256 = "ff3f2d681a8c724674d040a9e948fbb88710cabcbc37df46092f07f7324959d8"
	RecipeSchemaID         = "https://doppels.so/schemas/v1alpha1/recipe.schema.json"
	RecipeSchemaSHA256     = "5a0a4e86be1c5b4589e12d5851a5bef8c362814a199a4b8e3a4fad523a232f0f"
)
