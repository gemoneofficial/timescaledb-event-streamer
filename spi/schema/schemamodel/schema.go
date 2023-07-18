package schemamodel

func Int8() SchemaBuilder {
	return NewSchemaBuilder(INT8)
}

func Int16() SchemaBuilder {
	return NewSchemaBuilder(INT16)
}

func Int32() SchemaBuilder {
	return NewSchemaBuilder(INT32)
}

func Int64() SchemaBuilder {
	return NewSchemaBuilder(INT64)
}

func Float32() SchemaBuilder {
	return NewSchemaBuilder(FLOAT32)
}

func Float64() SchemaBuilder {
	return NewSchemaBuilder(FLOAT64)
}

func Boolean() SchemaBuilder {
	return NewSchemaBuilder(BOOLEAN)
}

func String() SchemaBuilder {
	return NewSchemaBuilder(STRING)
}

func Bytes() SchemaBuilder {
	return NewSchemaBuilder(BYTES)
}
