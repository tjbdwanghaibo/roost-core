package skill

// Host-facing RuntimeValue constructors. IntRuntimeValue takes a quantity of
// an unexported kind, which external Host implementations cannot name; these
// helpers build correctly-quantified read results from catalog data instead.

// ResourceRuntimeValue builds the value a Host returns for a ResourceRead.
func ResourceRuntimeValue(value int64) RuntimeValue {
	return IntRuntimeValue(value, quantityResourceAmount)
}

// AttributeRuntimeValue builds the value a Host returns for an
// AttributeRead, carrying the quantity the catalog declares for the handle
// (dimensionless when the handle is not cataloged).
func AttributeRuntimeValue(catalog GameplayCatalog, handle AttributeHandle, value int64) RuntimeValue {
	quantity := quantityDimensionless
	for _, entry := range catalog.Attributes.Entries {
		if entry.Handle == handle {
			quantity = entry.Quantity
			break
		}
	}
	return IntRuntimeValue(value, quantity)
}
