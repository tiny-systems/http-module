package openapi_client

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/davecgh/go-spew/spew"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/goccy/go-json"
	"github.com/swaggest/jsonschema-go"
	"github.com/tiny-systems/module/api/v1alpha1"
	"github.com/tiny-systems/module/module"
	"github.com/tiny-systems/module/registry"
)

const (
	ComponentName = "openapi_call"
	RequestPort   = "request"
	ResponsePort  = "response"
	ErrorPort     = "error"
)

type Context any

type Settings struct {
	OperationName    OperationName    `json:"operationName" tab:"Request" title:"Operation" description:"Operation name"`
	EnabledResponses EnabledResponses `json:"enabledResponses" tab:"Request" title:"Enabled HTTP responses" description:"Each response will be presented as a port, multiple choices."`
	Specification    string           `json:"specification" format:"textarea" title:"OpenAPI" description:"OpenAPI v3 specifications" required:"true" tab:"OpenAPI configuration"`
	EnableErrorPort  bool             `json:"enableErrorPort" required:"true" title:"Enable Error Port" tab:"General" description:"If error happen, error port will emit an error message"`
}

// EnabledResponses special type which can carry its value and possible options for enum values
type EnabledResponses struct {
	MultiEnum
}

// OperationName special type which can carry its value and possible options for enum values
type OperationName struct {
	Enum
}

type RequestMsg struct {
	component *Component
}

func (r RequestMsg) JSONSchema() (jsonschema.Schema, error) {
	s := jsonschema.Schema{}
	s.AddType(jsonschema.Object)

	if r.component.currentOperation == nil {
		return s, nil
	}

	op := r.component.currentOperation

	if len(op.Parameters) > 0 {
		var required []string

		parameters := (&jsonschema.Schema{}).WithType(jsonschema.Object.Type()).WithTitle("Parameters").ToSchemaOrBool()
		for _, p := range op.Parameters {

			if p.Value == nil {
				continue
			}

			param := p.Value
			// @todo add support of re-usable components

			paramSchema := &jsonschema.Schema{}
			parameters.TypeObjectEns().WithPropertiesItem(param.Name, paramSchema.ToSchemaOrBool())
		}
		if len(required) > 0 {
			parameters.TypeObjectEns().WithRequired(required...)
		}
		s.WithPropertiesItem("parameters", parameters)
	}

	d, err := s.MarshalJSON()
	if err != nil {
		return s, err
	}
	spew.Dump(string(d))
	return s, nil
}

type ResponseMsg struct {
	code      string
	component *Component
}

func (r ResponseMsg) JSONSchema() (jsonschema.Schema, error) {

	name := jsonschema.Schema{}
	name.AddType(jsonschema.String)

	return name, nil
}

type Error struct {
	Context Context `json:"context"`
	Error   string  `json:"error"`
}

type Request struct {
	Context Context    `json:"context" configurable:"true" title:"Context" description:"Arbitrary message to be send alongside with encoded message"`
	Request RequestMsg `json:"request" required:"true" title:"Request message" description:""`
}

type Response struct {
	Context  Context     `json:"context"`
	Response ResponseMsg `json:"response"`
}

type Component struct {
	settings Settings
	spec     *openapi3.T
	//

	currentOperation *openapi3.Operation
	//

	allOperations             []string
	allOperationsDescriptions []string
	//
	allServers []string
	//
	allResponsesCodes        []string
	allResponsesDescriptions []string
}

func (h *Component) GetInfo() module.ComponentInfo {
	return module.ComponentInfo{
		Name:        ComponentName,
		Description: "OpenAPI request",
		Info:        "Sends HTTP requests configured by openAPI specification",
		Tags:        []string{"openAPI", "client"},
	}
}

func (h *Component) Handle(ctx context.Context, handler module.Handler, port string, msg interface{}) any {

	switch port {
	case v1alpha1.SettingsPort:
		in, ok := msg.(Settings)
		if !ok {
			return fmt.Errorf("invalid settings")
		}

		err := h.discover(ctx, in)

		//
		h.settings.OperationName.Value = in.OperationName.Value
		h.settings.OperationName.Options = h.allOperations
		h.settings.OperationName.OptionLabels = h.allOperationsDescriptions

		h.settings.EnabledResponses.Value = filterStringsByAppearance(in.EnabledResponses.Value, h.allResponsesCodes)
		h.settings.EnabledResponses.Options = h.allResponsesCodes
		h.settings.EnabledResponses.OptionLabels = h.allResponsesDescriptions

		return err

	case RequestPort:
		in, ok := msg.(Request)
		if !ok {
			return fmt.Errorf("invalid input")
		}

		_, err := h.invoke(ctx, in.Request)
		if err != nil {
			if !h.settings.EnableErrorPort {
				return err
			}
			return handler(ctx, ErrorPort, Error{
				Context: in.Context,
				Error:   err.Error(),
			})
		}
		return handler(ctx, ResponsePort, Response{
			Response: ResponseMsg{},
			Context:  in.Context,
		})

	default:
		return fmt.Errorf("port %s is not supoprted", port)
	}
}

func (h *Component) invoke(ctx context.Context, msg any) ([]byte, error) {
	//
	return nil, nil
}

