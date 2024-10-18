package echo

import (
	"context"
	b64 "encoding/base64"
	"fmt"
	"github.com/tiny-systems/http-module/components/etc"
	"github.com/tiny-systems/module/module"
	"github.com/tiny-systems/module/registry"
	"strings"
)

const (
	ComponentName        = "basic-auth-header-parser"
	InPort        string = "in"
	OutPort       string = "out"
)

type Context any

type InMessage struct {
	Context Context      `json:"context" configurable:"true" required:"true" title:"Context" description:"Arbitrary message to be send further"`
	Headers []etc.Header `json:"headers" required:"true" description:"HTTP headers list"`
}

type OutMessage struct {
	Context  Context `json:"context"`
	Found    bool    `json:"found"`
	User     string  `json:"user"`
	Password string  `json:"password"`
}

type Component struct {
}

func (t *Component) Instance() module.Component {
	return &Component{}
}

func (t *Component) GetInfo() module.ComponentInfo {
	return module.ComponentInfo{
		Name:        ComponentName,
		Description: "Basic Auth Headers Parser",
		Info:        "Parses HTTP headers for Basic Authorization header",
		Tags:        []string{"HTTP", "Auth"},
	}
}

func (t *Component) Handle(ctx context.Context, handler module.Handler, _ string, msg interface{}) error {
	in, ok := msg.(InMessage)
	if !ok {
		return fmt.Errorf("msg type not inMessage")
	}

	for _, h := range in.Headers {
		if strings.ToLower(h.Key) != "authorization" {
			continue
		}
		if !strings.HasPrefix(h.Value, "Basic ") {
			continue
		}

		val := strings.TrimPrefix(h.Value, "Basic ")

		decodedValue, err := b64.StdEncoding.DecodeString(val)
		if err != nil {
			continue
		}

		decodedParts := strings.Split(string(decodedValue), ":")

		if len(decodedParts) < 2 {
			continue
		}

		return handler(ctx, OutPort, OutMessage{
			User:     decodedParts[0],
			Password: decodedParts[1],
			Found:    true,
			Context:  in.Context,
		})
	}

	return handler(ctx, OutPort, OutMessage{
		Context: in.Context,
	})
}

func (t *Component) Ports() []module.Port {
	return []module.Port{
		{
			Name:          InPort,
			Label:         "In",
			Source:        true,
			Configuration: InMessage{},
			Position:      module.Left,
		},
		{
			Name:          OutPort,
			Label:         "Out",
			Source:        false,
			Configuration: OutMessage{},
			Position:      module.Right,
		},
	}
}

var _ module.Component = (*Component)(nil)

func init() {
	registry.Register(&Component{})
}
