package echo

import (
	"context"
	"fmt"
	"github.com/tiny-systems/http-module/components/etc"
	"github.com/tiny-systems/module/module"
	"testing"
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
				handler: func(ctx context.Context, port string, data interface{}) any {
					t1.Errorf("handler err")
					return nil
				},
			},
		},
		{
			name: "No headers",
			args: args{
				ctx:  context.Background(),
				port: "",
				msg:  InMessage{},
				handler: func(ctx context.Context, port string, data interface{}) any {
					msg := data.(OutMessage)
					if msg.Found {
						return fmt.Errorf("should not found")
					}
					return nil
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
				handler: func(ctx context.Context, port string, data interface{}) any {
					msg := data.(OutMessage)
					if msg.Found {
						return fmt.Errorf("should not found")
					}
					return nil
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
				handler: func(ctx context.Context, _ string, data interface{}) any {
					msg, ok := data.(OutMessage)
					if !ok {
						return fmt.Errorf("invalid type")
					}
					if !msg.Found {
						return fmt.Errorf("should found")
					}
					if msg.User != "username1" {
						return fmt.Errorf("invalid user")
					}
					if msg.Password != "password2" {
						return fmt.Errorf("invalid password")
					}
					return nil
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
				handler: func(ctx context.Context, _ string, data interface{}) any {
					msg, ok := data.(OutMessage)
					if !ok {
						return fmt.Errorf("invalid type")
					}
					if !msg.Found {
						return fmt.Errorf("should found")
					}
					if msg.User != "username1" {
						return fmt.Errorf("invalid user")
					}
					if msg.Password != "password1" {
						return fmt.Errorf("invalid password")
					}
					return nil
				},
			},
		},
	}
	for _, tt := range tests {
		t1.Run(tt.name, func(t1 *testing.T) {
			t := &Component{}
			if err := t.Handle(tt.args.ctx, tt.args.handler, tt.args.port, tt.args.msg); (err != nil) != tt.wantErr {
				t1.Errorf("Handle() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