func (h *Component) Ports() []module.Port {

	ports := []module.Port{
		{
			Name:     RequestPort,
			Label:    "Request",
			Position: module.Left,
			Configuration: Request{
				Request: RequestMsg{
					component: h,
				},
			},
		},
		{
			Name:          v1alpha1.SettingsPort,
			Label:         "Settings",
			Configuration: h.settings,
		},
	}

	for _, r := range h.settings.EnabledResponses.Value {

		var (
			description = r
		)
		for idx, val := range h.settings.EnabledResponses.Options {
			if val != r {
				continue
			}
			description = h.settings.EnabledResponses.OptionLabels[idx]
		}

		ports = append(ports, module.Port{
			Name:     strings.ToTitle(r),
			Position: module.Right,
			Label:    description,
			Source:   true,
			Configuration: Response{
				Response: ResponseMsg{
					code:      r,
					component: h,
				},
			},
		})
	}

	if !h.settings.EnableErrorPort {
		return ports
	}

	return append(ports, module.Port{
		Position:      module.Bottom,
		Name:          ErrorPort,
		Label:         "Error",
		Source:        true,
		Configuration: Error{},
	})
}

func (h *Component) discover(ctx context.Context, settings Settings) error {

	if settings.Specification == "" {
		return fmt.Errorf("openAPI specification is empty")
	}

	loader := openapi3.NewLoader()
	spec, err := loader.LoadFromData([]byte(settings.Specification))
	if err != nil {
		return fmt.Errorf("failed to load OpenAPI spec: %w", err)
	}

	// Validate the specification
	if err := spec.Validate(ctx); err != nil {
		return fmt.Errorf("invalid OpenAPI spec: %w", err)
	}

	h.spec = spec

	var serverNames []string
	for _, s := range spec.Servers {
		serverNames = append(serverNames, s.URL)
	}
	sort.Strings(serverNames)
	h.allServers = serverNames

	var (
		operations                            []string
		operationDescriptions                 []string
		currentOperationResponseCodes         []string
		currentOperationResponsesDescriptions []string
	)

	pathsMap := spec.Paths.Map()

	var paths []string
	for path := range pathsMap {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	for _, path := range paths {
		pathItem := pathsMap[path]

		operationsMap := pathItem.Operations()

		var methods []string
		for method, _ := range operationsMap {
			methods = append(methods, method)
		}
		sort.Strings(methods)

		for _, method := range methods {
			op := operationsMap[method]

			opID := op.OperationID
			if opID == "" {
				// if no operation ID make it as METHOD PATH e.g. PUT /user
				opID = fmt.Sprintf("%s %s", method, path)
			}

			responsesMap := op.Responses.Map()

			var httpCodes []string
			for code := range responsesMap {
				httpCodes = append(httpCodes, code)
			}
			sort.Strings(httpCodes)
			// http codes of responses of currently selected operation

			for _, code := range httpCodes {
				resp := responsesMap[code]

				//@todo implement Ref re-use
				if resp.Value == nil {
					continue
				}

				var responseDescription = code
				if resp.Value.Description != nil {
					responseDescription = fmt.Sprintf("%s (%s)", responseDescription, *resp.Value.Description)
				}

				if settings.OperationName.Value != opID {
					continue
				}

				h.currentOperation = op
				// current operation selected
				// to generate currently selected all available responses (enum with titles)
				currentOperationResponseCodes = append(currentOperationResponseCodes, code)
				currentOperationResponsesDescriptions = append(currentOperationResponsesDescriptions, responseDescription)
			}

			opDescription := op.Description

			if opDescription == "" {
				opDescription = op.Summary
			}
			if opDescription != "" {
				opDescription = fmt.Sprintf("(%s)", opDescription)
			}
			opDescription = fmt.Sprintf("[%s] %s %s", method, op.OperationID, opDescription)

			// operations enum
			operationDescriptions = append(operationDescriptions, opDescription)
			operations = append(operations, op.OperationID)
		}
	}
	if len(operations) == 0 {
		return fmt.Errorf("no operations found")
	}

	h.allOperations = operations
	h.allOperationsDescriptions = operationDescriptions
	////

	if settings.OperationName.Value == "" {
		return fmt.Errorf("please select an operation")
	}

	if len(currentOperationResponseCodes) == 0 {
		return fmt.Errorf("operation has no responses")
	}

	h.allResponsesCodes = currentOperationResponseCodes
	h.allResponsesDescriptions = currentOperationResponsesDescriptions

	return nil
}

func deleteFromSlice(resp string, vals []string) []string {
	for idx, r := range vals {
		if r != resp {
			continue
		}
		return append(vals[:idx], vals[idx+1:]...)
	}
	return vals
}

func (h *Component) Instance() module.Component {
	return &Component{}
}

type MultiError struct {
	Errors []error
}

func (m MultiError) Error() string {
	var str = ""
	for _, err := range m.Errors {
		str += err.Error() + "\n"
	}
	return str
}

var _ error = (*MultiError)(nil)
var _ jsonschema.Exposer = (*OperationName)(nil)
var _ jsonschema.Exposer = (*EnabledResponses)(nil)

var _ json.Marshaler = (*OperationName)(nil)
var _ json.Unmarshaler = (*OperationName)(nil)
var _ json.Marshaler = (*EnabledResponses)(nil)
var _ json.Unmarshaler = (*EnabledResponses)(nil)

var _ jsonschema.Exposer = (*RequestMsg)(nil)
var _ jsonschema.Exposer = (*ResponseMsg)(nil)

//
//var _ json.Marshaler = (*ResponseMsg)(nil)
//var _ json.Unmarshaler = (*ResponseMsg)(nil)

//var _ json.Marshaler = (*RequestMsg)(nil)
//var _ json.Unmarshaler = (*RequestMsg)(nil)

var _ module.Component = (*Component)(nil)

func init() {
	registry.Register(&Component{})
}

func filterStringsByAppearance(source []string, filter []string) []string {
	// Create a map for O(1) lookup of filter strings
	filterMap := make(map[string]bool)
	for _, s := range filter {
		filterMap[s] = true
	}

	// Create result slice with initial capacity of source length
	result := make([]string, 0, len(source))

	// Filter strings that appear in the filter array
	for _, s := range source {
		if filterMap[s] {
			result = append(result, s)
		}
	}

	return result
}
