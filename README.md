# TinySystems HTTP Module

HTTP server and client components for building web-facing automations.

## Components

| Component | Description |
|-----------|-------------|
| HTTP Server | Embedded HTTP server with configurable routes and TLS support |
| HTTP Client | Make outbound HTTP requests with full header and body control |
| Basic Auth Parser | Parse and validate HTTP Basic Authentication headers |
| OpenAPI Request | Make HTTP calls driven by an OpenAPI/Swagger specification |

## Installation

```shell
helm repo add tinysystems https://tiny-systems.github.io/module/
helm install http-module tinysystems/tinysystems-operator \
  --set controllerManager.manager.image.repository=ghcr.io/tiny-systems/http-module
```

## Run locally

```shell
go run cmd/main.go run --name=http-module --namespace=tinysystems --version=1.0.0
```

## Part of TinySystems

This module is part of the [TinySystems](https://github.com/tiny-systems) platform -- a visual flow-based automation engine running on Kubernetes.

## License

This module's source code is MIT-licensed. It depends on the [TinySystems Module SDK](https://github.com/tiny-systems/module) (BSL 1.1). See [LICENSE](LICENSE) for details.
