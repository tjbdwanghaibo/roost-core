package skill

func quantitiesCompatible(actual, expected quantityKind) bool {
	return actual == quantityUnknown || expected == quantityUnknown || actual == expected
}

func quantityType(quantity quantityKind) valueType {
	return valueType{Base: valueKindInt, Quantity: quantity}
}
