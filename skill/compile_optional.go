package skill

func withoutOptional(value valueType) valueType {
	value.Optional = false
	return value
}

func optionalCompatible(actual, expected valueType) bool {
	return !actual.Optional || expected.Optional
}
