# Tiny Systems HTTP Module

HTTP server and client components for building web-facing automations.

## Components

| Component | Description |
|-----------|-------------|
| HTTP Server | Embedded HTTP server with configurable routes and TLS support |
| HTTP Client | Make outbound HTTP requests with full header and body control |
| Basic Auth Parser | Parse and validate HTTP Basic Authentication headers |
| PromQL Query | Run a PromQL query against any Prometheus-compatible API and get the matching series with their labels. Set a range for a trend rather than a single number. |
| LogQL Query | Search Loki for log lines across every pod at once, each carrying its stream labels. |

## Notes

**The metrics and logs components are addressed by URL, not by deployment.** An
in-cluster Prometheus or Loki and Grafana Cloud, Amazon Managed Prometheus,
Thanos, Mimir or VictoriaMetrics are the same component with a different base
URL; set the tenant header for the multi-tenant ones. Give the credential per
request from the trigger widget rather than storing it in settings.

A malformed query is permanent and will not be retried — it returns 400 every
time. 429 and 5xx are the backend asking for time, and are retryable. Both
components report `truncated`; a capped sample read as the whole picture is how
a wrong conclusion gets drawn confidently.

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

## Part of Tiny Systems

This module is part of the [Tiny Systems](https://github.com/tiny-systems) platform -- a visual flow-based automation engine running on Kubernetes.

## License

This module's source code is MIT-licensed. It depends on the [Tiny Systems Module SDK](https://github.com/tiny-systems/module) (BSL 1.1). See [LICENSE](LICENSE) for details.
