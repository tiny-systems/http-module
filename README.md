# Tiny Systems HTTP module
HTTP clients and servers



### Build locally
```shell
go run cmd/main.go tools build --devkey abcd11111e --name github.com/tiny-systems/http-module --version v1.0.5 --platform-api-url http://localhost:8281
```

Run locally
```shell
 HOSTNAME=http-1 OTLP_DSN=http://test.token@localhost:2345 go run cmd/main.go run --name localsecond/http-module-v1 --namespace=tinysystems --version=1.0.5

```
