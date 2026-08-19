package manifest

// Published v1alpha1 schema identities are part of the binary contract. Each
// digest fixes the complete, versioned external-$ref bundle rather than only
// the root file, and never depends on a consumer checkout containing schemas.
const (
	CapabilitySchemaID     = "https://doppels.so/schemas/v1alpha1/capability.schema.json"
	CapabilitySchemaSHA256 = "531ff668bef1930d271f98ddd4d53484dde28b7e3b770df009bb580d1e93967a"
	RecipeSchemaID         = "https://doppels.so/schemas/v1alpha1/recipe.schema.json"
	RecipeSchemaSHA256     = "14cb3191c7ba24b7895ad4f52eb57531a3fe76b4fe8c855eaf1246d28148fa66"
)
