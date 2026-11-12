[![Tests](https://github.com/netascode/go-crosswork/actions/workflows/test.yml/badge.svg)](https://github.com/netascode/go-crosswork/actions/workflows/test.yml)

# go-crosswork

`go-crosswork` is a Go client library for Cisco Crosswork Network Controller. It features a simple, extensible API and [advanced JSON manipulation](#result-manipulation).

## Getting Started

### Installing

To start using `go-crosswork`, install Go and `go get`:

`$ go get -u github.com/netascode/go-crosswork`

### Basic Usage

```go
package main

import "github.com/netascode/go-crosswork"

func main() {
    client, _ := crosswork.NewClient("https://1.1.1.1:30603", "user", "pwd")

    res, _ := client.Get("/crosswork/inventory/v1/nodes/count")
    println(res.Get("number_of_nodes").String())
}
```

This will print something like:

```
33
```

#### Result manipulation

`crosswork.Res` uses GJSON to simplify handling JSON results. See the [GJSON](https://github.com/tidwall/gjson) documentation for more detail.

```go
res, _ := client.Post("/crosswork/inventory/v1/nodes/query", `{"limit":10}`)

for _, node := range res.Get("data").Array() {
    println(node.Get("host_name").String())
}
```

#### POST data creation

`crosswork.Body` is a wrapper for [SJSON](https://github.com/tidwall/sjson). SJSON supports a path syntax simplifying JSON creation.

```go
body := crosswork.Body{}.
    Set("data.0.profile", "example").
    Set("data.0.user_pass.0.user_name", "admin").
    Set("data.0.user_pass.0.password", "secret").
    Set("data.0.user_pass.0.type", "ROBOT_USERPASS_SSH")
client.Post("/crosswork/inventory/v1/credentials", body.Str, crosswork.NoLogPayload)
```

## Documentation

See the [documentation](https://godoc.org/github.com/netascode/go-crosswork) for more details.
