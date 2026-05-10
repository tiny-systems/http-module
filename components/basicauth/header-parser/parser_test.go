package echo

import (
	"context"
	"fmt"
	"testing"

	"github.com/tiny-systems/http-module/components/etc"
	"github.com/tiny-systems/module/module"
)

func TestComponent_Handle(t1 *testing.T) {
	type args struct {
		ctx     context.Context
		handler module.Handler
		port    string
		msg     interface{}
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
	}{
		{
			name:    "Invalid message type",
			wantErr: true,
			args: args{
				ctx:  context.Background(),
				port: "",
				msg:  OutMessage{},
				handler: func(ctx context.Context, port string, data interface{}) module.Result {
					t1.Errorf("handler err")
					return module.Result{}
				},
			},
		},
		{
			name: "No headers",
			args: args{
				ctx:  context.Background(),
				port: "",
				msg:  InMessage{},
				handler: func(ctx context.Context, port string, data interface{}) module.Result {
					msg := data.(OutMessage)
					if msg.Found {
						return module.Fail(fmt.Errorf("should not found"))
					}
					return module.Result{}
				},
			},
		},
		{
			name:    "No auth headers",
			wantErr: true,
			args: args{
				ctx:  context.Background(),
				port: "",
				msg: []etc.Header{
					{
						Key:   "test",
						Value: "s",
					},
				},
				handler: func(ctx context.Context, port string, data interface{}) module.Result {
					msg := data.(OutMessage)
					if msg.Found {
						return module.Fail(fmt.Errorf("should not found"))
					}
					return module.Result{}
				},
			},
		},
		{
			name:    "Auth headers, wrong password",
			wantErr: true,
			args: args{
				ctx:  context.Background(),
				port: InPort,
				msg: InMessage{
					Headers: []etc.Header{
						{
							Key:   "Authorization",
							Value: "Basic dXNlcm5hbWUxOnBhc3N3b3JkMQ==",
						},
					},
				},
				handler: func(ctx context.Context, _ string, data interface{}) module.Result {
					msg, ok := data.(OutMessage)
					if !ok {
						return module.Fail(fmt.Errorf("invalid type"))
					}
					if !msg.Found {
						return module.Fail(fmt.Errorf("should found"))
					}
					if msg.User != "username1" {
						return module.Fail(fmt.Errorf("invalid user"))
					}
					if msg.Password != "password2" {
						return module.Fail(fmt.Errorf("invalid password"))
					}
					return module.Result{}
				},
			},
		},
		{
			name:    "Auth headers, all good",
			wantErr: false,
			args: args{
				ctx:  context.Background(),
				port: InPort,
				msg: InMessage{
					Headers: []etc.Header{
						{
							Key:   "Authorization",
							Value: "Basic dXNlcm5hbWUxOnBhc3N3b3JkMQ==",
						},
					},
				},
				handler: func(ctx context.Context, _ string, data interface{}) module.Result {
					msg, ok := data.(OutMessage)
					if !ok {
						return module.Fail(fmt.Errorf("invalid type"))
					}
					if !msg.Found {
						return module.Fail(fmt.Errorf("should found"))
					}
					if msg.User != "username1" {
						return module.Fail(fmt.Errorf("invalid user"))
					}
					if msg.Password != "password1" {
						return module.Fail(fmt.Errorf("invalid password"))
					}
					return module.Result{}
				},
			},
		},
	}
	for _, tt := range tests {
		t1.Run(tt.name, func(t1 *testing.T) {
			t := &Component{}
			r := t.Handle(tt.args.ctx, tt.args.handler, tt.args.port, tt.args.msg)
			if (r.Err() != nil) != tt.wantErr {
				t1.Errorf("Handle() error = %v, wantErr %v", r.Err(), tt.wantErr)
			}
		})
	}
}
