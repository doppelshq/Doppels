package manifest

// Published v1alpha1 schema identities are part of the binary contract. Each
// digest fixes the complete, versioned external-$ref bundle rather than only
// the root file, and never depends on a consumer checkout containing schemas.
const (
	CapabilitySchemaID     = "https://doppels.so/schemas/v1alpha1/capability.schema.json"
	CapabilitySchemaSHA256 = "ff3f2d681a8c724674d040a9e948fbb88710cabcbc37df46092f07f7324959d8"
	RecipeSchemaID         = "https://doppels.so/schemas/v1alpha1/recipe.schema.json"
	RecipeSchemaSHA256     = "a2b7183baf504c875ab619e6443afcd9fb8eb3281ed65bc1d79fbc7548ff919b"
)
