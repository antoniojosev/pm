# pm/client

Official Go client for the [pm](../README.md) daemon's HTTP API v1: ports and
processes on a development machine, with one owner for all of them.

    go get github.com/antoniojosev/pm/client

Imports only the standard library: no third-party package is compiled into your
binary. The API it speaks is documented in [`docs/api/openapi.yaml`](../docs/api/openapi.yaml).

    c := client.New("http://127.0.0.1:9797")
    lease, _ := c.Leases.Create(ctx, client.LeaseRequest{Owner: "wt-qa", Count: 2})
    unit, _ := c.Run(ctx, client.RunSpec{Name: "api-qa", Dir: dir, Cmd: cmd,
        Owner: "wt-qa", Ports: lease.Ports, Limits: client.Limits{Memory: "2G"}})
    logs, _ := c.Logs(ctx, "api-qa", 50)
    c.Owners.Delete(ctx, "wt-qa") // stops api-qa and releases the lease

Calls: `Version`, `Leases.Create/List/Delete`, `Run`, `Stop`, `Logs`,
`Owners.Stop/Delete`.

**API version.** The client speaks API `client.APIVersion` (1). Before its first
request it calls `Version`; a daemon that reports another generation, or has no
such endpoint, fails every call with a `*client.VersionError` (`errors.Is(err,
client.ErrIncompatible)`). `client.WithoutVersionCheck()` turns that off.

**Errors.** A refused request is a `*client.Error` with the HTTP `Status` and pm's
own `Message`. `Owners.Stop` and `Delete` return what they did even when they fail.

Runnable example: [`example_test.go`](example_test.go).
