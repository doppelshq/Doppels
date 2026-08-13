package manifest

type ValidationOptions struct {
	Root      string
	CheckHost bool
	Host      Host
}

type ValidationResult struct {
	Catalog     *Catalog
	Diagnostics []Diagnostic
}

func Validate(documents []Loaded, options ValidationOptions) ValidationResult {
	catalog := NewCatalog(options.Root, documents)
	var diagnostics []Diagnostic
	for _, document := range documents {
		diagnostics = append(diagnostics, structuralDiagnostics(document)...)
	}
	diagnostics = append(diagnostics, semanticDiagnostics(catalog)...)
	if options.CheckHost {
		host := options.Host
		if host == nil {
			host = OSHost{}
		}
		diagnostics = append(diagnostics, hostDiagnostics(catalog, host)...)
	}
	SortDiagnostics(diagnostics)
	return ValidationResult{Catalog: catalog, Diagnostics: diagnostics}
}
