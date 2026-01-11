package openapi_client

import (
	"github.com/goccy/go-json"
	"github.com/swaggest/jsonschema-go"
)

// Enum special type which can carry its value and possible options for enum values
type Enum struct {
	Value        string
	Options      []string
	OptionLabels []string
}

// MarshalJSON treat like underlying Value string
func (r Enum) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.Value)
}

// UnmarshalJSON treat like underlying Value string
func (r *Enum) UnmarshalJSON(data []byte) error {
	return json.Unmarshal(data, &r.Value)
}

func (r Enum) JSONSchema() (jsonschema.Schema, error) {
	name := jsonschema.Schema{}
	name.AddType(jsonschema.String)
	name.WithDefault(r.Value)
	name.WithExtraPropertiesItem("shared", true)
	enums := make([]interface{}, len(r.Options))
	for k, v := range r.Options {
		enums[k] = v
	}
	name.WithEnum(enums...)
	if len(r.OptionLabels) > 0 {
		name.WithExtraPropertiesItem("enumTitles", r.OptionLabels)
	}
	//d, _ := name.MarshalJSON()
	//
	//fmt.Println(string(d))
	return name, nil
}

// MultiEnum special type which can carry its value and possible options for enum values
type MultiEnum struct {
	Value        []string
	Options      []string
	OptionLabels []string
}

// MarshalJSON treat like underlying Value string
func (r MultiEnum) MarshalJSON() ([]byte, error) {

	val := r.Value
	if val == nil {
		val = []string{}
	}
	return json.Marshal(val)
}

// UnmarshalJSON treat like underlying Value string
func (r *MultiEnum) UnmarshalJSON(data []byte) error {
	return json.Unmarshal(data, &r.Value)
}

func (r MultiEnum) JSONSchema() (jsonschema.Schema, error) {
	name := jsonschema.Schema{}
	name.AddType(jsonschema.Array)
	name.WithDefault(r.Value)
	name.WithItems(*(&jsonschema.Items{}).WithSchemaOrBool((&jsonschema.Schema{}).WithType(jsonschema.String.Type()).ToSchemaOrBool())).ToSchemaOrBool()

	name.WithExtraPropertiesItem("shared", true)
	enums := make([]interface{}, len(r.Options))
	for k, v := range r.Options {
		enums[k] = v
	}
	name.WithEnum(enums...)
	if len(r.OptionLabels) > 0 {
		name.WithExtraPropertiesItem("enumTitles", r.OptionLabels)
	}
	return name, nil
}
